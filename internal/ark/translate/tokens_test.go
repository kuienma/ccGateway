package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEstimateTokensCountsAllTextSources(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"system": "你是助手",
		"max_tokens": 64,
		"messages": [
			{"role": "user", "content": "北京今天天气怎么样"},
			{"role": "assistant", "content": [{"type": "text", "text": "我查一下"}]}
		]
	}`)

	count, err := EstimateTokens(body)
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	if count <= 0 {
		t.Fatalf("估算值应为正数，实际 %d", count)
	}

	// 中文约 1 token/字：上述约 20 个汉字，估算值不该低于字数的一半。
	if count < 10 {
		t.Fatalf("中文估算明显偏低: %d", count)
	}
}

// TestEstimateTokensCountsSystemPrompt 单独钉住 system 这一路输入。
//
// 上面那条把 system、两条消息的文本混在一个断言里，任何一路被漏掉都还能满足
// count >= 10，所以它抓不住「system 没计入」。system prompt 在 Claude Code 里是
// 最大的一块固定输入（工具说明、环境信息全在里面），漏掉它会系统性低估。
func TestEstimateTokensCountsSystemPrompt(t *testing.T) {
	t.Parallel()

	without := []byte(`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	with := []byte(`{"max_tokens":8,"system":"你是一个严谨的助手，回答要简短",
		"messages":[{"role":"user","content":"hi"}]}`)

	bare, err := EstimateTokens(without)
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	withSystem, err := EstimateTokens(with)
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	if withSystem <= bare {
		t.Fatalf("带 system 的估算值应更大: %d vs %d", withSystem, bare)
	}
}

