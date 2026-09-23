package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/anthropic"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/ark"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/bedrock"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/config"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/httpapi"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/llm"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/modelcatalog"
)

const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute
)

func run(ctx context.Context, logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	models, err := modelcatalog.NewManager(cfg.ModelConfigFile)
	if err != nil {
		return err
	}

	runtime, err := newRuntime(ctx, cfg.AWSRegion)
	if err != nil {
		return err
	}
	providers, err := buildProviders(cfg, models, runtime)
	if err != nil {
		return err
	}
	server := newHTTPServer(cfg, models, providers, logger)
	logger.Info("Bedrock Anthropic 网关已启动", "address", cfg.ListenAddr, "region", cfg.AWSRegion)
	return runServer(ctx, server, models.Reload, cfg.ShutdownTimeout, logger)
}

// buildProviders 装配 provider 表。
//
// 装配条件是「有凭证」而不是「目录里现在用到了」：providers 表在启动时定死，
// 而 models.Reload 挂在 SIGHUP 上可以在运行期热加模型。若按目录内容决定是否装配，
// 启动时没有 ark 模型的部署在热加 ark 模型之后会永远拿到
// 「模型所属 provider 未启用: ark」，即使 ARK_API_KEY 早已配好，也只能靠重启恢复。
//
// 同时保留快速失败：目录里已经登记了 ark 模型却没有凭证时直接启动失败，
// 不留到首个请求才报错。
func buildProviders(cfg config.Config, models *modelcatalog.Manager, runtime bedrock.Runtime) (map[string]llm.Provider, error) {
	providers := map[string]llm.Provider{llm.ProviderBedrock: runtime}
	if cfg.ArkAPIKey == "" {
		if models.UsesProvider(llm.ProviderArk) {
			return nil, fmt.Errorf("模型配置登记了 ark 模型，必须设置 ARK_API_KEY")
		}
		return providers, nil
	}
	providers[llm.ProviderArk] = ark.NewClient(ark.Config{
		APIKey:  cfg.ArkAPIKey,
		BaseURL: cfg.ArkBaseURL,
		Timeout: cfg.ArkTimeout,
	})
	return providers, nil
}

func newRuntime(ctx context.Context, region string) (bedrock.Runtime, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(
		ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithHTTPClient(bedrock.NewHTTPClient()),
		awsconfig.WithRetryer(bedrock.NewRetryer),
	)
	if err != nil {
		return nil, err
	}
	return bedrock.NewClient(bedrockruntime.NewFromConfig(awsCfg)), nil
}

func newHTTPServer(cfg config.Config, models anthropic.ModelResolver, providers map[string]llm.Provider, logger *slog.Logger) *http.Server {
	api := httpapi.NewServer(httpapi.ServerConfig{
		APIKey:                  cfg.GatewayAPIKey,
		MaxBodyBytes:            cfg.MaxBodyBytes,
		StreamHeartbeatInterval: cfg.StreamHeartbeatInterval,
		MaxConcurrentStreams:    cfg.MaxConcurrentStreams,
		StreamQueueTimeout:      cfg.StreamQueueTimeout,
	},
		models, providers, logger)
	return &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
}
