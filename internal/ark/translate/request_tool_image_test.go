package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// tool_result 无法承载图片 part（OpenAI 协议里 tool 消息的 content 只能是字符串），
// 所以图片被抽出、放进紧随所有 tool 消息之后的一条合成 user 消息。
//
// 顺序必须是：tool 消息保持相邻 → 合成 user（图片）→ 主消息。
// tool 消息与带 tool_calls 的 assistant 之间的绑定不能被打断，这是本文件
// 最重要的不变量，见 TestBuildRequestKeepsToolMessagesAdjacentWhenMovingImages。

const wantMediaMarker = "[tool result media moved to the following user message]"

// 超过 WHOLE_DATA_URL_MIN_BYTES（8KB）才会被识别为图片，所以测试数据必须够大。
func largeImageDataURL() string {
	return "data:image/png;base64," + strings.Repeat("iVBORw0KGgoAAAANSUhEUgAAAAE", 400)
}

func TestBuildRequestMovesToolResultImageToUserMessage(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "call_1", "name": "Read", "input": {}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "call_1", "content": [
					{"type": "text", "text": "截图如下"},
					{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "` + strings.Repeat("iVBORw0KGgoAAAANSUhEUgAAAAE", 400) + `"}}
				]}
			]}
		]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("tool_result 带图不应被拒绝: %v", rejection)
	}

	if len(req.Messages) != 3 {
		t.Fatalf("应产出 3 条消息（assistant + tool + user），实际 %d: %s", len(req.Messages), dumpRoles(req.Messages))
	}

	// [0] assistant（工具调用）
	if req.Messages[0].Role != "assistant" {
		t.Fatalf("Messages[0] 应是 assistant，实际 %q", req.Messages[0].Role)
	}

	// [1] tool 消息：图片已被抽走，原位是 marker
	if req.Messages[1].Role != "tool" {
		t.Fatalf("Messages[1] 应是 tool，实际 %q", req.Messages[1].Role)
	}
	if req.Messages[1].ToolCallID != "call_1" {
		t.Fatalf("Messages[1] tool_call_id 不符，实际 %q", req.Messages[1].ToolCallID)
	}
	gotTool := req.Messages[1].Content.Text
	if !strings.Contains(gotTool, "截图如下") {
		t.Fatalf("tool 消息应保留原文本，实际 %q", gotTool)
	}
	if !strings.Contains(gotTool, wantMediaMarker) {
		t.Fatalf("tool 消息应含媒体搬迁 marker，实际 %q", gotTool)
	}
	if strings.Contains(gotTool, "iVBORw0KGgo") {
		t.Fatalf("tool 消息不应再含图片数据，实际 %q", gotTool)
	}

	// [2] 合成 user 消息：图片在这里
	if req.Messages[2].Role != "user" {
		t.Fatalf("Messages[2] 应是合成 user，实际 %q", req.Messages[2].Role)
	}
	assertContentJSON(t, req.Messages[2].Content,
		`[{"type":"text","text":"[tool result media of call_1]"},`+
			`{"type":"image_url","image_url":{"url":"`+largeImageDataURL()+`"}}]`)
}

