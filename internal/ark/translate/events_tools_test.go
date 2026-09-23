package translate

import (
	"strings"
	"testing"
)

// toolStartChunk 是工具首发分片：带 id、name 与空 arguments。
func toolStartChunk(toolIndex int, id, name string) StreamChunk {
	call := StreamToolCall{Index: toolIndex, ID: id, Type: "function"}
	call.Function.Name = name
	return StreamChunk{
		ID:      "s1",
		Model:   "m",
		Choices: []StreamChoice{{Index: 0, Delta: StreamDelta{Role: "assistant", ToolCalls: []StreamToolCall{call}}}},
	}
}

// toolArgChunk 是参数分片：只有 index 与 arguments。
func toolArgChunk(toolIndex int, arguments string) StreamChunk {
	call := StreamToolCall{Index: toolIndex}
	call.Function.Arguments = arguments
	return StreamChunk{
		ID:      "s1",
		Model:   "m",
		Choices: []StreamChoice{{Index: 0, Delta: StreamDelta{Role: "assistant", ToolCalls: []StreamToolCall{call}}}},
	}
}

func TestEventEmitterSingleToolCall(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	// 复刻实测的真实分片边界（见设计文档 §2）。
	chunks := []StreamChunk{
		toolStartChunk(0, "call_1", "get_weather"),
		toolArgChunk(0, `{"city": "`),
		toolArgChunk(0, "北京"),
		toolArgChunk(0, `"`),
		toolArgChunk(0, "}"),
		finishChunk("tool_calls", 397, 81),
	}
	for _, chunk := range chunks {
		if err := emitter.Consume(chunk); err != nil {
			t.Fatalf("Consume 失败: %v", err)
		}
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	assertTypes(t, log.types(), []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_delta",
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	})

	block := log.events[1]["content_block"].(map[string]any)
	if block["type"] != "tool_use" || block["id"] != "call_1" || block["name"] != "get_weather" {
		t.Fatalf("tool_use 块 = %#v", block)
	}
	if _, ok := block["input"].(map[string]any); !ok {
		t.Fatalf("tool_use 块的 input 应是空对象，实际 %#v", block["input"])
	}

	// 四个分片顺序拼起来必须还原成完整 JSON。
	var joined string
	for _, event := range log.ofType(t, "content_block_delta") {
		delta := event["delta"].(map[string]any)
		if delta["type"] != "input_json_delta" {
			t.Fatalf("delta.type = %v，期望 input_json_delta", delta["type"])
		}
		joined += delta["partial_json"].(string)
	}
	if joined != `{"city": "北京"}` {
		t.Fatalf("分片拼接 = %q，期望 {\"city\": \"北京\"}", joined)
	}

	if log.ofType(t, "message_delta")[0]["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatal("stop_reason 应为 tool_use")
	}
}

func TestEventEmitterParallelToolCalls(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	chunks := []StreamChunk{
		toolStartChunk(0, "call_a", "get_weather"),
		toolArgChunk(0, `{"city":"北京"}`),
		toolStartChunk(1, "call_b", "get_weather"),
		toolArgChunk(1, `{"city":"上海"}`),
		finishChunk("tool_calls", 400, 90),
	}
	for _, chunk := range chunks {
		if err := emitter.Consume(chunk); err != nil {
			t.Fatalf("Consume 失败: %v", err)
		}
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	assertTypes(t, log.types(), []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_stop", // 工具 0
		"content_block_start", "content_block_delta", "content_block_stop", // 工具 1
		"message_delta", "message_stop",
	})

	first := log.events[1]
	second := log.events[4]
	if first["index"].(float64) != 0 || second["index"].(float64) != 1 {
		t.Fatalf("两个工具块的 index 应为 0 和 1，实际 %v 与 %v", first["index"], second["index"])
	}
	if first["content_block"].(map[string]any)["id"] != "call_a" {
		t.Fatalf("工具 0 的 id = %v", first["content_block"].(map[string]any)["id"])
	}
	if second["content_block"].(map[string]any)["id"] != "call_b" {
		t.Fatalf("工具 1 的 id = %v", second["content_block"].(map[string]any)["id"])
	}

	// 两个块的参数不能串到对方的 index 上。
	deltas := log.ofType(t, "content_block_delta")
	if deltas[0]["index"].(float64) != 0 || deltas[1]["index"].(float64) != 1 {
		t.Fatalf("参数分片的 index 串了: %v / %v", deltas[0]["index"], deltas[1]["index"])
	}
}

