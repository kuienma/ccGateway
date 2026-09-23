package translate

import (
	"encoding/json"
	"testing"
)

// collectEvents 收集状态机产出的事件，按 type 顺序返回，便于断言序列。
type eventLog struct {
	events []map[string]any
}

func (l *eventLog) emit(payload []byte) error {
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return err
	}
	l.events = append(l.events, decoded)
	return nil
}

func (l *eventLog) types() []string {
	result := make([]string, 0, len(l.events))
	for _, event := range l.events {
		result = append(result, event["type"].(string))
	}
	return result
}

func (l *eventLog) ofType(t *testing.T, kind string) []map[string]any {
	t.Helper()
	var result []map[string]any
	for _, event := range l.events {
		if event["type"] == kind {
			result = append(result, event)
		}
	}
	return result
}

func assertTypes(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("事件序列长度 = %d，期望 %d\n实际: %v\n期望: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("事件[%d] = %q，期望 %q\n实际: %v\n期望: %v", i, got[i], want[i], got, want)
		}
	}
}

func textChunk(content string) StreamChunk {
	return StreamChunk{
		ID:      "s1",
		Model:   "m",
		Choices: []StreamChoice{{Index: 0, Delta: StreamDelta{Role: "assistant", Content: content}}},
	}
}

func reasoningChunk(reasoning string) StreamChunk {
	return StreamChunk{
		ID:    "s1",
		Model: "m",
		Choices: []StreamChoice{{Index: 0, Delta: StreamDelta{
			Role: "assistant", Content: "", ReasoningContent: reasoning,
		}}},
	}
}

func finishChunk(reason string, prompt, completion int) StreamChunk {
	return StreamChunk{
		ID:      "s1",
		Model:   "m",
		Choices: []StreamChoice{{Index: 0, FinishReason: reason, Delta: StreamDelta{Role: "assistant"}}},
		Usage:   &Usage{PromptTokens: prompt, CompletionTokens: completion},
	}
}

func TestEventEmitterTextOnlySequence(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("deepseek-v4-flash-ga-260731", false, log.emit)

	for _, chunk := range []StreamChunk{textChunk("你"), textChunk("好"), finishChunk("stop", 10, 2)} {
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
		"content_block_stop",
		"message_delta",
		"message_stop",
	})

	start := log.events[0]["message"].(map[string]any)
	if start["role"] != "assistant" || start["type"] != "message" {
		t.Fatalf("message_start.message = %#v", start)
	}
	// model 以上游 chunk 回显的值为准（与非流式 BuildResponse 用 source.Model 同源），
	// 而不是网关请求时传入的 modelID —— 这里 chunk 回显 "m"，就应该吐 "m"。
	if start["model"] != "m" {
		t.Fatalf("model 应取上游回显值 \"m\"，实际 %v", start["model"])
	}
	if start["id"] == "" || start["id"] == nil {
		t.Fatal("message_start 必须带 id")
	}

	block := log.events[1]["content_block"].(map[string]any)
	if block["type"] != "text" {
		t.Fatalf("首块应为 text，实际 %v", block["type"])
	}
	if log.events[1]["index"].(float64) != 0 {
		t.Fatalf("首块 index 应为 0，实际 %v", log.events[1]["index"])
	}

	delta := log.events[2]["delta"].(map[string]any)
	if delta["type"] != "text_delta" || delta["text"] != "你" {
		t.Fatalf("delta = %#v", delta)
	}

	messageDelta := log.events[5]
	if messageDelta["delta"].(map[string]any)["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %#v", messageDelta["delta"])
	}
	usage := messageDelta["usage"].(map[string]any)
	if usage["output_tokens"].(float64) != 2 {
		t.Fatalf("output_tokens = %v", usage["output_tokens"])
	}
	if usage["input_tokens"].(float64) != 10 {
		t.Fatalf("message_delta 应补上真实 input_tokens，实际 %v", usage["input_tokens"])
	}
}

func TestEventEmitterFallsBackToModelIDWhenChunkOmitsModel(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("deepseek-v4-flash-ga-260731", false, log.emit)

	// 首个 chunk 不带 model 字段：必须回落到网关传入的 modelID。
	// 少了这条回落，客户端会收到 "model":""。
	noModel := StreamChunk{
		ID:      "s1",
		Choices: []StreamChoice{{Index: 0, Delta: StreamDelta{Role: "assistant", Content: "答"}}},
	}
	if err := emitter.Consume(noModel); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	if err := emitter.Consume(finishChunk("stop", 1, 1)); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	start := log.events[0]["message"].(map[string]any)
	if start["model"] != "deepseek-v4-flash-ga-260731" {
		t.Fatalf("chunk 未带 model 时应回落到 modelID，实际 %#v", start["model"])
	}
}

