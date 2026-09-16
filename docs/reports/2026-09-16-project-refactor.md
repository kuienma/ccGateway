# 全项目重构验收报告（2026-09-16）

状态：ready_for_review。已按负责人审核决定修复生命周期退出回归，提交再次审查；不等同于负责人最终验收。

## 生命周期审核修复（本轮最新证据）

执行目录：`/Users/sh/zhshl/gateway/.worktrees/refactor-httpapi`；分支与 HEAD 仍为 `refactor/httpapi-responsibilities` / `d30a128a4bd45d976b30b7629bb841d73d2484a8`。本轮开工保留首轮全部工作区改动。没有修改其他生产模块、新增配置、调用真实 AWS 或操作现有服务。

本轮仅修改五个文件：`cmd/gateway/lifecycle.go`、`cmd/gateway/lifecycle_test.go`、`README.md`、`docs/design/2026-09-10-bedrock-anthropic-gateway.md`、本报告。

**原因与修复：** 首轮在 defer 中无条件等待 reloadDone，同步 os.ReadFile 阻塞时可无限延长退出。现在任何退出分支都只建立一次 `context.WithTimeout(context.Background(), timeout)`，先取消热更新，HTTP Shutdown 与后台任务等待共用该截止时间。正常结束在期限内回收；期限耗尽返回 `errors.Join(result, cleanupCtx.Err())`，保留服务／Shutdown 原始错误，并能通过 `errors.Is(err, context.DeadlineExceeded)` 判断超时。没有增加第二轮 timeout，也没有强制取消全部在途 HTTP 请求。无法中断的同步文件调用允许仍未退出，不能声称超时路径已回收全部任务；main 将按原错误退出路径终止进程，信号订阅在返回时释放。

**失败／通过证据：** 全部命令显式使用 `GOTOOLCHAIN=go1.24.0`，实际版本输出为 `go version go1.24.0 darwin/arm64`。

- 修复前新增 `TestServeBoundsBlockedReload`，通过 channel 确认 reload 已进入后才触发关闭；测试 cleanup 释放 stub。`go test -count=1 ./cmd/gateway -run TestServeBoundsBlockedReload -v` 退出码 **1**。HTTP 正常关闭、Shutdown 超时、服务失败、Shutdown 自身失败四个子场景全部失败，日志均为“关闭期限耗尽后仍等待阻塞 reload，或清理重新获得一轮 timeout”。完整日志：[red.log](/tmp/gateway-lifecycle-review-lv4n4goz/red.log)，命令/目录/退出码：[red.json](/tmp/gateway-lifecycle-review-lv4n4goz/red.json)。
- 修复后目标包退出码 **0**，四个场景各约 **0.25s** 返回。Shutdown 超时场景在 `ctx.Done()` 后仅给 100ms 调度余量，防止再追加一轮 250ms 的清理超时。服务失败与 Shutdown 失败场景同时检查 `errors.Is` 原始错误和 DeadlineExceeded。
- 新增 `TestServePreservesShutdownAndServerErrors`，验证关闭过程中返回的服务错误和 Shutdown 错误同时保留；现有 `TestServeWaitsForReloadAndShutdown`、`TestRunServerGracefullyDrainsRequest` 等正常回收和在途请求完成测试保留并通过。

下表均为本轮修复后实际执行的新证据。完整命令、执行目录、环境、输出及退出码：[results.json](/tmp/gateway-lifecycle-review-lv4n4goz/results.json)；复现脚本：[verify.py](/tmp/gateway-lifecycle-review-lv4n4goz/verify.py)。

