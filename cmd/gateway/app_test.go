package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/config"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/llm"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/modelcatalog"
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

func TestBuildProvidersRequiresArkKeyOnlyWhenArkModelRegistered(t *testing.T) {
	t.Parallel()

	newManager := func(t *testing.T, body string) *modelcatalog.Manager {
		t.Helper()
		path := filepath.Join(t.TempDir(), "models.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("写入模型配置失败: %v", err)
		}
		manager, err := modelcatalog.NewManager(path)
		if err != nil {
			t.Fatalf("创建目录管理器失败: %v", err)
		}
		return manager
	}

	bedrockOnly := newManager(t, `{"default":"a","models":[{"id":"a"}]}`)
	providers, err := buildProviders(config.Config{}, bedrockOnly, nil, testLogger())
	if err != nil {
		t.Fatalf("纯 bedrock 目录不应要求 Ark 凭证: %v", err)
	}
	if _, exists := providers[llm.ProviderArk]; exists {
		t.Fatal("既无凭证又未登记 ark 模型时不应装配 ark provider")
	}
	if _, exists := providers[llm.ProviderBedrock]; !exists {
		t.Fatal("bedrock provider 必须始终装配")
	}

	// 有凭证就装配，与目录当前是否登记 ark 模型无关：models.Reload 挂在 SIGHUP 上，
	// 运维随时可能热加 ark 模型，而 providers 表是启动时定死的 map，
	// 若按目录内容决定是否装配，热加后的 ark 请求会永远拿到 500 且只能靠重启恢复。
	providers, err = buildProviders(config.Config{ArkAPIKey: "secret"}, bedrockOnly, nil, testLogger())
	if err != nil {
		t.Fatalf("有凭证时装配失败: %v", err)
	}
	if _, exists := providers[llm.ProviderArk]; !exists {
		t.Fatal("有 ARK_API_KEY 时应装配 ark provider，以便 SIGHUP 热加 ark 模型后立即可用")
	}

	withArk := newManager(t, `{"default":"a","models":[{"id":"a"},{"id":"b","provider":"ark"}]}`)
	if _, err := buildProviders(config.Config{}, withArk, nil, testLogger()); err == nil ||
		!strings.Contains(err.Error(), "ARK_API_KEY") {
		t.Fatalf("登记了 ark 模型却缺凭证时应启动失败: %v", err)
	}

	providers, err = buildProviders(
		config.Config{ArkAPIKey: "secret", ArkBaseURL: "https://example.test/api/v3", ArkTimeout: time.Minute},
		withArk, nil, testLogger())
	if err != nil {
		t.Fatalf("提供凭证后装配失败: %v", err)
	}
	if _, exists := providers[llm.ProviderArk]; !exists {
		t.Fatal("应装配 ark provider")
	}
	if _, exists := providers[llm.ProviderBedrock]; !exists {
		t.Fatal("bedrock provider 必须始终装配")
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
