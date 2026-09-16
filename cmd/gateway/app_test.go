package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/config"
)

func TestRunRejectsConfigurationBeforeStarting(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("GATEWAY_API_KEY", "test-key")
	if err := run(context.Background(), testLogger()); err == nil || !strings.Contains(err.Error(), "AWS_REGION") {
		t.Fatalf("error=%v", err)
	}
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("MODEL_CONFIG_FILE", filepath.Join(t.TempDir(), "missing.json"))
	if err := run(context.Background(), testLogger()); err == nil || !strings.Contains(err.Error(), "读取模型配置") {
		t.Fatalf("error=%v", err)
	}
}

func TestNewHTTPServerPreservesTransportPolicy(t *testing.T) {
	t.Parallel()
	cfg := config.Config{ListenAddr: "127.0.0.1:0", GatewayAPIKey: "key", MaxBodyBytes: 1024, StreamHeartbeatInterval: time.Second, MaxConcurrentStreams: 2, StreamQueueTimeout: time.Second}
	server := newHTTPServer(cfg, nil, nil, testLogger())
	if server.Addr != cfg.ListenAddr || server.ReadHeaderTimeout != 10*time.Second || server.IdleTimeout != 2*time.Minute || server.WriteTimeout != 0 || server.ReadTimeout != 0 || server.BaseContext != nil {
		t.Fatalf("HTTP transport policy changed: %+v", server)
	}
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("health=%d %s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`)))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("auth=%d", response.Code)
	}
}
