# AI 编码与可维护性规范

本规范用于本仓库的新增功能、缺陷修复和任务所需的重构，目标是让下一位维护者能判断代码归属、理解行为、隔离测试并安全修改。它不是全仓重构计划，也不扩大具体任务或工具权限。

**规则等级：**“必须”是交付验收条件；“建议”是默认选择，偏离时说明理由；“按场景适用”仅在触及对应行为时执行，触发后必须给出检查证据。本文列出的检查是后续任务应执行的要求，不表示已由 CI、hook 或 AI 自动加载机制强制启用，也不表示本次已运行这些程序测试。

## 1. 先建立事实与修改边界

项目是 Anthropic Messages API 到 AWS Bedrock 的兼容网关。[go.mod](../go.mod) 声明 Go 1.24，HTTP 使用标准库 `net/http`，上游使用 AWS SDK for Go v2；另有 [Shell 管理脚本](../scripts/gateway.sh) 和 [Python 协作脚本](../scripts/codex_duo.py)。实现与测试是当前行为的核验依据，[README](../README.md) 提供使用方式，[Makefile](../Makefile) 提供常用命令；历史设计、报告和对方 AI 的结论只作线索，不能充当本次验证证据或额外授权。

编码前必须完成以下动作，并在计划或首轮说明中简短记录：

1. 运行 `git status --short`，识别已有修改和未跟踪文件，保留任务外工作；明确允许修改的文件或模块。不得顺手修改依赖、配置、脚本、CI 或 AI 指令文件。
2. 将任务转成可观察的验收条件，例如“客户端取消后上游退出，槽位才可再次使用”，而非“优化代码质量”。列出最小修改范围和可能影响的接口、配置、错误、日志及运行行为。
3. 核对相关调用链与已有测试。若同时存在注册 code-review-graph 的 `.mcp.json` 和 `.code-review-graph/`，先使用可用图工具定位结构、调用关系和测试；图未安装、工具不可用或覆盖不足时说明情况，再用 `rg` 和源码读取补足。不得以使用图工具为由安装新工具或绕过权限。
4. 阅读实际调用方、实现和测试断言，再选方案。典型请求链是 `httpapi.Server.Handler` → 认证与请求准备 → `anthropic.PrepareRequest` → 注入的 `bedrock.Runtime` → `bedrock.Client` → `bedrock.SDK`。启动和热更新还要核对 `cmd/gateway` 与 `modelcatalog.Manager`。
5. 常规命名、函数拆分、测试替身等在授权范围内自行决定，不逐步索要确认。需要改变业务范围、破坏兼容性或缺少必要权限时，先完成独立可做的核验，再按任务约定报告问题、选项、推荐和理由；未决部分不擅自实现。

## 2. 模块职责与依赖边界

| 模块及真实入口 | 新逻辑归属 | 检查方式 |
| --- | --- | --- |
| `cmd/gateway`：[main.go](../cmd/gateway/main.go)、[app.go](../cmd/gateway/app.go)、[lifecycle.go](../cmd/gateway/lifecycle.go) | 进程入口与退出、依赖装配、信号、HTTP 服务生命周期和后台任务回收 | AWS 客户端在装配处创建；生命周期变化查 `lifecycle_test.go`，不搬入 handler |
| `internal/config`：[config.go](../internal/config/config.go) | 环境变量读取、默认值、优先级、类型及取值校验 | 新配置进入 `Config` 与 `LoadFromLookup`，测试缺失、空白、非法值和边界 |
| `internal/modelcatalog`：[parse.go](../internal/modelcatalog/parse.go)、[catalog.go](../internal/modelcatalog/catalog.go)、[manager.go](../internal/modelcatalog/manager.go) | 纯 JSON 校验、名称解析、文件读取与不可变快照原子更新 | `Parse` 不读文件；`Resolve` 不访问环境或 AWS；`Reload` 成功前不发布新快照 |
| `internal/anthropic`：[request.go](../internal/anthropic/request.go)、[error.go](../internal/anthropic/error.go) | 协议字段转换、模型解析接口和协议错误定义 | 转换可用字节输入和 resolver stub 独立测试，不依赖 HTTP writer 或 SDK 客户端 |
| `internal/bedrock`：[client.go](../internal/bedrock/client.go)、[httpclient.go](../internal/bedrock/httpclient.go) | AWS SDK 参数适配、传输与重试策略、SDK 流消费及关闭 | 使用 `SDK` 替身核对输入与错误链；不承担网关认证、HTTP 状态映射或 SSE 编码 |
| `internal/httpapi`：[server.go](../internal/httpapi/server.go)、[request.go](../internal/httpapi/request.go)、[errors.go](../internal/httpapi/errors.go)、[sse.go](../internal/httpapi/sse.go)、[stream_slots.go](../internal/httpapi/stream_slots.go)、[completion.go](../internal/httpapi/completion.go) | 路由、认证、请求准备、响应与错误映射、SSE、并发控制、请求完成日志 | handler 经已有接口调用协议与上游能力；不随意创建 AWS 客户端、读取环境或堆积转换代码 |