| 命令（GOTOOLCHAIN=go1.24.0） | 退出码 | 日志 |
| --- | --- | --- |
| `go test -count=1 ./cmd/gateway -v` | 0 | [target.log](/tmp/gateway-lifecycle-review-lv4n4goz/target.log) |
| `go test -count=1 ./...` | 0 | [test.log](/tmp/gateway-lifecycle-review-lv4n4goz/test.log) |
| `go test -race -count=1 ./...` | 0 | [race.log](/tmp/gateway-lifecycle-review-lv4n4goz/race.log) |
| `go vet ./...` | 0 | [vet.log](/tmp/gateway-lifecycle-review-lv4n4goz/vet.log) |
| `CGO_ENABLED=0 GOOS=linux go build -trimpath -o /tmp/gateway-lifecycle-review-lv4n4goz/gateway-linux ./cmd/gateway` | 0 | [linux-build.log](/tmp/gateway-lifecycle-review-lv4n4goz/linux-build.log) |
| `git diff --check` | 0 | [diff-check.log](/tmp/gateway-lifecycle-review-lv4n4goz/diff-check.log) |
| `gofmt -l cmd/gateway/lifecycle.go cmd/gateway/lifecycle_test.go` | 0 | [gofmt.log](/tmp/gateway-lifecycle-review-lv4n4goz/gofmt.log) |

全量 test/race 覆盖 cmd/gateway 及全部五个 internal 包，无失败或 race 报告；vet 与 diff/gofmt 检查无输出。Linux 二进制仅写入本轮独立临时目录。Shell 与 Docker 构建本轮未重复运行（未修改其入口或策略，首轮证据单独归档）；未运行真实 AWS、部署、服务重启或容器。

## 基线与最终代码位置

- 工作目录：`/Users/sh/zhshl/gateway/.worktrees/refactor-httpapi`。
- 分支：`refactor/httpapi-responsibilities`。
- 开工和最终 HEAD 均为 `d30a128a4bd45d976b30b7629bb841d73d2484a8`；本轮改动保留在工作区（含下列未跟踪新文件），未提交、合并、推送或部署。此前 HTTP 职责拆分成果保留。
- 开工工作区干净；基线 `go test -count=1 ./...` 退出码 0，五个 internal 包通过，cmd/gateway 当时无测试。初期工具链为本机 Go 1.26.2；最终验收显式使用 `GOTOOLCHAIN=go1.24.0`，go.mod/go.sum 未改动。
- 未安装 code-review-graph，使用文件检索；核对 README、两份设计、历史 plans/superpowers plans、全部生产包、脚本、静态测试和构建入口。历史未勾选任务没有被当作本轮授权。

## 各模块结论与技术决定

| 范围 | 改造或核验结论 | 验收依据 |
| --- | --- | --- |
| cmd/gateway | main 薄入口；app 装配配置、目录、AWS 和 HTTP；lifecycle 持有信号订阅、服务与热更新任务。新增最小 httpService 生命周期接口及 reload 函数注入，无新内部应用框架 | 配置/目录失败、服务失败/ErrServerClosed、取消、关闭超时、等待 reload、本地在途请求正常完成 |
| internal/config | 正整数和 duration 辅助解析；按原顺序校验；bitSize 保留 MAX_CONCURRENT_STREAMS 的 int 与 MAX_BODY_BYTES 的 int64 边界 | 空白/默认/正负/零/溢出、区域优先级及错误信息测试 |
| internal/modelcatalog | parse.go 纯解析校验；catalog.go 只读名称解析；manager.go 文件加载与原子 Store；成功前完整校验，失败不替换 | 任意动态模型、冲突/尾随 JSON、成功和失败热更新、文件缺失、8 个并发 reader 与旧快照测试 |
| internal/httpapi | NewServer 接收 ServerConfig，resolver/runtime/logger 保持显式；20 个原 server_test 测试全部迁移保留，当前 HTTP 共 25 个顶层测试；沿用原请求准备、Token、ID、槽位和完成日志实现 | 全量/race；补认证优先级、拒绝不访问上游、SSE 顺序/Flush/错误关联、写失败等待上游退出 |
| internal/anthropic | 核验无需生产改造；当前转换职责单一，无有意义重复可抽取 | 现有字段透传、model/stream 删除、版本/beta 与 output_config.format 测试通过 |
| internal/bedrock | 提取 consumeStream 管理 SDK 流资源，并监听取消；其他 SDK 请求形态、CountTokens、错误包装不变；httpclient.go 核验无需改造 | reader 级及真实 SDK + 假 HTTP Client 的正常/异常/取消/回调失败关闭测试、次数一次及 SDK 错误身份测试 |
| scripts、tests/static | 启动脚本已有 PID/端口归属校验，当前装配重构无需改变命令、路径或运行方式 | bash 语法与隔离假进程测试通过；未调用真实 gateway.sh start/stop/restart |
| Makefile、Dockerfile、.dockerignore | 保持 Go 1.24、静态构建、CA 证书、模型文件路径、非 root 65532:65532 和 /gateway 入口，无需修改 | Linux 静态构建及 Docker 构建通过；未运行容器 |
| README 与 docs | 同步模块职责、生命周期、可隔离验证；纠正“最多三次重试”为三次尝试；旧白名单标记为历史约束；完成日志字段以实际实现为准 | 文档与最终代码人工比对 |

