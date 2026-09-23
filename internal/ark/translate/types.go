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
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
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