必须沿用现有结构，不为套用分层术语新增 controller/service/repository 层或框架。判断位置时看“该行为因什么需求改变”：协议字段变化进入 `anthropic`，SDK 参数变化进入 `bedrock`，HTTP 表现变化进入 `httpapi`；跨模块任务分别在各自职责内修改。

现有注入点及测试边界：

- `anthropic.ModelResolver` 仅有 `Resolve(name string) (string, bool)`。生产可注入 `modelcatalog.Manager`，转换或 HTTP 测试用包内 `resolverStub`，不必加载真实配置文件。
- `bedrock.Runtime` 提供 `Invoke`、`Stream`、`CountTokens`，均接收 `context.Context`；`Stream` 通过 `func([]byte) error` 回调交付事件。`httpapi.NewServer` 接收 `ServerConfig`、resolver、runtime 和 logger，HTTP 测试用 runtime stub 控制成功、失败与取消。
- `bedrock.SDK` 是三个 SDK 方法的适配边界，`bedrock.NewClient(sdk)` 接收它。SDK 参数形状、返回值和错误传播在此测试，不要求 HTTP 层了解 SDK 类型。
- `config.LoadFromLookup(lookup LookupFunc)` 接收查询函数；`Load()` 才接入 `os.LookupEnv`。配置解析测试优先注入 map 查询，避免并行测试互相污染进程环境。
- 生命周期已有包内 `httpService` 和 `reload func() error` 注入点，可用 service stub 控制失败与阻塞；真实 HTTP 关闭测试使用独立本地监听器。

## 3. 实施规则

### 3.1 职责、命名与抽象

| 等级 | 动作 | 验收时检查 |
| --- | --- | --- |
| 必须 | 函数聚焦一个可描述的职责；拆分混合协议转换、外部 I/O 和响应写出的流程 | 能用一句话说明每个新增函数负责什么；调用者能看清顺序、错误出口与资源所有权 |
| 必须 | 命名表达业务含义，例如 `modelID`、`streamQueueTimeout`；注释解释原因、约束和不明显行为 | 不使用含义不明的 `data2`、`handleAll`；注释不逐句翻译代码，不遗留失效描述 |
| 必须 | 避免职责混杂的巨型函数、泛化工具包、重复实现和任务外清理 | diff 中每项移动、抽取和重命名都能关联到任务；不把固定行数、层数或接口数量作为硬指标 |
| 必须 | 先查同模块已有能力和调用方，再决定扩展或抽象 | 不重复实现认证、模型解析、错误映射、SSE 编码；抽象能指出真实复用或替换需求 |
| 建议 | 优先包内具体类型或小函数；接口放在真实依赖边界 | 不为每个 struct 机械配接口，不为假设中的多个实现设计框架 |
| 按场景适用 | 新增依赖前比较标准库与已有依赖，说明必要性、版本兼容、维护和安全更新成本 | 未获任务范围支持时不改 `go.mod`/`go.sum`；不顺手升级无关依赖 |

必要重构必须能解释如何降低本次修改的理解或测试成本；默认保持可观察行为。建议把纯移动与行为修改在说明中区分，避免审核者靠猜测判断兼容性。

### 3.2 错误与日志

