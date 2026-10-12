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
	// thinking 刻意不在这里声明：解析与类型校验都由 parseThinking 一手承担
	// （WantsThinking 也走它），全仓库单一实现。这里再留一份字段就是给
	// 「两侧同义判断静默分叉」留门 —— 但删它的同时必须把它原先兼任的类型闸门
	// 一起搬过去，否则 thinking 类型写错会从 400 退化成静默关掉思考。
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
	// Source 用指针，因为「没有 source」和「source 是空对象」要给出不同的错误。
	Source *anthropicImageSource `json:"source"`
}

// anthropicImageSource 是 image 块的 source，base64 与 url 两种形态的字段并集。
type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
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
	// 超过模型上限的 max_tokens 直接钳制，不报错 —— Claude Code 常按
	// Anthropic 模型的上限填值，拒绝会让请求白失败。
	if request.MaxTokens > MaxOutputTokens {
		request.MaxTokens = MaxOutputTokens
	}
	// Anthropic 协议里 max_tokens 是必填，但缺省时**不能**在网关层加统一校验：
	// bedrock 路径靠上游返回 400，加校验就改变了 bedrock 的行为（本分支的硬约束是
	// 「Bedrock 路径行为零变化」）。所以只在 ark 这一侧补齐：
	// 字段带 omitempty，0 值会让 max_tokens 整个消失，Ark 转而用它自己的默认输出上限，
	// 而 DeepSeek V4 Flash 把思考也计入输出 —— 默认上限偏小会让回答在思考中途被截断
	// （stop_reason: max_tokens + 空 content），且没有任何信号。
	// 回落到我们的钳制上限是方向安全的选择，而且这个值本身不是新的：
	// 客户端填超限值时下发的就是它。负值不动 —— 那是明确的客户端错误，留给上游拒绝，
	// 悄悄改写成 32768 反而掩盖问题。两条路径的这处差异已在 README 已知限制里申报。
	if request.MaxTokens == 0 {
		request.MaxTokens = MaxOutputTokens
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
	//
	// 判断**必须**走 parseThinking（WantsThinking 也走它）：请求侧（这里）与响应侧
	// （client.go 传给 BuildResponse / NewEventEmitter 的 includeThinking）各写一份同义
	// 判断时，两侧可以静默分叉而九个包全绿，后果是请求侧开思考、响应侧丢弃 thinking 块 ——
	// 用户为被扔掉的 reasoning 付费。配对断言见 TestBuildRequestThinkingMatchesWantsThinking。
	//
	// 同一个调用还兼作类型闸门：thinking 写成对象以外的形态要报 400，不能静默当成没开。
	wantsThinking, thinkingRejection := parseThinking(anthropicBody)
	if thinkingRejection != nil {
		return nil, thinkingRejection
	}
	request.Thinking = &Thinking{Type: "disabled"}
	if wantsThinking {
		request.Thinking.Type = "enabled"
	}

	if len(source.System) > 0 {
		system, rejection := flattenText(source.System, "system")
		if rejection != nil {
			return nil, rejection
		}
		if system != "" {
			request.Messages = append(request.Messages, ChatMessage{Role: "system", Content: TextContent(system)})
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
		return []ChatMessage{{Role: message.Role, Content: TextContent(text)}}, nil
	}

	var blocks []anthropicBlock
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		return nil, &Rejection{Kind: kindInvalidRequest, Message: field + " 必须是字符串或块数组"}
	}

	var (
		// parts 按客户端给的块顺序累积。图文顺序承载指代关系
		// （「这张图上面那段文字」），重排会改变语义。
		parts     []ContentPart
		hasImage  bool
		toolCalls []ToolCall
		results   []ChatMessage
	)
	for blockIndex, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				parts = append(parts, ContentPart{Type: "text", Text: block.Text})
			}
		case "image":
			part, rejection := convertImageSource(block.Source, fmt.Sprintf("%s[%d]", field, blockIndex))
			if rejection != nil {
				return nil, rejection
			}
			parts = append(parts, part)
			hasImage = true
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
			content, rejection := flattenText(block.Content, fmt.Sprintf("%s[%d] tool_result 的 content", field, blockIndex))
			if rejection != nil {
				return nil, rejection
			}
			results = append(results, ChatMessage{
				Role:       "tool",
				Content:    TextContent(content),
				ToolCallID: block.ToolUseID,
			})
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

	// 有内容块或工具调用时产出主消息；内容全是 tool_result 时不产出（已拆成独立的
	// role=tool 消息）；内容真的为空时仍产出一条空消息，因为丢消息会破坏 Ark 侧
	// user/assistant 交替，比发一条空消息更危险。
	if len(parts) > 0 || len(toolCalls) > 0 || len(results) == 0 {
		converted = append(converted, ChatMessage{
			Role:      message.Role,
			Content:   buildContent(parts, hasImage),
			ToolCalls: toolCalls,
		})
	}
	return converted, nil
}

