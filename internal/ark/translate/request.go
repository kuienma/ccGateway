package translate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// blockSeparator 是多个文本块合并成单个字符串时的分隔符。
const blockSeparator = "\n\n"

const kindInvalidRequest = "invalid_request_error"

// anthropicRequest 只声明需要转换的字段，其余（top_k、cache_control 等）随解析丢弃。
type anthropicRequest struct {
	System        json.RawMessage    `json:"system"`
	Messages      []anthropicMessage `json:"messages"`
	MaxTokens     int                `json:"max_tokens"`
	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	StopSequences []string           `json:"stop_sequences"`
	Metadata      *struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
	Tools      []anthropicTool `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`
	Thinking   *struct {
		Type string `json:"type"`
	} `json:"thinking"`
}

// anthropicTool 是 Anthropic 的工具定义，input_schema 原样搬到 OpenAI 的
// function.parameters，不做任何 schema 改写。
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// toolChoiceFunction 用具名 struct 而非 map，保证序列化时 type 在 function 之前，
// 与 OpenAI 协议示例一致（map 会按键的字典序输出，function 会排到前面）。
type toolChoiceFunction struct {
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicBlock 覆盖 text / image / tool_use / tool_result / thinking 五种块，
// 字段是各块类型的并集，按 Type 取用。
type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Thinking  string          `json:"thinking"`
}

// BuildRequest 把 Anthropic 请求体转成 Ark 请求。
// anthropicBody 是 anthropic.PrepareRequest 处理过的 body（已摘掉 model / stream）。
func BuildRequest(modelID string, anthropicBody []byte, stream bool) (*ChatRequest, *Rejection) {
	var source anthropicRequest
	if err := json.Unmarshal(anthropicBody, &source); err != nil {
		return nil, &Rejection{Kind: kindInvalidRequest, Message: "请求体必须是 JSON 对象"}
	}

	request := &ChatRequest{
		Model:       modelID,
		MaxTokens:   source.MaxTokens,
		Temperature: source.Temperature,
		TopP:        source.TopP,
		Stop:        source.StopSequences,
		Stream:      stream,
	}
	if stream {
		// 末尾额外一个 choices 为空的 usage chunk，用于拿到真实 token 数。
		request.StreamOptions = &StreamOptions{IncludeUsage: true}
	}
	if source.Metadata != nil {
		request.User = source.Metadata.UserID
	}

	for index, tool := range source.Tools {
		// 校验与下发用同一个 trim 后的值：模型回吐的 tool name 不会带空白，
		// 原样下发会让后续轮次的 tool_use.name 对不上。
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			return nil, &Rejection{
				Kind:    kindInvalidRequest,
				Message: fmt.Sprintf("tools[%d].name 不能为空", index),
			}
		}
		request.Tools = append(request.Tools, Tool{
			Type: "function",
			Function: FunctionSpec{
				Name:        name,
				Description: tool.Description,
				Parameters:  tool.InputSchema,
			},
		})
	}

	toolChoice, rejection := convertToolChoice(source.ToolChoice)
	if rejection != nil {
		return nil, rejection
	}
	request.ToolChoice = toolChoice

	// Ark 不传 thinking 时默认开启思考，而 Anthropic 协议在客户端未请求时
	// 不应返回 thinking 块，因此这里始终显式下发。
	request.Thinking = &Thinking{Type: "disabled"}
	if source.Thinking != nil && source.Thinking.Type == "enabled" {
		request.Thinking.Type = "enabled"
	}

	if len(source.System) > 0 {
		system, rejection := flattenText(source.System, "system")
		if rejection != nil {
			return nil, rejection
		}
		if system != "" {
			request.Messages = append(request.Messages, ChatMessage{Role: "system", Content: system})
		}
	}

	for index, message := range source.Messages {
		converted, rejection := convertMessage(index, message)
		if rejection != nil {
			return nil, rejection
		}
		request.Messages = append(request.Messages, converted...)
	}
	return request, nil
}

// convertToolChoice 把 Anthropic 的 tool_choice 转成 OpenAI 形态。
// auto → "auto"，any → "required"，none → "none"，tool → 指定函数。
func convertToolChoice(raw json.RawMessage) (any, *Rejection) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return nil, &Rejection{Kind: kindInvalidRequest, Message: "tool_choice 必须是对象"}
	}
	switch choice.Type {
	case "auto":
		return "auto", nil
	case "any":
		return "required", nil
	case "none":
		return "none", nil
	case "tool":
		if choice.Name == "" {
			return nil, &Rejection{Kind: kindInvalidRequest, Message: "tool_choice.name 不能为空"}
		}
		specific := toolChoiceFunction{Type: "function"}
		specific.Function.Name = choice.Name
		return specific, nil
	default:
		return nil, &Rejection{
			Kind:    kindInvalidRequest,
			Message: "tool_choice.type 不支持: " + choice.Type,
		}
	}
}

