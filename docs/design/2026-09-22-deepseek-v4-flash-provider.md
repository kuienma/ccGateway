# DeepSeek V4 Flash 接入设计（火山方舟 Ark 通道）

- 日期：2026-09-22
- 目标：让 Claude Code 能通过本网关调用 DeepSeek V4 Flash，复用现有鉴权、并发控制与 SSE 心跳机制
- 状态：已实施（§11 的未决问题已回填实测结论，未验证项见文末）

## 1. 背景

当前网关是**纯字节透传**结构：`anthropic.PrepareRequest` 只摘掉 `model`/`stream` 并注入 `anthropic_version`
（`internal/anthropic/request.go:43-48`），`bedrock.Client` 把 body 原样交给 Bedrock `InvokeModel`
（`internal/bedrock/client.go:34-45`），`sse.go` 把 Bedrock 吐出的 chunk 原样包成 SSE 事件、连事件名都是直接从
chunk 的 `type` 字段读出来的（`internal/httpapi/sse.go:96,118-126`）。

这套结构能成立的唯一前提是：**Bedrock 上的 Anthropic 模型请求与响应两端都原生说 Anthropic Messages 协议**。
DeepSeek 说的是 OpenAI Chat Completions 协议，因此透传对它直接失效 —— 只往 `configs/models.json` 加别名不可行。

参考实现来源：`~/zhshl/nova` 已在生产使用同一模型，通道、凭证与踩过的坑均可直接复用。

## 2. 通道与实测结论

选定通道（nova 生产同款）：

| 项 | 值 |
| --- | --- |
| 模型 ID | `deepseek-v4-flash-ga-260731` |
| Base URL | `https://ark.cn-beijing.volces.com/api/v3`（拼 `/chat/completions`） |
| 凭证 | `DOUBAO_ARK_API_KEY`，`Authorization: Bearer <key>`，36 位 UUID 形态 |
| 协议 | 标准 OpenAI Chat Completions + 火山私有 `thinking` 字段 |

> nova 里另有 `deepseek-v4-flash-202605` 走腾讯 TokenHub，该通道**拒绝 `json_schema`**（有专门降级层
> `enhanced_llm_factory_schema_fallback.go:17-24`，源于 26-08-21 线上事故），不采用。

2026-09-22 实测（curl 直连）结论：

| 验证项 | 结果 |
| --- | --- |
| 流式 `tool_calls` 增量 | 标准 OpenAI 形状；首发 chunk 带 `id` + `function.name` + 空 `arguments` + `index`，后续只有 `arguments` 片段 |
| `arguments` 分片边界 | 会断在 JSON 语法中间：`{"city": "` → `北京` → `"` → `, "unit": "` → `c` → `elsius` → `"` → `}` |
| 并行多工具 | 支持，`index:0` / `index:1` 各自独立 call id；**分片按工具分组下发、无交错**，`index` 可靠（与 §11 第 2 条的真机复核一致） |
| 多轮 `tool_result` 回传 | 正常闭环，喂 `role:"tool"` + `tool_call_id` 后能基于结果作答 |
| `thinking:{"type":"disabled"}` | 被接受，`reasoning_content` 归零 |
| 不传 `thinking` | **默认思考开启**，返回 `reasoning_content` 与 `completion_tokens_details.reasoning_tokens` |
| `stream_options.include_usage` | 支持；**usage 在末尾出现两次** —— 带 `finish_reason` 的那个 chunk 上就有一份，之后还有一个 `choices:[]` 的纯 usage chunk（同一份数字），再 `data: [DONE]` |
| 事件顺序 | `reasoning_content` **全部吐完**才开始 `tool_calls` / `content` |

> ⚠️ **流式 usage 依赖上游一个非标准行为。** OpenAI 规范只要求在末尾那个 `choices:[]` chunk 上给
> usage；Ark **额外**在 `finish_reason` 那个 chunk 上也给一份，而本网关的状态机正是靠这一份：
> `EventEmitter.complete()` 在收到 `finish_reason` 的那一刻就写出带 usage 的 `message_delta`
> 并置 `finished`，之后那个纯 usage chunk 会被早退丢弃。
> 真机 fixture（`internal/ark/sse_test.go` 的 `arkToolStream`、`internal/ark/stream_test.go` 的
> `arkReasoningStream`，两者的 finish chunk 都带 usage）证实今天的 Ark 确实如此，**所以这不是
> 活跃缺陷**。但它是单点依赖：上游哪天规范化（usage 只在末尾给），每一次流式请求的
> `message_delta` 都会报 `input_tokens: 0 / output_tokens: 0`，**不报错、不打日志**。
> 失效表现已在 README 已知限制里申报。真要彻底修，得把 `message_delta` + `message_stop`
> 从 `complete()` 推迟到 `Finish()`，那是改被 review 过两轮的状态机，不在接入前做。