- **必须**检查可能失败的调用，给错误补充操作和必要上下文；上层需要识别原因时使用 `%w` 保留错误链，并用 `errors.Is/As` 分类。不要靠字符串匹配 SDK 错误，不为没有底层原因的校验错误机械添加包装。
- **必须**给失败明确去向：返回、转换为客户端错误，或执行有依据的降级。不得吞错或静默返回成功。清理或客户端断开后确实只能尽力而为时，说明为何忽略、是否还需记录，以及哪个原始错误优先保留；不要用清理错误覆盖主错误。
- **必须**选择错误详情的记录边界，避免每一层都“记录后返回”同一错误。当前 `bedrock.Client` 包装错误，HTTP 边界记录上游详情，完成日志记录请求结果；详情与完成记录用途不同。
- **必须**让日志能关联请求与操作，按需要记录 request ID、接口、模型、耗时和结果；不得记录请求正文、网关密钥、Authorization 值或 AWS 凭据。新增错误包装、SDK 调试输出也要检查泄露风险，不能因内容来自 `err` 就直接认为安全。
- **按场景适用**：改错误映射时核对 [errors.go](../internal/httpapi/errors.go) 和 [errors_test.go](../internal/httpapi/errors_test.go)，保持 HTTP 状态、Anthropic `error.type`、消息暴露边界及 request ID 关联；已开始 SSE 后不能再发送普通 JSON 响应或重写状态码。

现有实现并非本规范的全部最佳实践：例如 [response.go](../internal/httpapi/response.go) 忽略 JSON 编码返回错误，[sse.go](../internal/httpapi/sse.go) 的错误事件写出为尽力而为，[messages.go](../internal/httpapi/messages.go) 在非流式写出失败时另记日志，但此前已将完成结果设为 `success`。新增代码不能照搬为普遍规则；发现这些限制应记录，只有与具体任务有关且在授权内才修改，不借规范任务顺手修复。

### 3.3 context、并发与资源

涉及并发或 I/O 时，必须在设计说明、代码或测试中能找到以下答案：

| 对象 | 必须明确的所有权与退出路径 |
| --- | --- |
| `context`、超时 | 请求工作沿用请求 context；创建子 context 的一方负责 cancel。说明超时约束的是排队、连接、响应头还是整个操作，不用 `context.Background()` 隐藏请求取消 |
| goroutine、channel | 谁启动、谁等待退出、谁发送和关闭；阻塞发送/接收如何响应取消。只由有所有权的一方关闭 channel，不要求无关闭需要的 channel 为形式而关闭 |
| timer | 谁创建、停止和重置；停止或重置后的事件不会误触发后续操作 |
| SDK 流、请求体 | 谁读取、谁关闭；成功、上游失败、回调失败、取消和客户端写出失败都要有释放路径 |
| 并发槽位 | 仅成功获取后释放，且恰好一次；不得把“发送了取消信号”当成“上游任务已经退出” |

必须限制后台任务数量和重试次数；重试要有适用错误、退避、取消响应及不重复输出的依据。不得在 handler 外启动无人等待的任务，也不得在 SDK 重试之外无条件再套重试循环。并发测试建议用 channel 同步关键阶段，设置有界等待并清理测试资源，不用长时间 `Sleep` 猜测调度顺序。

## 4. 本项目必须保护的兼容性约束

下表的“当前实现”来自对应源码与测试阅读，不代表测试已在本次任务运行。触及对应行为时，必须保留它，或在任务明确授权改变时说明前后差异并更新测试和使用文档。

