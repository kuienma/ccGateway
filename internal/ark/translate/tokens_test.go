package translate

import (
	"encoding/json"
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
		t.Fatalf("input_schema 的贡献只有 %d token，低于其文本量下界 %d —— "+
			"name/description/input_schema 必须全部计入", tooled-bare, len(schema)/3)
	}
}

// TestEstimateTokensChargesToolDefinitionsDenser 钉住工具定义比同样长度的普通文本更贵。
//
// 实测一个工具让 prompt_tokens 从 89 涨到 397（+308），而那份工具定义的文本量按
// 3 字符/token 只值约 100 token —— 说明模板把工具声明展开后还要加一层包装格式，
// 按普通文本的密度估工具定义会系统性低估近 3 倍。这里不去焊死倍率（只有一个数据点），
// 只锁住「工具定义必须按更密的系数计」这个方向。
func TestEstimateTokensChargesToolDefinitionsDenser(t *testing.T) {
	t.Parallel()

	const payload = `{"type":"object","properties":{"city":{"type":"string"}}}`
	asTool := []byte(`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"t","description":"d","input_schema":` + payload + `}]}`)
	asText := []byte(`{"max_tokens":8,"messages":[{"role":"user","content":"hi"},
		{"role":"assistant","content":` + string(mustJSONString(payload)) + `}]}`)

	toolCount, err := EstimateTokens(asTool)
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	textCount, err := EstimateTokens(asText)
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	if toolCount <= textCount {
		t.Fatalf("同一段文本作为工具定义的估算 %d 未超过作为普通消息文本的 %d："+
			"工具定义在模板里还要包一层声明格式，必须按更密的系数计", toolCount, textCount)
	}
}

// mustJSONString 把一段文本编成 JSON 字符串字面量，供上面的用例把同样的内容
// 分别塞进 tools 与 messages。
func mustJSONString(text string) []byte {
	encoded, err := json.Marshal(text)
	if err != nil {
		panic(err)
	}
	return encoded
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
	// 只比大小会被 perMessageOverhead 蒙混：多两条消息本身就带 8 token 固定开销，
	// 即使正文全被丢掉也能通过。所以要把开销扣掉再比。
	overheadOnly := bareCount + 2*perMessageOverhead
	if toolCount <= overheadOnly {
		t.Fatalf("tool_use / tool_result 的正文未计入：估算 %d 未超过纯消息开销 %d",
			toolCount, overheadOnly)
	}
	// tool_result 里有 26 个汉字，单这一项就该贡献 20+ token。
	if toolCount < overheadOnly+20 {
		t.Fatalf("tool_result 正文的贡献明显偏低: %d vs 下界 %d", toolCount, overheadOnly+20)
	}
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

func TestBuildRequestClampsMaxTokens(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"max_tokens":999999,"messages":[{"role":"user","content":"hi"}]}`)
	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("超限的 max_tokens 应钳制而非报错: %v", rejection)
	}
	if req.MaxTokens != MaxOutputTokens {
		t.Fatalf("MaxTokens = %d，期望钳制到 %d", req.MaxTokens, MaxOutputTokens)
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