两个对设计有利的事实：

1. **顺序天然对齐 Anthropic**。Anthropic 要求 `thinking` 块排在其他 content 块之前，Ark 的吐出顺序正好如此，
   因此转换层**无需缓冲重排**，可纯流式边收边转。
2. **`tool_calls` 增量与 `input_json_delta` 近乎一一对应**，基本是机械翻译。

## 3. 核心架构决策：转换放在 provider 内部

现有 `bedrock.Runtime` 接口（`internal/bedrock/client.go:20-24`）的 `Stream` 签名是：

```go
Stream(ctx context.Context, modelID string, body []byte, emit func([]byte) error) error
```

`emit` 的既有语义就是"吐一个 Anthropic 格式、带 `type` 字段的 chunk JSON"，而 `sse.go` 只负责读 `type` 并包成
SSE 事件。**因此只要新 provider 的 `emit` 吐出合法 Anthropic chunk，`internal/httpapi/sse.go` 一行都不用改** ——
心跳、并发槽位、客户端断开取消、错误事件全部照旧复用。

据此确定：**协议转换完全封装在 provider 内部**，对上只暴露 Anthropic 语义。被否决的方案是在 `httpapi` 层转换，
那会侵入 `sse.go` 的事件循环，且让 HTTP 层背上协议知识。

## 4. 模块划分

```
internal/llm/            (新) 中立 Provider 接口 + 错误分类接口
internal/ark/            (新) Ark provider：HTTP 调用 + SSE 解析
  ├── client.go            net/http + Bearer，不引 SDK（照 nova 做法）
  ├── stream.go            OpenAI SSE 解析（移植 nova llm_tools_stream.go:209-302）
  └── translate/           (新) 纯函数转换层，无 IO，便于表驱动测试
      ├── request.go         Anthropic → OpenAI
      ├── response.go        OpenAI → Anthropic（非流式）
      └── events.go          OpenAI delta → Anthropic 事件序列（核心）
internal/modelcatalog/   (改) models.json 增 provider 字段
internal/anthropic/      (改) PrepareRequest 按 provider 分流
internal/httpapi/        (改) runtime 单例 → provider 路由；错误分类去 smithy 耦合
internal/config/         (改) 新增 Ark 凭证与 base URL
```

`internal/llm.Provider` 即现有 `bedrock.Runtime` 的三个方法原样抬升，`bedrock.Client` 与 `ark.Client` 各自实现。

需要改的既有接口（破坏性，但范围可控）：

- `modelcatalog.Catalog.Resolve` 目前返回 `(string, bool)`（`catalog.go:11`），需返回 provider 归属；
  `anthropic.ModelResolver` 接口同步调整。
- `anthropic.PreparedRequest` 增 `Provider` 字段；`PrepareRequest` 只在 Bedrock 路径注入 `anthropic_version`。
- `httpapi.Server.runtime` 字段（`server.go:16`）由单个 `bedrock.Runtime` 改为按 provider 查表。
- `classifyUpstreamError`（`errors.go:31-63`）现在硬依赖 smithy 类型，需拆成"provider 各自给出
  (status, kind, message)"，HTTP 层只做兜底。

## 5. 流式事件映射（核心）

Anthropic 的 `index` 是**全局递增的 content block 序号**，而 OpenAI 的 `tool_calls[].index` 是工具内部序号。
转换器必须维护自己的块计数器与映射表，不能把 OpenAI 的 index 直接当 Anthropic index 用。