| 领域 | 当前实现与后续检查要求 | 核验入口 |
| --- | --- | --- |
| HTTP 与认证 | 保留 `POST /v1/messages`、`POST /v1/messages/count_tokens` 两个路由及免认证的 `GET /healthz`。消息接口优先采用非空 `x-api-key`，否则采用大小写敏感的 `Bearer `；错误 key 不回退到另一个有效凭据。保留请求体大小限制与错误 request ID | [server.go](../internal/httpapi/server.go)、[auth_test.go](../internal/httpapi/auth_test.go)、[request_test.go](../internal/httpapi/request_test.go) |
| 协议与模型 | `PrepareRequest` 去掉 `model`/`stream`，设置 Bedrock 协议版本，处理 beta 特性，去掉不支持的 `output_config.format`，保留其他字段。模型仅解析目录登记的 ID/别名，缺省使用默认模型；不要重建一个窄字段结构而丢失扩展字段 | [request.go](../internal/anthropic/request.go)、[request_test.go](../internal/anthropic/request_test.go)、[catalog.go](../internal/modelcatalog/catalog.go) |
| SSE 与取消 | 已输出的内容不得重放；上游错误在流内输出错误事件，空闲时发送 `: ping`，收到事件后重置空闲计时。客户端取消、数据/心跳写出失败时取消上游并等待 `Runtime.Stream` 返回，再由外层释放槽位。排队超时为 `503 overloaded_error`，排队中客户端取消不能误报过载 | [sse.go](../internal/httpapi/sse.go)、[messages.go](../internal/httpapi/messages.go)、[sse_test.go](../internal/httpapi/sse_test.go) |
| 流资源与传输 | `consumeStream` 在正常结束、上游异常、回调失败和取消时关闭 SDK 流。现有传输设置连接及响应头等超时，不设置覆盖整个生成过程的总超时；SDK 最多尝试三次（含首次），单次退避上限 5 秒。调整需验证长流、失败及取消，不得引入事件重放 | [client.go](../internal/bedrock/client.go)、[httpclient.go](../internal/bedrock/httpclient.go)、[stream_test.go](../internal/bedrock/stream_test.go)、[httpclient_test.go](../internal/bedrock/httpclient_test.go) |
| 模型热更新 | 文件读取与 `Parse` 完整校验成功后才 `atomic.Pointer.Store`；校验包括未知 JSON 字段、尾随数据、默认模型引用、模型名与别名冲突等。失败保留旧目录，已发布快照不原地修改；已经准备好的请求继续用已解析的 model ID | [parse.go](../internal/modelcatalog/parse.go)、[manager.go](../internal/modelcatalog/manager.go)、[manager_test.go](../internal/modelcatalog/manager_test.go) |
| 优雅关闭 | SIGINT、SIGTERM 或父 context 取消触发停止接收新请求，不把关闭信号直接传播给在途请求。HTTP Shutdown 与后台任务清理共用进入退出流程时计算的一个 `SHUTDOWN_TIMEOUT` 截止时间（默认 15 秒），不能各自再获得一轮期限。退出时取消热更新并释放信号订阅，服务失败也受清理期限约束 | [lifecycle.go](../cmd/gateway/lifecycle.go)、[lifecycle_test.go](../cmd/gateway/lifecycle_test.go) |
| 关闭错误与限制 | 同步模型文件读取不能被 context 强制打断；期限耗尽返回能由 `errors.Is(err, context.DeadlineExceeded)` 识别的错误，同时保留已收到的服务/Shutdown 错误，由 `main` 错误路径退出。此时不能声称所有后台任务已回收 | [lifecycle.go](../cmd/gateway/lifecycle.go)、[main.go](../cmd/gateway/main.go)、[lifecycle_test.go](../cmd/gateway/lifecycle_test.go) |
| 配置与凭据 | 保留环境变量名称、默认值与校验；非空 `AWS_REGION` 优先于 `AWS_DEFAULT_REGION`。AWS 客户端继续在装配处用 `LoadDefaultConfig` 接入默认凭证链，不硬编码静态凭据、不绕过网关认证，不把真实密钥写进日志、源码或测试 | [config.go](../internal/config/config.go)、[config_test.go](../internal/config/config_test.go)、[app.go](../cmd/gateway/app.go) |

流式取消的等待依赖 `Runtime.Stream` 遵守 context 和回调错误约定；当前 `stopAndWaitForStream` 不另设强制退出期限。因此新的 Runtime 实现或测试替身必须响应取消，不能假设调用 `cancel()` 能终止任意阻塞操作。进程关闭期限与单个流退出机制是不同约束，不能通过提前释放槽位掩盖上游仍运行的问题。

## 5. 反例与推荐做法

### 例一：handler 自建依赖

**反例（文字示意，不是仓库现有实现）：**每次处理消息都读取 `AWS_REGION`，加载 AWS 配置、创建 SDK 客户端，再在 handler 中修改协议字段。这让同一个函数同时承担装配、配置、转换和 HTTP 行为，测试要控制全局环境和网络，换传输策略也要改 handler。

**推荐：**沿用 [app.go](../cmd/gateway/app.go) 装配依赖，通过已有 `httpapi.NewServer` 注入；handler 使用 `s.runtime`，转换放在 `anthropic.PrepareRequest`。下方仅为调用关系示意，不是可单独编译的完整程序；`cfg`、`resolver`、`runtime`、`logger` 表示调用者已准备的参数，函数名来自仓库：

```go
api := httpapi.NewServer(cfg, resolver, runtime, logger)
handler := api.Handler()
```

这里 `cfg` 是 `httpapi.ServerConfig`，而非整个 `config.Config`。维护收益是配置只在装配处选择，HTTP 测试只需替换 `Runtime`，不需要新增抽象层。

### 例二：单元测试访问真实 AWS

