package ark

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/llm"
)

func streamingHandler(payload string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/event-stream" {
			http.Error(w, "缺少 Accept 头", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(payload))
	}
}

func collectStream(t *testing.T, client *Client, body string) []map[string]any {
	t.Helper()
	var events []map[string]any
	err := client.Stream(context.Background(), "m", []byte(body), func(chunk []byte) error {
		var decoded map[string]any
		if err := json.Unmarshal(chunk, &decoded); err != nil {
			return err
		}
		events = append(events, decoded)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream 失败: %v", err)
	}
	return events
}

func TestStreamRequestsStreamingAndEmitsAnthropicEvents(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, streamingHandler(arkToolStream))
	events := collectStream(t, client, `{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)

	if len(events) == 0 {
		t.Fatal("未产出任何事件")
	}
	if events[0]["type"] != "message_start" {
		t.Fatalf("首个事件 = %v，期望 message_start", events[0]["type"])
	}
	if last := events[len(events)-1]["type"]; last != "message_stop" {
		t.Fatalf("末个事件 = %v，期望 message_stop", last)
	}

	// arkToolStream 是一次工具调用，应产出 tool_use 块与 tool_use 停止原因。
	var sawToolUse bool
	for _, event := range events {
		if event["type"] != "content_block_start" {
			continue
		}
		if event["content_block"].(map[string]any)["type"] == "tool_use" {
			sawToolUse = true
		}
	}
	if !sawToolUse {
		t.Fatal("应产出 tool_use 内容块")
	}

	// 每个事件都必须带 type —— httpapi/sse.go 依赖它生成 SSE 事件名。
	for i, event := range events {
		if _, ok := event["type"].(string); !ok {
			t.Fatalf("事件[%d] 缺少 type: %#v", i, event)
		}
	}
}

// TestStreamSendsStreamingFlagsUpstream 钉住 Stream 与 post 之间的**接线**。
//
// 假上游不管请求体写什么都会回吐 SSE，所以 `post(..., false)` 这个错误在其余用例里
// 完全不可见：事件序列照样正确，全绿。真实 Ark 收到 stream 缺省的请求会返回一整个
// JSON 响应体而不是 SSE 流，且没有 stream_options.include_usage 时末尾那个纯 usage
// chunk 不会下发（token 统计归零）。这两条只有在这里断言上游**实际收到的 body** 才能锁住。
func TestStreamSendsStreamingFlagsUpstream(t *testing.T) {
	t.Parallel()

	bodies := make(chan []byte, 1)
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		received, _ := io.ReadAll(r.Body)
		bodies <- received
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q，流式请求必须声明 text/event-stream", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(arkToolStream))
	})

	collectStream(t, client, `{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)

	// 用指针解码：stream 带 omitempty，传 false 时字段整个消失，指针能把
	// 「显式 false」和「压根没发」都报出来。
	var sent struct {
		Stream        *bool `json:"stream"`
		StreamOptions *struct {
			IncludeUsage *bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(<-bodies, &sent); err != nil {
		t.Fatalf("上游收到的 body 不是合法 JSON: %v", err)
	}
	if sent.Stream == nil || !*sent.Stream {
		t.Fatal("上游 body 未带 stream=true：Stream 必须以 stream=true 调 post，否则真实 Ark 不会开流")
	}
	if sent.StreamOptions == nil || sent.StreamOptions.IncludeUsage == nil || !*sent.StreamOptions.IncludeUsage {
		t.Fatal("上游 body 未带 stream_options.include_usage=true：末尾纯 usage chunk 不会下发，token 统计会归零")
	}
}

func TestStreamDoesNotEmitThinkingByDefault(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, streamingHandler(arkToolStream))
	events := collectStream(t, client, `{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)

	for _, event := range events {
		if event["type"] != "content_block_start" {
			continue
		}
		if event["content_block"].(map[string]any)["type"] == "thinking" {
			t.Fatal("客户端未请求 thinking 时不应产出 thinking 块")
		}
	}
}

func TestStreamReportsTruncatedStream(t *testing.T) {
	t.Parallel()

	// 有内容但没有 finish_reason，也没有 [DONE]。
	truncated := `data: {"choices":[{"delta":{"content":"半","role":"assistant"},"index":0}],"id":"s","model":"m"}

`
	client := newTestClient(t, streamingHandler(truncated))
	err := client.Stream(context.Background(), "m",
		[]byte(`{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`),
		func([]byte) error { return nil })
	if err == nil {
		t.Fatal("断流必须报错，不能当成正常结束")
	}
	if !strings.Contains(err.Error(), "finish_reason") {
		t.Fatalf("错误信息应指出缺少 finish_reason，实际 %v", err)
	}
}

func TestStreamMapsUpstreamErrorBeforeStreaming(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"限流"}}`))
	})

	err := client.Stream(context.Background(), "m",
		[]byte(`{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`),
		func([]byte) error { return nil })
	if err == nil {
		t.Fatal("上游 429 必须报错")
	}
	var statusErr llm.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("错误应实现 llm.StatusError，实际 %T", err)
	}
	if statusErr.Status() != http.StatusTooManyRequests {
		t.Fatalf("Status = %d", statusErr.Status())
	}
}

