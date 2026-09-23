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

	"github.com/zhshl/bedrock-anthropic-gateway/internal/anthropic"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/ark/translate"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/llm"
)

// 编译期钉住接口契约：Client 必须能装进 provider 表，两种错误都必须被 HTTP 层按
// StatusError 分类。
var (
	_ llm.Provider    = (*Client)(nil)
	_ llm.StatusError = (*APIError)(nil)
	_ llm.StatusError = (*rejectionError)(nil)
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewClient(Config{APIKey: "test-key", BaseURL: server.URL, Timeout: 5 * time.Second})
}

func TestInvokeTranslatesBothDirections(t *testing.T) {
	t.Parallel()

	var received map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("请求路径 = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &received); err != nil {
			t.Errorf("上游收到的 body 不是合法 JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r1","model":"m","choices":[{"finish_reason":"stop",
			"message":{"role":"assistant","content":"你好"}}],
			"usage":{"prompt_tokens":5,"completion_tokens":2}}`))
	})

	out, err := client.Invoke(context.Background(), "deepseek-v4-flash-ga-260731",
		[]byte(`{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("Invoke 失败: %v", err)
	}

	if received["model"] != "deepseek-v4-flash-ga-260731" {
		t.Fatalf("上游 model = %v", received["model"])
	}
	if received["stream"] != nil && received["stream"] != false {
		t.Fatalf("非流式请求不应带 stream=true，实际 %v", received["stream"])
	}

	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("返回值不是合法 JSON: %v", err)
	}
	if decoded["type"] != "message" {
		t.Fatalf("应返回 Anthropic 格式响应，实际 %#v", decoded)
	}
	content := decoded["content"].([]any)
	if content[0].(map[string]any)["text"] != "你好" {
		t.Fatalf("content = %#v", content)
	}
}

// TestInvokeFallsBackToRequestedModelWhenUpstreamOmitsModel 钉住 Invoke 与 BuildResponse
// 之间的**接线**，而不是 BuildResponse 这个函数本身。
//
// model 空值回落的全部生产价值都在 Invoke 里那一行实参上：把它写成 ""（重构漏传、
// 或有人以为该字段没用）产出的就是 "model":""。translate 包里那条用例测的是函数，
// 而本包其余用例的假上游全部硬编码 "model":"m"，回落分支在 client 层永远走不到，
// 结构上抓不到这类回归 —— 所以这条用例必须让上游**不回显** model。
func TestInvokeFallsBackToRequestedModelWhenUpstreamOmitsModel(t *testing.T) {
	t.Parallel()

	const requestedModel = "deepseek-v4-flash-ga-260731"
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 上游刻意不带 model 字段。
		_, _ = w.Write([]byte(`{"id":"r1","choices":[{"finish_reason":"stop",
			"message":{"role":"assistant","content":"你好"}}],
			"usage":{"prompt_tokens":5,"completion_tokens":2}}`))
	})

	out, err := client.Invoke(context.Background(), requestedModel,
		[]byte(`{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("Invoke 失败: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("返回值不是合法 JSON: %v", err)
	}
	if decoded["model"] != requestedModel {
		t.Fatalf("上游不回显 model 时，返回体的 model 应回落到请求的 %q，实际 %v",
			requestedModel, decoded["model"])
	}
}

func TestInvokeRejectsImageBeforeCallingUpstream(t *testing.T) {
	t.Parallel()

	called := false
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	body := []byte(`{"max_tokens":64,"messages":[{"role":"user","content":[
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"x"}}]}]}`)
	_, err := client.Invoke(context.Background(), "m", body)
	if err == nil {
		t.Fatal("含图片的请求必须报错")
	}
	if called {
		t.Fatal("转换阶段被拒绝的请求不应发往上游")
	}

	var statusErr llm.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("错误应实现 llm.StatusError，实际 %T", err)
	}
	if statusErr.Status() != http.StatusBadRequest {
		t.Fatalf("Status = %d，期望 400", statusErr.Status())
	}
	if statusErr.Kind() != "invalid_request_error" {
		t.Fatalf("Kind = %q", statusErr.Kind())
	}
}

func TestInvokeMapsUpstreamErrorStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		status   int
		wantKind string
	}{
		{"401", http.StatusUnauthorized, "authentication_error"},
		{"403", http.StatusForbidden, "permission_error"},
		{"429", http.StatusTooManyRequests, "rate_limit_error"},
		{"400", http.StatusBadRequest, "invalid_request_error"},
		{"500", http.StatusInternalServerError, "api_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":{"code":"x","message":"上游拒绝"}}`))
			})

			_, err := client.Invoke(context.Background(), "m",
				[]byte(`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
			if err == nil {
				t.Fatal("上游非 2xx 必须报错")
			}
			var statusErr llm.StatusError
			if !errors.As(err, &statusErr) {
				t.Fatalf("错误应实现 llm.StatusError，实际 %T", err)
			}
			if statusErr.Status() != tc.status {
				t.Fatalf("Status = %d，期望 %d", statusErr.Status(), tc.status)
			}
			if statusErr.Kind() != tc.wantKind {
				t.Fatalf("Kind = %q，期望 %q", statusErr.Kind(), tc.wantKind)
			}
			if !strings.Contains(statusErr.ClientMessage(), "上游拒绝") {
				t.Fatalf("ClientMessage 应含上游描述，实际 %q", statusErr.ClientMessage())
			}
		})
	}
}

func TestInvokePropagatesContextCancel(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Invoke(ctx, "m", []byte(`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应透传 context.Canceled，实际 %v", err)
	}
}

