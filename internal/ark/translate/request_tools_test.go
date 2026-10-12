package translate

import (
	"encoding/json"
	"testing"
)

func TestBuildRequestConvertsToolDefinitions(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [{"role": "user", "content": "北京天气"}],
		"tools": [{
			"name": "get_weather",
			"description": "查询天气",
			"input_schema": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}
		}]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}
	if len(req.Tools) != 1 {
		t.Fatalf("工具数 = %d", len(req.Tools))
	}
	tool := req.Tools[0]
	if tool.Type != "function" {
		t.Fatalf("Type = %q，期望 function", tool.Type)
	}
	if tool.Function.Name != "get_weather" || tool.Function.Description != "查询天气" {
		t.Fatalf("Function = %#v", tool.Function)
	}

	var schema map[string]any
	if err := json.Unmarshal(tool.Function.Parameters, &schema); err != nil {
		t.Fatalf("parameters 应是合法 JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Fatalf("input_schema 应原样搬到 parameters，实际 %#v", schema)
	}
}

func TestBuildRequestConvertsToolUseAndToolResult(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [
			{"role": "user", "content": "北京天气"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "我查一下"},
				{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "北京"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "18度晴"}
			]}
		]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}

	if len(req.Messages) != 3 {
		t.Fatalf("消息数 = %d，期望 3", len(req.Messages))
	}

	assistant := req.Messages[1]
	if assistant.Role != "assistant" || assistant.Content.Text != "我查一下" {
		t.Fatalf("assistant 文本应保留，实际 %#v", assistant)
	}
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("tool_calls 数 = %d", len(assistant.ToolCalls))
	}
	call := assistant.ToolCalls[0]
	if call.ID != "toolu_1" || call.Type != "function" || call.Function.Name != "get_weather" {
		t.Fatalf("ToolCall = %#v", call)
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
		t.Fatalf("arguments 必须是 JSON 文本: %v", err)
	}
	if input["city"] != "北京" {
		t.Fatalf("input 应序列化进 arguments，实际 %q", call.Function.Arguments)
	}

	result := req.Messages[2]
	if result.Role != "tool" {
		t.Fatalf("tool_result 应转成 role=tool，实际 %q", result.Role)
	}
	if result.ToolCallID != "toolu_1" {
		t.Fatalf("ToolCallID = %q", result.ToolCallID)
	}
	if result.Content.Text != "18度晴" {
		t.Fatalf("Content = %q", result.Content.Text)
	}
}

func TestBuildRequestSplitsMultipleToolResults(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [{"role": "user", "content": [
			{"type": "tool_result", "tool_use_id": "t1", "content": "r1"},
			{"type": "tool_result", "tool_use_id": "t2", "content": "r2"}
		]}]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("两个 tool_result 应拆成两条 message，实际 %d 条", len(req.Messages))
	}
	if req.Messages[0].ToolCallID != "t1" || req.Messages[1].ToolCallID != "t2" {
		t.Fatalf("tool_call_id 顺序错误: %#v", req.Messages)
	}
}

func TestBuildRequestFlattensToolResultBlockContent(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [{"role": "user", "content": [
			{"type": "tool_result", "tool_use_id": "t1", "content": [{"type": "text", "text": "结果"}]}
		]}]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}
	if req.Messages[0].Content.Text != "结果" {
		t.Fatalf("tool_result 的块数组 content 应压平，实际 %q", req.Messages[0].Content.Text)
	}
}

func TestBuildRequestDropsHistoricalThinkingBlocks(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "内部推理", "signature": "sig-abc"},
				{"type": "text", "text": "答案"}
			]}
		]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("历史 thinking 块应被丢弃而非报错: %v", rejection)
	}
	if req.Messages[1].Content.Text != "答案" {
		t.Fatalf("thinking 内容不应进入 content，实际 %q", req.Messages[1].Content.Text)
	}
}

func TestBuildRequestToolChoice(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  any
	}{
		{"auto", `{"type":"auto"}`, "auto"},
		{"any", `{"type":"any"}`, "required"},
		{"none", `{"type":"none"}`, "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw := []byte(`{"max_tokens":64,"messages":[{"role":"user","content":"hi"}],"tool_choice":` + tc.input + `}`)
			req, rejection := BuildRequest("m", raw, false)
			if rejection != nil {
				t.Fatalf("不应被拒绝: %v", rejection)
			}
			if req.ToolChoice != tc.want {
				t.Fatalf("ToolChoice = %#v，期望 %#v", req.ToolChoice, tc.want)
			}
		})
	}
}