// convertMessage 把一条 Anthropic 消息转成一条或多条 OpenAI 消息。
// tool_result 块必须各自成为独立的 role=tool 消息；
// tool_use 块合并进同一条 assistant 消息的 tool_calls。
//
// 注意：必须先按 block.Type 分类，只把确定是文本的部分交给 flattenText。
// flattenText 对未知块类型一律拒绝，把整块 content 原样丢进去会让所有
// 带工具的请求全部被拒。
func convertMessage(index int, message anthropicMessage) ([]ChatMessage, *Rejection) {
	field := fmt.Sprintf("messages[%d].content", index)
	trimmed := strings.TrimSpace(string(message.Content))

	// 纯字符串 content 没有块结构，直接走文本路径。
	if trimmed == "" || trimmed == "null" || trimmed[0] == '"' {
		text, rejection := flattenText(message.Content, field)
		if rejection != nil {
			return nil, rejection
		}
		return []ChatMessage{{Role: message.Role, Content: text}}, nil
	}

	var blocks []anthropicBlock
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		return nil, &Rejection{Kind: kindInvalidRequest, Message: field + " 必须是字符串或块数组"}
	}

	var (
		texts     []string
		toolCalls []ToolCall
		results   []ChatMessage
	)
	for blockIndex, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				texts = append(texts, block.Text)
			}
		case "thinking", "redacted_thinking":
			// 历史推理内容不回传给 Ark，丢弃。
		case "tool_use":
			if block.ID == "" || block.Name == "" {
				return nil, &Rejection{
					Kind:    kindInvalidRequest,
					Message: fmt.Sprintf("%s[%d] tool_use 缺少 id 或 name", field, blockIndex),
				}
			}
			arguments := strings.TrimSpace(string(block.Input))
			if arguments == "" || arguments == "null" {
				arguments = "{}"
			}
			toolCalls = append(toolCalls, ToolCall{
				ID:       block.ID,
				Type:     "function",
				Function: FunctionCall{Name: block.Name, Arguments: arguments},
			})
		case "tool_result":
			if block.ToolUseID == "" {
				return nil, &Rejection{
					Kind:    kindInvalidRequest,
					Message: fmt.Sprintf("%s[%d] tool_result 缺少 tool_use_id", field, blockIndex),
				}
			}
			content, rejection := flattenText(block.Content, fmt.Sprintf("%s[%d].content", field, blockIndex))
			if rejection != nil {
				return nil, rejection
			}
			results = append(results, ChatMessage{
				Role:       "tool",
				Content:    content,
				ToolCallID: block.ToolUseID,
			})
		case "image":
			return nil, &Rejection{
				Kind:    kindInvalidRequest,
				Message: "该模型不支持图片输入，请移除消息中的图片块",
			}
		case "":
			return nil, &Rejection{
				Kind:    kindInvalidRequest,
				Message: fmt.Sprintf("%s[%d] 缺少 type", field, blockIndex),
			}
		default:
			return nil, &Rejection{
				Kind:    kindInvalidRequest,
				Message: fmt.Sprintf("%s[%d] 不支持的块类型: %s", field, blockIndex, block.Type),
			}
		}
	}

	// tool_result 拆出的 role=tool 消息必须排在最前：OpenAI 要求它们紧跟带
	// tool_calls 的 assistant 消息，中间插入一条 user 消息会打断这个绑定。
	// Anthropic 允许 tool_result 与 text 混在同一条消息里，所以这里要重排。
	converted := results

	// 有文本或工具调用时产出主消息；内容全是 tool_result 时不产出（已拆成独立的
	// role=tool 消息）；内容真的为空时仍产出一条空消息，因为丢消息会破坏 Ark 侧
	// user/assistant 交替，比发一条空消息更危险。
	if len(texts) > 0 || len(toolCalls) > 0 || len(results) == 0 {
		converted = append(converted, ChatMessage{
			Role:      message.Role,
			Content:   strings.Join(texts, blockSeparator),
			ToolCalls: toolCalls,
		})
	}
	return converted, nil
}

// flattenText 把 Anthropic 的 content（字符串或块数组）压成单个字符串。
// 遇到图片块直接拒绝；未知块类型也拒绝，避免静默丢内容。
func flattenText(raw json.RawMessage, field string) (string, *Rejection) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}

	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return "", &Rejection{Kind: kindInvalidRequest, Message: field + " 必须是字符串或块数组"}
		}
		return text, nil
	}

	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", &Rejection{Kind: kindInvalidRequest, Message: field + " 必须是字符串或块数组"}
	}

	var parts []string
	for blockIndex, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				parts = append(parts, block.Text)
			}
		case "image":
			return "", &Rejection{
				Kind:    kindInvalidRequest,
				Message: "该模型不支持图片输入，请移除消息中的图片块",
			}
		case "":
			return "", &Rejection{
				Kind:    kindInvalidRequest,
				Message: fmt.Sprintf("%s[%d] 缺少 type", field, blockIndex),
			}
		default:
			return "", &Rejection{
				Kind:    kindInvalidRequest,
				Message: fmt.Sprintf("%s[%d] 不支持的块类型: %s", field, blockIndex, block.Type),
			}
		}
	}
	return strings.Join(parts, blockSeparator), nil
}