## 缺陷修复（与结构调整区分）

1. **热更新任务未退出**：原 for-range 仅等信号，signal.Stop 不会关闭 channel。先仅给函数增加未使用的 context 参数作为测试接缝，保留原循环；`go test -count=1 ./cmd/gateway -run TestReloadModelsStopsOnCancellation` 退出码 **1**，报“父 context 取消后热更新任务仍等待信号”。改为 select 监听 context/信号关闭，并由 serve 取消、等待任务回收；随后 `go test -count=1 ./cmd/gateway` 和该包 race 均 **0**。首轮对后台任务的等待没有截止时间，阻塞文件读取时仍会卡住退出；该回归已在下述本轮审核修复中消除。信号订阅在所有返回路径 defer 清理。
2. **空闲流取消依赖 SDK reader 自行退出**：先只把原消费循环抽成 consumeStream，保留行为；`go test -count=1 ./internal/bedrock -run TestConsumeStream` 退出码 **1**，报“context 取消后仍等待 SDK reader，流未关闭”。最小修复为 select 监听取消，统一 defer Close，事件结束继续传播 reader 错误；`go test -race -count=1 ./internal/bedrock -run TestConsumeStream` 退出码 **0**。随后真实 SDK 反序列化、假 HTTP 响应与阻塞 io.Pipe 的取消测试也通过。

开发过程中的额外失败均已解决：HTTP 测试迁移一次出现 import 清理错误；SDK 测试初稿使用 AnonymousCredentials 时命中 SDK Bearer adapter nil provider，改为显式测试假凭据；流式 Accept 实际映射到 X-Amzn-Bedrock-Accept，修正测试断言。上述命令退出码均为 1，修正后相关包 race 为 0，未修改生产认证策略或依赖版本。

## 兼容性结论

- 三条路由、x-api-key 优先于 Bearer、状态与响应结构、请求 ID 对应关系不变。未知模型、非法输入和超大 body 在上游前拒绝。
- 动态目录继续生效，不修改 configs/models.json；完整校验后替换，旧快照不变。所有环境变量、默认值、空白处理及 AWS_REGION 优先级保留。
- Anthropic 字段透传与过滤、Bedrock CountTokens、SSE 单 handler 写入、事件顺序、Flush、空闲心跳、排队超时、取消和上游退出后释放槽位保留；无流重放。
- 保留 AWS 默认凭据链、代理/冻结 HTTP Client、连接与响应头超时、最多三次调用尝试、5 秒最大退避；没有新增流总超时。
- 完成日志字段、结果分类和记录次数不变；生产日志实现没有新增正文/凭据记录。HTTP Shutdown 与后台任务清理使用独立于请求的同一个超时 context，沿用原有 timeout 值，未把父取消绑定到请求 context。

## 首轮执行证据（归档，不代替本轮修复验证）

下表 Go 命令环境为 `GOTOOLCHAIN=go1.24.0`。完整输出与机器可读结果保存在 `/tmp/gateway-refactor-verify-hkm7044u/`（01.log 至 06.log、results.json、verify.py）。

