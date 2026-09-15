# Local Gateway Stability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 提升必须经过本机 Mihomo 代理访问 Bedrock 时的请求成功率、超时可控性和运行可观测性。

**Architecture:** 请求转换层先消除已知 Bedrock 不兼容字段；AWS SDK 使用显式 Transport 和有界重试；HTTP 层以单写协程提供 SSE 心跳并限制流并发。启动脚本加强 PID/端口归属校验，防止旧进程健康响应造成误判。

**Tech Stack:** Go 1.24、AWS SDK for Go v2、net/http、Bash、macOS launchd/Mihomo

**Spec:** `docs/design/2026-09-15-local-gateway-stability.md`

## Global Constraints

- Bedrock 请求必须经过本机代理，禁止加入 `NO_PROXY`。
- 已开始输出的流式请求不得自动重放。
- 不给流式调用设置覆盖完整响应生命周期的 HTTP Client 总超时。
- 所有生产代码改动遵循测试先行并观察 RED→GREEN。

---

### Task 1: Bedrock 请求兼容

**Files:**
- Modify: `internal/anthropic/request.go`
- Test: `internal/anthropic/request_test.go`

**Interfaces:**
- Consumes: Anthropic Messages JSON
- Produces: `PrepareRequest(raw []byte, betaFeatures []string, resolver ModelResolver) (PreparedRequest, *RequestError)`，输出不包含 `output_config.format`

- [ ] 添加测试：输入 `{"output_config":{"format":{...},"effort":"high"}}`，断言仅删除 `format` 并保留 `effort`。
- [ ] 运行 `go test ./internal/anthropic -run OutputConfig -count=1`，确认因 `format` 仍存在而失败。
- [ ] 在 `PrepareRequest` 编码前解析 `output_config` 对象、删除 `format`，对象为空时删除整个字段。
- [ ] 再次运行目标测试并确认通过。

### Task 2: 稳定性配置

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `StreamHeartbeatInterval time.Duration`、`MaxConcurrentStreams int`、`StreamQueueTimeout time.Duration`

- [ ] 添加默认值、显式值和非法值测试。
- [ ] 运行 `go test ./internal/config -count=1`，确认新字段测试失败。
- [ ] 解析 `STREAM_HEARTBEAT_INTERVAL`、`MAX_CONCURRENT_STREAMS`、`STREAM_QUEUE_TIMEOUT`，要求均为正值。
- [ ] 再次运行配置测试并确认通过。

### Task 3: AWS HTTP Transport 与有界重试

**Files:**
- Modify: `.gitignore`
- Create: `internal/bedrock/httpclient.go`
- Create: `internal/bedrock/httpclient_test.go`
- Create: `cmd/gateway/main.go`（修正原 `gateway` 忽略规则后纳入版本控制）

**Interfaces:**
- Produces: `NewHTTPClient() *awshttp.BuildableClient` 和 `NewRetryer() aws.Retryer`

- [ ] 添加行为测试：慢建连受 Dial timeout 限制；重试器 `MaxAttempts()==3`。
- [ ] 运行 `go test ./internal/bedrock -run 'HTTPClient|Retryer' -count=1`，确认缺少接口而失败。
- [ ] 将 `.gitignore` 的 `gateway` 改为 `/gateway`，把现有 `cmd/gateway/main.go` 纳入版本控制。
- [ ] 用 `awshttp.NewBuildableClient` 配置 Dial 10 秒、KeepAlive 30 秒、TLS 10 秒、ResponseHeader 120 秒、IdleConn 90 秒、MaxIdle 100、PerHost 20；用 `retry.AddWithMaxBackoffDelay` 限制为 5 秒。
- [ ] 在 `LoadDefaultConfig` 中注入 HTTP Client 和每次创建的新 Retryer。
- [ ] 运行 Bedrock 包测试并确认通过。

### Task 4: SSE 心跳与流式并发保护

**Files:**
- Modify: `internal/httpapi/server.go`
- Modify: `internal/httpapi/messages.go`
- Modify: `internal/httpapi/sse.go`
- Test: `internal/httpapi/server_test.go`

**Interfaces:**
- `NewServer` 新增 `streamHeartbeatInterval time.Duration`、`maxConcurrentStreams int`、`streamQueueTimeout time.Duration`
- SSE 心跳格式固定为 `: ping\n\n`

- [ ] 添加测试：阻塞 Runtime 在心跳周期后产生 `: ping`；并发槽占满后第二个请求在队列超时返回 503。
- [ ] 运行 `go test ./internal/httpapi -run 'Heartbeat|ConcurrentStreams' -count=1`，确认失败。
- [ ] 用事件 channel 将 Bedrock callback 数据交给 handler goroutine，所有 `ResponseWriter` 写入在该 goroutine串行执行。
- [ ] 在写 SSE 头之前获取流式信号量；超时返回 `overloaded_error`，请求结束释放槽位。
- [ ] 运行 HTTP API 测试并确认通过，再运行 `go test -race ./internal/httpapi`。

### Task 5: 请求耗时日志

**Files:**
- Modify: `internal/httpapi/messages.go`
- Modify: `internal/httpapi/sse.go`
- Test: `internal/httpapi/server_test.go`

**Interfaces:**
- Produces: 日志字段 `request_id`、`operation`、`model_id`、`stream`、`duration_ms`、`result`

- [ ] 使用内存 slog handler 添加成功、上游失败和客户端取消测试。
- [ ] 运行目标测试并确认缺少耗时日志而失败。
- [ ] 在 handler 边界记录一次完成日志，错误正文和请求正文不得进入日志。
- [ ] 运行 HTTP API 测试并确认通过。

### Task 6: 启动脚本进程归属校验

**Files:**
- Modify: `scripts/gateway.sh`
- Create: `tests/static/test_gateway_start_check.sh`

**Interfaces:**
- Produces: `owns_listen_port PID PORT`；healthz 成功仅在新 PID 存活且拥有监听端口时算启动成功

- [ ] 创建隔离 runtime、假 `curl`/`lsof`/gateway 命令的脚本测试，模拟 healthz 来自旧进程，断言启动返回非零。
- [ ] 运行 `sh tests/static/test_gateway_start_check.sh`，确认旧逻辑误报导致测试失败。
- [ ] 增加端口解析和 `lsof` 校验；无 `lsof` 时 healthz 后再次检查新 PID。
- [ ] 运行脚本测试及 `zsh -n scripts/gateway.sh` 并确认通过。

### Task 7: 完整验证与部署

**Files:**
- Modify: `README.md`
- Runtime: `.runtime/gateway`、`.runtime/gateway.pid`

**Interfaces:**
- Consumes: 前六项稳定性改动
- Produces: 运行中的本地网关与配置说明

- [ ] 更新 README，记录新增环境变量、代理必须项、SSE 心跳语义和推荐 Mihomo keepalive。
- [ ] 运行 `gofmt`、`go test ./...`、`go test -race ./...`、`go vet ./...`、`git diff --check`。
- [ ] 构建 `.runtime/gateway`，通过现有 launchd 服务重启并同步 PID 文件。
- [ ] 运行 healthz、真实非流式请求和流式请求；确认 HTTP 200 且网关连接 `127.0.0.1:7897`。
- [ ] 检查新日志中没有新增 panic、`output_config.format` 或直连地区限制错误。