func TestBuildRequestToolChoiceSpecificTool(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [{"role": "user", "content": "hi"}],
		"tool_choice": {"type": "tool", "name": "get_weather"}
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}
	encoded, err := json.Marshal(req.ToolChoice)
	if err != nil {
		t.Fatalf("序列化 tool_choice 失败: %v", err)
	}
	want := `{"type":"function","function":{"name":"get_weather"}}`
	if string(encoded) != want {
		t.Fatalf("ToolChoice = %s，期望 %s", encoded, want)
	}
}

func TestBuildRequestThinkingPolicy(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		thinking string
		want     string
	}{
		{"未传时主动关闭", ``, "disabled"},
		{"显式 disabled", `,"thinking":{"type":"disabled"}`, "disabled"},
		{"显式 enabled", `,"thinking":{"type":"enabled","budget_tokens":2048}`, "enabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw := []byte(`{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]` + tc.thinking + `}`)
			req, rejection := BuildRequest("m", raw, false)
			if rejection != nil {
				t.Fatalf("不应被拒绝: %v", rejection)
			}
			if req.Thinking == nil {
				t.Fatal("Thinking 必须显式设置，不能留空让 Ark 用默认值")
			}
			if req.Thinking.Type != tc.want {
				t.Fatalf("Thinking.Type = %q，期望 %q", req.Thinking.Type, tc.want)
			}
		})
	}
}

// TestBuildRequestOrdersToolResultsBeforeText 锁定混合形状的排序：
// OpenAI 要求 role=tool 消息紧跟带 tool_calls 的 assistant 消息，
// 中间插入 user 消息会打断绑定，所以 tool_result 必须排在同条消息的 text 之前。
func TestBuildRequestOrdersToolResultsBeforeText(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [
			{"role": "user", "content": "北京和上海天气"},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "t1", "name": "get_weather", "input": {"city": "北京"}},
				{"type": "tool_use", "id": "t2", "name": "get_weather", "input": {"city": "上海"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "t1", "content": "北京 18 度"},
				{"type": "tool_result", "tool_use_id": "t2", "content": "上海 22 度"},
				{"type": "text", "text": "顺便说说明天"}
			]}
		]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}

	// user + assistant(tool_calls) + tool(t1) + tool(t2) + user(text)
	if len(req.Messages) != 5 {
		t.Fatalf("消息数 = %d，期望 5: %#v", len(req.Messages), req.Messages)
	}
	if req.Messages[1].Role != "assistant" || len(req.Messages[1].ToolCalls) != 2 {
		t.Fatalf("Messages[1] 应是带两个 tool_calls 的 assistant，实际 %#v", req.Messages[1])
	}

	type shape struct {
		role       string
		content    string
		toolCallID string
	}
	want := []shape{
		{"tool", "北京 18 度", "t1"},
		{"tool", "上海 22 度", "t2"},
		{"user", "顺便说说明天", ""},
	}
	for offset, expected := range want {
		got := req.Messages[2+offset]
		actual := shape{got.Role, got.Content.Text, got.ToolCallID}
		if actual != expected {
			t.Fatalf("Messages[%d] = %#v，期望 %#v", 2+offset, actual, expected)
		}
	}
}

// TestBuildRequestTrimsToolName 锁定：工具名的非空校验用 trim 后的值，
// 下发也必须用 trim 后的值。否则模型回吐的 tool name 不带空格，
// 后续轮次的 tool_use.name 会对不上。
func TestBuildRequestTrimsToolName(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{"name": "  get_weather  ", "input_schema": {"type": "object"}}]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}
	if len(req.Tools) != 1 {
		t.Fatalf("工具数 = %d", len(req.Tools))
	}
	if req.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("工具名应去掉首尾空白，实际 %q", req.Tools[0].Function.Name)
	}
}

// TestBuildRequestKeepsEmptyContentMessage 锁定裁决 A：内容为空数组的消息不能被丢弃，
// 否则会破坏 Ark 侧 user/assistant 交替。只有「内容全是 tool_result」时才不产出主消息。
func TestBuildRequestKeepsEmptyContentMessage(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [{"role": "user", "content": []}]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("空 content 的消息仍应产出一条消息，实际 %d 条: %#v", len(req.Messages), req.Messages)
	}
	if req.Messages[0].Role != "user" || req.Messages[0].Content.Text != "" {
		t.Fatalf("应产出一条空内容的 user 消息，实际 %#v", req.Messages[0])
	}
}
