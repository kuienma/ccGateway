package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/anthropic"
)

type resolverStub map[string]string

func (r resolverStub) Resolve(name string) (string, bool) {
	modelID, ok := r[name]
	return modelID, ok
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

func TestHealthDoesNotRequireAuthentication(t *testing.T) {
	t.Parallel()

	server := newTestServer(&runtimeStub{}, resolverStub{})
	response := performRequest(server, http.MethodGet, "/healthz", nil, nil)

	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("health response = %d %s", response.Code, response.Body.String())
	}
}

func TestMessagesRequiresGatewayAuthentication(t *testing.T) {
	t.Parallel()

	server := newTestServer(&runtimeStub{}, resolverStub{})
	response := performRequest(server, http.MethodPost, "/v1/messages", []byte(`{}`), nil)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	assertErrorType(t, response.Body.Bytes(), "authentication_error")
}

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

func TestMessagesAcceptsBearerTokenAndStreamsSSE(t *testing.T) {
	t.Parallel()

	runtime := &runtimeStub{streamChunks: [][]byte{
		[]byte(`{"type":"message_start","message":{"id":"msg_1"}}`),
		[]byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`),
		[]byte(`{"type":"message_stop"}`),
	}}
	server := newTestServer(runtime, resolverStub{"": "global.anthropic.claude-opus-5"})
	headers := map[string]string{"Authorization": "Bearer " + testGatewayKey}
	requestBody := []byte(`{"max_tokens":10,"messages":[],"stream":true}`)
	response := performRequest(server, http.MethodPost, "/v1/messages", requestBody, headers)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("Content-Type = %q", contentType)
	}
	for _, eventType := range []string{"message_start", "content_block_delta", "message_stop"} {
		if !strings.Contains(response.Body.String(), "event: "+eventType+"\n") {
			t.Fatalf("SSE 缺少 %s: %s", eventType, response.Body.String())
		}
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

func TestMessagesRejectsUnknownModel(t *testing.T) {
	t.Parallel()

	server := newTestServer(&runtimeStub{}, resolverStub{})
	headers := map[string]string{"x-api-key": testGatewayKey}
	response := performRequest(server, http.MethodPost, "/v1/messages", []byte(`{"model":"unknown"}`), headers)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	assertErrorType(t, response.Body.Bytes(), "not_found_error")
}

func TestMessagesEnforcesBodyLimit(t *testing.T) {
	t.Parallel()

	server := NewServer(testGatewayKey, 8, resolverStub{}, &runtimeStub{}, discardLogger())
	headers := map[string]string{"x-api-key": testGatewayKey}
	response := performRequest(server, http.MethodPost, "/v1/messages", bytes.Repeat([]byte("x"), 9), headers)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestClassifyUpstreamErrorMapsRateLimit(t *testing.T) {
	t.Parallel()

	err := &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusTooManyRequests}},
		Err:      &smithy.GenericAPIError{Code: "ThrottlingException", Message: "请求过于频繁"},
	}
	status, kind, message := classifyUpstreamError(err)
	if status != http.StatusTooManyRequests || kind != anthropic.ErrorTypeRateLimit {
		t.Fatalf("映射结果 = %d, %s", status, kind)
	}
	if message != "请求过于频繁" {
		t.Fatalf("message = %q", message)
	}
}

func TestClassifyUpstreamErrorFallsBackToBadGatewayForMissingStatus(t *testing.T) {
	t.Parallel()

	err := &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: 0}},
		Err:      errors.New("unexpected EOF"),
	}
	status, kind, message := classifyUpstreamError(err)
	if status != http.StatusBadGateway || kind != anthropic.ErrorTypeAPI {
		t.Fatalf("映射结果 = %d, %s", status, kind)
	}
	if message != "Bedrock 请求失败" {
		t.Fatalf("message = %q", message)
	}
}

const testGatewayKey = "test-gateway-key"

func newTestServer(runtime *runtimeStub, resolver resolverStub) *Server {
	return NewServer(testGatewayKey, 1<<20, resolver, runtime, discardLogger())
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