| 命令 | 退出码 | 结果 |
| --- | --- | --- |
| `go test -count=1 ./...` | 0 | cmd/gateway 与全部五个 internal 包通过 |
| `go test -race -count=1 ./...` | 0 | cmd/gateway 与全部五个 internal 包通过 |
| `go vet ./...` | 0 | 通过 |
| `bash -n scripts/gateway.sh tests/static/test_gateway_start_check.sh` | 0 | 通过 |
| `bash tests/static/test_gateway_start_check.sh` | 0 | 隔离假进程测试通过；预期的启动失败日志属于断言场景 |
| `git diff --check` | 0 | 通过 |
| `gofmt -l <all Go files>` | 0 | 无输出，所有 Go 文件已格式化 |
| `CGO_ENABLED=0 GOOS=linux go build -trimpath -o /tmp/gateway-refactor-verify-hkm7044u/gateway-linux ./cmd/gateway` | 0 | 通过 |
| `docker info --format '{{.ServerVersion}}'` | 0 | daemon 29.4.0 可用 |
| `docker build --tag gateway-refactor-check:20260916 .` | 0 | Go 1.24-alpine 构建通过，本地镜像 `sha256:e835decb61f1089136b1cd8f34e453a28bf139e5ed11e39a376da45d913ce868`，无发布/运行 |
| `file /tmp/gateway-refactor-verify-hkm7044u/gateway-linux` | 0 | ELF Linux aarch64，statically linked |
| `go version -m /tmp/gateway-refactor-verify-hkm7044u/gateway-linux` | 0 | Go 1.24.0，CGO_ENABLED=0，GOOS=linux，GOARCH=arm64 |

分阶段命令均实际执行：`go test -count=1 ./internal/config`（重构前/后均 0）；`go test -race -count=1 ./internal/modelcatalog`（拆分前/后均 0）；`go test -race -count=1 ./cmd/gateway ./internal/httpapi` 为 0；`go test -race -count=1 ./internal/anthropic ./internal/bedrock ./internal/httpapi` 为 0。本轮未运行 integration tag、真实 AWS 请求、部署或服务重启；这些是范围排除，不是测试失败或凭据阻断。

## 剩余限制与审核重点

- 没有已知阻断项，也没有需要业务拍板的新增接口或配置。
- 退出截止时间耗尽即返回可由 errors.Is 识别的 DeadlineExceeded，并保留已收到的原始服务／Shutdown 错误，由 main 按既有错误路径退出；没有新增强制取消全部在途请求的策略。context 不会中断同步 os.ReadFile，因此超时返回时可能仍有热更新任务尚未回收，但不会继续无限等待。
- 本地测试无法证明真实 AWS 服务/代理可达性；真实凭据与部署验证明确未运行。Docker 仅构建，未做运行中的容器验证。
- 嵌套 worktree 的 Go 1.24 构建 VCS 元数据指向外层仓库 HEAD `6fa8100b915631323c8a73c56eeed05193162169`。已核实 go env GOMOD、go list 的源码目录均为本 worktree，包含 app.go/lifecycle.go/main.go；本 worktree HEAD 仍为 d30a128。审查来源以本报告目录和工作区 diff 为准，不能以该二进制 VCS 字段代替代码位置。
- 最终由技术负责人检查实际工作区（包括未跟踪新文件）与证据后验收。

## 修改文件清单

- `README.md`
- `cmd/gateway/app.go`
- `cmd/gateway/app_test.go`
- `cmd/gateway/lifecycle.go`
- `cmd/gateway/lifecycle_test.go`
- `cmd/gateway/main.go`
- `docs/design/2026-09-10-bedrock-anthropic-gateway.md`
- `docs/plans/2026-09-10-bedrock-anthropic-gateway.md`
- `docs/reports/2026-09-16-project-refactor.md`
- `docs/superpowers/plans/2026-09-15-local-gateway-stability.md`
- `docs/superpowers/plans/2026-09-16-project-refactor.md`
- `internal/bedrock/client.go`
- `internal/bedrock/client_test.go`
- `internal/bedrock/sdk_stream_test.go`
- `internal/bedrock/stream_test.go`
- `internal/config/config.go`
- `internal/config/config_test.go`
- `internal/httpapi/auth_test.go`
- `internal/httpapi/completion_test.go`
- `internal/httpapi/count_tokens_test.go`
- `internal/httpapi/errors_test.go`
- `internal/httpapi/messages_test.go`
- `internal/httpapi/request_test.go`
- `internal/httpapi/server.go`
- `internal/httpapi/server_test.go`
- `internal/httpapi/sse_test.go`
- `internal/httpapi/test_helpers_test.go`
- `internal/modelcatalog/catalog.go`
- `internal/modelcatalog/catalog_test.go`
- `internal/modelcatalog/manager.go`
- `internal/modelcatalog/manager_test.go`
- `internal/modelcatalog/parse.go`