| Ark / OpenAI | Anthropic 事件 |
| --- | --- |
| 首个 chunk 到达 | `message_start`（`usage` 里 **`input_tokens` / `output_tokens` 都填 0 占位**，真实值只在末尾的 `message_delta` 给） |
| `delta.reasoning_content` 首次非空 | `content_block_start`(thinking) + `content_block_delta`(`thinking_delta`) |
| `delta.reasoning_content` 后续 | `content_block_delta`(`thinking_delta`) |
| 出现非空 `content` 或 `tool_calls` | 先给上一块 `content_block_stop` |
| `delta.content` 首次非空 | `content_block_start`(text) + `content_block_delta`(`text_delta`) |
| `tool_calls[i]` 首发（带 id+name） | `content_block_start`(tool_use{id, name, input:{}})，登记 `i → 全局 index` |
| `tool_calls[i].arguments` 片段 | `content_block_delta`(`input_json_delta`{partial_json})，**裸字节透传不解析** |
| `finish_reason` 出现 | `content_block_stop` + `message_delta`{stop_reason, usage.output_tokens} + `message_stop` |
| `data: [DONE]` | 不产生事件，仅作完整性哨兵 |

`finish_reason` → `stop_reason`：`stop`→`end_turn`，`tool_calls`→`tool_use`，`length`→`max_tokens`，
其余（含 `content_filter`）→ `end_turn`。`content_filter` 的语义在 Anthropic 枚举里没有对应值，
只能并进 `end_turn`，但 `ark.Client` 会为它记一条 warn 日志（见 §7 的保真度说明）。

> ⚠️ **`message_start` 的 `input_tokens` 填 0，不是估算值**（`internal/ark/translate/events.go`
> 的 `ensureStarted`，用例 `events_test.go` 把这个 0 钉成了刻意的时序设计）。真实 Anthropic API
> 在 `message_start` 就给真值，本网关的 ark 路径不给 —— 只读 `message_start.usage` 的客户端会
> 永远看到 0。
>
> **这一条对将来的 `count_tokens` 真机校准很重要**：校准要拿真实 `input_tokens` 做锚，
> 而流式路径的真值**只在 `message_delta` 里**。若采样脚本从 `message_start` 取值，会得到
> 一串 0，进而把「估算严重偏高」这种完全相反的结论喂给校准 —— 那会把估算往低估方向调，
> 正是 §7 要求避免的方向。非流式路径没有这个坑（`usage` 在响应体里一次给全）。

### 必须遵守的三条不变式

1. **`content:""` 恒存在**。实测每个 chunk 都带 `"content":""`（空字符串，非 `null`）：

   ```json
   {"delta":{"content":"","reasoning_content":"The","role":"assistant"},"index":0}
   ```

   不能因为字段存在就开 text 块 —— 必须判非空，否则会在 thinking 阶段误开空 text block，破坏事件序列。

2. **`arguments` 片段绝不中途解析**。分片会断在 JSON 转义符或多字节 UTF-8 中间，只有整条拼完才是合法 JSON。
   直接作为 `partial_json` 原样转发给客户端（Anthropic 协议本身就允许分片）。nova 的
   `stream_utf8_accumulator.go` 处理多字节截断，一并移植。

3. **流不完整必须报错**。没收到 `finish_reason` 或 `[DONE]` 就断流，要当可重试错误上报，不能静默当成正常结束
   （照 nova `llm_tools_stream.go:316-318`）。这条同时能缓解现有网关"流式无静默超时"的问题。

## 6. 请求转换映射

| Anthropic | OpenAI / Ark |
| --- | --- |
| `system`（string 或 block 数组） | 提升为首条 `{"role":"system"}` message |
| `messages[].content` 文本块 | 拼接为字符串 |
| assistant `tool_use` 块 | `tool_calls[{id, type:"function", function:{name, arguments}}]`，`input` 序列化为字符串 |
| user `tool_result` 块 | 拆成独立 `{"role":"tool", tool_call_id, content}` message |
| assistant `thinking` 块（历史回传） | **丢弃**（Ark 不接受回传 reasoning） |
| `tool_result.is_error` | **丢弃**（OpenAI 的 `role:"tool"` 消息没有失败标记字段）；模型只能从工具回包的文本自行推断失败 |
| `tool_choice.disable_parallel_tool_use` | **丢弃**（OpenAI 的 `tool_choice` 无对应开关）；客户端要求禁用并行工具时 ark 路径仍可能并行 |
| `tools[].input_schema` | `tools[].function.parameters` |
| `tool_choice` auto/any/tool | `"auto"` / `"required"` / `{"type":"function","function":{"name":…}}` |
| `stop_sequences` | `stop` |
| `metadata.user_id` | `user` |
| `temperature` / `top_p` / `max_tokens` | 同名；`max_tokens` 缺省（或为 0）时补成 32768（见 §7「输出上限」） |
| `top_k` | 丢弃（无对应） |
| `cache_control` | 丢弃（Ark 是自动缓存，语义不同） |
| image 块 | **拦截报错**（见 §7） |