// TestEstimateTokensIncludesFixedRequestOverhead 钉住对话模板的固定开销。
//
// 实测（docs/design/2026-09-22-deepseek-v4-flash-provider.md §2 那次 curl）一条极短
// 请求的 prompt_tokens 是 89，而请求内容本身只有几个 token —— 差额是 Ark/DeepSeek 的
// 对话模板与思考脚手架的固定开销。只按字符估算会把这条请求估成 5 token，绝对值上低估
// 近 80 —— 方向正好是要避免的那一侧，所以必须有一份固定开销打底。
//
// 这里只断言「存在数十 token 的固定开销」，不把 89 这个具体数字焊死：它来自单次实测，
// 精确值要真机复核。
func TestEstimateTokensIncludesFixedRequestOverhead(t *testing.T) {
	t.Parallel()

	count, err := EstimateTokens([]byte(`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	if count < 40 {
		t.Fatalf("极短请求估算 %d：实测同等请求真实 prompt_tokens 是 89，"+
			"只按字符估算会低估近 80，必须计入对话模板的固定开销", count)
	}
}

// realMinimalPrompt 是真机实测的极短请求成本（权威数据，见 docs/design §2 那次 curl）：
// 不带工具的极短请求 prompt_tokens = 89。
const realMinimalPrompt = 89

// TestEstimateTokensDoesNotInflateFixedRequestOverhead 是上面那条的**上界配对**。
//
// 只有下界时 `perRequestOverhead` 从 80 改成 1000 → 九个包全绿（本任务实测）。
// 上一轮曾有一条绝对值断言碰巧能抓住这个变异，但它是以**错误的理由**失败的
// （报「工具成本不随数量增长」，而工具模型一行都没动），修 N2 时把那条断言正确地
// 解耦成增量比较，却没有补上真正的上界护栏 —— 于是这个承重常量变成了单向锚定。
//
// 这与同一轮给 `perToolOverhead` 补上界的理由是同一条：高估是安全方向，但**不是没有代价**。
// 固定开销打得过高会让**每一个**请求（连空请求都算上）被整体抬高，Claude Code 会误判
// 上下文快满、提前截断或触发压缩，而在只有下界的情况下没有任何用例会响。
//
// 上界取实测值的两倍，与 TestEstimateTokensMatchesRealToolCost 用同一把尺子。
func TestEstimateTokensDoesNotInflateFixedRequestOverhead(t *testing.T) {
	t.Parallel()

	count, err := EstimateTokens([]byte(`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	if ceiling := realMinimalPrompt * 2; int(count) > ceiling {
		t.Fatalf("极短请求估算 %d 超过真机实测 %d 的两倍（上界 %d）："+
			"对话模板的固定开销打得过高会把每一个请求整体抬高，"+
			"Claude Code 会误判上下文快满、提前截断或触发压缩", count, realMinimalPrompt, ceiling)
	}
}

func TestEstimateTokensIsConservative(t *testing.T) {
	t.Parallel()

	short := []byte(`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	long := []byte(`{"max_tokens":8,"messages":[{"role":"user","content":"` +
		"这是一段明显更长的中文输入用于验证估算随长度单调增加" + `"}]}`)

	shortCount, err := EstimateTokens(short)
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	longCount, err := EstimateTokens(long)
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	if longCount <= shortCount {
		t.Fatalf("更长输入的估算值应更大: %d vs %d", longCount, shortCount)
	}
}

// marginalTokens 返回「同一请求把一段文本塞进用户消息后多出来的估算量」。
//
// 下面两条系数护栏都必须用增量而不是绝对值：绝对值里含 perRequestOverhead（80）与
// perMessageOverhead，把系数改坏后那份固定开销仍然能撑过任何按文本长度算的下界 ——
// 实测过，用绝对值时「CJK 不再按 1 token/字」这个变异照样全绿，断言是空的。
func marginalTokens(t *testing.T, text string) int {
	t.Helper()

	baseline, err := EstimateTokens([]byte(`{"max_tokens":8,"messages":[{"role":"user","content":""}]}`))
	if err != nil {
		t.Fatalf("估算基线失败: %v", err)
	}
	encoded, err := json.Marshal(text)
	if err != nil {
		t.Fatalf("编码用例文本失败: %v", err)
	}
	loaded, err := EstimateTokens([]byte(`{"max_tokens":8,"messages":[{"role":"user","content":` +
		string(encoded) + `}]}`))
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	return int(loaded - baseline)
}

// TestEstimateTokensDoesNotUnderestimateChinese 钉住方向性要求本身：估算必须偏向高估。
//
// 只做单调性断言是不够的 —— 把系数改成「每 100 个字符 1 token」照样单调递增。
// 实测 DeepSeek 分词器对中文约 0.6~1 token/字，这里按 1 token/字 取上界，
// 所以中文文本的**增量**必须 >= 汉字数。低估的后果是 Claude Code 以为上下文还有
// 余量、真实请求被上游拒绝；高估只是让它保守一点。
func TestEstimateTokensDoesNotUnderestimateChinese(t *testing.T) {
	t.Parallel()

	const text = "这是一段用来验证中文估算不会偏低的输入文字"
	runes := len([]rune(text))

	if got := marginalTokens(t, text); got < runes {
		t.Fatalf("中文文本的估算增量 %d 低于汉字数 %d：CJK 必须按 1 token/字 取上界，"+
			"否则真实请求会超限被上游拒绝", got, runes)
	}
}

// TestEstimateTokensDoesNotUnderestimateJSONText 钉住非 CJK 那一路的系数上界。
//
// JSON 与代码片段比英文散文密得多（散文约 4 字符/token，JSON 约 3 字符/token），
// 按 4 字符/token 估就是低估。这里断言 JSON 文本的估算**增量**不低于字符数/3，
// 把系数锁在偏高估的一侧。
//
// 两次返工都记在这里，因为它们正是「断言看着对、其实锁不住」的样本：
// 第一版下界取 len/3.5，把系数改回 4 后照样全绿（name/description 那点额外量补上了差额）；
// 第二版改成 len/3 但仍用绝对值、且文本放在 tools 里，于是先被 perRequestOverhead
// 撑过下界、又被工具那条更密的系数接管，测的根本不是非 CJK 文本的系数。
// 现在文本放在普通消息里、只比增量，下界才真正由 nonCJKCharsPerToken 决定。
func TestEstimateTokensDoesNotUnderestimateJSONText(t *testing.T) {
	t.Parallel()

	const payload = `{"type":"object","properties":{"city":{"type":"string","description":"city name"},` +
		`"unit":{"type":"string","enum":["celsius","fahrenheit"],"description":"temperature unit"},` +
		`"days":{"type":"integer","minimum":1,"maximum":14,"description":"forecast horizon in days"},` +
		`"include_hourly":{"type":"boolean","description":"whether to include hourly breakdown"}},` +
		`"required":["city"],"additionalProperties":false}`

	floor := len(payload) / 3
	if got := marginalTokens(t, payload); got < floor {
		t.Fatalf("JSON 文本的估算增量 %d 低于下界 %d：非 CJK 系数必须按 JSON 的密度取"+
			"（约 3 字符/token），按英文散文的 4 字符/token 会系统性低估", got, floor)
	}
}

func TestEstimateTokensIncludesToolDefinitions(t *testing.T) {
	t.Parallel()

	const schema = `{"type":"object","properties":{"city":{"type":"string",` +
		`"description":"the city to query"}},"required":["city"]}`
	withoutTools := []byte(`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	withTools := []byte(`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"get_weather","description":"查询指定城市的天气",
		"input_schema":` + schema + `}]}`)

	bare, err := EstimateTokens(withoutTools)
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	tooled, err := EstimateTokens(withTools)
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	// 工具定义要占上下文，且占比可观：实测同一输入带 tools 后 prompt_tokens 从 89 涨到 397。
	if tooled <= bare {
		t.Fatalf("带工具定义的估算值应更大: %d vs %d", tooled, bare)
	}
	// 只断言「更大」会被 name 一个字段蒙混过去：漏掉 input_schema 时 tooled 仍然大于 bare。
	// 这里按 input_schema 自身的文本量给下界 —— 第一版写的是 tooled >= bare*2，
	// 变异验证时发现那个倍率并不由 schema 是否计入决定（固定开销一进 bare，倍率就跟着
	// 变），承诺强于实际验证，遂改成直接量 schema 的贡献。
	if int(tooled-bare) < len(schema)/3 {
		t.Fatalf("带工具与不带工具的估算差只有 %d token，低于 input_schema 的文本量下界 %d —— "+
			"工具定义必须显著计入估算。注意本条守的是「增量够大」这个量级属性，"+
			"而这个增量里 perToolOverhead 占绝对主体；"+
			"「name / description / input_schema 逐项都计入」由 TestEstimateTokensCountsToolText 守"+
			"（它把固定开销相减抵消，与该常量取值无关）", tooled-bare, len(schema)/3)
	}
}

// realToolCost 是真机实测的单个工具定义成本（权威数据，见 docs/design §2）：
// 同一请求体不带工具时 prompt_tokens = 89，加上下面那个 get_weather 工具后是 397。
const realToolCost = 397 - 89

// realToolDefinition 是那次实测所用的工具定义（brief 里的 get_weather）。
const realToolDefinition = `{"name":"get_weather","description":"查询指定城市的天气",` +
	`"input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}`

// marginalToolTokens 返回「同一请求带 count 个 realToolDefinition 后多出来的估算量」。
//
// 与 marginalTokens 同一条理由，而且这条理由是被实测打过脸的：本轮初版的
// TestEstimateTokensScalesToolCostWithCount 用**绝对值**比较，reviewer 把
// perRequestOverhead 从 80 调到 1000 之后它确实失败了 —— 但是以**错误的理由**失败，
// 报「工具成本不随数量增长」，而工具模型一行都没动。
// 「失败」不等于「因为对的理由失败」，所以工具那一路的断言同样必须走增量。
func marginalToolTokens(t *testing.T, count int) int {
	t.Helper()

	const messages = `"messages":[{"role":"user","content":"hi"}]`
	baseline, err := EstimateTokens([]byte(`{"max_tokens":8,` + messages + `}`))
	if err != nil {
		t.Fatalf("估算基线失败: %v", err)
	}

	tools := make([]string, count)
	for i := range tools {
		tools[i] = realToolDefinition
	}
	loaded, err := EstimateTokens([]byte(`{"max_tokens":8,` + messages +
		`,"tools":[` + strings.Join(tools, ",") + `]}`))
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	return int(loaded - baseline)
}

// TestEstimateTokensMatchesRealToolCost 把真机观测直接变成断言：一个工具定义的估算
// 增量必须**落在** [308, 616] 这个区间内。
//
// 下界 = 实测的 308。这是整个工具那一路最重要的护栏，也是本任务最贵的一条教训：
// 初版只断言「工具定义比普通文本贵」，那是个**方向对、量级错**的护栏 —— 实测增量只有 43，
// 是真实值的 1/7.2；换成 Claude Code 常态的十几个工具，估算只有真实值的约 39%，
// 正落在「必须偏向高估」要避免的那一侧，而当时全部用例都是绿的。
//
// 上界 = 实测的 2 倍。高估是安全方向，但**不是没有代价**：把 perToolOverhead 调到
// 10 倍（3000）时九个包全绿，而那会让 Claude Code 误判上下文快满、提前截断或触发压缩。
// 只有下界就是「半边护栏」—— 本项目反复出现的形状，成本只有一条断言。
func TestEstimateTokensMatchesRealToolCost(t *testing.T) {
	t.Parallel()

	got := marginalToolTokens(t, 1)
	if got < realToolCost {
		t.Fatalf("单个工具定义的估算增量 %d 低于真机实测的 %d token："+
			"工具成本结构上不是按字符长度走的（那份定义只有 77 个字符），"+
			"主体是模板为每个工具展开声明的固定开销，必须按固定成本建模并保证高估",
			got, realToolCost)
	}
	if ceiling := realToolCost * 2; got > ceiling {
		t.Fatalf("单个工具定义的估算增量 %d 超过真机实测 %d 的两倍（上界 %d）："+
			"高估虽是安全方向，但过度高估会让 Claude Code 误判上下文快满、"+
			"提前截断或触发压缩", got, realToolCost, ceiling)
	}
}

// TestEstimateTokensScalesToolCostWithCount 配对上面那条：单工具达标也可能是把成本写死成
// 一份常量（不随工具数量增长），那样十几个工具的常态形态照样严重低估，而上面那条全绿。
func TestEstimateTokensScalesToolCostWithCount(t *testing.T) {
	t.Parallel()

	one := marginalToolTokens(t, 1)
	three := marginalToolTokens(t, 3)

	// 三个同样的工具应当花掉约三倍；下界给到 2.5 倍，留出文本项取整的余量。
	if three < one*5/2 {
		t.Fatalf("三个工具的估算增量 %d 不到单个工具 %d 的 2.5 倍：工具成本必须随数量增长，"+
			"否则带十几个工具的常态请求会严重低估", three, one)
	}
}

// estimateWithSingleTool 返回「带**恰好一个**工具（定义为 toolJSON）」时的估算值。
//
// 工具数量固定为 1 是关键：`perToolOverhead * len(tools)` 这一项在两次调用里完全相同，
// 相减后精确抵消，剩下的差值只可能来自工具定义的文本。
func estimateWithSingleTool(t *testing.T, toolJSON string) int {
	t.Helper()

	count, err := EstimateTokens([]byte(`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[` + toolJSON + `]}`))
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	return int(count)
}

// TestEstimateTokensCountsToolText 独立锁住「工具定义的文本必须计入估算」这件事本身，
// 且**不依赖 perToolOverhead 的取值**。
//
// 为什么必须单独有这一条：TestEstimateTokensMatchesRealToolCost 与
// TestEstimateTokensIncludesToolDefinitions 量的都是「带工具 vs 不带工具」的增量，而那个
// 增量里固定开销占绝对主体（实测 300，而这份定义的文本只值 32）。于是把 tokens.go 里收集
// 工具文本的那个循环整段删掉、**同时**把 perToolOverhead 调到 400（仍落在那两条用例允许的
// 窗口 [276, 584] 内）→ 九个包全绿（本任务实测）。单删循环时也只靠 `300 < 308` 剩下的
// 8 个 token 余量才被抓住 —— 而 `TestEstimateTokensIncludesToolDefinitions` 的失败消息一度
// 声称守的就是「name/description/input_schema 必须全部计入」，它的下界 33 早被 300 的固定
// 开销吞掉，承诺强于实际。那条消息已经改成只声称「增量够大」并把逐项属性指回本用例
// （合并前最后一轮 fix，A4/T3d），但分工本身没变：量级属性在那边，逐字段属性在这里。
//
// 更要紧的是这是个**定时炸弹**：perToolOverhead 被明确标注「单点数据，待真机多点校准」，
// 下次有人把它校准到 >308，「工具文本计入」这个属性就会在校准的同一刻静默失去护栏。
//
// 做法：两个请求都带恰好一个工具，只有工具定义的文本量不同，固定开销相减抵消，
// 于是断言对窗口内（乃至窗口外）任何 perToolOverhead 取值都成立。
// 三个字段各锁一条子用例：只漏掉其中一行（例如不再收集 input_schema）同样会被抓住。
func TestEstimateTokensCountsToolText(t *testing.T) {
	t.Parallel()

	// 体量刻意给大：文本贡献要显著高于取整噪声，失败时的数字才有说服力。
	payload := strings.Repeat("tool-definition-text-", 30)
	// input_schema 计入的是它的**原始 JSON 字节**，所以下界按整段 schema 的长度算。
	schema := `{"type":"object","description":"` + payload + `"}`

	// 基线的工具定义是空对象：三个字段全空 → 工具文本为空串，估算里这一项恰好是 0，
	// 于是增量不受 estimateTextTokens 向上取整的影响，下界可以直接写 len/3。
	baseline := estimateWithSingleTool(t, `{}`)

	cases := []struct {
		field string
		tool  string
		text  string
	}{
		{"name", `{"name":"` + payload + `"}`, payload},
		{"description", `{"description":"` + payload + `"}`, payload},
		{"input_schema", `{"input_schema":` + schema + `}`, schema},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()

			floor := len(tc.text) / 3
			if got := estimateWithSingleTool(t, tc.tool) - baseline; got < floor {
				t.Fatalf("工具定义的 %s 字段贡献了 %d token，低于其文本量下界 %d —— "+
					"工具定义的文本必须计入估算。本用例与 perToolOverhead 的取值无关"+
					"（两侧工具数量相同、固定开销已相减抵消），所以校准那个常量不会削弱它",
					tc.field, got, floor)
			}
		})
	}
}

// TestEstimateTokensCountsToolUseAndToolResultBlocks 钉住多轮工具循环的输入。
//
// brief 给的实现把每条消息交给 flattenText，而 flattenText 对 tool_use / tool_result
// 一律返回 Rejection，于是整条消息贡献 0 token。Claude Code 的真实会话里工具回包
// （文件内容、命令输出）往往是上下文里最大的一块，漏掉它会严重低估 —— 与「必须偏向
// 高估」的方向性要求相反，所以估算路径不能复用会拒绝的那套块校验。
func TestEstimateTokensCountsToolUseAndToolResultBlocks(t *testing.T) {
	t.Parallel()

	bare := []byte(`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	withTools := []byte(`{"max_tokens":8,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"get_weather",
			"input":{"city":"北京","unit":"celsius"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1",
			"content":"北京今天晴，最高气温二十六度，最低气温十五度，空气质量良"}]}
	]}`)

	bareCount, err := EstimateTokens(bare)
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	toolCount, err := EstimateTokens(withTools)
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	// 只比大小会被每条消息的固定开销蒙混：多两条消息本身就带 8 token 开销，
	// 即使正文全被丢掉也能通过。所以要把开销扣掉再比。
	//
	// 这里的 8 刻意写字面量而不是 2*perMessageOverhead：用实现自己的常量算期望值
	// 是同义反复 —— 那个常量取任何值断言都成立，等于它完全没有护栏。
	overheadOnly := bareCount + 8
	if toolCount <= overheadOnly {
		t.Fatalf("tool_use / tool_result 的正文未计入：估算 %d 未超过纯消息开销 %d",
			toolCount, overheadOnly)
	}
	// tool_result 里有 26 个汉字，单这一项就该贡献 20+ token。
	if toolCount < overheadOnly+20 {
		t.Fatalf("tool_result 正文的贡献明显偏低: %d vs 下界 %d", toolCount, overheadOnly+20)
	}
}

// TestEstimateTokensCountsUnknownBlocksByRawJSON 钉住 collectEstimateText 的 default 分支
// 兑现它的文档承诺：「按原始 JSON 长度计入」。
//
// 之前那版注释这么写，代码却只 append 了 block.Type（几个字符），把分支改成零贡献时全绿。
// 后果：客户端拿 image / document / 未来新增的块类型调 count_tokens 会得到严重偏低的值 ——
// image 的 base64 数据往往是整个请求里最大的一块。count_tokens 是独立端点，客户端完全
// 可以只问不发，所以「这类块反正会被 BuildRequest 拒绝」不是理由。
func TestEstimateTokensCountsUnknownBlocksByRawJSON(t *testing.T) {
	t.Parallel()

	// 用一个体量可观的未知块：只按 type 粗计的实现会把它当成十几个字符。
	payload := strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVld", 40) // 约 1.2KB 的 base64 形态
	body := []byte(`{"max_tokens":8,"messages":[{"role":"user","content":[` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + payload + `"}}]}]}`)
	bare := []byte(`{"max_tokens":8,"messages":[{"role":"user","content":[]}]}`)

	bareCount, err := EstimateTokens(bare)
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	withImage, err := EstimateTokens(body)
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}

	floor := len(payload) / 3
	if got := int(withImage - bareCount); got < floor {
		t.Fatalf("未知块的估算增量 %d 低于其原始 JSON 的文本量下界 %d —— "+
			"default 分支必须按原始 JSON 计入，只算块类型名会让 image 的 base64 数据凭空消失",
			got, floor)
	}
}

// TestEstimateTokensCountsUndecodableBlockByRawJSON 钉住「单个块解码失败」那条回落路径。
//
// 它与上面那条（default 分支）走的是不同代码路径：这里块本身不是对象、连
// anthropicBlock 都解不进去。把该分支改成零贡献时九个包全绿，而被零掉的可能正是
// 多轮工具循环里最大的那个 tool_result —— 恰是 Claude Code 的主要形态。
func TestEstimateTokensCountsUndecodableBlockByRawJSON(t *testing.T) {
	t.Parallel()

	// 块是字符串而不是对象：json.Unmarshal 进 anthropicBlock 必然失败。
	payload := strings.Repeat("undecodable-block-payload-", 40)
	body := []byte(`{"max_tokens":8,"messages":[{"role":"user","content":[` +
		string(mustJSONString(payload)) + `]}]}`)
	bare := []byte(`{"max_tokens":8,"messages":[{"role":"user","content":[]}]}`)

	bareCount, err := EstimateTokens(bare)
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	withBlock, err := EstimateTokens(body)
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}

	floor := len(payload) / 3
	if got := int(withBlock - bareCount); got < floor {
		t.Fatalf("解码失败的块其估算增量 %d 低于其原始 JSON 的文本量下界 %d —— "+
			"该回落路径必须按原始 JSON 计入，零贡献会让最大的那块输入凭空消失", got, floor)
	}
}

// mustJSONString 把一段文本编成 JSON 字符串字面量。
func mustJSONString(text string) []byte {
	encoded, err := json.Marshal(text)
	if err != nil {
		panic(err)
	}
	return encoded
}

func TestEstimateTokensRejectsMalformedBody(t *testing.T) {
	t.Parallel()

	if _, err := EstimateTokens([]byte(`[]`)); err == nil {
		t.Fatal("畸形请求体应报错")
	}
}

func TestWantsThinking(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want bool
	}{
		{"未传", `{"max_tokens":8,"messages":[]}`, false},
		{"enabled", `{"max_tokens":8,"messages":[],"thinking":{"type":"enabled","budget_tokens":1024}}`, true},
		{"disabled", `{"max_tokens":8,"messages":[],"thinking":{"type":"disabled"}}`, false},
		{"畸形", `{`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := WantsThinking([]byte(tc.body)); got != tc.want {
				t.Fatalf("WantsThinking = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// TestBuildRequestThinkingMatchesWantsThinking 把请求侧与响应侧的 thinking 判断钉在一起。
//
// 两侧原本是两份独立实现（request.go 内联比较 Type == "enabled"，tokens.go 里的
// WantsThinking 又写一遍），没有编译期耦合也没有配对断言 —— 让它们分叉后九个包全绿。
// 后果是静默花钱：请求侧下发 thinking: enabled、Ark 照旧产出 reasoning_content 并计费，
// 响应侧却把 thinking 块丢掉，用户为被扔掉的 reasoning 付费，且现象上完全看不出来。
// 这是本项目第五次跨路径不一致，所以这条断言必须存在。
//
// **这条断言的边界必须写明**：它锁的是「两侧配对」，**锁不住谓词本身**。
// 把 parseThinking 改成恒真或恒假时两侧同步漂移，`gotEnabled == wantEnabled` 恒成立，
// 本用例照样是绿的（实测两个方向都绿）。谓词本身由 TestWantsThinking 与
// TestBuildRequestThinkingPolicy 用**字面量期望值**锁住 —— 这个分工是刻意的，
// 但不写出来后人会误以为本用例能兜住谓词。
func TestBuildRequestThinkingMatchesWantsThinking(t *testing.T) {
	t.Parallel()

	bodies := []string{
		`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`,
		`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":1024}}`,
		`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`,
		`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}],"thinking":null}`,
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			t.Parallel()

			req, rejection := BuildRequest("m", []byte(body), false)
			if rejection != nil {
				t.Fatalf("BuildRequest 失败: %v", rejection)
			}
			if req.Thinking == nil {
				t.Fatal("thinking 必须始终显式下发：Ark 不传时默认开启思考")
			}

			// 请求侧下发 enabled ⟺ 响应侧会透出 thinking 块。两者必须严格同真同假。
			wantEnabled := WantsThinking([]byte(body))
			gotEnabled := req.Thinking.Type == "enabled"
			if gotEnabled != wantEnabled {
				t.Fatalf("请求侧下发 thinking.type=%q（enabled=%v），而响应侧 WantsThinking=%v —— "+
					"两侧判断分叉会让用户为被丢弃的 reasoning 付费",
					req.Thinking.Type, gotEnabled, wantEnabled)
			}
			if !gotEnabled && req.Thinking.Type != "disabled" {
				t.Fatalf("未请求 thinking 时必须显式下发 disabled，实际 %q", req.Thinking.Type)
			}
		})
	}
}

// TestBuildRequestValidatesThinkingType 钉住 thinking 字段的**类型闸门**。
//
// 这条是删字段留下的坑：`anthropicRequest.Thinking` 原先除了「第二个 thinking 判断来源」
// 之外，还顺带承担类型校验 —— 解析整个请求体时 JSON 的类型不匹配会直接让 BuildRequest
// 返回 400。把字段删掉（为了消掉两份独立谓词）之后，`"thinking":"enabled"` 这种写法
// 从 400 变成 200 且**静默关掉思考**：客户端以为开了思考，实际没开，没有任何信号。
//
// 教训：删掉一个看起来冗余的东西之前，要确认它的**全部**职责，而不只是最显眼的那个。
func TestBuildRequestValidatesThinkingType(t *testing.T) {
	t.Parallel()

	const messages = `"messages":[{"role":"user","content":"hi"}]`

	// 类型写错 → 必须 400，不能静默当成「没请求思考」。
	badCases := []struct {
		name string
		body string
	}{
		{"字符串", `{"max_tokens":8,` + messages + `,"thinking":"enabled"}`},
		{"数字", `{"max_tokens":8,` + messages + `,"thinking":123}`},
		{"数组", `{"max_tokens":8,` + messages + `,"thinking":["enabled"]}`},
		{"布尔", `{"max_tokens":8,` + messages + `,"thinking":true}`},
	}
	for _, tc := range badCases {
		t.Run("类型错/"+tc.name, func(t *testing.T) {
			t.Parallel()
			_, rejection := BuildRequest("m", []byte(tc.body), false)
			if rejection == nil {
				t.Fatal("thinking 类型写错必须报错：静默关掉思考会让客户端以为开了思考而毫无信号")
			}
			if rejection.Kind != "invalid_request_error" {
				t.Fatalf("Rejection.Kind = %q，期望 invalid_request_error", rejection.Kind)
			}
		})
	}

	// 类型正确 → 必须照常开启思考（反向配对：光会报错不算对，不能把合法请求也拒了）。
	t.Run("类型对/正常开启", func(t *testing.T) {
		t.Parallel()
		body := []byte(`{"max_tokens":8,` + messages + `,"thinking":{"type":"enabled","budget_tokens":1024}}`)
		req, rejection := BuildRequest("m", body, false)
		if rejection != nil {
			t.Fatalf("合法的 thinking 对象不应被拒: %v", rejection)
		}
		if req.Thinking == nil || req.Thinking.Type != "enabled" {
			t.Fatalf("thinking 应下发 enabled，实际 %#v", req.Thinking)
		}
	})
}

func TestBuildRequestClampsMaxTokens(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"max_tokens":999999,"messages":[{"role":"user","content":"hi"}]}`)
	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("超限的 max_tokens 应钳制而非报错: %v", rejection)
	}
	// 期望值写字面量 32768 而不是 MaxOutputTokens：拿实现自己的常量当期望值是同义反复，
	// 把 MaxOutputTokens 改成 999999999 时钳制实际上不再发生，而断言照样成立。
	// 32768 是该模型的输出上限这一事实，本身就该被钉在用例里。
	if req.MaxTokens != 32768 {
		t.Fatalf("MaxTokens = %d，期望钳制到 32768", req.MaxTokens)
	}
	if MaxOutputTokens != 32768 {
		t.Fatalf("MaxOutputTokens = %d，该模型的输出上限是 32768", MaxOutputTokens)
	}
}

// TestBuildRequestKeepsMaxTokensUnderLimit 与上面那条配对：单有钳制那条时，
// 把实现写成「无条件赋值 MaxOutputTokens」也能全绿，而那会把客户端要求的
// max_tokens=64 悄悄放大到 32K（流式长回复的费用与延迟都会翻几十倍）。
func TestBuildRequestKeepsMaxTokensUnderLimit(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("BuildRequest 失败: %v", rejection)
	}
	if req.MaxTokens != 64 {
		t.Fatalf("MaxTokens = %d，未超限时必须原样下发 64", req.MaxTokens)
	}
}
