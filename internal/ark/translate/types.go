// Package translate 在 Anthropic Messages 协议与 OpenAI Chat Completions 协议之间做双向转换。
// 本包是纯函数集合，不做任何 IO，便于表驱动测试。
package translate

import "encoding/json"

// ChatRequest 是发往 Ark /chat/completions 的请求体。
type ChatRequest struct {
	Model         string         `json:"model"`
	Messages      []ChatMessage  `json:"messages"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	Stop          []string       `json:"stop,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
	Tools         []Tool         `json:"tools,omitempty"`
	ToolChoice    any            `json:"tool_choice,omitempty"`
	Thinking      *Thinking      `json:"thinking,omitempty"`
	User          string         `json:"user,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// Thinking 是火山方舟的私有字段，type 取 enabled / disabled。
type Thinking struct {
	Type string `json:"type"`
}

type ChatMessage struct {
	Role       string         `json:"role"`
	Content    MessageContent `json:"content"`
	ToolCalls  []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// MessageContent 是 OpenAI 消息的 content 字段，它在协议里有两种合法形态：
// 字符串，或 content parts 数组。
//
// 不含图片时**必须**序列化成字符串：放开图片是在既有线上格式之外加一条新路径，
// 把所有请求都改成 parts 数组是另一回事（Ark 对两种形态的兼容程度、以及
// role=tool 这类只接受字符串的消息，都不在本次改动的验证范围内）。
// 回归锁见 TestBuildRequestKeepsPlainTextContentAsString。
type MessageContent struct {
	// Text 在 Parts 为空时生效，直接序列化成字符串。
	Text string
	// Parts 非空时取代 Text，序列化成 content parts 数组。
	Parts []ContentPart
}

// TextContent 构造纯文本形态的 content。
func TextContent(text string) MessageContent {
	return MessageContent{Text: text}
}

func (c MessageContent) MarshalJSON() ([]byte, error) {
	if len(c.Parts) == 0 {
		return json.Marshal(c.Text)
	}
	return json.Marshal(c.Parts)
}

// ContentPart 是 content parts 数组的元素，按 Type 取用其余字段。
type ContentPart struct {
	Type string `json:"type"`
	// Text 带 omitempty：image_url part 不能带 text 字段。
	// 代价是空文本会让 type=text 的 part 丢掉 text 字段而不合法，
	// 所以产出 text part 前必须保证文本非空（见 convertMessage）。
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL string `json:"url"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name string `json:"name"`
	// Arguments 是 JSON 文本而非对象，这是 OpenAI 协议的规定。
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function FunctionSpec `json:"function"`
}

type FunctionSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// Rejection 表示转换阶段发现的客户端错误，Kind 使用 anthropic.ErrorType* 的值。
type Rejection struct {
	Kind    string
	Message string
}

func (r *Rejection) Error() string {
	return r.Kind + ": " + r.Message
}
