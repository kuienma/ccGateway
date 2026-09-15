package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/bedrock"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/config"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/httpapi"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/modelcatalog"
)

const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(context.Background(), logger); err != nil {
		logger.Error("网关退出", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	models, err := modelcatalog.NewManager(cfg.ModelConfigFile)
	if err != nil {
		return err
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(
		ctx,
		awsconfig.WithRegion(cfg.AWSRegion),
		awsconfig.WithHTTPClient(bedrock.NewHTTPClient()),
		awsconfig.WithRetryer(bedrock.NewRetryer),
	)
	if err != nil {
		return err
	}
	runtime := bedrock.NewClient(bedrockruntime.NewFromConfig(awsCfg))
	api := httpapi.NewServer(
		cfg.GatewayAPIKey,
		cfg.MaxBodyBytes,
		models,
		runtime,
		logger,
		cfg.StreamHeartbeatInterval,
		cfg.MaxConcurrentStreams,
		cfg.StreamQueueTimeout,
	)
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}

	reloadSignals := make(chan os.Signal, 1)
	signal.Notify(reloadSignals, syscall.SIGHUP)
	defer signal.Stop(reloadSignals)
	go reloadModels(reloadSignals, models, logger)

	shutdownCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("Bedrock Anthropic 网关已启动", "address", cfg.ListenAddr, "region", cfg.AWSRegion)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-shutdownCtx.Done():
		gracefulCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		return server.Shutdown(gracefulCtx)
	}
}

func reloadModels(signals <-chan os.Signal, models *modelcatalog.Manager, logger *slog.Logger) {
	for range signals {
		if err := models.Reload(); err != nil {
			logger.Error("模型配置热更新失败，继续使用上一版", "error", err)
			continue
		}
		logger.Info("模型配置热更新成功")
	}
}
