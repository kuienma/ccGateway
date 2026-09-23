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
	if start["model"] != "deepseek-v4-flash-ga-260731" {
		t.Fatalf("model = %v", start["model"])
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

	for _, event := range log.events {
		if encoded, _ := json.Marshal(event); string(encoded) != "" {
			if block, ok := event["content_block"].(map[string]any); ok && block["type"] == "thinking" {
				t.Fatal("includeThinking=false 时不应产出 thinking 块")
			}
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
	if err := emitter.Consume(finishChunk("stop", 7, 4)); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	// 末尾纯 usage chunk：choices 为空，不应产出任何事件。
	before := len(log.events)
	if err := emitter.Consume(StreamChunk{ID: "s1", Model: "m", Usage: &Usage{PromptTokens: 7, CompletionTokens: 4}}); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	if len(log.events) != before {
		t.Fatalf("纯 usage chunk 不应产出事件，新增了 %d 个", len(log.events)-before)
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
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
