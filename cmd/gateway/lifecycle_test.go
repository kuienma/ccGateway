package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestReloadModelsStopsOnCancellation(t *testing.T) {
	signals := make(chan os.Signal)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { reloadModels(ctx, signals, nil, testLogger()); close(done) }()
	defer func() { cancel(); close(signals); <-done }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("父 context 取消后热更新任务仍等待信号")
	}
}

type serviceStub struct {
	listen   func() error
	shutdown func(context.Context) error
}

func (s serviceStub) ListenAndServe() error              { return s.listen() }
func (s serviceStub) Shutdown(ctx context.Context) error { return s.shutdown(ctx) }

func TestServeReturnsServerErrors(t *testing.T) {
	for _, want := range []error{errors.New("listen failed"), http.ErrServerClosed} {
		t.Run(want.Error(), func(t *testing.T) {
			server := serviceStub{listen: func() error { return want }, shutdown: func(context.Context) error { t.Error("不应关闭已退出服务"); return nil }}
			got := serve(context.Background(), server, nil, nil, time.Second, testLogger())
			if errors.Is(want, http.ErrServerClosed) {
				want = nil
			}
			if !errors.Is(got, want) {
				t.Fatalf("error = %v, want %v", got, want)
			}
		})
	}
}

func TestServeWaitsForReloadAndShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 1)
	reloadEntered, reloadRelease := make(chan struct{}), make(chan struct{})
	listenRelease, shutdownEntered := make(chan struct{}), make(chan struct{})
	server := serviceStub{
		listen: func() error { <-listenRelease; return http.ErrServerClosed },
		shutdown: func(ctx context.Context) error {
			if ctx.Err() != nil {
				t.Error("优雅关闭 context 不应已取消")
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Error("关闭缺少超时")
			}
			close(shutdownEntered)
			close(listenRelease)
			return nil
		},
	}
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, server, signals, func() error { close(reloadEntered); <-reloadRelease; return nil }, time.Second, testLogger())
	}()
	signals <- syscall.SIGHUP
	<-reloadEntered
	cancel()
	<-shutdownEntered
	select {
	case err := <-done:
		t.Fatalf("未等待正在执行的 reload: %v", err)
	default:
	}
	close(reloadRelease)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServePropagatesShutdownTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listening, stopped := make(chan struct{}), make(chan struct{})
	server := serviceStub{
		listen:   func() error { close(listening); <-stopped; return http.ErrServerClosed },
		shutdown: func(ctx context.Context) error { close(stopped); <-ctx.Done(); return ctx.Err() },
	}
	done := make(chan error, 1)
	go func() { done <- serve(ctx, server, nil, nil, time.Millisecond, testLogger()) }()
	<-listening
	cancel()
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
}

func TestReloadModelsContinuesAfterFailureAndStopsOnClosedChannel(t *testing.T) {
	signals := make(chan os.Signal, 2)
	signals <- syscall.SIGHUP
	signals <- syscall.SIGHUP
	close(signals)
	calls := 0
	var logs bytes.Buffer
	reloadModels(context.Background(), signals, func() error {
		calls++
		if calls == 1 {
			return errors.New("invalid catalog")
		}
		return nil
	}, slog.New(slog.NewJSONHandler(&logs, nil)))
	if calls != 2 || !strings.Contains(logs.String(), "热更新失败") || !strings.Contains(logs.String(), "热更新成功") {
		t.Fatalf("calls=%d logs=%s", calls, logs.String())
	}
}

func TestRunServerStartupFailure(t *testing.T) {
	want := errors.New("bind failed")
	err := runServer(context.Background(), serviceStub{listen: func() error { return want }}, nil, time.Second, testLogger())
	if !errors.Is(err, want) {
		t.Fatal(err)
	}
}

