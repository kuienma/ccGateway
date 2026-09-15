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
	"sync"
	"testing"
	"time"

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

type blockingRuntime struct {
	runtimeStub
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type delayedCancelRuntime struct {
	runtimeStub
	started        chan struct{}
	cancelObserved chan struct{}
	allowExit      chan struct{}
	mu             sync.Mutex
	calls          int
}

func (r *delayedCancelRuntime) Stream(ctx context.Context, _ string, _ []byte, _ func([]byte) error) error {
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.mu.Unlock()
	if call > 1 {
		return nil
	}
	close(r.started)
	<-ctx.Done()
	close(r.cancelObserved)
	<-r.allowExit
	return ctx.Err()
}

func (r *delayedCancelRuntime) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type pacedStreamRuntime struct {
	runtimeStub
	interval time.Duration
	chunks   int
}

func (r *pacedStreamRuntime) Stream(ctx context.Context, _ string, _ []byte, emit func([]byte) error) error {
	for i := 0; i < r.chunks; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.interval):
		}
		if err := emit([]byte(`{"type":"content_block_delta"}`)); err != nil {
			return err
		}
	}
	return nil
}

func (r *blockingRuntime) Stream(ctx context.Context, modelID string, body []byte, _ func([]byte) error) error {
	r.modelID = modelID
	r.body = append([]byte(nil), body...)
	r.once.Do(func() { close(r.started) })
	select {
	case <-r.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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

func TestMessagesLogsCompletionFields(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	runtime := &runtimeStub{response: []byte(`{"type":"message","content":[]}`)}
	server := NewServer(
		testGatewayKey,
		1<<20,
		resolverStub{"opus-5": "model-id"},
		runtime,
		slog.New(slog.NewJSONHandler(&logs, nil)),
		time.Hour,
		4,
		time.Second,
	)
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

func TestStreamSendsHeartbeatWhileWaitingForBedrock(t *testing.T) {
	t.Parallel()

	runtime := &blockingRuntime{started: make(chan struct{}), release: make(chan struct{})}
	server := NewServer(
		testGatewayKey,
		1<<20,
		resolverStub{"opus-5": "model-id"},
		runtime,
		discardLogger(),
		5*time.Millisecond,
		1,
		20*time.Millisecond,
	)
	go func() {
		<-runtime.started
		time.Sleep(20 * time.Millisecond)
		close(runtime.release)
	}()

	response := performRequest(
		server,
		http.MethodPost,
		"/v1/messages",
		[]byte(`{"model":"opus-5","max_tokens":1,"messages":[],"stream":true}`),
		map[string]string{"x-api-key": testGatewayKey},
	)
	if !strings.Contains(response.Body.String(), ": ping\n\n") {
		t.Fatalf("SSE 缺少心跳: %q", response.Body.String())
	}
}

func TestStreamHeartbeatOnlyFiresWhileIdle(t *testing.T) {
	t.Parallel()

	runtime := &pacedStreamRuntime{interval: 5 * time.Millisecond, chunks: 5}
	server := NewServer(
		testGatewayKey, 1<<20, resolverStub{"opus-5": "model-id"}, runtime,
		discardLogger(), 20*time.Millisecond, 1, 20*time.Millisecond,
	)
	response := performRequest(
		server, http.MethodPost, "/v1/messages",
		[]byte(`{"model":"opus-5","max_tokens":1,"messages":[],"stream":true}`),
		map[string]string{"x-api-key": testGatewayKey},
	)
	if strings.Contains(response.Body.String(), ": ping\n\n") {
		t.Fatalf("持续收到上游事件时不应发送心跳: %q", response.Body.String())
	}
}

func TestStreamReturnsOverloadedWhenConcurrencySlotIsBusy(t *testing.T) {
	t.Parallel()

	runtime := &blockingRuntime{started: make(chan struct{}), release: make(chan struct{})}
	server := NewServer(
		testGatewayKey,
		1<<20,
		resolverStub{"opus-5": "model-id"},
		runtime,
		discardLogger(),
		time.Hour,
		1,
		5*time.Millisecond,
	)
	body := []byte(`{"model":"opus-5","max_tokens":1,"messages":[],"stream":true}`)
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- performRequest(server, http.MethodPost, "/v1/messages", body, map[string]string{"x-api-key": testGatewayKey})
	}()
	<-runtime.started

	second := performRequest(server, http.MethodPost, "/v1/messages", body, map[string]string{"x-api-key": testGatewayKey})
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", second.Code, second.Body.String())
	}
	assertErrorType(t, second.Body.Bytes(), anthropic.ErrorTypeOverloaded)
	close(runtime.release)
	<-firstDone
}

func TestStreamLogsClientCancellation(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	runtime := &blockingRuntime{started: make(chan struct{}), release: make(chan struct{})}
	server := NewServer(
		testGatewayKey,
		1<<20,
		resolverStub{"opus-5": "model-id"},
		runtime,
		slog.New(slog.NewJSONHandler(&logs, nil)),
		time.Hour,
		1,
		time.Second,
	)
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"opus-5","max_tokens":1,"messages":[],"stream":true}`),
	).WithContext(ctx)
	request.Header.Set("x-api-key", testGatewayKey)
	done := make(chan struct{})
	go func() {
		server.Handler().ServeHTTP(httptest.NewRecorder(), request)
		close(done)
	}()
	<-runtime.started
	cancel()
	<-done

	if !strings.Contains(logs.String(), `"result":"client_canceled"`) {
		t.Fatalf("日志未标记客户端取消: %s", logs.String())
	}
}

func TestStreamSlotRemainsOccupiedUntilRuntimeExits(t *testing.T) {
	t.Parallel()

	runtime := &delayedCancelRuntime{
		started: make(chan struct{}), cancelObserved: make(chan struct{}), allowExit: make(chan struct{}),
	}
	server := NewServer(
		testGatewayKey, 1<<20, resolverStub{"opus-5": "model-id"}, runtime,
		discardLogger(), time.Hour, 1, 5*time.Millisecond,
	)
	body := []byte(`{"model":"opus-5","max_tokens":1,"messages":[],"stream":true}`)
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)).WithContext(ctx)
	request.Header.Set("x-api-key", testGatewayKey)
	firstDone := make(chan struct{})
	go func() {
		server.Handler().ServeHTTP(httptest.NewRecorder(), request)
		close(firstDone)
	}()
	<-runtime.started
	cancel()
	<-runtime.cancelObserved

	second := performRequest(server, http.MethodPost, "/v1/messages", body, map[string]string{"x-api-key": testGatewayKey})
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("runtime 尚未退出时第二个请求 status = %d, body = %s", second.Code, second.Body.String())
	}
	if calls := runtime.callCount(); calls != 1 {
		t.Fatalf("runtime 调用次数 = %d，槽位提前释放", calls)
	}
	close(runtime.allowExit)
	<-firstDone
}

func TestCanceledWhileWaitingForStreamSlotIsNotOverloaded(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	runtime := &blockingRuntime{started: make(chan struct{}), release: make(chan struct{})}
	server := NewServer(
		testGatewayKey, 1<<20, resolverStub{"opus-5": "model-id"}, runtime,
		slog.New(slog.NewJSONHandler(&logs, nil)), time.Hour, 1, time.Second,
	)
	body := []byte(`{"model":"opus-5","max_tokens":1,"messages":[],"stream":true}`)
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- performRequest(server, http.MethodPost, "/v1/messages", body, map[string]string{"x-api-key": testGatewayKey})
	}()
	<-runtime.started

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)).WithContext(ctx)
	request.Header.Set("x-api-key", testGatewayKey)
	cancel()
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if strings.Contains(response.Body.String(), "overloaded_error") {
		t.Fatalf("客户端取消不应返回 overloaded: %s", response.Body.String())
	}
	if !strings.Contains(logs.String(), `"result":"client_canceled"`) {
		t.Fatalf("日志未标记客户端取消: %s", logs.String())
	}
	close(runtime.release)
	<-firstDone
}

func TestNonStreamingClientCancellationDoesNotWriteGatewayError(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	server := NewServer(
		testGatewayKey, 1<<20, resolverStub{"opus-5": "model-id"},
		&runtimeStub{err: context.Canceled}, slog.New(slog.NewJSONHandler(&logs, nil)),
		time.Hour, 1, time.Second,
	)
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

func TestCountTokensClientCancellationDoesNotWriteGatewayError(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	server := NewServer(
		testGatewayKey, 1<<20, resolverStub{"opus-5": "model-id"},
		&runtimeStub{err: context.Canceled}, slog.New(slog.NewJSONHandler(&logs, nil)),
		time.Hour, 1, time.Second,
	)
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

func TestNonStreamingUpstreamFailureWritesAndLogsGatewayError(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	server := NewServer(
		testGatewayKey, 1<<20, resolverStub{"opus-5": "model-id"},
		&runtimeStub{err: errors.New("upstream reset")}, slog.New(slog.NewJSONHandler(&logs, nil)),
		time.Hour, 1, time.Second,
	)
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
	server := NewServer(
		testGatewayKey,
		1<<20,
		resolverStub{"opus-5": "model-id"},
		&runtimeStub{tokenCount: 12},
		slog.New(slog.NewJSONHandler(&logs, nil)),
		time.Hour,
		4,
		time.Second,
	)
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

	server := NewServer(testGatewayKey, 8, resolverStub{}, &runtimeStub{}, discardLogger(), time.Hour, 4, time.Second)
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
	return NewServer(testGatewayKey, 1<<20, resolver, runtime, discardLogger(), time.Hour, 4, time.Second)
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