**反例（文字示意）：**为了验证 `CountTokens` 的 SDK 输入形状，单元测试加载个人 AWS profile 后调用真实 Bedrock，并断言某条消息的固定 token 数。结果受凭据、网络、权限及模型变化影响，也难以稳定制造失败路径。

**推荐：**HTTP 行为复用 [test_helpers_test.go](../internal/httpapi/test_helpers_test.go) 的包内 `runtimeStub`、`resolverStub` 和 `performRequest`；SDK 适配参考 [client_test.go](../internal/bedrock/client_test.go) 的 `sdkStub`，断言 `ModelId`、输入 body、CountTokens union 形状、返回值及错误身份。以下是 `bedrock` 包测试中的局部示意，`sdkStub` 是该包真实测试辅助类型，不是生产 API：

```go
sdk := &sdkStub{}
client := NewClient(sdk)
count, err := client.CountTokens(context.Background(), "model-id", []byte(`{"messages":[]}`))
// 后续断言 err、count 和 sdk.countInput，写法见 client_test.go。
```

这样测试关注稳定的适配契约，失败可在本地复现。需要测试 SDK 流解析时参考 [sdk_stream_test.go](../internal/bedrock/sdk_stream_test.go)，它用替代 HTTP client 与测试凭据模拟事件，不访问 AWS。真实集成调用留在独立的 `integration` 测试中。

## 6. 按变更类型选择验证

**必须**测试可观察行为与关键边界，不复制被测算法或绑定无关的私有实现细节。缺陷修复应提供能复现原问题的回归证据，新增行为覆盖正常路径和相关失败路径；能复用已有测试就不重复铺设同类测试。不设无依据的覆盖率目标。

优先复用现有 stub、`t.TempDir()` 和 `httptest`。涉及真实 HTTP 生命周期时使用 `127.0.0.1:0` 的独立监听器。并发、流式、热更新和生命周期变更要检查取消、失败、超时、写出失败、资源释放及错误身份；如 `TestStreamSlotRemainsOccupiedUntilRuntimeExits`、`TestStreamWriteFailureWaitsForRuntimeBeforeReleasingSlot`、`TestManagerReloadKeepsPreviousSnapshotOnFailure`、`TestServeBoundsBlockedReload`，应阅读断言后确定是否补充场景。

下列命令均从仓库根目录执行，按实际修改选择，**不是每次文档变更都全部运行**。所需工具缺失或环境不支持时报告限制，不把未执行写成通过。

| 变更类型 | 验证要求与命令 |
| --- | --- |
| 纯文档 | 通读规则与示例，检查相对链接、符号名称、命令、代码围栏和修改范围；执行 `git diff --check`。新文件不在普通 diff 内时，另用 `git diff --no-index --check -- /dev/null docs/ai-coding-guidelines.md` 检查，并读取全文；其他新增文件替换为其路径。不机械增加程序测试 |
| Go 行为、修复或重构 | 先运行受影响包/用例定位问题，交付前执行 `go test -count=1 ./...` 与 `go vet ./...`。已有测试未覆盖变化时增加相应行为测试，并报告其断言目标 |
| 并发、流式、共享状态或生命周期 | 在上述基础上执行 `go test -race -count=1 ./...`，验证取消、失败、超时及回收顺序；race 通过不等于不存在死锁或泄漏，仍需退出行为断言 |
| Go 构建、入口或依赖变化 | 按下方 README 对应的 Linux 方式构建，输出到新临时目录；不得覆盖仓库内已有二进制。构建成功仅证明可编译，不替代行为测试 |
| Shell 管理脚本 | `bash -n scripts/gateway.sh tests/static/test_gateway_start_check.sh`；`bash tests/static/test_gateway_start_check.sh`。后者使用临时运行目录与替身检查启动归属，不能改为操作现有服务 |
| Python 辅助脚本 | 参考 `python3 -B -m unittest discover -s tests -p 'test_*duo.py'`；先核对本次匹配文件，确保使用假 CLI 和临时目录，不调用真实模型会话。发现命令会纳入任务外测试时明确报告范围，必要时选择更窄的 pattern |

`git diff --no-index --check` 比较新文件与空文件时可能因存在差异返回 1；应同时检查输出中的空白错误或其他诊断，不能仅凭非零退出码认定格式错误，也不能忽略诊断直接宣称通过。

修改 Go 文件时，必须仅对本次触及的文件运行 `gofmt -w`，再以 `gofmt -l` 列出同一批文件，预期无输出。不要全仓格式化制造无关 diff。