func TestBuildRequestKeepsToolMessagesAdjacentWhenMovingImages(t *testing.T) {
	t.Parallel()

	// 并行的两个 tool_result 都带图：两条 tool 消息必须相邻，
	// 合成 user 消息只能出现在它们**之后**。
	raw := []byte(`{
		"max_tokens": 64,
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "call_1", "name": "A", "input": {}},
				{"type": "tool_use", "id": "call_2", "name": "B", "input": {}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "call_1", "content": [
					{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "` + strings.Repeat("iVBORw0KGgoAAAANSUhEUgAAAAE", 400) + `"}}
				]},
				{"type": "tool_result", "tool_use_id": "call_2", "content": [
					{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "` + strings.Repeat("iVBORw0KGgoAAAANSUhEUgAAAAE", 400) + `"}}
				]}
			]}
		]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}

	if got := dumpRoles(req.Messages); got != "assistant,tool,tool,user" {
		t.Fatalf("消息顺序 = %q，期望 assistant,tool,tool,user", got)
	}

	// 两条 tool 消息的 tool_call_id 必须保持原顺序且相邻。
	if req.Messages[1].ToolCallID != "call_1" || req.Messages[2].ToolCallID != "call_2" {
		t.Fatalf("tool 消息顺序错乱: %q, %q", req.Messages[1].ToolCallID, req.Messages[2].ToolCallID)
	}

	// 合成 user 消息应带两个图片 part，且各有归属标注。
	encoded, err := json.Marshal(req.Messages[3].Content)
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	if n := strings.Count(string(encoded), `"type":"image_url"`); n != 2 {
		t.Fatalf("合成 user 消息应含 2 个图片 part，实际 %d: %s", n, encoded)
	}
	if !strings.Contains(string(encoded), "[tool result media of call_1]") ||
		!strings.Contains(string(encoded), "[tool result media of call_2]") {
		t.Fatalf("合成 user 消息应含两条归属标注，实际 %s", encoded)
	}
}

func TestBuildRequestMergesToolResultImagesIntoOneUserMessage(t *testing.T) {
	t.Parallel()

	// 三个 tool_result 带图（其中一个无图），只能产出**一条**合成 user 消息。
	raw := []byte(`{
		"max_tokens": 64,
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "c1", "name": "A", "input": {}},
				{"type": "tool_use", "id": "c2", "name": "B", "input": {}},
				{"type": "tool_use", "id": "c3", "name": "C", "input": {}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "c1", "content": [
					{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "` + strings.Repeat("iVBORw0KGgoAAAANSUhEUgAAAAE", 400) + `"}}
				]},
				{"type": "tool_result", "tool_use_id": "c2", "content": "纯文本结果"},
				{"type": "tool_result", "tool_use_id": "c3", "content": [
					{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "` + strings.Repeat("iVBORw0KGgoAAAANSUhEUgAAAAE", 400) + `"}}
				]}
			]}
		]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}

	if got := dumpRoles(req.Messages); got != "assistant,tool,tool,tool,user" {
		t.Fatalf("消息顺序 = %q，期望 assistant,tool,tool,tool,user", got)
	}
	if req.Messages[2].Content.Text != "纯文本结果" {
		t.Fatalf("无图 tool 消息应保持原文，实际 %q", req.Messages[2].Content.Text)
	}
}

func TestBuildRequestKeepsSmallDataURLAsText(t *testing.T) {
	t.Parallel()

	// < 8KB 的 data URL 不识别为图片，按 CC Switch 的语义继续当文本。
	small := "data:image/png;base64,iVBOR"
	raw := []byte(`{
		"max_tokens": 64,
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "c1", "name": "A", "input": {}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "c1", "content": "` + small + `"}
			]}
		]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}
	if got := dumpRoles(req.Messages); got != "assistant,tool" {
		t.Fatalf("小 data URL 不该产出合成 user 消息，实际顺序 %q", got)
	}
	if req.Messages[1].Content.Text != small {
		t.Fatalf("小 data URL 应原样保留为文本，实际 %q", req.Messages[1].Content.Text)
	}
}

func TestBuildRequestStaysByteStableWhenToolResultHasNoImage(t *testing.T) {
	t.Parallel()

	// 没有图片时输出必须逐字节不变，保住 prompt cache。
	raw := []byte(`{
		"max_tokens": 64,
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "c1", "name": "A", "input": {}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "c1", "content": "普通结果"}
			]}
		]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}
	if got := dumpRoles(req.Messages); got != "assistant,tool" {
		t.Fatalf("无图请求消息数应为 2，实际 %q", got)
	}
	if req.Messages[1].Content.Text != "普通结果" {
		t.Fatalf("无图 tool 内容应原样，实际 %q", req.Messages[1].Content.Text)
	}
}

func TestBuildRequestStillRejectsImageInsideSystem(t *testing.T) {
	t.Parallel()

	// system 无法用「插一条后续消息」承载图片，继续拒绝。
	raw := []byte(`{
		"max_tokens": 64,
		"system": [{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "iVBOR"}}],
		"messages": [{"role": "user", "content": "hi"}]
	}`)

	_, rejection := BuildRequest("m", raw, false)
	if rejection == nil {
		t.Fatal("system 里的图片必须继续被拒绝")
	}
	if !strings.Contains(rejection.Message, "不支持图片块") {
		t.Fatalf("拒绝文案应点明不支持图片块，实际 %q", rejection.Message)
	}
}

func dumpRoles(messages []ChatMessage) string {
	roles := make([]string, 0, len(messages))
	for _, message := range messages {
		roles = append(roles, message.Role)
	}
	return strings.Join(roles, ",")
}
