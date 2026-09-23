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

// buildProviders 按模型目录实际用到的 provider 装配实现。
// 目录里登记了 ark 模型却没有凭证时直接启动失败，不留到首个请求才报错。
func buildProviders(cfg config.Config, models *modelcatalog.Manager, runtime bedrock.Runtime) (map[string]llm.Provider, error) {
	providers := map[string]llm.Provider{llm.ProviderBedrock: runtime}
	if !models.UsesProvider(llm.ProviderArk) {
		return providers, nil
	}
	if cfg.ArkAPIKey == "" {
		return nil, fmt.Errorf("模型配置登记了 ark 模型，必须设置 ARK_API_KEY")
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