### thinking 的处理策略

实测 Ark **不传 `thinking` 时默认开启思考**，但 Anthropic 协议在客户端未请求 thinking 时不会返回 thinking 块，
Claude Code 可能不认。因此采取保守策略：

- 客户端传 `thinking:{"type":"enabled"}` → 转 Ark `thinking:{"type":"enabled"}`，`reasoning_content` 转成
  `thinking` 块透出（`budget_tokens` 无对应，丢弃）
- 其余情况（含完全不传）→ **主动注入 `thinking:{"type":"disabled"}`**，与 nova 的
  `shouldDisableThinking`（`llm.go:780-788`）一致；万一仍收到 `reasoning_content`，丢弃不转

~~待验证~~ **已实测（见 §11 第 1 条）**：Anthropic 的 `thinking` 块带 `signature` 字段，本网关不生成
`signature_delta`，实测 Claude Code 容忍不带 `signature` 的 thinking 块、未报错，因此**不补占位值**；
回传的 signature 仍在请求转换时丢弃。

## 7. 不支持能力与降级

| 能力 | 处理 |
| --- | --- |
| 图片 / 多模态输入 | nova 模型目录未标 `Vision`（对比 `provider.go:472` 的 Gemini 显式标注）。转换层检测到 image 块即返回 `invalid_request_error`，明确说明该模型不支持图片，**不静默丢弃** |
| `/v1/messages/count_tokens` | Ark 无此端点。本地估算（中文约 1 token/字，非 CJK 按 3 字符/token），**偏保守高估**，避免 Claude Code 低估上下文而超限。**对工具定义系统性偏低估，见下方说明** |
| `thinking` 枚举值 | 只校验类型（非对象 → 400），**不校验枚举值**：`{}`、`{"type":"enable"}`（拼错）一律静默按 disabled 处理，只有 `{"type":"enabled"}` 开启。与真实 Anthropic API 不对齐；且类型闸门只在 `/v1/messages` 上，同一个非法请求体打 `count_tokens` 会得到 200 |
| prompt caching | `cache_control` 丢弃；Ark 的 `cached_tokens` 只读不可控 |
| 输出上限 | nova 对该模型特判 32K 输出（`chat_model_budget.go:28-35`，注释指出"推理可能很长"），转换层对 `max_tokens` 做上限钳制（超限钳制而非报错）。**缺省时补成同一个 32768**：字段带 `omitempty`，不下发会让 Ark 用自己的默认上限，而思考也计入输出 —— 上限偏小会让回答在思考中途被截断（`stop_reason: max_tokens` + 空 content）且无任何信号。**这一处与 bedrock 路径行为不同**（bedrock 透传缺省值、由上游返回 400）：不在网关层加统一必填校验，因为那会改变 bedrock 的行为。负值不改写，留给上游拒绝 |
| `stop_reason` 保真度 | `content_filter` 与「命中 stop sequence」都被映射成 `end_turn`，且 `stop_sequence` 恒为 `null`（上游不回显命中的停止词）。客户端因此无法区分「正常说完」／「被内容过滤截断」／「命中停止词」。**不改映射**（Anthropic 枚举无对应值，硬造会让严格校验的客户端报错），但 `content_filter` 会在 `ark.Client` 记一条 warn 日志（两条路径都有），这是运维唯一能看出「这次是被过滤掉的」的线索 |
| 上游协议违约 | 三类情形硬失败而不是把损坏数据转发给客户端：①`tool_calls` 缺 id / name；②一次响应里两个调用共用同一个 `tool_use.id`（客户端会把两次调用混为一次）；③`arguments` 拼完不是 JSON 对象（`null`、半截 JSON、标量、数组）。流式与非流式两侧对齐。流式还有第四类：**并行工具的参数分片若交错下发**，状态机无法处理，流会中途报错（真机实测是严格分组、无交错，见 §11 第 2 条） |
| 上游错误原文 | 上游错误体里的消息回给客户端，但截到 512 字节（`maxClientErrorMessageBytes`）：非 JSON 形状时兜底值是整段原文，8KiB 既撑大响应也可能泄漏上游/代理的内部细节。**与 bedrock 侧策略不同**（那边兜底是固定文案「上游请求失败」，只有 `smithy.APIError` 才取上游消息）—— 保留差异是因为「Bedrock 路径行为零变化」是本次的硬约束 |