func TestEventEmitterSkipsEmptyContent(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	// 纯思考阶段的 chunk：content 是空串，reasoning 未开启透出。
	for _, chunk := range []StreamChunk{reasoningChunk("思考"), textChunk(""), finishChunk("stop", 5, 1)} {
		if err := emitter.Consume(chunk); err != nil {
			t.Fatalf("Consume 失败: %v", err)
		}
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	// 全程没有非空内容，不应开任何 content block。
	assertTypes(t, log.types(), []string{"message_start", "message_delta", "message_stop"})
}

func TestEventEmitterThinkingThenText(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", true, log.emit)

	chunks := []StreamChunk{
		reasoningChunk("先"), reasoningChunk("想"),
		textChunk("答"), finishChunk("stop", 8, 3),
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
		"content_block_start", // thinking
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",  // thinking 关闭
		"content_block_start", // text
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	})

	thinkingStart := log.events[1]
	if thinkingStart["content_block"].(map[string]any)["type"] != "thinking" {
		t.Fatalf("首块应为 thinking，实际 %#v", thinkingStart["content_block"])
	}
	if thinkingStart["index"].(float64) != 0 {
		t.Fatalf("thinking 块 index = %v", thinkingStart["index"])
	}

	thinkingDelta := log.events[2]["delta"].(map[string]any)
	if thinkingDelta["type"] != "thinking_delta" || thinkingDelta["thinking"] != "先" {
		t.Fatalf("thinking_delta = %#v", thinkingDelta)
	}

	textStart := log.events[5]
	if textStart["content_block"].(map[string]any)["type"] != "text" {
		t.Fatalf("第二块应为 text，实际 %#v", textStart["content_block"])
	}
	if textStart["index"].(float64) != 1 {
		t.Fatalf("text 块 index 应为 1（全局递增），实际 %v", textStart["index"])
	}
}

func TestEventEmitterDropsReasoningWhenDisabled(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	for _, chunk := range []StreamChunk{reasoningChunk("不该出现"), textChunk("答"), finishChunk("stop", 1, 1)} {
		if err := emitter.Consume(chunk); err != nil {
			t.Fatalf("Consume 失败: %v", err)
		}
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	// assertTypes 只比对 type 序列、不看 content_block.type，所以这条独立断言有价值。
	for _, event := range log.events {
		if block, ok := event["content_block"].(map[string]any); ok && block["type"] == "thinking" {
			t.Fatal("includeThinking=false 时不应产出 thinking 块")
		}
	}
	assertTypes(t, log.types(), []string{
		"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	})
}

func TestEventEmitterIgnoresUsageOnlyChunk(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	if err := emitter.Consume(textChunk("hi")); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	// 纯 usage chunk（choices 为空数组）：先吃掉 usage，再按长度守卫提前返回，
	// 既不能 panic（不可直取 Choices[0]），也不能产出任何事件。
	//
	// 它刻意排在 finish **之前** —— 排在 finish 之后会被 finished 早退直接挡掉，
	// len(Choices)==0 这条守卫就永远走不到，用例会自我失效（而那条早退分支
	// 已由 ConsumeAfterFinishIsIgnored 覆盖）。
	before := len(log.events)
	if err := emitter.Consume(StreamChunk{ID: "s1", Model: "m", Usage: &Usage{PromptTokens: 7, CompletionTokens: 4}}); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	if len(log.events) != before {
		t.Fatalf("纯 usage chunk 不应产出事件，新增了 %d 个", len(log.events)-before)
	}
	// finish chunk 带同一份 usage —— 实测上游就是这样把 usage 重复给两次。
	// 记录 usage 必须是赋值：写成累加这里会得到 14/8。
	if err := emitter.Consume(finishChunk("stop", 7, 4)); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	deltas := log.ofType(t, "message_delta")
	if len(deltas) != 1 {
		t.Fatalf("message_delta 数 = %d", len(deltas))
	}
	usage := deltas[0]["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 7 {
		t.Fatalf("usage 重复出现必须覆盖而非累加，input_tokens = %v，期望 7", usage["input_tokens"])
	}
	if usage["output_tokens"].(float64) != 4 {
		t.Fatalf("usage 重复出现必须覆盖而非累加，output_tokens = %v，期望 4", usage["output_tokens"])
	}
}

func TestEventEmitterKeepsUsageFromChoicelessChunk(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	// 本用例里唯一的 usage 来源就是这个 choices 为空的 chunk：它必须在长度守卫提前返回
	// **之前**就被记下。若把 usage 的读取挪到长度检查之后，这份 usage 会整份丢掉。
	// 同时它不带 choices，所以不该启动消息 —— message_start 此时还不能出现。
	if err := emitter.Consume(StreamChunk{ID: "s1", Model: "m", Usage: &Usage{PromptTokens: 7, CompletionTokens: 4}}); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	if len(log.events) != 0 {
		t.Fatalf("choices 为空的 chunk 不应启动消息，实际产出 %v", log.types())
	}

	if err := emitter.Consume(textChunk("hi")); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	// 此时 usage 已记下 7/4，但 message_start 必须仍填 0 占位 ——
	// 真实值要等到 message_delta 才给，这是 brief 明确的时序设计。
	startUsage := log.events[0]["message"].(map[string]any)["usage"].(map[string]any)
	if startUsage["input_tokens"].(float64) != 0 || startUsage["output_tokens"].(float64) != 0 {
		t.Fatalf("message_start 的 usage 必须是 0 占位，实际 %#v", startUsage)
	}

	// finish chunk 刻意不带 usage，坐实上面那个 chunk 是唯一来源。
	noUsageFinish := StreamChunk{
		ID:      "s1",
		Model:   "m",
		Choices: []StreamChoice{{Index: 0, FinishReason: "stop", Delta: StreamDelta{Role: "assistant"}}},
	}
	if err := emitter.Consume(noUsageFinish); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	deltas := log.ofType(t, "message_delta")
	if len(deltas) != 1 {
		t.Fatalf("message_delta 数 = %d", len(deltas))
	}
	usage := deltas[0]["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 7 || usage["output_tokens"].(float64) != 4 {
		t.Fatalf("choices 为空的 chunk 带的 usage 必须被记下，实际 %#v", usage)
	}
}

func TestEventEmitterMessageIDShape(t *testing.T) {
	t.Parallel()

	// id 形态由 newMessageID 统一构造，流式与非流式共用，所以这里锁住的是两条路径的形态。
	cases := []struct{ name, upstreamID, want string }{
		{"带上游 id", "s1", "msg_s1"},
		{"上游未给 id", "", "msg_ark"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			log := &eventLog{}
			emitter := NewEventEmitter("m", false, log.emit)
			chunk := StreamChunk{
				ID:      tc.upstreamID,
				Model:   "m",
				Choices: []StreamChoice{{Index: 0, Delta: StreamDelta{Role: "assistant", Content: "答"}}},
			}
			if err := emitter.Consume(chunk); err != nil {
				t.Fatalf("Consume 失败: %v", err)
			}
			if got := log.events[0]["message"].(map[string]any)["id"]; got != tc.want {
				t.Fatalf("message id = %v，期望 %q", got, tc.want)
			}
		})
	}
}

func TestEventEmitterStopReasonMapping(t *testing.T) {
	t.Parallel()

	cases := []struct{ finish, want string }{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"tool_calls", "tool_use"},
	}
	for _, tc := range cases {
		t.Run(tc.finish, func(t *testing.T) {
			t.Parallel()
			log := &eventLog{}
			emitter := NewEventEmitter("m", false, log.emit)
			if err := emitter.Consume(textChunk("x")); err != nil {
				t.Fatalf("Consume 失败: %v", err)
			}
			if err := emitter.Consume(finishChunk(tc.finish, 1, 1)); err != nil {
				t.Fatalf("Consume 失败: %v", err)
			}
			if err := emitter.Finish(true); err != nil {
				t.Fatalf("Finish 失败: %v", err)
			}
			deltas := log.ofType(t, "message_delta")
			if len(deltas) != 1 {
				t.Fatalf("message_delta 数 = %d", len(deltas))
			}
			if got := deltas[0]["delta"].(map[string]any)["stop_reason"]; got != tc.want {
				t.Fatalf("stop_reason = %v，期望 %q", got, tc.want)
			}
		})
	}
}

func TestEventEmitterFinishWithoutFinishReasonFails(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)
	if err := emitter.Consume(textChunk("半句")); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}

	// 没收到 finish_reason 就结束 —— 上游断流，必须报错让上层按可重试处理。
	if err := emitter.Finish(true); err == nil {
		t.Fatal("缺少 finish_reason 时 Finish 必须报错")
	}
}