// TestInvokeAppliesTimeoutToWholeRequest 钉住 ARK_TIMEOUT 的语义：它必须约束非流式
// 请求的总时长。只挂 Transport.ResponseHeaderTimeout 是不够的 —— 响应头一到，
// 读 body 就再无期限，而 httpapi 侧没有任何 deadline，r.Context() 只在客户端断开时取消。
func TestInvokeAppliesTimeoutToWholeRequest(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 先把响应头发出去，绕过 ResponseHeaderTimeout，只留下读 body 这一段。
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	// Cleanup 是 LIFO：先放开 handler，再关服务器，避免 Close 等待挂住的 handler。
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	client := NewClient(Config{APIKey: "k", BaseURL: server.URL, Timeout: 50 * time.Millisecond})
	failed := make(chan error, 1)
	go func() {
		_, err := client.Invoke(context.Background(), "m",
			[]byte(`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
		failed <- err
	}()

	// 有界等待：没有总时长上限时 io.ReadAll 会一直挂着，这里要让用例快速失败
	// 而不是把整个 go test 拖到超时 panic。
	select {
	case err := <-failed:
		if err == nil {
			t.Fatal("上游迟迟不发完 body 时应因超时失败")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("应透传 context.DeadlineExceeded，实际 %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Config.Timeout 未约束非流式请求的总时长：响应头已到，读 body 无期限挂住")
	}
}

// TestInvokeNormalizesTrailingSlashInBaseURL 覆盖运维把 ARK_BASE_URL 写成带尾斜杠的
// 情形：拼出 //chat/completions 会让真实 Ark 返回 404。
func TestInvokeNormalizesTrailingSlashInBaseURL(t *testing.T) {
	t.Parallel()

	paths := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r1","model":"m","choices":[{"finish_reason":"stop",
			"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(server.Close)

	client := NewClient(Config{APIKey: "k", BaseURL: server.URL + "/", Timeout: 5 * time.Second})
	if _, err := client.Invoke(context.Background(), "m",
		[]byte(`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)); err != nil {
		t.Fatalf("Invoke 失败: %v", err)
	}
	if path := <-paths; path != completionsPath {
		t.Fatalf("请求路径 = %q，BaseURL 带尾斜杠时不应拼出双斜杠", path)
	}
}

// TestInvokeAttributesBadUpstreamBodyToServer 钉住错误归因：BuildResponse 的输入来自
// 上游响应体，客户端没有责任，所以这条路径绝不能映射成 4xx（那既是向客户端撒谎，
// 也会让上游吐坏数据的故障躲过告警）。
func TestInvokeAttributesBadUpstreamBodyToServer(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r1","model":"m","choices":[]}`))
	})

	_, err := client.Invoke(context.Background(), "m",
		[]byte(`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if err == nil {
		t.Fatal("上游响应体不可解释时必须报错")
	}
	var statusErr llm.StatusError
	if errors.As(err, &statusErr) && statusErr.Status() < http.StatusInternalServerError {
		t.Fatalf("上游响应体损坏应归因服务端，实际 Status = %d", statusErr.Status())
	}
}

// reasoningResponse 是一条带 reasoning_content 的非流式响应，形态取自真机。
const reasoningResponse = `{"id":"r1","model":"m","choices":[{"finish_reason":"stop",
	"message":{"role":"assistant","content":"晴","reasoning_content":"先查天气"}}],
	"usage":{"prompt_tokens":89,"completion_tokens":11}}`

// TestInvokeEmitsThinkingWhenRequested 钉住 Invoke 与 BuildResponse 之间的**接线**。
//
// 与流式那条同一条教训：translate 包里已有用例覆盖 BuildResponse 在
// includeThinking=true 时产出 thinking 块，但它直接调函数，绕开了 client.go 里那个
// 实参。把实参写回固定 false（或重构漏传）时 translate 包全绿，只有从客户端请求体
// 出发走完整条 Invoke 才能抓住。两条路径各要一条，缺一条就有半边没护栏。
func TestInvokeEmitsThinkingWhenRequested(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reasoningResponse))
	})

	out, err := client.Invoke(context.Background(), "m",
		[]byte(`{"max_tokens":64,"thinking":{"type":"enabled","budget_tokens":1024},
			"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("Invoke 失败: %v", err)
	}

	var decoded struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("返回值不是合法 JSON: %v", err)
	}
	if len(decoded.Content) == 0 {
		t.Fatal("响应不含任何内容块")
	}
	// 断言首块就是 thinking，同时锁住「thinking 排最前」这条协议要求。
	if decoded.Content[0].Type != "thinking" {
		t.Fatalf("首个内容块 = %q，客户端请求 thinking 时应是 thinking 块: %#v",
			decoded.Content[0].Type, decoded.Content)
	}
	// 正文也要对，空壳块说明 reasoning_content 没搬过来。
	if decoded.Content[0].Thinking != "先查天气" {
		t.Fatalf("thinking 正文 = %q，期望上游 reasoning_content 原样透出",
			decoded.Content[0].Thinking)
	}
	// text 块不能因为多了 thinking 而丢。
	if len(decoded.Content) < 2 || decoded.Content[1].Type != "text" {
		t.Fatalf("thinking 块之后应仍有 text 块: %#v", decoded.Content)
	}
}