// TestStreamEmitsNothingWhenUpstreamStatusIsNot2xx 钉住「非 2xx 必须在**开流之前**映射
// 成错误」这个顺序本身。
//
// 上面那条用例名字里写着 BeforeStreaming，实际却锁不住顺序：把状态码检查挪到 scanSSE
// 之后它依然全绿 —— 那个 429 的响应体不是 SSE 形态，scanSSE 一个 chunk 都解析不出、
// 也不报错，随后 errorFromResponse 仍能凭 response.StatusCode 造出同样的 429。
// （已实测确认该变异存活。）
//
// 真正会出事的是上游以 SSE 形态下发错误：先开流再检查状态码，客户端会先收到 message_start
// 之类的半条消息、再收到 error 事件，既无法按可重试处理，也会让已经开始渲染的回答烂尾。
// 所以这里断言的是「一个事件都没产出」。
func TestStreamEmitsNothingWhenUpstreamStatusIsNot2xx(t *testing.T) {
	t.Parallel()

	// 响应体刻意用真机流式数据：先检查状态码的实现一个事件都不会产出；
	// 先开流的实现会把它当成正常流，整条消息都发给客户端之后才报错。
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(arkToolStream))
	})

	emitted := 0
	err := client.Stream(context.Background(), "m",
		[]byte(`{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`),
		func([]byte) error {
			emitted++
			return nil
		})
	if err == nil {
		t.Fatal("上游 429 必须报错")
	}
	if emitted != 0 {
		t.Fatalf("上游非 2xx 却产出了 %d 个事件：状态码检查必须发生在开流之前", emitted)
	}
	var statusErr llm.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("错误应实现 llm.StatusError，实际 %T", err)
	}
	if statusErr.Status() != http.StatusTooManyRequests {
		t.Fatalf("Status = %d", statusErr.Status())
	}
}

func TestStreamRejectsImageBeforeCallingUpstream(t *testing.T) {
	t.Parallel()

	called := false
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { called = true })

	body := []byte(`{"max_tokens":64,"messages":[{"role":"user","content":[
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"x"}}]}]}`)
	if err := client.Stream(context.Background(), "m", body, func([]byte) error { return nil }); err == nil {
		t.Fatal("含图片的流式请求必须报错")
	}
	if called {
		t.Fatal("被拒绝的请求不应发往上游")
	}
}

func TestStreamPropagatesEmitError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("客户端断开")
	client := newTestClient(t, streamingHandler(arkToolStream))
	err := client.Stream(context.Background(), "m",
		[]byte(`{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`),
		func([]byte) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("emit 错误应透传，实际 %v", err)
	}
}

// TestStreamDoesNotImposeTotalTimeoutOnSlowStream 钉住硬约束：Stream 绝不能像 Invoke
// 那样给 ctx 套 c.timeout。
//
// 这条缺陷在任何"短流"用例里都看不出来 —— 全部流式用例的假上游都是一次写完立即返回，
// 总时长远小于 Timeout，加不加 WithTimeout 都全绿。要等真实场景里模型输出时间超过
// ARK_TIMEOUT 才暴露，症状是"模型偶尔不说完话"，极难定位到网关。所以这里刻意让流的
// 总时长数倍于 Timeout，而响应头立即下发（绕过 Transport.ResponseHeaderTimeout）。
func TestStreamDoesNotImposeTotalTimeoutOnSlowStream(t *testing.T) {
	t.Parallel()

	const timeout = 80 * time.Millisecond
	head, tail, found := strings.Cut(arkToolStream, "data: "+doneMarker)
	if !found {
		t.Fatalf("arkToolStream 应含 %s 标记", doneMarker)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()
		_, _ = w.Write([]byte(head))
		flusher.Flush()
		// 模拟一个说得久的长回复：流仍在继续，只是下一段迟迟不来。
		select {
		case <-time.After(4 * timeout):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte("data: " + doneMarker + tail))
		flusher.Flush()
	}))
	t.Cleanup(server.Close)

	client := NewClient(Config{APIKey: "k", BaseURL: server.URL, Timeout: timeout})
	if err := client.Stream(context.Background(), "m",
		[]byte(`{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`),
		func([]byte) error { return nil }); err != nil {
		t.Fatalf("流的总时长超过 Config.Timeout 时不应失败（长回复会被硬截断），实际 %v", err)
	}
}

// TestStreamClosesBodyOnEmitError 钉住硬约束：post 的契约是「调用方负责关闭 Body」，
// Stream 必须在**所有**早退分支上关掉它。
//
// emit 回调报错是最早的一条早退分支，此时 body 远未读完。正确关闭会让 transport 判定
// 连接不可复用并断掉这条 TCP 连接，上游因此看到请求 context 被取消；漏关则连接一直挂着，
// 上游永远等不到取消 —— 这就是连接泄漏在测试里的可观测形态。
func TestStreamClosesBodyOnEmitError(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	canceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 先把请求体读到 EOF：net/http 只在此之后才开启探测对端关闭的后台读，
		// 也就是 r.Context() 能因连接断开而取消的前提。
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(arkToolStream))
		w.(http.Flusher).Flush()
		// 不返回：chunked 结束标记不下发，客户端那边的 body 就一定没读到 EOF，
		// 于是「关了」与「漏关了」在 transport 侧的行为可区分。
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	// Cleanup 是 LIFO：先放开 handler，再关服务器，避免 Close 等待挂住的 handler。
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	client := NewClient(Config{APIKey: "k", BaseURL: server.URL, Timeout: 5 * time.Second})
	sentinel := errors.New("客户端断开")
	err := client.Stream(context.Background(), "m",
		[]byte(`{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`),
		func([]byte) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("emit 错误应透传，实际 %v", err)
	}

	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("Stream 早退时未关闭响应体：上游连接被泄漏，请求 context 迟迟不被取消")
	}
}
