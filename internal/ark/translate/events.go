package translate

import (
	"encoding/json"
	"fmt"
)

// 块类型。
const (
	blockKindThinking = "thinking"
	blockKindText     = "text"
	blockKindToolUse  = "tool_use"
)

// EventEmitter 把 Ark 的扁平 delta 流重组成 Anthropic 的事件序列。
//
// 不变式：
//   - index 是全局递增的 content block 序号，与 OpenAI 的 tool_calls[].index 无关；
//   - 每个 content_block_start 都有配对的 content_block_stop；
//   - message_start 必为首个事件，message_stop 必为末个事件。
//
// 实测 Ark 会先吐完 reasoning_content 才开始 content / tool_calls，
// 恰好满足 Anthropic 要求 thinking 块排在最前，因此无需缓冲重排。
type EventEmitter struct {
	emit            func([]byte) error
	modelID         string
	includeThinking bool

	started   bool
	finished  bool
	messageID string

	nextIndex int
	openKind  string
	openIndex int

	// toolBlocks 把 OpenAI 的 tool_calls[].index 映射到 Anthropic 的 block index。
	toolBlocks map[int]int

	inputTokens  int
	outputTokens int
}

func NewEventEmitter(modelID string, includeThinking bool, emit func([]byte) error) *EventEmitter {
	return &EventEmitter{
		emit:            emit,
		modelID:         modelID,
		includeThinking: includeThinking,
		toolBlocks:      make(map[int]int),
	}
}

// Consume 处理一个上游 chunk，按需产出 Anthropic 事件。
func (e *EventEmitter) Consume(chunk StreamChunk) error {
	if e.finished {
		// message_stop 之后上游仍在发数据：忽略，避免产出非法事件。
		// scanSSE 见到 [DONE] 后是 continue 而非 break，之后的 data 行仍会回调到这里。
		return nil
	}
	if chunk.Usage != nil {
		// 实测末尾有两个 chunk 带同一份 usage（finish_reason 那个，以及 choices 为空的纯
		// usage chunk），所以这里必须是赋值而非累加，否则 token 会翻倍。
		// output_tokens 直接取 completion_tokens：reasoning_tokens 是 completion_tokens 的
		// 内部拆分明细（实测 prompt+completion=total 恒等），叠加会重复计数。
		e.inputTokens = chunk.Usage.PromptTokens
		e.outputTokens = chunk.Usage.CompletionTokens
	}
	if len(chunk.Choices) == 0 {
		// 末尾的纯 usage chunk。
		return nil
	}
	if err := e.ensureStarted(chunk); err != nil {
		return err
	}

	choice := chunk.Choices[0]
	if err := e.consumeReasoning(choice.Delta.ReasoningContent); err != nil {
		return err
	}
	if err := e.consumeText(choice.Delta.Content); err != nil {
		return err
	}
	if err := e.consumeToolCalls(choice.Delta.ToolCalls); err != nil {
		return err
	}
	// 只看 finish_reason 字段本身是否非空：MapStopReason("") 会返回 end_turn，
	// 拿映射结果判断流是否结束会把每个中间 chunk 都误判成结束。
	if choice.FinishReason != "" {
		return e.complete(MapStopReason(choice.FinishReason))
	}
	return nil
}

// Finish 在上游流结束后收尾。sawDone 表示是否见到 [DONE]。
//
// 缺 [DONE] 不单独报错：收到 finish_reason 就说明内容已完整，此时 complete 已把
// finished 置真并走到下面的早退，sawDone 只进错误信息用于诊断。
func (e *EventEmitter) Finish(sawDone bool) error {
	if e.finished {
		return nil
	}
	if !e.started {
		return fmt.Errorf("Ark 流未产出任何事件")
	}
	// 收到内容但没收到 finish_reason，说明流被截断。
	// 此时已有 content_block_start 发出，客户端在等后续事件，必须报错让上层按可重试处理。
	return fmt.Errorf("Ark 流意外结束：缺少 finish_reason（sawDone=%v）", sawDone)
}