// TestInvokeDoesNotEmitThinkingByDefault 是上面那条的反向配对：客户端没请求 thinking
// 时一个 thinking 块都不能有。缺了它，把接线改成恒 true 的变异不会被任何用例抓到。
func TestInvokeDoesNotEmitThinkingByDefault(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reasoningResponse))
	})

	out, err := client.Invoke(context.Background(), "m",
		[]byte(`{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("Invoke 失败: %v", err)
	}

	var decoded struct {
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("返回值不是合法 JSON: %v", err)
	}
	for _, block := range decoded.Content {
		if block.Type == "thinking" {
			t.Fatal("客户端未请求 thinking 时不应透出 thinking 块")
		}
	}
}

// TestCountTokensEstimatesLocally 钉住 CountTokens 纯本地估算：Ark 没有 count_tokens
// 端点，任何上游调用都会 404。
func TestCountTokensEstimatesLocally(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("CountTokens 不应发起上游请求：Ark 没有 count_tokens 端点")
	})

	body := []byte(`{"max_tokens":64,"messages":[{"role":"user","content":"北京今天天气怎么样"}]}`)
	count, err := client.CountTokens(context.Background(), "m", body)
	if err != nil {
		t.Fatalf("CountTokens 失败: %v", err)
	}
	if count <= 0 {
		t.Fatalf("估算值 = %d", count)
	}
	// 与 translate.EstimateTokens 的结果必须一致：中间做任何缩放/截断都会让
	// 上下文预算与真实估算脱钩。
	want, err := translate.EstimateTokens(body)
	if err != nil {
		t.Fatalf("EstimateTokens 失败: %v", err)
	}
	if count != want {
		t.Fatalf("CountTokens = %d，应与 translate.EstimateTokens 的 %d 一致", count, want)
	}
}

// TestCountTokensAttributesMalformedBodyToClient 钉住错误归因：请求体是客户端给的，
// 必须映射成 400 / invalid_request_error，而不是 5xx。
func TestCountTokensAttributesMalformedBodyToClient(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("CountTokens 不应发起上游请求")
	})

	_, err := client.CountTokens(context.Background(), "m", []byte(`[]`))
	if err == nil {
		t.Fatal("畸形请求体必须报错")
	}
	var statusErr llm.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("错误应实现 llm.StatusError，实际 %T", err)
	}
	if statusErr.Status() != http.StatusBadRequest {
		t.Fatalf("Status = %d，期望 400", statusErr.Status())
	}
	if statusErr.Kind() != anthropic.ErrorTypeInvalidRequest {
		t.Fatalf("Kind = %q，期望 %q", statusErr.Kind(), anthropic.ErrorTypeInvalidRequest)
	}
}

// TestRejectionKindMatchesAnthropicErrorType 把 translate 包内的 invalid_request_error
// 字面量与 anthropic.ErrorTypeInvalidRequest 钉在一起。translate 刻意零 import
// internal/anthropic 以保持纯函数独立，代价是两处常量没有编译期耦合；ark 是第一个
// 同时 import 两者的包，所以相等断言放在这里，避免哪天一侧被改动后错误类型静默分叉。
func TestRejectionKindMatchesAnthropicErrorType(t *testing.T) {
	t.Parallel()

	_, rejection := translate.BuildRequest("m", []byte(`不是 JSON`), false)
	if rejection == nil {
		t.Fatal("非法请求体应产生 Rejection")
	}
	if rejection.Kind != anthropic.ErrorTypeInvalidRequest {
		t.Fatalf("translate 的 Rejection.Kind = %q，与 anthropic.ErrorTypeInvalidRequest %q 不一致",
			rejection.Kind, anthropic.ErrorTypeInvalidRequest)
	}
}
