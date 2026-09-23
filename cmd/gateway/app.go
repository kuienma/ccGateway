package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/anthropic"
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
	providers := map[string]llm.Provider{llm.ProviderBedrock: runtime}
	server := newHTTPServer(cfg, models, providers, logger)
	logger.Info("Bedrock Anthropic 网关已启动", "address", cfg.ListenAddr, "region", cfg.AWSRegion)
	return runServer(ctx, server, models.Reload, cfg.ShutdownTimeout, logger)
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