### `count_tokens` 对工具定义偏低估（实测，接入后第一优先级）

真机实测（§2 那次 curl，权威数据）：同一请求体**不带工具**时 `prompt_tokens = 89`，
加上一个 `get_weather` 工具定义后是 **397** —— 单个工具定义的真实成本是 **308 token**。
而那份定义的文本一共只有 77 个字符，所以**工具成本结构上不按字符长度走**，
主体是对话模板为每个工具展开函数声明、渲染 JSON schema、附加使用说明的固定开销。

当前估算模型因此改成「每个工具固定 `perToolOverhead = 300` + 定义文本按普通密度计入」
（`internal/ark/translate/tokens.go:32-47`）。它对这个数据点准，但：

- **四个常数（80 / 4 / 3 / 300）全部是从单个数据点倒推的**，未做多点校准；
- 对**大型工具定义**（长 description、深 schema）可能仍偏低估 —— 固定开销不随定义体量增长；
- 因此 **真机多点校准是接入后的第一优先级**。校准时以
  `TestEstimateTokensMatchesRealToolCost` 那条真机锚定断言为准（它把「一个工具 ≥ 308 token」
  直接写成了断言），并注意 `TestEstimateTokensCountsToolText` 已把「工具文本必须计入」
  与该常量的取值解耦，校准那个常量不会削弱这条护栏。

## 8. 配置

```
ARK_API_KEY=            # 必填（启用 ark provider 时）；取值同 nova 的 DOUBAO_ARK_API_KEY
ARK_BASE_URL=https://ark.cn-beijing.volces.com/api/v3
ARK_TIMEOUT=            # 可选，默认与 Bedrock 侧对齐
```

本网关的变量名统一用 `ARK_` 前缀（不沿用 nova 的 `DOUBAO_ARK_API_KEY` —— 那个名字在 nova 里是因为 Ark 被登记成
`doubao` provider，本网关没有这层历史包袱），凭证值本身是同一个。

`configs/models.json` 增 `provider` 字段，缺省为 `bedrock` 以保持现有配置向后兼容：

```json
{
  "default": "opus-5",
  "models": [
    { "id": "global.anthropic.claude-opus-5", "aliases": ["opus-5"] },
    { "id": "deepseek-v4-flash-ga-260731", "provider": "ark", "aliases": ["deepseek-v4-flash", "ds-flash"] }
  ]
}
```

启动时校验：若有模型登记为 `ark` 但 `ARK_API_KEY` 为空，直接启动失败（与现有 `GATEWAY_API_KEY` 空值即失败的
风格一致，见 `internal/config/config.go:55-57`），不要留到首个请求才报错。

## 9. 测试策略

- **转换层为纯函数**，用表驱动测试覆盖；本次探测抓到的真实 chunk 序列固化成 golden fixture
- **事件序列不变式断言**：每个 `content_block_start` 必有配对 `content_block_stop`；index 全局单调；
  `message_start` 必为首、`message_stop` 必为尾
- **并行工具用例**：用实测的双 index 交错序列，断言两个 `tool_use` 块的 id 与 index 映射不串
- **多轮闭环用例**：`tool_use` → `tool_result` → 文本回答，走完整请求转换
- **拒绝类用例**：image 块必须报 `invalid_request_error` 而非静默丢弃
- **断流用例**：缺 `finish_reason` / 缺 `[DONE]` 必须报可重试错误
- 现有 `internal/httpapi` 测试应全部保持通过 —— sse.go 不改是本设计的验收标志之一

## 10. 分期建议

1. **第一期**：provider 抽象 + Ark 非流式（`Invoke`）+ 请求/响应转换。此时 Claude Code 已可用（非流式），
   风险最低，能尽早验证 tool 循环
2. **第二期**：流式事件转换。工作量集中在此，但有 fixture 兜底
3. **第三期**：count_tokens 估算、thinking 透出、max_tokens 钳制等收尾