// buildContent 决定 content 用字符串还是 parts 数组下发。
// 没有图片时合并成字符串，保持放开图片之前的线上格式逐字节不变；
// 有图片时保留数组，以维持图文的原始顺序。
func buildContent(parts []ContentPart, hasImage bool) MessageContent {
	if hasImage {
		return MessageContent{Parts: parts}
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		texts = append(texts, part.Text)
	}
	return TextContent(strings.Join(texts, blockSeparator))
}

// convertImageSource 把 Anthropic 的 image.source 转成 OpenAI 的 image_url part。
// field 是该图片块的路径，用于错误定位。
//
// 这里刻意不校验模型是否支持图片：网关内置一份模型能力表必然会过期
// （放开图片之前那句「该模型不支持图片输入」正是这么来的 —— 它是接第一个
// 不支持图片的 endpoint 时写死的，后来接了支持图片的模型就开始误报）。
// 能力的权威在上游，不支持图片的模型由 Ark 自己返回 400。
func convertImageSource(source *anthropicImageSource, field string) (ContentPart, *Rejection) {
	if source == nil {
		return ContentPart{}, &Rejection{
			Kind:    kindInvalidRequest,
			Message: field + " image 缺少 source",
		}
	}
	switch source.Type {
	case "base64":
		if source.MediaType == "" {
			return ContentPart{}, &Rejection{
				Kind:    kindInvalidRequest,
				Message: field + " image.source 缺少 media_type",
			}
		}
		if source.Data == "" {
			return ContentPart{}, &Rejection{
				Kind:    kindInvalidRequest,
				Message: field + " image.source 缺少 data",
			}
		}
		// OpenAI 协议没有独立的 base64 入参，图片一律走 url 字段的 data URI 形态。
		return imageURLPart("data:" + source.MediaType + ";base64," + source.Data), nil
	case "url":
		if source.URL == "" {
			return ContentPart{}, &Rejection{
				Kind:    kindInvalidRequest,
				Message: field + " image.source 缺少 url",
			}
		}
		return imageURLPart(source.URL), nil
	case "":
		return ContentPart{}, &Rejection{
			Kind:    kindInvalidRequest,
			Message: field + " image.source 缺少 type",
		}
	default:
		return ContentPart{}, &Rejection{
			Kind:    kindInvalidRequest,
			Message: fmt.Sprintf("%s image.source.type 不支持: %s", field, source.Type),
		}
	}
}

func imageURLPart(url string) ContentPart {
	return ContentPart{Type: "image_url", ImageURL: &ImageURL{URL: url}}
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
			// 消息正文里的图片已经能转成 image_url part（见 convertMessage），
			// 但走到 flattenText 的是 system 与 tool_result：OpenAI 协议里
			// 这两处的 content 只能是字符串，放不下图片。文案必须点明是**位置**
			// 不支持，否则用户会以为换个模型就能用。
			return "", &Rejection{
				Kind:    kindInvalidRequest,
				Message: field + " 不支持图片块：该位置在 OpenAI 协议里只能是纯文本",
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