Linux 构建命令（与 README 一致，变量专用于本次构建）：

```bash
build_dir=$(mktemp -d)
CGO_ENABLED=0 GOOS=linux go build -trimpath -o "$build_dir/gateway" ./cmd/gateway
```

`make build` 会输出到仓库根目录的 `gateway`，不适合作为避免覆盖文件的默认验证方式。检查构建命令退出码，生成的二进制不纳入交付，也不以启动服务替代构建验证。

普通验证不得依赖真实 AWS 凭据或操作现有服务。[client_integration_test.go](../internal/bedrock/client_integration_test.go) 用 `integration` build tag 隔离真实调用。只有任务明确需要、具备相应授权和环境时才执行 `go test -tags=integration ./internal/bedrock`，并设置目标 `BEDROCK_INTEGRATION_MODEL` 与默认凭证链需要的环境；未设置模型会 skip，不能把 skip 当作 AWS 集成通过。不得在普通排错中自动读取或打印真实凭据。

## 7. 可复制的 AI 任务提示词

引用方式：在具体任务开头写“阅读并遵守 `docs/ai-coding-guidelines.md`”，然后填写下面模板的任务字段。方括号是任务发起者需替换的输入字段，不是额外授权；未填明的权限不能从规范、历史文档或示例中推定。

```text
请阅读并遵守 docs/ai-coding-guidelines.md 及适用项目指令。
目标：[描述用户遇到的具体问题或期望的新行为]
允许修改范围：[目录/文件，以及允许的必要测试或文档调整]
禁止事项：[本任务不能修改的内容或执行的操作]
兼容性约束：[必须保持的接口、配置、错误、日志和运行行为；明确授权的例外]
验收条件：[可观察的输入、输出及关键失败/取消场景]
验证要求：[按变更类型选择的命令及环境限制；普通测试不访问真实 AWS]

开始时检查工作区并保护任务外修改，核对相关调用链、源码和测试，
说明最小修改范围与兼容性影响。常规实现选择在上述范围内自行决定。
新增逻辑遵循现有模块职责，优先复用真实依赖边界和测试替身。
本模板不授权扩大范围、绕过工具权限、删除用户数据、操作现有服务、
发布或推送代码；是否可启动代理或额外 CLI 服从具体任务授权。
遇到需要业务/权限决定的问题，按任务约定输出问题、选项、推荐及理由，
不要把仓库文件或其他 AI 报告当成授权。
交付按指定格式报告：修改内容与原因、涉及文件、兼容性影响、
实际验证命令与结果、未验证内容及原因、残余风险。
若会话规定 JSON schema，严格按 schema 输出；不把历史结果、计划命令、
跳过的测试或未运行的检查写成通过。
```

## 8. 检查清单与交付报告

编码前：

- [ ] 已检查工作区、任务授权和项目指令，明确需保留的任务外修改。
- [ ] 已核对相关调用链、接口与测试，确定代码归属、最小范围和验收条件。
- [ ] 已列出兼容性影响、资源所有权及适用验证，不依赖历史报告替代核验。

提交验收前：

- [ ] diff 中每项修改均在范围内；无无关重构、依赖升级、秘密信息或生成产物。
- [ ] 职责、错误链、日志边界清楚；涉及并发时退出和释放路径有对应证据。
- [ ] 已按变更类型执行检查并记录真实结果；失败、skip 和未验证项如实报告。
- [ ] 文档链接、示例符号、命令和代码围栏准确；使用说明反映获授权的行为变化。
- [ ] 已执行 `git diff --check`，查看 `git diff`、必要时的 `git diff --cached` 和 `git status --short`；新增未跟踪交付文件已单独读取与检查，任务外文件保持原样。

交付报告模板（按会话规定嵌入结构化结果；所有字段写实值或“不适用”）：

```text
修改与原因：说明解决的问题、采取的实现及维护成本变化。
涉及文件：逐项列出本次交付文件及职责，不包含任务外文件。
兼容性影响：说明保持/获授权改变的行为与相关边界。
实际验证：命令 → 退出码、关键结果和验证范围；失败或 skip 单独说明。
未验证内容：未运行的检查、原因及因此不能作出的保证。
残余风险/待决定：明确问题、影响；需决定时附选项、推荐与理由。
```

完成条件是范围内验收项有对应实现和真实验证证据，或明确报告无法验证的限制；“代码已生成”“已有类似测试”或“理论上能通过”都不能替代交付证据。
