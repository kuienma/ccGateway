package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// 图片块的断言一律落在**序列化后的 wire format** 上，而不是中间表示：
// 真正要保证的契约是「发给 Ark 的字节」，而 content 既要能是字符串又要能是
// parts 数组，内部表示换一次实现就会让断言失效，wire format 不会。

func TestBuildRequestConvertsBase64ImageToImageURL(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "这是什么"},
			{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "iVBOR"}}
		]}]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("图片请求不应被拒绝: %v", rejection)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("应产出 1 条消息，实际 %d", len(req.Messages))
	}

	const want = `[{"type":"text","text":"这是什么"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBOR"}}]`
	assertContentJSON(t, req.Messages[0].Content, want)
}

func TestBuildRequestConvertsURLImageToImageURL(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [{"role": "user", "content": [
			{"type": "image", "source": {"type": "url", "url": "https://example.com/a.png"}}
		]}]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("url 图片不应被拒绝: %v", rejection)
	}

	const want = `[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]`
	assertContentJSON(t, req.Messages[0].Content, want)
}

// TestBuildRequestPreservesTextImageOrder 图文顺序必须与客户端给的块顺序一致：
// 「这张图上面那段文字」之类的指代依赖顺序，重排会改变语义。
func TestBuildRequestPreservesTextImageOrder(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [{"role": "user", "content": [
			{"type": "image", "source": {"type": "url", "url": "https://example.com/1.png"}},
			{"type": "text", "text": "中间"},
			{"type": "image", "source": {"type": "url", "url": "https://example.com/2.png"}}
		]}]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}

	const want = `[{"type":"image_url","image_url":{"url":"https://example.com/1.png"}},` +
		`{"type":"text","text":"中间"},` +
		`{"type":"image_url","image_url":{"url":"https://example.com/2.png"}}]`
	assertContentJSON(t, req.Messages[0].Content, want)
}

// TestBuildRequestKeepsPlainTextContentAsString 是回归锁：不含图片的请求，
// content 必须仍然序列化成**字符串**而不是 parts 数组。
// 放开图片是在既有线上格式之外加一条新路径，不是把所有请求都改成新格式。
func TestBuildRequestKeepsPlainTextContentAsString(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"system": "系统提示",
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "第一段"},
			{"type": "text", "text": "第二段"}
		]}]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("应产出 system + user 两条消息，实际 %d", len(req.Messages))
	}

	assertContentJSON(t, req.Messages[0].Content, `"系统提示"`)
	assertContentJSON(t, req.Messages[1].Content, `"第一段\n\n第二段"`)
}

// TestBuildRequestKeepsEmptyContentAsString 空 content 也必须是字符串：
// 原实现会为空消息下发 "" 以维持 user/assistant 交替，换成 [] 可能被上游判成非法。
func TestBuildRequestKeepsEmptyContentAsString(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [{"role": "user", "content": []}]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}
	assertContentJSON(t, req.Messages[0].Content, `""`)
}

func TestBuildRequestRejectsImageWithoutSourceData(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{
			name: "缺 source",
			body: `{"type": "image"}`,
		},
		{
			name: "base64 缺 data",
			body: `{"type": "image", "source": {"type": "base64", "media_type": "image/png"}}`,
		},
		{
			name: "base64 缺 media_type",
			body: `{"type": "image", "source": {"type": "base64", "data": "iVBOR"}}`,
		},
		{
			name: "url 缺 url",
			body: `{"type": "image", "source": {"type": "url"}}`,
		},
		{
			name: "source.type 未知",
			body: `{"type": "image", "source": {"type": "file", "file_id": "f-1"}}`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			raw := []byte(`{"max_tokens": 64, "messages": [{"role": "user", "content": [` + testCase.body + `]}]}`)

			req, rejection := BuildRequest("m", raw, false)
			if req != nil {
				t.Fatal("畸形图片块不应返回 ChatRequest")
			}
			if rejection == nil {
				t.Fatal("畸形图片块必须被拒绝，不能静默丢图")
			}
			if rejection.Kind != kindInvalidRequest {
				t.Fatalf("Kind = %q", rejection.Kind)
			}
		})
	}
}

// TestBuildRequestMovesImageOutOfToolResult tool_result 里的图片不再被拒绝，
// 而是搬到一条合成 user 消息 —— OpenAI 的 role=tool 消息 content 只能是
// 字符串，装不下图片 part，但正文 user 消息装得下。
// 原位留 marker 而不是静默丢弃，见 request_tool_image_test.go 的完整覆盖。
func TestBuildRequestMovesImageOutOfToolResult(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "t-1", "name": "Read", "input": {}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "t-1", "content": [
					{"type": "image", "source": {"type": "url", "url": "https://example.com/a.png"}}
				]}
			]}
		]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("tool_result 里的图片应被搬走而非拒绝: %v", rejection)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("应产出 assistant + tool + user 三条，实际 %d", len(req.Messages))
	}
	if req.Messages[1].Role != "tool" || req.Messages[2].Role != "user" {
		t.Fatalf("顺序应为 tool 在前、合成 user 在后，实际 %s/%s",
			req.Messages[1].Role, req.Messages[2].Role)
	}
	if strings.Contains(req.Messages[1].Content.Text, "example.com") {
		t.Fatalf("tool 消息不该再含图片 URL，实际 %q", req.Messages[1].Content.Text)
	}
	assertContentJSON(t, req.Messages[2].Content,
		`[{"type":"text","text":"[tool result media of t-1]"},`+
			`{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]`)
}

func assertContentJSON(t *testing.T, content MessageContent, want string) {
	t.Helper()

	encoded, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("序列化 content: %v", err)
	}
	if string(encoded) != want {
		t.Fatalf("content wire format 不符\n want %s\n  got  %s", want, encoded)
	}
}
