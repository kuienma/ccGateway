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
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "你是助手" {
		t.Fatalf("首条应为 system，实际 %#v", req.Messages[0])
	}
	if req.Messages[1].Role != "user" || req.Messages[1].Content != "你好" {
		t.Fatalf("Messages[1] = %#v", req.Messages[1])
	}
	if req.Messages[2].Role != "assistant" || req.Messages[2].Content != "在" {
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
	if req.Messages[0].Content != "第一段\n\n第二段" {
		t.Fatalf("多文本块应以空行连接，实际 %q", req.Messages[0].Content)
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
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "规则一\n\n规则二" {
		t.Fatalf("system 块数组应合并，实际 %#v", req.Messages[0])
	}
}

func TestBuildRequestRejectsImageBlock(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [{"role": "user", "content": [
			{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "iVBOR"}}
		]}]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if req != nil {
		t.Fatal("含图片的请求不应返回 ChatRequest")
	}
	if rejection == nil {
		t.Fatal("含图片的请求必须被拒绝，不能静默丢弃")
	}
	if rejection.Kind != "invalid_request_error" {
		t.Fatalf("Kind = %q", rejection.Kind)
	}
	if !strings.Contains(rejection.Message, "图片") {
		t.Fatalf("错误信息应说明不支持图片，实际 %q", rejection.Message)
	}
}

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
