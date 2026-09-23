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

// perToolOverhead 是每个工具声明的固定成本。
//
// 真机实测（权威数据，见 docs/design §2 那次 curl）：同一请求体不带工具时
// prompt_tokens = 89，加上一个 get_weather 工具定义后是 397 —— **一个工具的真实成本
// 是 308 token**。而那份工具定义的文本一共只有 77 个字符，按任何「N 字符/token」
// （N 为正整数）都最多只能算出 77 —— 也就是说工具成本**结构上不是按字符长度走的**，
// 主体是模板为每个工具展开函数声明、渲染 JSON schema、附加使用说明的固定开销。
// 所以这里用固定成本建模，而不是调密度系数：把密度调到能满足 308 需要 0.227 字符/token，
// 整数系数根本表达不出来（详见 Task 11 报告 fix round 1 的 I1）。
//
// 300 直接取自那个数据点（308 减去文本本身的量），方向上保证高估。单点数据，
// 需多点校准 —— 校准时以 TestEstimateTokensMatchesRealToolCost 那条真机锚定断言为准。
//
// 刻意只留这一个旋钮：初版同时有密度系数与固定开销两个旋钮，结果互相兜底 ——
// 单独改坏任何一个用例都照样全绿（变异验证抓到），等于两条都没护栏。
const perToolOverhead = 300

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

	// 工具定义要进上下文，且占比可观（实测一个工具 +308 token）：name / description /
	// input_schema 的文本全部计入，另加每个工具的固定声明开销，后者是主体。
	var tools strings.Builder
	for _, tool := range source.Tools {
		tools.WriteString(tool.Name)
		tools.WriteString(tool.Description)
		tools.Write(tool.InputSchema)
	}

	total := perRequestOverhead + estimateTextTokens(builder.String()) +
		perMessageOverhead*len(source.Messages) +
		perToolOverhead*len(source.Tools) + estimateTextTokens(tools.String())
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

	// 先拆成原始块再逐块解码：default 分支要拿得到**原始 JSON** 才能兑现
	// 「按原始 JSON 长度计入」。只留 []anthropicBlock 的话，未知块在结构体里只剩
	// 一个 Type 字段，image 的 base64 数据、document 的正文全部凭空消失 ——
	// 客户端拿这类块调 count_tokens 会得到严重偏低的值。
	var rawBlocks []json.RawMessage
	if err := json.Unmarshal(raw, &rawBlocks); err != nil {
		return trimmed
	}

	parts := make([]string, 0, len(rawBlocks))
	for _, rawBlock := range rawBlocks {
		var block anthropicBlock
		if err := json.Unmarshal(rawBlock, &block); err != nil {
			// 单个块不可解码：按其原始 JSON 计入。
			parts = append(parts, string(rawBlock))
			continue
		}
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
			// image / document / 未来新增的块类型：按原始 JSON 长度计入（base64 数据
			// 也算在内）。这类块在 BuildRequest 阶段会被拒绝，真实请求走不到计费，
			// 但 count_tokens 是独立端点，客户端完全可以只问不发 —— 宁可多算。
			parts = append(parts, string(rawBlock))
		}
	}
	return strings.Join(parts, " ")
}

// estimateTextTokens 按字符类别估算 token 数：
// CJK 约 1 token/字（实测 0.6~1，取上界），其余按 nonCJKCharsPerToken 字符/token。
func estimateTextTokens(text string) int {
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
	return cjk + (other+nonCJKCharsPerToken-1)/nonCJKCharsPerToken
}

// WantsThinking 报告客户端是否显式请求了 thinking。
//
// 只认 thinking.type == "enabled"，其余（含完全不传）一律为假：Ark 不传 thinking
// 时默认开启思考，而 Anthropic 协议在客户端未请求时不应返回 thinking 块。
// 请求体畸形、或 thinking 字段类型写错时返回 false —— **报错由 BuildRequest 负责**，
// 它走同一个 parseThinking 拿 Rejection，见下。
func WantsThinking(anthropicBody []byte) bool {
	enabled, _ := parseThinking(anthropicBody)
	return enabled
}

// parseThinking 是全仓库唯一一份 thinking 字段的解析实现，同时承担两件事：
//   - 语义：客户端是否显式请求了 thinking（WantsThinking 与 BuildRequest 共用）；
//   - 类型闸门：thinking 写成对象以外的形态时给出 Rejection（BuildRequest 用）。
//
// 这两件事必须在同一个函数里。历史教训：
//   - 最初请求侧内联判断、响应侧另写 WantsThinking，两份独立实现可以静默分叉，
//     后果是请求侧开思考、响应侧丢弃 thinking 块，用户为被扔掉的 reasoning 付费；
//   - 于是把请求侧改成复用 WantsThinking，并删掉 anthropicRequest 里的 thinking 字段。
//     但那个字段除了「第二个判断来源」之外还有个不显眼的职责：**类型校验**。
//     删掉之后 `"thinking":"enabled"`（本该是对象却写成字符串）从 400 变成 200 且
//     静默关掉思考 —— 客户端以为开了思考，实际没开，没有任何信号。
//
// 所以现在由这一个函数同时给出「是否开启」与「类型是否合法」，两个职责都不再有第二份实现。
func parseThinking(anthropicBody []byte) (bool, *Rejection) {
	// 先用 RawMessage 接住：它对任何合法 JSON 都不报错，于是「整个请求体不是对象」
	// 与「thinking 字段类型不对」两种情况可以分开处理。
	var probe struct {
		Thinking json.RawMessage `json:"thinking"`
	}
	if err := json.Unmarshal(anthropicBody, &probe); err != nil {
		// 整个请求体都不是 JSON 对象：由 BuildRequest 开头那条统一检查报错，
		// 这里不抢它的措辞。
		return false, nil
	}

	trimmed := strings.TrimSpace(string(probe.Thinking))
	if trimmed == "" || trimmed == "null" {
		return false, nil
	}

	var thinking struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(probe.Thinking, &thinking); err != nil {
		return false, &Rejection{Kind: kindInvalidRequest, Message: "thinking 必须是对象"}
	}
	return thinking.Type == "enabled", nil
}
