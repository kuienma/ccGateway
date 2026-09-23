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
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicBlock 覆盖 text / image / tool_use / tool_result 四种块，
// 各自的专属字段在 Task 4 使用。
type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
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

// convertMessage 把一条 Anthropic 消息转成一条或多条 OpenAI 消息。
// Task 4 会在此扩展 tool_use / tool_result 分支。
func convertMessage(index int, message anthropicMessage) ([]ChatMessage, *Rejection) {
	text, rejection := flattenText(message.Content, fmt.Sprintf("messages[%d].content", index))
	if rejection != nil {
		return nil, rejection
	}
	return []ChatMessage{{Role: message.Role, Content: text}}, nil
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