// TestEventEmitterToolBlockIndexIsNotUpstreamIndex 是 index 映射表的真正护栏。
//
// 上面的并行用例里 OpenAI tool index 与 Anthropic block index 恰好相等（0/1 对 0/1），
// 所以「把上游 index 直接当 block index 用」这个错误在那条用例下**看不出来**。
// 本用例先垫一个 text 块把两者错开：tool index 0/1 对应 block index 1/2，
// 于是参数分片的 index 必须取映射值而非上游值。
func TestEventEmitterToolBlockIndexIsNotUpstreamIndex(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	chunks := []StreamChunk{
		textChunk("我查一下"),
		toolStartChunk(0, "call_a", "get_weather"),
		toolArgChunk(0, `{"city":"北京"}`),
		toolStartChunk(1, "call_b", "get_time"),
		toolArgChunk(1, `{"tz":"CST"}`),
		finishChunk("tool_calls", 400, 90),
	}
	for _, chunk := range chunks {
		if err := emitter.Consume(chunk); err != nil {
			t.Fatalf("Consume 失败: %v", err)
		}
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	starts := log.ofType(t, "content_block_start")
	if len(starts) != 3 {
		t.Fatalf("应有 3 个内容块（text + 两个 tool_use），实际 %d：%v", len(starts), log.types())
	}
	if starts[1]["index"].(float64) != 1 || starts[2]["index"].(float64) != 2 {
		t.Fatalf("两个 tool_use 块的 block index 应为 1 和 2，实际 %v 与 %v", starts[1]["index"], starts[2]["index"])
	}

	deltas := log.ofType(t, "content_block_delta")
	if len(deltas) != 3 {
		t.Fatalf("应有 3 个 delta（1 个 text + 2 个 input_json），实际 %d", len(deltas))
	}
	// 关键断言：工具参数分片的 index 是映射后的 block index（1、2），
	// 不是上游的 tool_calls[].index（0、1）。
	if deltas[1]["index"].(float64) != 1 {
		t.Fatalf("tool_calls[0] 的参数分片 index = %v，期望映射后的 block index 1", deltas[1]["index"])
	}
	if deltas[2]["index"].(float64) != 2 {
		t.Fatalf("tool_calls[1] 的参数分片 index = %v，期望映射后的 block index 2", deltas[2]["index"])
	}
	// 参数也不能串块。
	if deltas[1]["delta"].(map[string]any)["partial_json"] != `{"city":"北京"}` {
		t.Fatalf("block 1 的参数串了: %#v", deltas[1]["delta"])
	}
	if deltas[2]["delta"].(map[string]any)["partial_json"] != `{"tz":"CST"}` {
		t.Fatalf("block 2 的参数串了: %#v", deltas[2]["delta"])
	}
}

func TestEventEmitterTextThenToolUsesIncreasingIndex(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	chunks := []StreamChunk{
		textChunk("我查一下"),
		toolStartChunk(0, "call_1", "get_weather"),
		toolArgChunk(0, "{}"),
		finishChunk("tool_calls", 10, 5),
	}
	for _, chunk := range chunks {
		if err := emitter.Consume(chunk); err != nil {
			t.Fatalf("Consume 失败: %v", err)
		}
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	assertTypes(t, log.types(), []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_stop", // text
		"content_block_start", "content_block_delta", "content_block_stop", // tool_use
		"message_delta", "message_stop",
	})
	if log.events[1]["content_block"].(map[string]any)["type"] != "text" {
		t.Fatal("首块应为 text")
	}
	if log.events[4]["index"].(float64) != 1 {
		t.Fatalf("tool_use 块 index 应为 1，实际 %v", log.events[4]["index"])
	}
	// 无参数工具的 arguments 实测是 "{}"，必须原样透传成一个 input_json_delta。
	if got := log.events[5]["delta"].(map[string]any)["partial_json"]; got != "{}" {
		t.Fatalf("无参数工具的分片 = %v，期望 {}", got)
	}
}

func TestEventEmitterThinkingThenToolUse(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", true, log.emit)

	chunks := []StreamChunk{
		reasoningChunk("要查天气"),
		toolStartChunk(0, "call_1", "get_weather"),
		toolArgChunk(0, "{}"),
		finishChunk("tool_calls", 10, 5),
	}
	for _, chunk := range chunks {
		if err := emitter.Consume(chunk); err != nil {
			t.Fatalf("Consume 失败: %v", err)
		}
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	// 块类型写字面量而不是 blockKind* 常量：那些常量是实现自己的，拿它们当期望值时
	// 把常量改成 "thinkng" 也不会有任何用例失败，而这两个字符串是 Anthropic 的线格式、
	// 客户端按它们分派 —— 必须钉死。
	if log.events[1]["content_block"].(map[string]any)["type"] != "thinking" {
		t.Fatal("thinking 块必须排在 tool_use 之前")
	}
	if log.events[4]["content_block"].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("第二块应为 tool_use，实际 %#v", log.events[4]["content_block"])
	}
}

// TestEventEmitterStopReasonOverriddenWhenToolBlockOpened 锁住 stop_reason 覆盖逻辑。
//
// 若上游给了 finish_reason=stop 却发过 tool_calls，直接透传会产出
// 「end_turn + tool_use 块」的矛盾组合：Claude Code 见到 end_turn 就不执行工具，
// 整个 agent 循环当场断掉，且表现为「模型不肯用工具」极难定位。
// 因此只要开过 tool_use 块，stop_reason 必须是 tool_use，不看上游怎么说。
//
// 实测 Ark 的 finish_reason 语义自洽（今天不会触发），但状态机不该依赖上游永远自洽。
func TestEventEmitterStopReasonOverriddenWhenToolBlockOpened(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	chunks := []StreamChunk{
		toolStartChunk(0, "call_1", "get_weather"),
		toolArgChunk(0, `{"city":"北京"}`),
		// 上游自相矛盾：发过 tool_calls，却报 finish_reason=stop。
		finishChunk("stop", 10, 5),
	}
	for _, chunk := range chunks {
		if err := emitter.Consume(chunk); err != nil {
			t.Fatalf("Consume 失败: %v", err)
		}
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	deltas := log.ofType(t, "message_delta")
	if len(deltas) != 1 {
		t.Fatalf("message_delta 数 = %d", len(deltas))
	}
	if got := deltas[0]["delta"].(map[string]any)["stop_reason"]; got != "tool_use" {
		t.Fatalf("开过 tool_use 块时 stop_reason 必须被覆盖为 tool_use，实际 %v", got)
	}
}

// TestEventEmitterStopReasonNotOverriddenWithoutToolBlock 是上一条的反面：
// 没开过 tool_use 块时不能把 stop_reason 一律改成 tool_use，
// 否则覆盖逻辑写成无条件也能让上一条用例通过。
func TestEventEmitterStopReasonNotOverriddenWithoutToolBlock(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	if err := emitter.Consume(textChunk("答")); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	if err := emitter.Consume(finishChunk("stop", 1, 1)); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	if got := log.ofType(t, "message_delta")[0]["delta"].(map[string]any)["stop_reason"]; got != "end_turn" {
		t.Fatalf("没开过 tool_use 块时 stop_reason 应保持 end_turn，实际 %v", got)
	}
}

func TestEventEmitterOrphanArgumentFragmentFails(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	// 没有首发分片就来参数分片：协议被破坏，必须报错而不是猜一个块。
	if err := emitter.Consume(toolArgChunk(0, `{"a":1}`)); err == nil {
		t.Fatal("孤儿参数分片必须报错")
	}
}

// TestEventEmitterOrphanArgumentFragmentCollidingWithOpenBlockFails 是孤儿分片守卫的真正护栏。
//
// 上一条用例用的是 tool index 0 且当时没有任何块打开，所以即使删掉 toolBlocks 的 ok 判定，
// 零值 0 也会被下游的交错检查顺手兜住（只是错误信息退化成误导性的「存在交错」），用例照样绿。
//
// 本用例构造真正的碰撞：先用上游 tool index 3 开块（它映射成 block index 0），
// 再投一个 tool index 0 的孤儿分片 —— 此时 toolBlocks[0] 的零值 0 恰好等于当前 openIndex 0，
// 交错检查会放行，孤儿参数被静默写进工具 3 的块，零报错。
// 因此这里既断言必须报错，也断言错误必须出自「缺少首发分片」那条守卫，
// 而不是被交错检查误报。
func TestEventEmitterOrphanArgumentFragmentCollidingWithOpenBlockFails(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	// 上游 tool index 3 → Anthropic block index 0。
	if err := emitter.Consume(toolStartChunk(3, "call_3", "f")); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	before := len(log.ofType(t, "content_block_delta"))

	// tool index 0 从未有过首发分片：它的映射不存在，绝不能借用零值落到别人的块上。
	err := emitter.Consume(toolArgChunk(0, `{"a":1}`))
	if err == nil {
		t.Fatal("与已打开块 index 碰撞的孤儿参数分片必须报错，不能静默写进别的工具块")
	}
	if !strings.Contains(err.Error(), "缺少首发分片") {
		t.Fatalf("错误应出自缺少首发分片的守卫，实际 %v", err)
	}
	if after := len(log.ofType(t, "content_block_delta")); after != before {
		t.Fatalf("报错前不得写出任何参数 delta，delta 数从 %d 变成 %d", before, after)
	}
}

func TestEventEmitterInterleavedToolFragmentsFail(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	chunks := []StreamChunk{
		toolStartChunk(0, "call_a", "f"),
		toolArgChunk(0, `{"x":`),
		toolStartChunk(1, "call_b", "g"), // 工具 0 的块在此被关闭
	}
	for _, chunk := range chunks {
		if err := emitter.Consume(chunk); err != nil {
			t.Fatalf("Consume 失败: %v", err)
		}
	}

	// 回头再给工具 0 的分片 —— 它的块已关闭，无法再追加，必须报错。
	if err := emitter.Consume(toolArgChunk(0, `1}`)); err == nil {
		t.Fatal("交错的参数分片必须报错（详见任务说明的备选缓冲方案）")
	}
}

func TestEventEmitterDuplicateToolStartFails(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	if err := emitter.Consume(toolStartChunk(0, "call_a", "f")); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	// 同一个 tool index 再来一次首发分片：映射会被覆盖、旧块的分片会串到新块，
	// 必须报错而不是悄悄改映射。
	if err := emitter.Consume(toolStartChunk(0, "call_a2", "f")); err == nil {
		t.Fatal("重复的工具首发分片必须报错")
	}
}

func TestEventEmitterIncompleteToolStartFails(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	// 有 id 但没 name 的首发分片：无法构造合法的 tool_use 块。
	call := StreamToolCall{Index: 0, ID: "call_1", Type: "function"}
	chunk := StreamChunk{
		ID:      "s1",
		Model:   "m",
		Choices: []StreamChoice{{Index: 0, Delta: StreamDelta{ToolCalls: []StreamToolCall{call}}}},
	}
	err := emitter.Consume(chunk)
	if err == nil {
		t.Fatal("缺 name 的工具首发分片必须报错")
	}
	// 错误必须点名缺的是 name，措辞与非流式 BuildResponse 的同一守卫保持一致。
	if !strings.Contains(err.Error(), "缺少 name") || strings.Contains(err.Error(), " id") {
		t.Fatalf("错误应点名缺少的是 name 且不提 id，实际 %v", err)
	}
}

// TestEventEmitterToolStartWithoutIDFails 是上一条的镜像。
//
// openToolBlock 的守卫是 call.ID == "" || call.Function.Name == ""，但此前只有「有 id 没 name」
// 一条用例，把守卫削成只查 Name 整包测试仍全绿 —— 「有 name 没 id」这个分支无人把守。
// Anthropic 协议要求 tool_use 块的 id 与 name 双必填，缺 id 的块客户端无法回传结果。
func TestEventEmitterToolStartWithoutIDFails(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	// 有 name 但没 id 的首发分片：同样无法构造合法的 tool_use 块。
	call := StreamToolCall{Index: 0, Type: "function"}
	call.Function.Name = "get_weather"
	chunk := StreamChunk{
		ID:      "s1",
		Model:   "m",
		Choices: []StreamChoice{{Index: 0, Delta: StreamDelta{ToolCalls: []StreamToolCall{call}}}},
	}
	err := emitter.Consume(chunk)
	if err == nil {
		t.Fatal("缺 id 的工具首发分片必须报错")
	}
	// 同时不得提到 name，理由同非流式那条：不可区分的措辞含「缺少 id」子串。
	if !strings.Contains(err.Error(), "缺少 id") || strings.Contains(err.Error(), "name") {
		t.Fatalf("错误应点名缺少的是 id 且不提 name，实际 %v", err)
	}
}

func TestEventEmitterBlockStartStopAlwaysPaired(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", true, log.emit)

	chunks := []StreamChunk{
		reasoningChunk("想"), textChunk("说"),
		toolStartChunk(0, "c0", "f"), toolArgChunk(0, "{}"),
		toolStartChunk(1, "c1", "g"), toolArgChunk(1, "{}"),
		finishChunk("tool_calls", 1, 1),
	}
	for _, chunk := range chunks {
		if err := emitter.Consume(chunk); err != nil {
			t.Fatalf("Consume 失败: %v", err)
		}
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	var depth int
	var seenIndexes []float64
	for _, event := range log.events {
		switch event["type"] {
		case "content_block_start":
			depth++
			if depth != 1 {
				t.Fatalf("块不可嵌套，当前深度 %d", depth)
			}
			seenIndexes = append(seenIndexes, event["index"].(float64))
		case "content_block_stop":
			depth--
			if depth != 0 {
				t.Fatalf("content_block_stop 未配对，当前深度 %d", depth)
			}
		}
	}
	if depth != 0 {
		t.Fatal("流结束时仍有未关闭的内容块")
	}
	if len(seenIndexes) != 4 {
		t.Fatalf("应有 4 个内容块（thinking + text + 两个 tool_use），实际 %d：%v", len(seenIndexes), log.types())
	}
	for i, index := range seenIndexes {
		if int(index) != i {
			t.Fatalf("块 index 必须从 0 起严格递增，实际 %v", seenIndexes)
		}
	}
	if log.types()[0] != "message_start" {
		t.Fatal("message_start 必须是首个事件")
	}
	if last := log.types()[len(log.types())-1]; last != "message_stop" {
		t.Fatalf("message_stop 必须是末个事件，实际 %q", last)
	}
}
