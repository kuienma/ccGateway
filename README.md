# Bedrock Anthropic Gateway

一个独立的 Anthropic Messages API 兼容网关，把 Claude 客户端的请求按模型路由到不同上游：**AWS Bedrock** 上的 Anthropic 模型走纯字节透传，**火山方舟 Ark** 上的 DeepSeek 模型由网关在内部完成 Anthropic ⇄ OpenAI 协议转换。客户端两种情况下都只说 Anthropic Messages 协议。网关使用 AWS SDK 默认凭证链，不保存 AWS Access Key。

> ## ⚠️ 升级到本版本的破坏性变更
>
> 默认的 `configs/models.json` **已经登记了一个 `provider: "ark"` 的模型**（DeepSeek V4 Flash）。
> 网关在启动时就校验这件事，因此：
>
> **部署必须提供 `ARK_API_KEY`，否则网关启动失败**，报错为
> `模型配置登记了 ark 模型，必须设置 ARK_API_KEY`。
>
> 这条会打到所有只用 Bedrock 的现有部署。二选一：
>
> 1. 配上 `ARK_API_KEY`（见[启动](#启动)）；或
> 2. 从 `configs/models.json` 里删掉那条 `provider: "ark"` 的模型 —— 删干净后 `ARK_API_KEY` 不再必填。
>
> 刻意做成启动即失败、而不是留到第一个请求才报错：模型目录支持 `SIGHUP` 热更新，
> 若允许带着空 `ARK_API_KEY` 启动，热加一个 ark 模型后每个请求都会拿到
> 「模型所属 provider 未启用: ark」，而那时即使补好环境变量也只能靠重启恢复。

开发前请阅读 [AI 编码与可维护性规范](docs/ai-coding-guidelines.md)，按模块职责、兼容性约束和变更类型选择实现与验证方式。

## 支持接口

- `POST /v1/messages`：非流式与 SSE 流式 Messages API
- `POST /v1/messages/count_tokens`：Bedrock 模型调用 AWS 真实 `CountTokens` 端点；**Ark 模型是网关本地估算**（Ark 没有这个端点），不访问上游，刻意偏向高估，详见[已知限制](#ark-模型的已知限制)
- `GET /healthz`：存活检查

默认模型目录包含：

| 客户端名称 | 上游 model ID | provider |
| --- | --- | --- |
| `opus-5`、`claude-opus-5` | `global.anthropic.claude-opus-5` | `bedrock`（缺省） |
| `opus-4-8`、`claude-opus-4-8` | `global.anthropic.claude-opus-4-8` | `bedrock`（缺省） |
| `deepseek-v4-flash`、`ds-flash` | `deepseek-v4-flash-ga-260731` | `ark` |

目录中登记的完整上游 model ID 也可以直接作为客户端模型名。未登记的模型会返回 `not_found_error`。

`output_config`（包括 `format` 中的 JSON Schema）直接转发给 Bedrock，网关不按模型能力过滤或预先拒绝该字段。上游是否接受由实际模型判断；上游 API 错误保留错误消息，并转换为 Anthropic 错误结构。非流式响应保留上游 HTTP 错误状态；流式响应开始后通过 SSE `error` 事件返回错误类型与消息。

## 启动

先配置 AWS 默认凭证链。生产环境优先使用 EC2 Instance Profile、ECS Task Role 或 Web Identity；本地可以使用 `aws configure`、`AWS_PROFILE` 或环境变量。

```bash
export AWS_DEFAULT_REGION=us-east-1
export GATEWAY_API_KEY="$(openssl rand -hex 32)"
export ARK_API_KEY="<火山方舟 API Key>"   # 默认 models.json 登记了 ark 模型，不配会启动失败
go run ./cmd/gateway
```

区域同时支持 AWS_REGION 与 AWS_DEFAULT_REGION，若两者都设置则 AWS_REGION 优先。服务默认监听 `:8080`。启动时会立即校验 `configs/models.json`，AWS 凭证由 SDK 在首次调用时从默认凭证链解析。

### 火山方舟 Ark（DeepSeek）相关变量

```bash
ARK_API_KEY=                                        # 仅当 models.json 登记了 provider=ark 的模型时必填
ARK_BASE_URL=https://ark.cn-beijing.volces.com/api/v3 # 末尾不要带斜杠（带了也会被自动去掉）
ARK_TIMEOUT=120s                                    # 非流式请求的总时长上限；流式只用它约束「等到响应头」的时间
```

`ARK_API_KEY` 的取值与火山方舟控制台的 API Key 相同。只要 `ARK_API_KEY` 非空，网关就会装配 ark provider（即使当前 `models.json` 里还没有 ark 模型），这样 `SIGHUP` 热加 ark 模型后可以立即可用。

## 启停脚本

项目提供统一管理脚本：

```bash
./scripts/gateway.sh start    # 后台启动并通过 healthz 确认成功
./scripts/gateway.sh status   # 查看运行状态
./scripts/gateway.sh reload   # 修改 models.json 后热更新
./scripts/gateway.sh restart  # 重启
./scripts/gateway.sh stop     # SIGTERM 优雅停止
./scripts/gateway.sh logs     # 持续查看日志
./scripts/gateway.sh run      # 前台运行
```

默认读取项目根目录 `.env`，运行文件保存到 `.runtime/`。也可以通过 `GATEWAY_ENV_FILE` 和 `GATEWAY_RUNTIME_DIR` 指定其他路径。`stop` 默认等待 20 秒，可通过 `STOP_WAIT_SECONDS` 调整。

本地网络需要代理才能访问 Anthropic Bedrock 模型时，应把代理变量明确写入 `.env`，避免后台进程没有继承终端环境：

```bash
HTTP_PROXY=http://127.0.0.1:7897
HTTPS_PROXY=http://127.0.0.1:7897
ALL_PROXY=socks5://127.0.0.1:7897
```

不要把 `bedrock-runtime.<region>.amazonaws.com` 加入 `NO_PROXY`，除非已经确认当前公网出口所在地区受到 Anthropic 支持。

流式稳定性可以通过以下变量调整：

```bash
STREAM_HEARTBEAT_INTERVAL=15s # 网关发给 SSE 客户端的注释心跳
MAX_CONCURRENT_STREAMS=4      # 同时执行的上游流式请求数（两个 provider 共用同一批槽位）
STREAM_QUEUE_TIMEOUT=30s      # 等待流式并发槽位的最长时间
```

网关在上游暂无新事件时发送 `: ping` SSE 注释；每收到一个上游事件都会重新计算空闲时间，客户端应忽略注释。并发槽位在等待超时后返回 `503 overloaded_error`。心跳与并发槽位对两个 provider 一视同仁。Ark 侧的超时由 `ARK_TIMEOUT` 单独控制；AWS SDK 使用 10 秒建连超时、30 秒 TCP keepalive、120 秒响应头超时和最多三次调用尝试（含首次调用），单次最大退避限制为 5 秒；流式响应不设置覆盖整个生成过程的总超时。

Clash Verge Rev / Mihomo 建议为 Bedrock 域名配置固定的支持地区节点，并启用：

```yaml
keep-alive-interval: 15
keep-alive-idle: 15
disable-keep-alive: false
```

## Claude 客户端

Claude Code 连接本地网关只靠三个环境变量：`ANTHROPIC_BASE_URL`、`ANTHROPIC_AUTH_TOKEN`、`ANTHROPIC_MODEL`。

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080
export ANTHROPIC_AUTH_TOKEN='<与 GATEWAY_API_KEY 相同的值>'
export ANTHROPIC_MODEL=opus-5
export ANTHROPIC_DEFAULT_OPUS_MODEL=opus-5
export ANTHROPIC_DEFAULT_SONNET_MODEL=opus-4-8
export ANTHROPIC_DEFAULT_HAIKU_MODEL=opus-4-8
claude
```

要改用 DeepSeek，把 `ANTHROPIC_MODEL` 换成目录里的 ark 别名即可，网关按模型自动路由：

```bash
export ANTHROPIC_MODEL=ds-flash   # 或 deepseek-v4-flash
claude
```

两条实测结论，不知道会白折腾很久：

- **`claude --model <别名>` 不可用。** 客户端对 `--model` 有自己的白名单，网关的自定义别名会被它直接拒掉，请求根本发不出来。选模型只能用 `ANTHROPIC_MODEL` 环境变量。
- **启动时的 `[claude-code:unrecognized_model]` 警告可以忽略。** 它只是客户端不认识这个别名的提示，不阻止请求，后续调用一切正常。

此模式不要设置 `CLAUDE_CODE_USE_BEDROCK=1`，因为客户端连接的是 Anthropic 兼容网关，AWS 签名由网关完成。

> 如果是在一个 Claude Code 会话里再嵌套跑 Claude Code 做验证，必须用 `CLAUDE_CONFIG_DIR` 指向一个独立目录做隔离，否则内层会继承外层配置、**绕过 `ANTHROPIC_BASE_URL` 直连官方端点**，看起来「一切正常」但压根没经过网关。这是本项目踩过的最大的坑。

通用 Anthropic SDK 可以把 `base_url` 指向 `http://127.0.0.1:8080`，并把 API key 设置成 `GATEWAY_API_KEY`。

## 动态调整模型

编辑 `configs/models.json`，每个模型配置一个真实的上游 model ID、所属 `provider` 和若干客户端别名：

```json
{
  "default": "opus-5",
  "models": [
    {
      "id": "global.anthropic.claude-opus-5",
      "aliases": ["claude-opus-5", "opus-5"]
    },
    {
      "id": "deepseek-v4-flash-ga-260731",
      "provider": "ark",
      "aliases": ["deepseek-v4-flash", "ds-flash"]
    }
  ]
}
```

`provider` 只接受 `bedrock` 与 `ark`，**缺省为 `bedrock`**，所以本字段出现之前的配置文件不用改就能继续用。登记为 `ark` 的模型要求 `ARK_API_KEY` 非空，否则启动（或 `SIGHUP` 之后的请求）会失败。

修改后向进程发送 `SIGHUP`：

```bash
kill -HUP <gateway-pid>
```

新配置只有在 JSON 结构、默认模型和所有别名均通过校验后才会原子生效。校验失败时进程继续使用上一版。Docker 或 Kubernetes 可以把模型文件挂载到 `/configs/models.json` 后发送 `SIGHUP`。

## Ark 模型的已知限制

以下限制只适用于 `provider: "ark"` 的模型（DeepSeek V4 Flash）。Bedrock 路径的行为与本版本之前完全一致。

- **图片输入转发给上游，网关不判断模型能力。** 消息正文里的 `image` 块转成 OpenAI 的 `image_url`（`source.type: "base64"` 拼成 `data:<media_type>;base64,<data>` 的 data URI，`"url"` 原样取 `url`），图文顺序原样保留。**网关刻意不内置「哪个模型支持图片」的判断**：这份表必然会过期（本条此前写的就是「不支持图片」，它是接第一个纯文本 endpoint 时写死的，后来接了支持图片的模型便开始误报）。模型不支持图片时由 Ark 返回 400，网关如实转达上游原文。畸形的 `image` 块（缺 `source` / `data` / `media_type` / `url`，或 `source.type` 不是 `base64` / `url`）仍在本地返回 `invalid_request_error`，**不静默丢弃** —— 静默丢图会让模型答非所问且无从排查。
- **`tool_result` 里的图片会被搬到一条合成 user 消息。** OpenAI 协议中 `role=tool` 的 content 只能是字符串，装不下图片 part，因此网关把图片抽出来，放进紧随所有 tool 消息之后的一条 `user` 消息（正文消息可以带 `image_url`），原位留下 `[tool result media moved to the following user message]` 标记——**不静默丢弃**。消息顺序为 `assistant(tool_calls) → tool… → user(图片) → 主消息`，tool 消息与其 assistant 的绑定不被拆开。多条 tool_result 的图片合并进同一条 user 消息，每张图前带 `[tool result media of <tool_use_id>]` 归属标注。**整段字符串本身就是一张 base64 data URL 时同样会被抽出**，但需超过 8KB 阈值——低于阈值的维持文本，以免误判刻意检查微小内联数据的用法。没有图片时输出逐字节不变，prompt cache 不受影响。
- **`system` 里的图片仍然拒绝。** `system` 必须是最前面那条消息，无法用「插一条后续消息」的方式承载图片。返回的 `invalid_request_error` 会点明是**该位置**不支持而非模型不支持。
- **图片的 `count_tokens` 估算会严重高估。** 本地估算把 `image` 块按其原始 JSON 文本长度计入，而图片的 base64 原文动辄几十万字符，真实视觉 token 通常只有几百到几千 —— 一张图可能被估成几万 token。带图请求的 `count_tokens` 结果仅能当「远大于真实值」的上界看，不要用它做上下文余量判断。真实消耗要看响应 `usage.input_tokens`。
- **`count_tokens` 是网关本地估算，不访问上游。** Ark 没有 `CountTokens` 端点。估算刻意偏向高估（低估会让 Claude Code 误以为上下文还有余量、真实请求被上游拒绝）。**对工具定义仍偏低估**：真机实测单个工具定义的真实成本是 308 token（同一请求不带工具 `prompt_tokens=89`、带该工具 397），当前模型按「每个工具固定 300 + 定义文本按普通密度」计算，对小工具较准，**对大型工具定义可能仍偏低**。同一个 `/v1/messages/count_tokens` 接口在两个 provider 下精度不同，这是最容易引起误会的一点。
- **prompt caching 不可控。** 请求里的 `cache_control` 被丢弃；Ark 返回的 `cached_tokens` 只读，网关无法控制命中与否。
- **thinking 块不带 `signature`。** Ark 上游不提供该信息，网关也不伪造。实测 Claude Code 能正常消费不带 `signature` 的 thinking 块、不报错。客户端回传的 thinking 块会被丢弃，不会发给 Ark。
- **`thinking` 字段只校验类型、不校验枚举值。** 只有 `{"type":"enabled"}` 才开启思考；`{}`、`{"type":"enable"}`（拼错）、`{"type":"whatever"}` 都会被**静默当作 disabled**，没有任何报错或警告。这与真实 Anthropic API 不对齐（后者会拒绝非法枚举值）。写错时的现象是「以为开了思考、其实没开」，排查时值得先确认这个拼写。另外 `thinking` 写成对象以外的类型（字符串、数字、数组、布尔）会返回 400，但**只有 `/v1/messages` 这条路径有这个类型闸门**：同一个请求体打 `/v1/messages/count_tokens` 会得到 200，打 `/v1/messages` 得到 400。
- **`max_tokens` 上限 32768，超限钳制而非报错。** DeepSeek V4 Flash 是推理模型、思考部分也计入输出，故上限取 32K；客户端要求更大时网关钳制到 32768 并照常请求，不会因此失败。
- **`reasoning_tokens` 不计入 `output_tokens`。** 它是 `completion_tokens` 的内部明细（`prompt + completion = total` 恒等、`reasoning < completion`），不是额外加项 —— 不要在统计口径上把它再加一遍。
- **`max_tokens` 缺省时网关补成 32768，与 Bedrock 路径不同。** Anthropic 协议里 `max_tokens` 是必填。Bedrock 路径把缺省原样透传、由上游返回 400；ark 路径不下发就会让 Ark 用它自己的默认输出上限，而该模型把思考也计入输出，上限偏小会让回答在思考中途被截断（`stop_reason: max_tokens` + 空 content）且没有任何信号，所以这里补成钳制上限 32768。负值不改写，留给上游拒绝。**Claude Code 总会带 `max_tokens`，这条只影响其他客户端。**
- **流式 `usage` 依赖 Ark 的一个非标准行为。** OpenAI 规范只要求在末尾那个 `choices:[]` chunk 上给 usage，Ark 额外在 `finish_reason` 那个 chunk 上也给一份，而网关的状态机正是靠这一份（收到 `finish_reason` 就立即写出带 usage 的 `message_delta`）。真机确实如此，**今天不是缺陷**；但上游一旦规范化，每次流式请求的 `message_delta` 都会报 `input_tokens: 0 / output_tokens: 0`，**不报错也不打日志**。如果哪天看到流式 usage 全是 0、非流式却正常，就是这条。非流式不受影响。
- **流式 `message_start.usage` 恒为 0。** 真实 Anthropic API 在 `message_start` 就给 `input_tokens` 真值，ark 路径填 0 占位，真值只在末尾的 `message_delta` 里。只读 `message_start` 的客户端在 ark 路径上永远看到 0；做 token 采样／校准时必须从 `message_delta` 取。
- **`stop_reason` 有保真度损失。** `content_filter`（内容过滤截断）与「命中 `stop_sequences`」都会变成 `end_turn`，且 `stop_sequence` 恒为 `null`。客户端因此无法区分「正常说完」／「被过滤截断」／「命中停止词」。不改映射是刻意的：Anthropic 的枚举里没有对应值，硬造一个会让严格校验的客户端报错。**内容过滤这一种在服务端会记一条 warn 日志**（`Ark 响应被内容过滤截断…`，流式与非流式都有）—— 回答莫名截断时先查这条日志。
- **`tool_result.is_error` 被丢弃。** OpenAI 的 `role:"tool"` 消息没有失败标记字段，网关把 `is_error: true` 静默丢掉，模型只能从工具回包的文本自行推断失败。多轮工具循环里这会降低纠错率（Claude Code 用 `is_error` 表达命令失败），所以工具失败时让回包文本自己说清楚（例如以 `error:` 开头）比依赖这个字段可靠。
- **`tool_choice.disable_parallel_tool_use` 被丢弃。** OpenAI 的 `tool_choice` 没有对应开关。客户端要求禁用并行工具时，ark 路径仍可能一次返回多个 `tool_use` 块。
- **上游协议违约时宁可报错，也不转发损坏数据。** 以下情形网关直接失败（流式会在流中途发 `error` 事件）：`tool_calls` 缺 `id` / `name`；一次响应里两个工具调用共用同一个 id（客户端会把两次调用混为一次，第二个工具的结果永远回不去）；`arguments` 拼完不是 JSON 对象（`null`、半截 JSON、标量、数组）。另外**上游一旦交错下发并行工具的参数分片，流式会报错**（真机实测是严格分组、无交错，状态机按此实现；真出现交错需要改成缓冲方案）。
- **上游错误原文截到 512 字节，与 Bedrock 路径策略不同。** 上游错误体不是 `{"error":{"message":…}}` 形状时（反向代理的 HTML 502、纯文本错误页），网关取其原文开头回给客户端并标注「已截断」。保留开头是为了排障（Ark 的 400 会点名不合法的字段），截断是为了不撑大错误响应、也不把上游／代理的内部细节整段透出。Bedrock 路径的兜底仍是固定文案「上游请求失败」，两条路径在这点上不统一。

## Docker

```bash
docker build -t bedrock-anthropic-gateway .
docker run --rm -p 8080:8080 \
  --env-file .env \
  -v "$PWD/configs/models.json:/configs/models.json:ro" \
  bedrock-anthropic-gateway
```

本地 Docker 可在已被 .gitignore 排除的 .env 中提供 AWS 默认凭证链需要的变量。部署在 AWS 时应给容器绑定 IAM Role，不向镜像写入静态 AWS 凭证。该 Role 至少需要目标模型对应的 bedrock:InvokeModel、bedrock:InvokeModelWithResponseStream 与 bedrock:CountTokens 权限。

## 模块职责与生命周期

- `cmd/gateway/main.go` 是日志与进程退出入口；`app.go` 装配配置、模型目录、AWS 客户端和 HTTP 服务；`lifecycle.go` 管理信号、服务运行及后台任务回收。
- `internal/config` 解析环境变量；`internal/modelcatalog` 分别负责纯 JSON 校验、模型名解析和文件加载／原子快照替换。
- `internal/anthropic` 转换协议字段；`internal/bedrock` 负责 SDK 调用、传输策略和流资源关闭；`internal/httpapi` 负责路由、认证、请求准备、SSE、并发槽位及完成日志。
- `internal/llm` 定义 provider 抽象与模型归属；`internal/ark` 是火山方舟通道（HTTP 调用、SSE 行解析），其 `translate` 子包用纯函数完成 Anthropic ⇄ OpenAI 的请求/响应/流事件转换。**协议差异完全封装在 provider 内部**：`internal/httpapi/sse.go` 对上游是谁无感知，只按事件的 `type` 字段生成 SSE 事件名，`translate` 也不反向依赖 `internal/anthropic`。

收到 SIGINT、SIGTERM 或父 context 取消后，停止接收新请求；关闭信号不会直接取消所有请求。进入退出流程时取消热更新，HTTP Shutdown 与后台任务清理共用 `SHUTDOWN_TIMEOUT`（默认 `15s`）对应的一个截止时间，期限内等待在途请求与任务正常完成。服务自身失败也使用该期限等待清理。若同步模型文件读取仍阻塞，期限耗尽即返回可由 `errors.Is(err, context.DeadlineExceeded)` 识别的错误，保留原始服务／Shutdown 错误，由 main 的错误路径终止进程；此时不保证全部后台任务已回收。SIGHUP 和退出信号订阅在返回时释放。SDK 流在正常结束、异常、回调失败或取消时关闭，不重放已输出的内容。

## 验证

```bash
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
bash -n scripts/gateway.sh tests/static/test_gateway_start_check.sh
bash tests/static/test_gateway_start_check.sh
build_dir=$(mktemp -d)
CGO_ENABLED=0 GOOS=linux go build -trimpath -o "$build_dir/gateway" ./cmd/gateway
```

请求完成日志记录 request ID、接口、模型、是否流式、耗时和结果；上游失败另记错误详情。日志不记录请求正文、网关密钥或 AWS 凭证。

常规测试使用 stub、临时文件和独立本地监听器，不需要真实 AWS 凭据，也不操作现有服务。真实 AWS 测试由 `integration` build tag 隔离，只有明确需要访问 AWS 时才运行 `go test -tags=integration ./internal/bedrock` 并设置 `BEDROCK_INTEGRATION_MODEL`。
