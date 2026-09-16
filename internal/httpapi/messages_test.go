package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMessagesAcceptsAPIKeyAndForwardsNonStreamingResponse(t *testing.T) {
	t.Parallel()

	runtime := &runtimeStub{response: []byte(`{"id":"msg_1","type":"message","content":[]}`)}
	server := newTestServer(runtime, resolverStub{"opus-5": "global.anthropic.claude-opus-5"})
	headers := map[string]string{
		"x-api-key":      testGatewayKey,
		"anthropic-beta": "beta-a,beta-b",
	}
	requestBody := []byte(`{"model":"opus-5","max_tokens":10,"messages":[]}`)
	response := performRequest(server, http.MethodPost, "/v1/messages", requestBody, headers)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if response.Body.String() != string(runtime.response) {
		t.Fatalf("body = %s", response.Body.String())
	}
	if runtime.modelID != "global.anthropic.claude-opus-5" {
		t.Fatalf("modelID = %q", runtime.modelID)
	}
	var forwarded map[string]json.RawMessage
	if err := json.Unmarshal(runtime.body, &forwarded); err != nil {
		t.Fatalf("解析转发 body 失败: %v", err)
	}
	if _, exists := forwarded["model"]; exists {
		t.Fatal("转发 body 不应包含 model")
	}
	var betas []string
	if err := json.Unmarshal(forwarded["anthropic_beta"], &betas); err != nil || len(betas) != 2 {
		t.Fatalf("anthropic_beta = %#v, err = %v", betas, err)
	}
}

func TestMessagesLogsCompletionFields(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	runtime := &runtimeStub{response: []byte(`{"type":"message","content":[]}`)}
	server := NewServer(ServerConfig{APIKey: testGatewayKey, MaxBodyBytes: 1 << 20, StreamHeartbeatInterval: time.Hour, MaxConcurrentStreams: 4, StreamQueueTimeout: time.Second},
		resolverStub{"opus-5": "model-id"}, runtime, slog.New(slog.NewJSONHandler(&logs, nil)))
	response := performRequest(
		server,
		http.MethodPost,
		"/v1/messages",
		[]byte(`{"model":"opus-5","max_tokens":1,"messages":[]}`),
		map[string]string{"x-api-key": testGatewayKey},
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	for _, expected := range []string{
		`"operation":"messages"`,
		`"model_id":"model-id"`,
		`"stream":false`,
		`"result":"success"`,
		`"duration_ms":`,
	} {
		if !strings.Contains(logs.String(), expected) {
			t.Fatalf("日志缺少 %s: %s", expected, logs.String())
		}
	}
}

func TestNonStreamingClientCancellationDoesNotWriteGatewayError(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	server := NewServer(ServerConfig{APIKey: testGatewayKey, MaxBodyBytes: 1 << 20, StreamHeartbeatInterval: time.Hour, MaxConcurrentStreams: 1, StreamQueueTimeout: time.Second},
		resolverStub{"opus-5": "model-id"}, &runtimeStub{err: context.Canceled}, slog.New(slog.NewJSONHandler(&logs, nil)))
	response := performRequest(
		server, http.MethodPost, "/v1/messages",
		[]byte(`{"model":"opus-5","max_tokens":1,"messages":[]}`),
		map[string]string{"x-api-key": testGatewayKey},
	)
	if response.Body.Len() != 0 {
		t.Fatalf("客户端取消后不应写错误响应: %s", response.Body.String())
	}
	if strings.Contains(logs.String(), `"level":"ERROR"`) || !strings.Contains(logs.String(), `"result":"client_canceled"`) {
		t.Fatalf("客户端取消日志级别或结果错误: %s", logs.String())
	}
}

func TestNonStreamingUpstreamFailureWritesAndLogsGatewayError(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	server := NewServer(ServerConfig{APIKey: testGatewayKey, MaxBodyBytes: 1 << 20, StreamHeartbeatInterval: time.Hour, MaxConcurrentStreams: 1, StreamQueueTimeout: time.Second},
		resolverStub{"opus-5": "model-id"}, &runtimeStub{err: errors.New("upstream reset")}, slog.New(slog.NewJSONHandler(&logs, nil)))
	response := performRequest(
		server, http.MethodPost, "/v1/messages",
		[]byte(`{"model":"opus-5","max_tokens":1,"messages":[]}`),
		map[string]string{"x-api-key": testGatewayKey},
	)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(logs.String(), `"level":"ERROR"`) || !strings.Contains(logs.String(), `"result":"upstream_error"`) {
		t.Fatalf("上游失败日志级别或结果错误: %s", logs.String())
	}
}
