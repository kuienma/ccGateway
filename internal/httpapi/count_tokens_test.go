package httpapi

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCountTokensClientCancellationDoesNotWriteGatewayError(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	server := NewServer(ServerConfig{APIKey: testGatewayKey, MaxBodyBytes: 1 << 20, StreamHeartbeatInterval: time.Hour, MaxConcurrentStreams: 1, StreamQueueTimeout: time.Second},
		resolverStub{"opus-5": "model-id"}, &runtimeStub{err: context.Canceled}, slog.New(slog.NewJSONHandler(&logs, nil)))
	response := performRequest(
		server, http.MethodPost, "/v1/messages/count_tokens",
		[]byte(`{"model":"opus-5","messages":[]}`),
		map[string]string{"x-api-key": testGatewayKey},
	)
	if response.Body.Len() != 0 {
		t.Fatalf("客户端取消后不应写错误响应: %s", response.Body.String())
	}
	if strings.Contains(logs.String(), `"level":"ERROR"`) || !strings.Contains(logs.String(), `"result":"client_canceled"`) {
		t.Fatalf("客户端取消日志级别或结果错误: %s", logs.String())
	}
}

func TestCountTokensUsesBedrockResult(t *testing.T) {
	t.Parallel()

	runtime := &runtimeStub{tokenCount: 123}
	server := newTestServer(runtime, resolverStub{"opus-4-8": "global.anthropic.claude-opus-4-8"})
	headers := map[string]string{"x-api-key": testGatewayKey}
	body := []byte(`{"model":"opus-4-8","messages":[{"role":"user","content":"hello"}]}`)
	response := performRequest(server, http.MethodPost, "/v1/messages/count_tokens", body, headers)

	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"input_tokens":123}` {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if runtime.modelID != "global.anthropic.claude-opus-4-8" {
		t.Fatalf("modelID = %q", runtime.modelID)
	}
}

func TestCountTokensLogsCompletionFields(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	server := NewServer(ServerConfig{APIKey: testGatewayKey, MaxBodyBytes: 1 << 20, StreamHeartbeatInterval: time.Hour, MaxConcurrentStreams: 4, StreamQueueTimeout: time.Second},
		resolverStub{"opus-5": "model-id"}, &runtimeStub{tokenCount: 12}, slog.New(slog.NewJSONHandler(&logs, nil)))
	response := performRequest(
		server,
		http.MethodPost,
		"/v1/messages/count_tokens",
		[]byte(`{"model":"opus-5","messages":[]}`),
		map[string]string{"x-api-key": testGatewayKey},
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	for _, expected := range []string{
		`"operation":"count_tokens"`,
		`"model_id":"model-id"`,
		`"result":"success"`,
		`"duration_ms":`,
	} {
		if !strings.Contains(logs.String(), expected) {
			t.Fatalf("日志缺少 %s: %s", expected, logs.String())
		}
	}
}
