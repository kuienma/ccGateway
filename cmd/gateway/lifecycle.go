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
)

// httpService 仅包含生命周期需要的操作，允许测试独立于监听端口和 AWS。
type httpService interface {
	ListenAndServe() error
	Shutdown(context.Context) error
}

func runServer(ctx context.Context, server httpService, reload func() error, timeout time.Duration, logger *slog.Logger) error {
	shutdownCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP)
	defer signal.Stop(signals)
	return serve(shutdownCtx, server, signals, reload, timeout, logger)
}

func serve(ctx context.Context, server httpService, signals <-chan os.Signal, reload func() error, timeout time.Duration, logger *slog.Logger) error {
	reloadCtx, cancelReload := context.WithCancel(ctx)
	reloadDone := make(chan struct{})
	go func() {
		defer close(reloadDone)
		reloadModels(reloadCtx, signals, reload, logger)
	}()

	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.ListenAndServe() }()
	var result error
	select {
	case result = <-serverErrors:
		serverErrors = nil
		if errors.Is(result, http.ErrServerClosed) {
			result = nil
		}
	case <-ctx.Done():
	}

	// 从进入退出流程开始只计算一个期限，Shutdown 与任务回收共同使用。
	// 不把关闭信号传播到请求 context，允许在途请求在期限内正常完成。
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), timeout)
	defer cancelCleanup()
	cancelReload()
	if serverErrors != nil {
		result = server.Shutdown(cleanupCtx)
	}

	for serverErrors != nil || reloadDone != nil {
		select {
		case err := <-serverErrors:
			if !errors.Is(err, http.ErrServerClosed) {
				result = errors.Join(result, err)
			}
			serverErrors = nil
		case <-reloadDone:
			reloadDone = nil
		case <-cleanupCtx.Done():
			// 同步文件读取可能仍未返回；超时不意味着任务已回收。
			// 保留已经收到的服务或 Shutdown 错误，由 main 的错误路径退出进程。
			return errors.Join(result, cleanupCtx.Err())
		}
	}
	return result
}

func reloadModels(ctx context.Context, signals <-chan os.Signal, reload func() error, logger *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-signals:
			if !ok || ctx.Err() != nil {
				return
			}
			if err := reload(); err != nil {
				logger.Error("模型配置热更新失败，继续使用上一版", "error", err)
				continue
			}
			logger.Info("模型配置热更新成功")
		}
	}
}
