package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/llm"
)

type resolverStub map[string]string

// Resolve 把登记的模型名解析为 bedrock provider 下的模型标识，
// 保持既有用例的字面量写法不变。
func (r resolverStub) Resolve(name string) (llm.Resolution, bool) {
	modelID, ok := r[name]
	if !ok {
		return llm.Resolution{}, false
	}
	return llm.Resolution{ModelID: modelID, Provider: llm.ProviderBedrock}, true
}

type runtimeStub struct {
	modelID      string
	body         []byte
	response     []byte
	streamChunks [][]byte
	tokenCount   int32
	err          error
}

func (r *runtimeStub) Invoke(_ context.Context, modelID string, body []byte) ([]byte, error) {
	r.modelID = modelID
	r.body = append([]byte(nil), body...)
	return r.response, r.err
}

func (r *runtimeStub) Stream(_ context.Context, modelID string, body []byte, emit func([]byte) error) error {
	r.modelID = modelID
	r.body = append([]byte(nil), body...)
	for _, chunk := range r.streamChunks {
		if err := emit(chunk); err != nil {
			return err
		}
	}
	return r.err
}

func (r *runtimeStub) CountTokens(_ context.Context, modelID string, body []byte) (int32, error) {
	r.modelID = modelID
	r.body = append([]byte(nil), body...)
	return r.tokenCount, r.err
}

const testGatewayKey = "test-gateway-key"

func newTestServer(runtime *runtimeStub, resolver resolverStub) *Server {
	return NewServer(ServerConfig{APIKey: testGatewayKey, MaxBodyBytes: 1 << 20, StreamHeartbeatInterval: time.Hour, MaxConcurrentStreams: 4, StreamQueueTimeout: time.Second},
		resolver, map[string]llm.Provider{llm.ProviderBedrock: runtime}, discardLogger())
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func performRequest(server *Server, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func assertErrorType(t *testing.T, body []byte, expected string) {
	t.Helper()
	var envelope struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("解析错误响应失败: %v", err)
	}
	if envelope.Error.Type != expected {
		t.Fatalf("error.type = %q, body = %s", envelope.Error.Type, body)
	}
}

func assertRequestID(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	var envelope struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if id := response.Header().Get("request-id"); id == "" || envelope.RequestID != id {
		t.Fatalf("错误响应 request ID 不关联: %q / %q", id, envelope.RequestID)
	}
}
