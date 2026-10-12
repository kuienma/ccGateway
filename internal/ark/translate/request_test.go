package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildRequestLiftsSystemAndMapsBasics(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"system": "你是助手",
		"max_tokens": 512,
		"temperature": 0.5,
		"top_p": 0.9,
		"top_k": 40,
		"stop_sequences": ["\n\n"],
		"metadata": {"user_id": "u-1"},
		"messages": [
			{"role": "user", "content": "你好"},
			{"role": "assistant", "content": [{"type": "text", "text": "在"}]}
		]
	}`)

	req, rejection := BuildRequest("deepseek-v4-flash-ga-260731", raw, true)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}

	if req.Model != "deepseek-v4-flash-ga-260731" {
		t.Fatalf("Model = %q", req.Model)
	}
	if !req.Stream || req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
		t.Fatal("流式请求应带 stream_options.include_usage")
	}
	if req.MaxTokens != 512 {
		t.Fatalf("MaxTokens = %d", req.MaxTokens)
	}
	if req.Temperature == nil || *req.Temperature != 0.5 {
		t.Fatal("temperature 应透传")
	}
	if len(req.Stop) != 1 || req.Stop[0] != "\n\n" {
		t.Fatalf("Stop = %#v", req.Stop)
	}
	if req.User != "u-1" {
		t.Fatalf("User = %q", req.User)
	}

	if len(req.Messages) != 3 {
		t.Fatalf("消息数 = %d，期望 3（system + user + assistant）", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content.Text != "你是助手" {
		t.Fatalf("首条应为 system，实际 %#v", req.Messages[0])
	}
	if req.Messages[1].Role != "user" || req.Messages[1].Content.Text != "你好" {
		t.Fatalf("Messages[1] = %#v", req.Messages[1])
	}
	if req.Messages[2].Role != "assistant" || req.Messages[2].Content.Text != "在" {
		t.Fatalf("Messages[2] = %#v", req.Messages[2])
	}
}

func TestBuildRequestJoinsMultipleTextBlocks(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "第一段"},
			{"type": "text", "text": "第二段"}
		]}]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}
	if req.Messages[0].Content.Text != "第一段\n\n第二段" {
		t.Fatalf("多文本块应以空行连接，实际 %q", req.Messages[0].Content.Text)
	}
	if req.Stream {
		t.Fatal("非流式请求 Stream 应为 false")
	}
	if req.StreamOptions != nil {
		t.Fatal("非流式请求不应带 stream_options")
	}
}

func TestBuildRequestLiftsSystemBlockArray(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"system": [{"type": "text", "text": "规则一"}, {"type": "text", "text": "规则二"}],
		"max_tokens": 64,
		"messages": [{"role": "user", "content": "hi"}]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content.Text != "规则一\n\n规则二" {
		t.Fatalf("system 块数组应合并，实际 %#v", req.Messages[0])
	}
}

// 消息正文里的图片块从「一律拒绝」改成转成 OpenAI image_url 下发，
// 新行为的断言见 request_image_test.go。

func TestBuildRequestDropsUnsupportedFields(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"top_k": 40,
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "hi", "cache_control": {"type": "ephemeral"}}
		]}]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("top_k 与 cache_control 应被静默丢弃而非报错: %v", rejection)
	}

	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	for _, field := range []string{"top_k", "cache_control"} {
		if strings.Contains(string(encoded), field) {
			t.Fatalf("序列化结果不应包含 %s: %s", field, encoded)
		}
	}
}

func TestBuildRequestPassesThroughTopP(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"max_tokens":64,"top_p":0.25,"messages":[{"role":"user","content":"hi"}]}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}
	if req.TopP == nil {
		t.Fatal("top_p 应透传，实际为 nil")
	}
	if *req.TopP != 0.25 {
		t.Fatalf("TopP = %v，期望 0.25", *req.TopP)
	}
}

// TestBuildRequestKeepsExplicitZeroTemperature 锁定指针型字段的语义：
// 客户端显式传 temperature=0 时必须原样下发，不能被 omitempty 静默丢掉。
func TestBuildRequestKeepsExplicitZeroTemperature(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"max_tokens":64,"temperature":0,"top_p":0,"messages":[{"role":"user","content":"hi"}]}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}
	if req.Temperature == nil {
		t.Fatal("显式传入的 temperature=0 不应被丢弃")
	}
	if *req.Temperature != 0 {
		t.Fatalf("Temperature = %v，期望 0", *req.Temperature)
	}
	if req.TopP == nil || *req.TopP != 0 {
		t.Fatalf("显式传入的 top_p=0 不应被丢弃，实际 %#v", req.TopP)
	}

	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	for _, want := range []string{`"temperature":0`, `"top_p":0`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("序列化结果应包含 %s: %s", want, encoded)
		}
	}
}

func TestBuildRequestRejectsMalformedBody(t *testing.T) {
	t.Parallel()

	cases := []struct{ name, body string }{
		{"非对象", `[]`},
		{"messages 非数组", `{"max_tokens":1,"messages":"hi"}`},
		{"content 块缺 type", `{"max_tokens":1,"messages":[{"role":"user","content":[{"text":"hi"}]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, rejection := BuildRequest("m", []byte(tc.body), false); rejection == nil {
				t.Fatal("畸形请求体必须被拒绝")
			}
		})
	}
}
