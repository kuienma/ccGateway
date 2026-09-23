package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// 取自 2026-09-22 对 Ark 的真实响应（见设计文档 §2）。
const arkTextResponse = `{
	"choices": [{
		"finish_reason": "stop",
		"index": 0,
		"message": {"content": "ok", "reasoning_content": "We need answer with only two characters ok.", "role": "assistant"}
	}],
	"created": 1790133454,
	"id": "021790133452915006c460d0b2d36d003716bcb3522c0d185f955",
	"model": "deepseek-v4-flash-ga-260731",
	"object": "chat.completion",
	"usage": {"completion_tokens": 11, "prompt_tokens": 89, "total_tokens": 100,
		"completion_tokens_details": {"reasoning_tokens": 9}}
}`

// gatewayModelID 是网关请求时传入的 modelID，刻意与各用例上游回显的 model 都不相同：
// 这样凡是断言 model 的用例都同时钉住了「有回显时以上游回显值为准」，
// 回落逻辑写成无条件覆盖也会被抓出来。
const gatewayModelID = "gateway-requested-model"

func decodeAnthropicResponse(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("结果必须是合法 JSON: %v", err)
	}
	return decoded
}

func TestBuildResponseMapsTextAndUsage(t *testing.T) {
	t.Parallel()

	out, err := BuildResponse([]byte(arkTextResponse), gatewayModelID, false)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	decoded := decodeAnthropicResponse(t, out)

	if decoded["type"] != "message" {
		t.Fatalf("type = %v，期望 message", decoded["type"])
	}
	if decoded["role"] != "assistant" {
		t.Fatalf("role = %v", decoded["role"])
	}
	// id 在上游值前加 msg_ 前缀（Anthropic 真实 API 的形态），与流式路径共用同一构造。
	if decoded["id"] != "msg_021790133452915006c460d0b2d36d003716bcb3522c0d185f955" {
		t.Fatalf("id 应为 msg_ + 上游 id，实际 %v", decoded["id"])
	}
	if decoded["model"] != "deepseek-v4-flash-ga-260731" {
		t.Fatalf("model = %v", decoded["model"])
	}
	if decoded["stop_reason"] != "end_turn" {
		t.Fatalf("finish_reason=stop 应映射为 end_turn，实际 %v", decoded["stop_reason"])
	}

	content, ok := decoded["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("includeThinking=false 时只应有一个 text 块，实际 %#v", decoded["content"])
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "ok" {
		t.Fatalf("content[0] = %#v", block)
	}

	usage := decoded["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 89 {
		t.Fatalf("input_tokens = %v", usage["input_tokens"])
	}
	if usage["output_tokens"].(float64) != 11 {
		t.Fatalf("output_tokens = %v", usage["output_tokens"])
	}
}

// TestBuildResponseFallsBackToModelIDWhenUpstreamOmitsModel 钉住非流式的 model 空值回落。
//
// 流式 ensureStarted 早就有这条回落（chunk.Model 为空则用传入的 modelID），非流式一直没有，
// 上游若不回显 model，客户端就收到 "model":""。两条路径必须同口径。
func TestBuildResponseFallsBackToModelIDWhenUpstreamOmitsModel(t *testing.T) {
	t.Parallel()

	// 上游响应不带 model 字段。
	payload := `{"id":"r","choices":[{"finish_reason":"stop",
		"message":{"role":"assistant","content":"ok"}}],
		"usage":{"prompt_tokens":1,"completion_tokens":1}}`

	out, err := BuildResponse([]byte(payload), gatewayModelID, false)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if got := decodeAnthropicResponse(t, out)["model"]; got != gatewayModelID {
		t.Fatalf("上游不回显 model 时应回落到传入的 modelID %q，实际 %v", gatewayModelID, got)
	}
}

func TestBuildResponseEmitsThinkingBlockFirst(t *testing.T) {
	t.Parallel()

	out, err := BuildResponse([]byte(arkTextResponse), gatewayModelID, true)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	content := decodeAnthropicResponse(t, out)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("应有 thinking + text 两块，实际 %d 块", len(content))
	}
	first := content[0].(map[string]any)
	if first["type"] != "thinking" {
		t.Fatalf("thinking 块必须排在最前，实际首块 %v", first["type"])
	}
	if first["thinking"] != "We need answer with only two characters ok." {
		t.Fatalf("thinking 内容 = %v", first["thinking"])
	}
	if content[1].(map[string]any)["type"] != "text" {
		t.Fatalf("第二块应为 text，实际 %v", content[1])
	}
}

