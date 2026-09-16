package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/anthropic"
)

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
	var expected strings.Builder
	for _, chunk := range runtime.streamChunks {
		event, err := streamEventType(chunk)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&expected, "event: %s\ndata: %s\n\n", event, chunk)
	}
	if response.Body.String() != expected.String() || !response.Flushed {
		t.Fatalf("SSE 顺序或 Flush 异常: %s", response.Body.String())
	}

}

func TestStreamSendsHeartbeatWhileWaitingForBedrock(t *testing.T) {
	t.Parallel()

	runtime := &blockingRuntime{started: make(chan struct{}), release: make(chan struct{})}
	server := NewServer(ServerConfig{APIKey: testGatewayKey, MaxBodyBytes: 1 << 20, StreamHeartbeatInterval: 5 * time.Millisecond, MaxConcurrentStreams: 1, StreamQueueTimeout: 20 * time.Millisecond},
		resolverStub{"opus-5": "model-id"}, runtime, discardLogger())
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

	runtime := &pacedStreamRuntime{interval: 50 * time.Millisecond, chunks: 5}
	server := NewServer(ServerConfig{APIKey: testGatewayKey, MaxBodyBytes: 1 << 20, StreamHeartbeatInterval: 200 * time.Millisecond, MaxConcurrentStreams: 1, StreamQueueTimeout: 20 * time.Millisecond},
		resolverStub{"opus-5": "model-id"}, runtime, discardLogger())
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
	server := NewServer(ServerConfig{APIKey: testGatewayKey, MaxBodyBytes: 1 << 20, StreamHeartbeatInterval: time.Hour, MaxConcurrentStreams: 1, StreamQueueTimeout: 5 * time.Millisecond},
		resolverStub{"opus-5": "model-id"}, runtime, discardLogger())
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
	server := NewServer(ServerConfig{APIKey: testGatewayKey, MaxBodyBytes: 1 << 20, StreamHeartbeatInterval: time.Hour, MaxConcurrentStreams: 1, StreamQueueTimeout: time.Second},
		resolverStub{"opus-5": "model-id"}, runtime, slog.New(slog.NewJSONHandler(&logs, nil)))
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
	server := NewServer(ServerConfig{APIKey: testGatewayKey, MaxBodyBytes: 1 << 20, StreamHeartbeatInterval: time.Hour, MaxConcurrentStreams: 1, StreamQueueTimeout: 5 * time.Millisecond},
		resolverStub{"opus-5": "model-id"}, runtime, discardLogger())
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
	server := NewServer(ServerConfig{APIKey: testGatewayKey, MaxBodyBytes: 1 << 20, StreamHeartbeatInterval: time.Hour, MaxConcurrentStreams: 1, StreamQueueTimeout: time.Second},
		resolverStub{"opus-5": "model-id"}, runtime, slog.New(slog.NewJSONHandler(&logs, nil)))
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

type callbackRuntime struct {
	runtimeStub
	stream func(context.Context, func([]byte) error) error
}

func (r *callbackRuntime) Stream(ctx context.Context, _ string, _ []byte, emit func([]byte) error) error {
	return r.stream(ctx, emit)
}

type failingStreamWriter struct{ *httptest.ResponseRecorder }

func (w failingStreamWriter) Write([]byte) (int, error) { return 0, errors.New("client write failed") }

func TestStreamWriteFailureWaitsForRuntimeBeforeReleasingSlot(t *testing.T) {
	t.Parallel()
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprint(malformed), func(t *testing.T) {
			canceled, allowExit := make(chan struct{}), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(allowExit) }) }
			defer release()
			runtime := &callbackRuntime{stream: func(ctx context.Context, emit func([]byte) error) error {
				chunk := []byte(`{"type":"message_start"}`)
				if malformed {
					chunk = []byte(`invalid`)
				}
				if err := emit(chunk); err != nil {
					return err
				}
				<-ctx.Done()
				close(canceled)
				<-allowExit
				return ctx.Err()
			}}
			server := NewServer(ServerConfig{APIKey: testGatewayKey, MaxBodyBytes: 1024, StreamHeartbeatInterval: time.Hour, MaxConcurrentStreams: 1, StreamQueueTimeout: time.Millisecond}, resolverStub{"": "id"}, runtime, discardLogger())
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"stream":true}`))
			request.Header.Set("x-api-key", testGatewayKey)
			var writer http.ResponseWriter = httptest.NewRecorder()
			if !malformed {
				writer = failingStreamWriter{httptest.NewRecorder()}
			}
			done := make(chan struct{})
			go func() { server.Handler().ServeHTTP(writer, request); close(done) }()
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("写失败未取消上游")
			}
			if err := server.acquireStream(context.Background()); !errors.Is(err, errStreamQueueTimeout) {
				t.Fatalf("上游未退出时槽位提前释放: %v", err)
			}
			release()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("上游退出后 handler 未返回")
			}
			if len(server.streamSlots) != 0 {
				t.Fatal("请求结束后槽位未释放")
			}
		})
	}
}

func TestStreamUpstreamErrorKeepsRequestIDAndDoesNotReplay(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	runtime := &runtimeStub{streamChunks: [][]byte{[]byte(`{"type":"message_start"}`)}, err: errors.New("upstream reset")}
	server := NewServer(ServerConfig{APIKey: testGatewayKey, MaxBodyBytes: 1024, StreamHeartbeatInterval: time.Hour, MaxConcurrentStreams: 1, StreamQueueTimeout: time.Second}, resolverStub{"": "id"}, runtime, slog.New(slog.NewJSONHandler(&logs, nil)))
	response := performRequest(server, http.MethodPost, "/v1/messages", []byte(`{"stream":true}`), map[string]string{"x-api-key": testGatewayKey})
	if response.Code != 200 || !response.Flushed {
		t.Fatalf("response=%+v", response)
	}
	prefix := `event: message_start` + "\ndata: " + `{"type":"message_start"}` + "\n\nevent: error\ndata: "
	if !strings.HasPrefix(response.Body.String(), prefix) {
		t.Fatalf("事件顺序错误: %s", response.Body.String())
	}
	var envelope anthropic.ErrorEnvelope
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(response.Body.String(), prefix))), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.RequestID != response.Header().Get("request-id") || envelope.Error.Type != anthropic.ErrorTypeAPI {
		t.Fatalf("error=%+v", envelope)
	}
	if strings.Count(logs.String(), `"msg":"请求完成"`) != 1 || !strings.Contains(logs.String(), `"result":"upstream_error"`) {
		t.Fatalf("logs=%s", logs.String())
	}
}
