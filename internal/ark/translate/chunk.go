package translate

// StreamChunk 是 Ark 流式响应的单个 chunk（object=chat.completion.chunk）。
type StreamChunk struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
	// Usage 仅在末尾若干 chunk 出现（需请求时带 stream_options.include_usage）。
	Usage *Usage `json:"usage"`
}

type StreamChoice struct {
	Index        int         `json:"index"`
	FinishReason string      `json:"finish_reason"`
	Delta        StreamDelta `json:"delta"`
}

// StreamDelta 是一个增量片段。
// 注意 Content 恒为存在的字符串字段，纯思考阶段它是空串而非缺失 ——
// 判断是否要开 text 块必须看内容是否非空，不能看字段是否存在。
type StreamDelta struct {
	Role             string           `json:"role"`
	Content          string           `json:"content"`
	ReasoningContent string           `json:"reasoning_content"`
	ToolCalls        []StreamToolCall `json:"tool_calls"`
}

// StreamToolCall 的 Index 是工具在本次回复中的序号（非 Anthropic 的 content block 序号）。
// 首发分片带 ID 与 Function.Name，后续分片只有 Function.Arguments。
type StreamToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
		// Arguments 是 JSON 文本的一个片段，可能断在转义符或多字节字符中间。
		Arguments string `json:"arguments"`
	} `json:"function"`
}
