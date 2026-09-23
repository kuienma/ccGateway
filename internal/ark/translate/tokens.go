package translate

import (
	"encoding/json"
	"math"
	"strings"
	"unicode"
)

// MaxOutputTokens 是该模型的输出上限。
// DeepSeek V4 Flash 是推理模型，思考部分也计入输出，故上限取 32K。
const MaxOutputTokens = 32768

// nonCJKCharsPerToken 是非 CJK 字符的估算密度。
//
// 英文散文约 4 字符/token，但本网关的输入里占大头的是 JSON schema、代码片段、
// 命令输出，它们分词更密（约 3 字符/token）。取 3 而不是 4 是刻意的：估算的
// 方向性要求是偏向高估，按散文密度估 tool schema 会系统性低估。
const nonCJKCharsPerToken = 3

// perMessageOverhead 是每条消息的固定开销（角色标记、分隔符等）。
const perMessageOverhead = 4

// perRequestOverhead 是对话模板本身的固定开销。
//
// 实测（docs/design/2026-09-22-deepseek-v4-flash-provider.md §2）一条极短请求的
// prompt_tokens 是 89，而其内容本身只有几个 token —— 差额来自 Ark/DeepSeek 的对话
// 模板与思考脚手架，与请求长度无关。不打这份底，短请求会被低估近 80 token。
// 80 是按那次实测倒推的，只有一个数据点，精确值待真机复核（见 Task 11 报告）。
const perRequestOverhead = 80

// toolCharsPerToken 是工具定义文本的估算密度，刻意比 nonCJKCharsPerToken 更密。
//
// 依据是实测：一个工具让 prompt_tokens 涨了 308，而那份工具定义的文本量按
// 3 字符/token 只值约 100 —— 模板把函数声明展开后还要加一层包装格式。只有一个数据点，
// 所以这里不去凑那个倍率，只保证工具定义算得比普通文本贵（方向是高估）。
// 精确倍率待真机复核（见 Task 11 报告的待验证清单）。
//
// 刻意只留这一个旋钮：初版还加了一份 perToolOverhead，结果两个旋钮互相兜底 ——
// 单独改坏任何一个，用例都照样全绿（变异验证抓到），等于两条都没护栏。
const toolCharsPerToken = 2

// maxContentDepth 限制 content 嵌套的递归深度。
// 超过之后按原始 JSON 长度计入 —— 宁可多算，不可漏算。
const maxContentDepth = 4

// EstimateTokens 本地估算 Anthropic 请求体的输入 token 数。
//
// Ark 没有 count_tokens 端点，这里只能估算。刻意偏向高估：
// Claude Code 用这个值做上下文预算，低估会让它以为还有余量、结果真实请求
// 被上游拒绝；高估只是让它保守一点。
func EstimateTokens(anthropicBody []byte) (int32, error) {
	var source anthropicRequest
	if err := json.Unmarshal(anthropicBody, &source); err != nil {
		return 0, err
	}

	var builder strings.Builder
	builder.WriteString(collectEstimateText(source.System, 0))
	for _, message := range source.Messages {
		builder.WriteString(collectEstimateText(message.Content, 0))
	}

	// 工具定义要进上下文，且占比可观：实测同一输入带一个工具后 prompt_tokens 从 89
	// 涨到 397（+308），所以 name / description / input_schema 全部计入，且按更密的
	// toolCharsPerToken 计 —— 那份工具定义的文本量按 3 字符/token 只值约 100 token，
	// 差额是模板把函数声明展开后的包装格式。
	var tools strings.Builder
	for _, tool := range source.Tools {
		tools.WriteString(tool.Name)
		tools.WriteString(tool.Description)
		tools.Write(tool.InputSchema)
	}

	total := perRequestOverhead + estimateTextTokens(builder.String()) +
		perMessageOverhead*len(source.Messages) + estimateToolTokens(tools.String())
	// 理论上要 GB 级请求体才会溢出，但截断成负数比返回上限危险得多。
	if total > math.MaxInt32 {
		return math.MaxInt32, nil
	}
	return int32(total), nil
}

// collectEstimateText 尽力把 Anthropic 的 content 展平成用于估算的文本。
//
// 与 flattenText 的分工是关键：flattenText 服务于转换路径，遇到 tool_use /
// tool_result / 未知块一律返回 Rejection。估算路径不能那样做 —— 一旦因为
// Rejection 就丢掉整条消息，多轮工具循环里最大的那块输入（工具回包：文件内容、
// 命令输出）会整个不计数，估算从偏高变成严重偏低，正是要避免的方向。
// 所以这里对任何不认识的形态都回落到「按原始 JSON 长度计入」。
func collectEstimateText(raw json.RawMessage, depth int) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}

	// 纯字符串 content 交给 flattenText，与转换路径共用同一套解码。
	if trimmed[0] == '"' {
		if text, rejection := flattenText(raw, "estimate"); rejection == nil {
			return text
		}
		return trimmed
	}

	if depth >= maxContentDepth {
		return trimmed
	}

	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return trimmed
	}

	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			parts = append(parts, block.Text)
		case "thinking", "redacted_thinking":
			// 历史推理内容虽然不回传给 Ark，但客户端把它算在上下文里，
			// 估算宁可跟着算上。
			parts = append(parts, block.Thinking)
		case "tool_use":
			parts = append(parts, block.Name, string(block.Input))
		case "tool_result":
			parts = append(parts, collectEstimateText(block.Content, depth+1))
		default:
			// image 等块在 BuildRequest 阶段会被拒绝，真实请求走不到计费；
			// 估算阶段仍按块类型名粗计，保证不返回 0。
			parts = append(parts, block.Type)
		}
	}
	return strings.Join(parts, " ")
}

// estimateTextTokens 按字符类别估算 token 数：
// CJK 约 1 token/字（实测 0.6~1，取上界），其余按 nonCJKCharsPerToken 字符/token。
func estimateTextTokens(text string) int {
	return estimateWithDensity(text, nonCJKCharsPerToken)
}

// estimateToolTokens 估算工具定义文本，非 CJK 部分按更密的 toolCharsPerToken 计。
func estimateToolTokens(text string) int {
	return estimateWithDensity(text, toolCharsPerToken)
}

func estimateWithDensity(text string, charsPerToken int) int {
	var cjk, other int
	for _, r := range text {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			cjk++
			continue
		}
		other++
	}
	// other 向上取整，避免短输入被估成 0。
	return cjk + (other+charsPerToken-1)/charsPerToken
}

// WantsThinking 报告客户端是否显式请求了 thinking。
//
// 只认 thinking.type == "enabled"，其余（含完全不传）一律为假：Ark 不传 thinking
// 时默认开启思考，而 Anthropic 协议在客户端未请求时不应返回 thinking 块。
// 请求体畸形时返回 false —— 真正的报错由 BuildRequest 负责。
func WantsThinking(anthropicBody []byte) bool {
	var source struct {
		Thinking *struct {
			Type string `json:"type"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(anthropicBody, &source); err != nil {
		return false
	}
	return source.Thinking != nil && source.Thinking.Type == "enabled"
}