## 11. 未决问题（已回填实测结论）

1. **thinking 块缺 `signature` 时 Claude Code 是否报错 —— 不报错，无需补占位值。**
   实测流式响应里 thinking 块出现在 index 0（在 text 块之前）、含 17 个 `thinking_delta`、
   **不带 `signature` 字段**，Claude Code 正常消费全程未报错。因此初版不生成 `signature_delta`
   的决定保留，也不需要伪造占位值。客户端回传的 thinking 块仍然丢弃、不发给 Ark。

2. **DeepSeek 工具调用在 Claude Code 长链路下的稳定性 —— 实测通过。**
   完整多轮工具循环走通，有服务端日志为证：两次连续请求（2548ms 返回 `tool_use`
   → 1602ms 基于 `tool_result` 作答）。并行双工具实测分帧**严格分组、无交错**，
   block index 0/1 各自映射稳定，两个工具的参数各自合法且不串位。
   结论：网关侧的转换不构成瓶颈；模型能力本身仍建议先小范围试用再推广。

   ⚠️ 验证方法上的坑（值得单独记一句）：在一个 Claude Code 会话里嵌套跑 Claude Code 验证时，
   **必须用 `CLAUDE_CONFIG_DIR` 指向独立目录做隔离**，否则内层会继承外层配置、绕过
   `ANTHROPIC_BASE_URL` 直连官方端点 —— 现象是「全都正常」，实际一个请求都没经过网关。
   这是本项目踩过的最大的坑，任何「验证通过」的结论都要先确认流量真的进了网关。

3. **Ark 侧的限流阈值与重试语义 —— 本次验证未触发限流，无观察数据，待接入后补。**
   实测期间没有碰到 429 或任何限流响应，因此**阈值与退避语义仍然未知**，
   现状是沿用 Bedrock 侧同构的重试策略（`ARK_TIMEOUT` 控制非流式总时长，
   流式只约束「等到响应头」的时间）。这条不能算已验证 —— 接入后需要观察真实限流表现再调整。

## 12. 未验证项（实施完成时仍然成立的缺口）

标为「已实施」指代码与用例齐备、Ark 路径已真机走通，**不代表下面这些也验证过**：

1. **Bedrock 路径未做真机回归。** 实施环境没有 AWS 凭证（无 `aws` CLI、无 `AWS_*` 环境变量、
   无 `~/.aws/`），Bedrock 真机链路跑不了。改为三项静态验收替代：
   两条路径最实质的分叉点 `anthropic_version` / `anthropic_beta` 的注入有用例锁住
   （`internal/anthropic/request_test.go` 的 `TestPrepareRequestKeepsAnthropicVersionForBedrock`
   与 `TestPrepareRequestPreservesFieldsAndMapsModel`，另有两条反向用例锁 ark 路径**不**注入）；
   `providerFor` 的路由在**两个 provider 同时在册**（即唯一会上线的配置）的前提下有正反两向的
   用例覆盖，三个入口各一组，且每条都同时断言「另一个 provider 没被调用」
   （`internal/httpapi/routing_test.go`）；`internal/httpapi/sse.go` 相对 `main`
   的差异经核对仅为 3 行签名改动 + 2 条日志文案。**这不等于「Bedrock 回归通过」。**
2. **Ark 限流与重试语义无任何观察数据**（§11 第 3 条）。
3. **`count_tokens` 的真机多点校准未完成**，四个常数均来自单个数据点（见 §7）。
4. **`thinking` 枚举值不校验**这件事与真实 Anthropic API 的差异是已知且刻意保留的
   （与删字段前的行为精确一致、不是回归），但**未验证 Claude Code 在写错枚举值时的表现**。
   （若将来要补枚举值校验，必须同时让 `EstimateTokens` 也走 `parseThinking`，否则
   `/v1/messages` 与 `count_tokens` 会多出一处新的两侧分叉。）
5. **`max_tokens = 32768` 未经真机确认被 Ark 接受。** 真机那次 curl 用的是小 `max_tokens`，
   钳制路径（客户端填超限值 → 下发 32768）与新补的缺省路径（不填 → 下发 32768）走的是
   同一个值，但都没有真机样本。值本身取自 nova 生产对该模型的特判上限，风险低；
   若接入后看到「不带 `max_tokens` 的请求一律 400」，先查这一条。
