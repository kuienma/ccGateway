package translate

import (
	"encoding/json"
	"fmt"
)

// ChatResponse 是 Ark /chat/completions 的非流式响应体。
type ChatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   *Usage       `json:"usage"`
}

type ChatChoice struct {
	Index        int                 `json:"index"`
	FinishReason string              `json:"finish_reason"`
	Message      ChatResponseMessage `json:"message"`
}

type ChatResponseMessage struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content"`
	ToolCalls        []ToolCall `json:"tool_calls"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// anthropicResponse 是回给客户端的 Anthropic Messages 响应。
type anthropicResponse struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Role         string          `json:"role"`
	Model        string          `json:"model"`
	Content      []responseBlock `json:"content"`
	StopReason   string          `json:"stop_reason"`
	StopSequence *string         `json:"stop_sequence"`
	Usage        anthropicUsage  `json:"usage"`
}

type responseBlock struct {
	Type string `json:"type"`
	// text 块
	Text string `json:"text,omitempty"`
	// thinking 块
	Thinking string `json:"thinking,omitempty"`
	// tool_use 块
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// FinishReasonContentFilter 是 OpenAI 协议表示「被内容过滤截断」的 finish_reason。
//
// Anthropic 的 stop_reason 枚举里没有对应值，硬造一个会让严格校验的客户端报错，
// 所以 MapStopReason 把它归到 end_turn —— 代价是客户端分不清「正常说完」与
// 「被过滤截断」。为了不让这件事在服务端也无迹可寻，client 层按这个常量留一条
// warn 日志（translate 是纯函数集合、不做 IO，所以留痕点在 client 而不是这里）。
const FinishReasonContentFilter = "content_filter"

// FinishReasonOf 从 Ark 非流式响应体里取出 finish_reason，**仅供留痕/日志使用**。
//
// 刻意复用 ChatResponse 而不另写一份窄结构：上游响应的形状在本包只声明一次，
// 免得日后字段改名时漏掉这里。代价是多一次 json.Unmarshal，相对一次上游往返
// （推理模型动辄数秒）可以忽略。解析失败或没有 choices 时返回空串 ——
// 它不承担任何校验职责，校验在 BuildResponse。
func FinishReasonOf(payload []byte) string {
	var source ChatResponse
	if err := json.Unmarshal(payload, &source); err != nil || len(source.Choices) == 0 {
		return ""
	}
	return source.Choices[0].FinishReason
}

// MapStopReason 把 OpenAI 的 finish_reason 映射成 Anthropic 的 stop_reason。
// 未知值一律按 end_turn 处理，避免客户端收到不认识的枚举。
func MapStopReason(finishReason string) string {
	switch finishReason {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

// BuildResponse 把 Ark 非流式响应转成 Anthropic 响应。
// modelID 是网关本次请求的模型名，仅在上游不回显 model 时作为回落值使用。
// includeThinking 为真时把 reasoning_content 作为 thinking 块透出（排在最前）。
func BuildResponse(payload []byte, modelID string, includeThinking bool) ([]byte, error) {
	var source ChatResponse
	if err := json.Unmarshal(payload, &source); err != nil {
		return nil, fmt.Errorf("解析 Ark 响应: %w", err)
	}
	if len(source.Choices) == 0 {
		return nil, fmt.Errorf("Ark 响应不含 choices")
	}
	choice := source.Choices[0]

	// model 以上游回显值为准，与流式 ensureStarted 同一取值来源：万一 Ark 做模型路由或降级，
	// 两条路径都会如实报告实际模型，而不是我们请求的那个。
	// 上游没回显时回落到传入的 modelID，避免客户端收到 "model":""。
	model := source.Model
	if model == "" {
		model = modelID
	}

	result := anthropicResponse{
		// 与流式路径共用 newMessageID，确保两条路径给客户端的 id 形态一致。
		ID:         newMessageID(source.ID),
		Type:       "message",
		Role:       "assistant",
		Model:      model,
		StopReason: MapStopReason(choice.FinishReason),
	}
	if source.Usage != nil {
		result.Usage = anthropicUsage{
			InputTokens:  source.Usage.PromptTokens,
			OutputTokens: source.Usage.CompletionTokens,
		}
	}

	// Anthropic 要求 thinking 块排在其他块之前。
	if includeThinking && choice.Message.ReasoningContent != "" {
		result.Content = append(result.Content, responseBlock{
			Type:     "thinking",
			Thinking: choice.Message.ReasoningContent,
		})
	}
	// Ark 在纯工具调用时 content 是空字符串，不能产出空 text 块。
	if choice.Message.Content != "" {
		result.Content = append(result.Content, responseBlock{
			Type: "text",
			Text: choice.Message.Content,
		})
	}
	// seenToolIDs 拦跨调用的 id 重复：Ark 若在一次响应里给两个不同的工具调用下发同一个 id，
	// 客户端会把两次调用当成一次（只回一个 tool_result），第二个工具的结果永远回不去，
	// 表现为「模型重复调用同一个工具」，静默且极难定位到网关。
	// 与流式 openToolBlock 的同一守卫保持一致（含错误措辞）。
	seenToolIDs := make(map[string]bool, len(choice.Message.ToolCalls))
	for index, call := range choice.Message.ToolCalls {
		// Anthropic 要求 tool_use 块的 id 与 name 双必填，而下面 responseBlock 的 ID / Name
		// 两个字段都带 omitempty：上游给空 id 或空 name 时，那个字段会在序列化结果里整个
		// 消失，客户端拿到一个无法执行、也无法回传结果的 tool_use 块，而网关一声不响。
		// 这里硬失败而不是填占位符 —— 宁可报错，也不要把损坏数据转发给客户端。
		// 与流式 openToolBlock 的守卫是同一条语义，两条路径必须一致（含错误措辞）。
		// 分开两条判断只为点名实际缺失的字段：这条错误是运维在 5xx 里唯一能看到的信息。
		if call.ID == "" {
			return nil, fmt.Errorf("tool_calls[%d] 缺少 id", index)
		}
		if call.Function.Name == "" {
			return nil, fmt.Errorf("tool_calls[%d] 缺少 name", index)
		}
		if seenToolIDs[call.ID] {
			return nil, fmt.Errorf("tool_calls[%d] 的 id %q 与前面的工具调用重复，上游在一次响应里给不同调用下发了相同 id",
				index, call.ID)
		}
		seenToolIDs[call.ID] = true
		input := json.RawMessage(call.Function.Arguments)
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		// Anthropic 的 tool_use.input 必须是对象，所以这里不能只用 json.Valid：
		// 123 / "abc" / [1,2] / true / null 都是合法 JSON，放过去会让客户端拿到
		// 非对象的 input。其中 null 最糟——omitempty 对 4 字节的 RawMessage 不生效，
		// 字段照样吐出，客户端走的是空指针路径而不是类型错误。
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(input, &probe); err != nil {
			return nil, fmt.Errorf("tool_calls[%d].function.arguments 必须是 JSON 对象: %w", index, err)
		}
		// JSON null 解进 map 是合法的 no-op（不报错，probe 为 nil），必须单独拦。
		if probe == nil {
			return nil, fmt.Errorf("tool_calls[%d].function.arguments 必须是 JSON 对象，实际为 null", index)
		}
		result.Content = append(result.Content, responseBlock{
			Type:  "tool_use",
			ID:    call.ID,
			Name:  call.Function.Name,
			Input: input,
		})
	}
	// 产出里只要有 tool_use 块，stop_reason 就必须是 tool_use，不管上游 finish_reason 说什么。
	// 直接透传会产出「end_turn + tool_use 块」的矛盾组合：客户端见到 end_turn 就不执行工具，
	// agent 循环当场断掉，且表现为「模型不肯用工具」极难定位到网关。
	// 与流式 EventEmitter.complete 里那条 sawToolUse 覆盖是同一条语义，两条路径必须一致。
	for _, block := range result.Content {
		if block.Type == "tool_use" {
			result.StopReason = "tool_use"
			break
		}
	}
	// content 为空数组时保持 [] 而不是 null，Claude Code 按数组解析。
	if result.Content == nil {
		result.Content = []responseBlock{}
	}
	return json.Marshal(result)
}