func TestBuildResponseMapsToolCalls(t *testing.T) {
	t.Parallel()

	payload := `{
		"id": "resp-1",
		"model": "m",
		"choices": [{
			"finish_reason": "tool_calls",
			"index": 0,
			"message": {"content": "", "role": "assistant", "tool_calls": [
				{"id": "call_1", "type": "function", "index": 0,
				 "function": {"name": "get_weather", "arguments": "{\"city\": \"北京\"}"}}
			]}
		}],
		"usage": {"completion_tokens": 20, "prompt_tokens": 100}
	}`

	out, err := BuildResponse([]byte(payload), gatewayModelID, false)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	decoded := decodeAnthropicResponse(t, out)

	if decoded["stop_reason"] != "tool_use" {
		t.Fatalf("finish_reason=tool_calls 应映射为 tool_use，实际 %v", decoded["stop_reason"])
	}
	content := decoded["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content 为空字符串时不应产出 text 块，实际 %#v", content)
	}
	block := content[0].(map[string]any)
	if block["type"] != "tool_use" || block["id"] != "call_1" || block["name"] != "get_weather" {
		t.Fatalf("tool_use 块 = %#v", block)
	}
	input := block["input"].(map[string]any)
	if input["city"] != "北京" {
		t.Fatalf("arguments 应解析成 input 对象，实际 %#v", input)
	}
}

func TestBuildResponseStopReasonMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		finish string
		want   string
	}{
		{"stop", "end_turn"},
		{"tool_calls", "tool_use"},
		{"length", "max_tokens"},
		{"content_filter", "end_turn"},
		{"", "end_turn"},
	}
	for _, tc := range cases {
		t.Run(tc.finish, func(t *testing.T) {
			t.Parallel()
			payload := `{"id":"r","model":"m","choices":[{"finish_reason":"` + tc.finish +
				`","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
			out, err := BuildResponse([]byte(payload), gatewayModelID, false)
			if err != nil {
				t.Fatalf("转换失败: %v", err)
			}
			if got := decodeAnthropicResponse(t, out)["stop_reason"]; got != tc.want {
				t.Fatalf("finish_reason=%q → %v，期望 %q", tc.finish, got, tc.want)
			}
		})
	}
}

func TestBuildResponseRejectsEmptyChoices(t *testing.T) {
	t.Parallel()

	if _, err := BuildResponse([]byte(`{"id":"r","model":"m","choices":[]}`), gatewayModelID, false); err == nil {
		t.Fatal("choices 为空必须报错，不能返回空响应给客户端")
	}
}

func TestBuildResponseKeepsMalformedToolArgumentsVisible(t *testing.T) {
	t.Parallel()

	payload := `{"id":"r","model":"m","choices":[{"finish_reason":"tool_calls",
		"message":{"role":"assistant","content":"","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"f","arguments":"{不是JSON"}}]}}],
		"usage":{"prompt_tokens":1,"completion_tokens":1}}`

	if _, err := BuildResponse([]byte(payload), gatewayModelID, false); err == nil {
		t.Fatal("工具参数不是合法 JSON 时必须报错，不能静默产出损坏的 input")
	}
}

// toolArgumentsResponse 造一个只有一个 tool_call 的响应，arguments 原样嵌入。
// arguments 在 Ark 侧是 JSON 字符串，所以这里要二次转义。
func toolArgumentsResponse(t *testing.T, arguments string) []byte {
	t.Helper()
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatalf("构造用例失败: %v", err)
	}
	return []byte(`{"id":"r","model":"m","choices":[{"finish_reason":"tool_calls",` +
		`"message":{"role":"assistant","content":"","tool_calls":[` +
		`{"id":"c1","type":"function","function":{"name":"f","arguments":` + string(encoded) + `}}]}}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
}

// toolIdentityResponse 造一个只有一个 tool_call 的响应，id 与 name 由调用方指定。
// 上游给缺字段还是给空串，在 Go 解码后都是空串，两种形态等价。
func toolIdentityResponse(t *testing.T, id, name string) []byte {
	t.Helper()
	encodedID, err := json.Marshal(id)
	if err != nil {
		t.Fatalf("构造用例失败: %v", err)
	}
	encodedName, err := json.Marshal(name)
	if err != nil {
		t.Fatalf("构造用例失败: %v", err)
	}
	return []byte(`{"id":"r","model":"m","choices":[{"finish_reason":"tool_calls",` +
		`"message":{"role":"assistant","content":"","tool_calls":[` +
		`{"id":` + string(encodedID) + `,"type":"function","function":{"name":` + string(encodedName) +
		`,"arguments":"{}"}}]}}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
}

// TestBuildResponseRejectsToolCallWithoutID 钉住非流式 tool_use 的 id 必填。
//
// Anthropic 协议要求 tool_use 块的 id 与 name 双必填，而 responseBlock 的这两个字段都带
// omitempty —— 上游给空 id 时，序列化结果里 id 会整个消失，客户端拿到一个无法回传结果的
// 损坏块，而网关一声不响。与流式 openToolBlock 的硬守卫同一口径：宁可报错，
// 也不要填占位符把损坏数据转发给客户端。
func TestBuildResponseRejectsToolCallWithoutID(t *testing.T) {
	t.Parallel()

	out, err := BuildResponse(toolIdentityResponse(t, "", "get_weather"), gatewayModelID, false)
	if err == nil {
		t.Fatalf("tool_calls 缺 id 必须报错，不能产出缺字段的 tool_use 块，实际产出 %s", out)
	}
	// 错误必须点名缺的是 id：这句话是运维在 5xx 里唯一能看到的信息。
	// 同时不得提到 name —— 「缺少 id 或 name」这种不可区分的措辞包含「缺少 id」子串，
	// 只查前半句会让它蒙混过关。
	if !strings.Contains(err.Error(), "缺少 id") || strings.Contains(err.Error(), "name") {
		t.Fatalf("错误应点名缺少的是 id 且不提 name，实际 %v", err)
	}
}

// TestBuildResponseRejectsToolCallWithoutName 是上一条的镜像：name 同样必填。
// 缺 name 的 tool_use 块客户端不知道该执行哪个工具。
func TestBuildResponseRejectsToolCallWithoutName(t *testing.T) {
	t.Parallel()

	out, err := BuildResponse(toolIdentityResponse(t, "call_1", ""), gatewayModelID, false)
	if err == nil {
		t.Fatalf("tool_calls 缺 name 必须报错，不能产出缺字段的 tool_use 块，实际产出 %s", out)
	}
	if !strings.Contains(err.Error(), "缺少 name") || strings.Contains(err.Error(), " id") {
		t.Fatalf("错误应点名缺少的是 name 且不提 id，实际 %v", err)
	}
}

// Anthropic 的 tool_use.input 必须是对象。合法但非对象的 JSON（尤其 null）
// 会让客户端走空指针路径而不是报类型错误，必须在网关这一层拦掉。
func TestBuildResponseRejectsNonObjectToolArguments(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		arguments string
	}{
		{"null", "null"},
		{"数字标量", "123"},
		{"字符串标量", `"abc"`},
		{"数组", "[1,2]"},
		{"布尔", "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := BuildResponse(toolArgumentsResponse(t, tc.arguments), gatewayModelID, false)
			if err == nil {
				t.Fatalf("arguments=%s 是合法 JSON 但不是对象，必须报错，实际产出 %s", tc.arguments, out)
			}
		})
	}
}

func TestBuildResponseAcceptsObjectToolArguments(t *testing.T) {
	t.Parallel()

	for _, arguments := range []string{`{}`, `{"city": "北京"}`} {
		out, err := BuildResponse(toolArgumentsResponse(t, arguments), gatewayModelID, false)
		if err != nil {
			t.Fatalf("arguments=%s 是合法对象，不应报错: %v", arguments, err)
		}
		content := decodeAnthropicResponse(t, out)["content"].([]any)
		block := content[0].(map[string]any)
		if _, ok := block["input"].(map[string]any); !ok {
			t.Fatalf("arguments=%s 的 input 应是对象，实际 %#v", arguments, block["input"])
		}
	}
}

// content 最终为空时必须保持 []，客户端按数组解析，null 会直接崩。
func TestBuildResponseKeepsEmptyContentAsArray(t *testing.T) {
	t.Parallel()

	payload := `{"id":"r","model":"m","choices":[{"finish_reason":"stop",
		"message":{"role":"assistant","content":""}}],
		"usage":{"prompt_tokens":1,"completion_tokens":1}}`

	out, err := BuildResponse([]byte(payload), gatewayModelID, false)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if !strings.Contains(string(out), `"content":[]`) {
		t.Fatalf("content 为空时必须序列化成 []，实际 %s", out)
	}
	content, ok := decodeAnthropicResponse(t, out)["content"].([]any)
	if !ok || len(content) != 0 {
		t.Fatalf("content 应是空数组，实际 %#v", content)
	}
}

// TestBuildResponseStopReasonOverriddenWhenToolUseBlockPresent 锁住非流式的 stop_reason 覆盖。
//
// 上游若自相矛盾地给出 finish_reason=stop + 非空 tool_calls，直接透传 MapStopReason
// 会产出「end_turn + tool_use 块」：客户端见到 end_turn 就不执行工具，agent 循环当场
// 断掉，且表现为「模型不肯用工具」极难定位到网关。
// 与流式 EventEmitter 的 sawToolUse 覆盖是同一条语义，两条路径必须给出一致的结果。
//
// 实测 Ark 的 finish_reason 语义自洽（今天不会触发），但两条路径都不该依赖上游永远自洽。
func TestBuildResponseStopReasonOverriddenWhenToolUseBlockPresent(t *testing.T) {
	t.Parallel()

	// finish_reason 刻意是 stop，而 tool_calls 非空。
	payload := `{
		"id": "resp-1",
		"model": "m",
		"choices": [{
			"finish_reason": "stop",
			"index": 0,
			"message": {"content": "", "role": "assistant", "tool_calls": [
				{"id": "call_1", "type": "function", "index": 0,
				 "function": {"name": "get_weather", "arguments": "{\"city\": \"北京\"}"}}
			]}
		}],
		"usage": {"completion_tokens": 20, "prompt_tokens": 100}
	}`

	out, err := BuildResponse([]byte(payload), gatewayModelID, false)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	decoded := decodeAnthropicResponse(t, out)

	if decoded["stop_reason"] != "tool_use" {
		t.Fatalf("响应含 tool_use 块时 stop_reason 必须被覆盖为 tool_use，实际 %v", decoded["stop_reason"])
	}
	// 覆盖 stop_reason 不能动 content：tool_use 块本身要原样保留。
	content, ok := decoded["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content 应只含一个 tool_use 块，实际 %#v", decoded["content"])
	}
	block := content[0].(map[string]any)
	if block["type"] != "tool_use" || block["id"] != "call_1" || block["name"] != "get_weather" {
		t.Fatalf("tool_use 块 = %#v", block)
	}
}
