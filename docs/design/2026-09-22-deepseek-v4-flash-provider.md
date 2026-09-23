# DeepSeek V4 Flash 接入设计（火山方舟 Ark 通道）

- 日期：2026-09-22
- 目标：让 Claude Code 能通过本网关调用 DeepSeek V4 Flash，复用现有鉴权、并发控制与 SSE 心跳机制
- 状态：待评审

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
| 并行多工具 | 支持，`index:0` / `index:1` 各自独立 call id，chunk 交错但 index 可靠 |
| 多轮 `tool_result` 回传 | 正常闭环，喂 `role:"tool"` + `tool_call_id` 后能基于结果作答 |
| `thinking:{"type":"disabled"}` | 被接受，`reasoning_content` 归零 |
| 不传 `thinking` | **默认思考开启**，返回 `reasoning_content` 与 `completion_tokens_details.reasoning_tokens` |
| `stream_options.include_usage` | 支持；末尾多一个 `choices:[]` 的纯 usage chunk，再 `data: [DONE]` |
| 事件顺序 | `reasoning_content` **全部吐完**才开始 `tool_calls` / `content` |

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
| 首个 chunk 到达 | `message_start`（`input_tokens` 用本地估算占位，末尾用真实值修正） |
| `delta.reasoning_content` 首次非空 | `content_block_start`(thinking) + `content_block_delta`(`thinking_delta`) |
| `delta.reasoning_content` 后续 | `content_block_delta`(`thinking_delta`) |
| 出现非空 `content` 或 `tool_calls` | 先给上一块 `content_block_stop` |
| `delta.content` 首次非空 | `content_block_start`(text) + `content_block_delta`(`text_delta`) |
| `tool_calls[i]` 首发（带 id+name） | `content_block_start`(tool_use{id, name, input:{}})，登记 `i → 全局 index` |
| `tool_calls[i].arguments` 片段 | `content_block_delta`(`input_json_delta`{partial_json})，**裸字节透传不解析** |
| `finish_reason` 出现 | `content_block_stop` + `message_delta`{stop_reason, usage.output_tokens} + `message_stop` |
| `data: [DONE]` | 不产生事件，仅作完整性哨兵 |

`finish_reason` → `stop_reason`：`stop`→`end_turn`，`tool_calls`→`tool_use`，`length`→`max_tokens`。

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
| `tools[].input_schema` | `tools[].function.parameters` |
| `tool_choice` auto/any/tool | `"auto"` / `"required"` / `{"type":"function","function":{"name":…}}` |
| `stop_sequences` | `stop` |
| `metadata.user_id` | `user` |
| `temperature` / `top_p` / `max_tokens` | 同名 |
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

待验证：Anthropic 的 `thinking` 块带 `signature` 字段。初版不生成 `signature_delta`，观察 Claude Code 是否容忍；
若不容忍则补一个占位值，并在请求转换时丢弃回传的 signature。

## 7. 不支持能力与降级

| 能力 | 处理 |
| --- | --- |
| 图片 / 多模态输入 | nova 模型目录未标 `Vision`（对比 `provider.go:472` 的 Gemini 显式标注）。转换层检测到 image 块即返回 `invalid_request_error`，明确说明该模型不支持图片，**不静默丢弃** |
| `/v1/messages/count_tokens` | Ark 无此端点。本地估算（中文约 1 token/字，英文约 4 字符/token），**偏保守高估**，避免 Claude Code 低估上下文而超限 |
| prompt caching | `cache_control` 丢弃；Ark 的 `cached_tokens` 只读不可控 |
| 输出上限 | nova 对该模型特判 32K 输出（`chat_model_budget.go:28-35`，注释指出"推理可能很长"），转换层对 `max_tokens` 做上限钳制 |

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

## 11. 未决问题

1. thinking 块缺 `signature` 时 Claude Code 是否报错（待第三期实测）
2. DeepSeek 的工具调用稳定性是否够支撑 Claude Code 的长链路 —— **这是模型能力问题，网关无法弥补**，
   建议第一期落地后先小范围试用再决定是否推广
3. Ark 侧的限流阈值与重试语义未知，第一期先沿用 Bedrock 侧的 3 次重试策略，观察后调整