func TestEventEmitterFinishWithoutDoneAfterFinishReasonSucceeds(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)
	if err := emitter.Consume(textChunk("完整")); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	if err := emitter.Consume(finishChunk("stop", 1, 1)); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}

	// 收到 finish_reason 就说明内容已完整，此时缺 [DONE] 不单独报错 ——
	// 否则会让用户为一次已经收全的回复白等一轮重试。
	if err := emitter.Finish(false); err != nil {
		t.Fatalf("已收到 finish_reason 时缺 [DONE] 不应报错，实际 %v", err)
	}
	if got := log.types()[len(log.types())-1]; got != "message_stop" {
		t.Fatalf("末个事件 = %q，期望 message_stop", got)
	}
}

func TestEventEmitterFinishBeforeAnyChunkFails(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)
	if err := emitter.Finish(false); err == nil {
		t.Fatal("一个 chunk 都没收到时 Finish 必须报错")
	}
	if len(log.events) != 0 {
		t.Fatal("没有 chunk 时不应产出任何事件")
	}
}

func TestEventEmitterConsumeAfterFinishIsIgnored(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)
	if err := emitter.Consume(textChunk("x")); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	if err := emitter.Consume(finishChunk("stop", 1, 1)); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	before := len(log.events)
	// 上游在 finish 之后又发内容 chunk：忽略，不能再开块（message_stop 已发出）。
	if err := emitter.Consume(textChunk("多余")); err != nil {
		t.Fatalf("finish 后的多余 chunk 应被忽略而非报错: %v", err)
	}
	if len(log.events) != before {
		t.Fatal("message_stop 之后不应再产出事件")
	}
}
