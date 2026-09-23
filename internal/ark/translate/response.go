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
// includeThinking 为真时把 reasoning_content 作为 thinking 块透出（排在最前）。
func BuildResponse(payload []byte, includeThinking bool) ([]byte, error) {
	var source ChatResponse
	if err := json.Unmarshal(payload, &source); err != nil {
		return nil, fmt.Errorf("解析 Ark 响应: %w", err)
	}
	if len(source.Choices) == 0 {
		return nil, fmt.Errorf("Ark 响应不含 choices")
	}
	choice := source.Choices[0]

	result := anthropicResponse{
		ID:         source.ID,
		Type:       "message",
		Role:       "assistant",
		Model:      source.Model,
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
	for index, call := range choice.Message.ToolCalls {
		input := json.RawMessage(call.Function.Arguments)
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		if !json.Valid(input) {
			return nil, fmt.Errorf("tool_calls[%d].function.arguments 不是合法 JSON", index)
		}
		result.Content = append(result.Content, responseBlock{
			Type:  "tool_use",
			ID:    call.ID,
			Name:  call.Function.Name,
			Input: input,
		})
	}
	// content 为空数组时保持 [] 而不是 null，Claude Code 按数组解析。
	if result.Content == nil {
		result.Content = []responseBlock{}
	}
	return json.Marshal(result)
}