// 使用独立监听器，父取消只停止接收新请求，正在处理的请求继续完成。
func TestRunServerGracefullyDrainsRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	entered, release, shutdownEntered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		if r.Context().Err() != nil {
			t.Error("父取消不应取消在途请求")
		}
		_, _ = io.WriteString(w, "completed")
	})}
	defer server.Close()
	service := serviceStub{
		listen:   func() error { return server.Serve(listener) },
		shutdown: func(ctx context.Context) error { close(shutdownEntered); return server.Shutdown(ctx) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runServer(ctx, service, nil, time.Second, testLogger()) }()
	responseDone := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 2 * time.Second}
		response, err := client.Get("http://" + listener.Addr().String())
		if err == nil {
			defer response.Body.Close()
			body, readErr := io.ReadAll(response.Body)
			err = readErr
			if string(body) != "completed" {
				err = fmt.Errorf("body = %q", body)
			}
		}
		responseDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("请求未进入")
	}
	cancel()
	<-shutdownEntered
	select {
	case err := <-done:
		t.Fatalf("在途请求结束前退出: %v", err)
	default:
	}
	unblock()
	if err := <-responseDone; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// 同步 reload 无法被 context 中断，但不能让退出等待超过同一轮关闭期限。
func TestServeBoundsBlockedReload(t *testing.T) {
	for _, mode := range []string{"shutdown_success", "shutdown_timeout", "server_failure", "shutdown_failure"} {
		t.Run(mode, func(t *testing.T) {
			const timeout = 250 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			signals := make(chan os.Signal, 1)
			reloadEntered, reloadRelease, reloadExited := make(chan struct{}), make(chan struct{}), make(chan struct{})
			listenRelease, shutdownExpired := make(chan struct{}), make(chan struct{})
			var stopOnce sync.Once
			stopListening := func() { stopOnce.Do(func() { close(listenRelease) }) }
			serverErr, shutdownErr := errors.New("listen failed"), errors.New("shutdown failed")
			server := serviceStub{
				listen: func() error {
					<-listenRelease
					if mode == "server_failure" {
						return serverErr
					}
					return http.ErrServerClosed
				},
				shutdown: func(ctx context.Context) error {
					stopListening()
					if mode == "shutdown_timeout" {
						<-ctx.Done()
						close(shutdownExpired)
						return errors.Join(shutdownErr, ctx.Err())
					}
					if mode == "shutdown_failure" {
						return shutdownErr
					}
					return nil
				},
			}
			done, exited := make(chan error, 1), make(chan struct{})
			go func() {
				defer close(exited)
				done <- serve(ctx, server, signals, func() error {
					close(reloadEntered)
					<-reloadRelease
					close(reloadExited)
					return nil
				}, timeout, testLogger())
			}()
			t.Cleanup(func() {
				cancel()
				stopListening()
				close(reloadRelease)
				<-reloadExited
				<-exited
			})
			signals <- syscall.SIGHUP
			<-reloadEntered
			if mode == "server_failure" {
				stopListening()
			} else {
				cancel()
			}

			wait := time.Second
			if mode == "shutdown_timeout" {
				// Shutdown 已耗尽同一个期限；清理不得再获得一轮 timeout。
				<-shutdownExpired
				wait = 100 * time.Millisecond
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("阻塞 reload 退出错误 = %v，缺少 DeadlineExceeded", err)
				}
				if mode == "server_failure" && !errors.Is(err, serverErr) {
					t.Fatalf("原始服务错误丢失: %v", err)
				}
				if (mode == "shutdown_timeout" || mode == "shutdown_failure") && !errors.Is(err, shutdownErr) {
					t.Fatalf("原始 Shutdown 错误丢失: %v", err)
				}
			case <-time.After(wait):
				t.Fatal("关闭期限耗尽后仍等待阻塞 reload，或清理重新获得一轮 timeout")
			}
		})
	}
}

func TestServePreservesShutdownAndServerErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listening, stopped := make(chan struct{}), make(chan struct{})
	serverErr, shutdownErr := errors.New("serve failed"), errors.New("shutdown failed")
	server := serviceStub{
		listen:   func() error { close(listening); <-stopped; return serverErr },
		shutdown: func(context.Context) error { close(stopped); return shutdownErr },
	}
	done := make(chan error, 1)
	go func() { done <- serve(ctx, server, nil, nil, time.Second, testLogger()) }()
	<-listening
	cancel()
	if err := <-done; !errors.Is(err, serverErr) || !errors.Is(err, shutdownErr) {
		t.Fatalf("原始服务或 Shutdown 错误丢失: %v", err)
	}
}
