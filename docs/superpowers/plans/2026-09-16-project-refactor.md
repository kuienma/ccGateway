# 全项目渐进重构执行清单

**Goal:** 保持现有协议、运行方式及 Go 1.24/依赖版本，明确职责并补足高风险行为测试。
**Architecture:** 留在现有 cmd/internal 边界内；启动装配、生命周期和目录存储按职责拆分，HTTP 基础参数使用具名结构，外部运行依赖显式注入。不引入通用框架。
**Spec:** 本轮负责人执行指令；现行行为参考 README 与 docs/design，历史计划仅供比对。
**执行方式:** 当前 worktree 内单会话顺序执行，用户已授权常规选择；不启动代理、不部署、不调用真实 AWS、不推送。

基线：`refactor/httpapi-responsibilities`，`d30a128a4bd45d976b30b7629bb841d73d2484a8`，工作区干净；未安装 code-review-graph。基线 `go test -count=1 ./...` 退出码 0，cmd 无测试，五个 internal 包通过。

- [x] 1. **启动与生命周期**（cmd/gateway/main.go、app.go、lifecycle.go 及测试）：main 当前同时装配和运行，reloadModels 无停止路径。拆出装配函数；服务运行拥有信号订阅和后台任务，退出时取消并等待热更新，Shutdown 使用独立超时 context，不绑定请求 context。先复现 reload 不退出，再补启动失败、父取消、正常退出、超时和在途请求测试；运行 `go test -count=1 ./cmd/gateway`。
- [x] 2. **配置边界**（internal/config）：重复正整数和 duration 解析提取类型明确的助手，保留各变量顺序和 int/int64 溢出区别。增加空白、默认、优先级、零/负数/溢出测试后重构；运行 `go test -count=1 ./internal/config`。
- [x] 3. **模型目录**（internal/modelcatalog）：将纯 JSON 校验、Resolve、文件读取/atomic 管理分别放入 parse.go、catalog.go、manager.go；保持完整校验后 Store。补并发解析、成功更新、读取失败和解析失败旧快照测试；运行 `go test -race -count=1 ./internal/modelcatalog`。
- [x] 4. **HTTP 入口和测试职责**（internal/httpapi、cmd/gateway）：NewServer 改收 ServerConfig + 显式 resolver/runtime/logger；迁移全部调用；server_test 分为路由、认证、请求、消息、Token、SSE、错误和辅助代码，保持原断言。补认证优先级、拒绝不调用上游、SSE 顺序/Flush/错误关联；运行 `go test -race -count=1 ./internal/httpapi`。
- [x] 5. **协议及 SDK 边界**（internal/anthropic、internal/bedrock）：核验字段透传与过滤、冻结 Client/代理/重试；通过 SDK 提供的流 reader 注入补正常/异常/取消/回调失败 Close 测试，仅在测试证明缺陷时最小修改。运行 `go test -race -count=1 ./internal/anthropic ./internal/bedrock`。
- [x] 6. **全量交付**（README、docs、scripts、tests、Makefile、Dockerfile）：核验历史计划与实现差异，修订当前说明；运行全量 test/race/vet、Shell 语法与隔离测试、Go 格式、diff 检查及临时目录 Linux 构建。Docker 未修改则不要求构建；历史真实 AWS/重启步骤不执行。报告逐项退出码、文件清单、兼容性和风险。

关键选择：保持小函数和现有包，避免新应用框架；保持目录动态登记，不恢复旧白名单；不改变已落地 HTTP 请求准备、Token、request ID、槽位与完成日志语义。

六项已实现并验证，最终证据见 `../../reports/2026-09-16-project-refactor.md`。