func (e *EventEmitter) ensureStarted(chunk StreamChunk) error {
	if e.started {
		return nil
	}
	e.started = true
	e.messageID = "msg_ark"
	if chunk.ID != "" {
		e.messageID = "msg_" + chunk.ID
	}
	// model 取网关本次请求用的 modelID，而不是某个 chunk 回显的值：message_start 必须在
	// 流开头就发出，此时只有 modelID 是确定非空的（路由层已解析）。实测 Ark 会把同一个
	// modelID 原样回显在每个 chunk 的 model 字段里，两者在线上恒等。
	// 上游只在流末尾给 usage，这里先填 0，结束时在 message_delta 里补真实值。
	return e.write(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            e.messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         e.modelID,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func (e *EventEmitter) consumeReasoning(reasoning string) error {
	if reasoning == "" || !e.includeThinking {
		return nil
	}
	if err := e.openBlockOfKind(blockKindThinking, nil); err != nil {
		return err
	}
	return e.write(map[string]any{
		"type":  "content_block_delta",
		"index": e.openIndex,
		"delta": map[string]any{"type": "thinking_delta", "thinking": reasoning},
	})
}

// consumeText 只在内容非空时开块 —— Ark 的 delta.content 恒存在，
// 纯思考阶段是空串，凭字段存在开块会产出空的 text 块并破坏事件序列。
func (e *EventEmitter) consumeText(content string) error {
	if content == "" {
		return nil
	}
	if err := e.openBlockOfKind(blockKindText, nil); err != nil {
		return err
	}
	return e.write(map[string]any{
		"type":  "content_block_delta",
		"index": e.openIndex,
		"delta": map[string]any{"type": "text_delta", "text": content},
	})
}

// consumeToolCalls 在 Task 9 实现。
func (e *EventEmitter) consumeToolCalls(calls []StreamToolCall) error {
	return nil
}

// openBlockOfKind 确保当前打开的块是指定类型；类型不同则先关闭旧块再开新块。
// contentBlock 为 nil 时按 kind 生成默认的空块体。
func (e *EventEmitter) openBlockOfKind(kind string, contentBlock map[string]any) error {
	if e.openKind == kind && kind != blockKindToolUse {
		return nil
	}
	if err := e.closeOpenBlock(); err != nil {
		return err
	}
	if contentBlock == nil {
		switch kind {
		case blockKindThinking:
			contentBlock = map[string]any{"type": blockKindThinking, "thinking": ""}
		case blockKindText:
			contentBlock = map[string]any{"type": blockKindText, "text": ""}
		default:
			return fmt.Errorf("缺少 %s 块的块体", kind)
		}
	}
	e.openKind = kind
	e.openIndex = e.nextIndex
	e.nextIndex++
	return e.write(map[string]any{
		"type":          "content_block_start",
		"index":         e.openIndex,
		"content_block": contentBlock,
	})
}

func (e *EventEmitter) closeOpenBlock() error {
	if e.openKind == "" {
		return nil
	}
	index := e.openIndex
	e.openKind = ""
	return e.write(map[string]any{"type": "content_block_stop", "index": index})
}

// complete 关闭当前块并收尾整条消息。
func (e *EventEmitter) complete(stopReason string) error {
	if err := e.closeOpenBlock(); err != nil {
		return err
	}
	// input_tokens 在 message_start 时还拿不到，这里一并给出真实值。
	if err := e.write(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{
			"input_tokens":  e.inputTokens,
			"output_tokens": e.outputTokens,
		},
	}); err != nil {
		return err
	}
	e.finished = true
	return e.write(map[string]any{"type": "message_stop"})
}

func (e *EventEmitter) write(event map[string]any) error {
	encoded, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("编码 Anthropic 事件: %w", err)
	}
	return e.emit(encoded)
}
