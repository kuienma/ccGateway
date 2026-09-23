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
