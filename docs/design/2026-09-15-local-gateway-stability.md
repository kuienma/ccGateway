# 本地网关稳定性设计

## 目标

在 Bedrock 必须经过本机 Mihomo 代理的前提下，减少可避免的失败，缩短代理故障后的等待时间，并让长时间 SSE 请求保持可观测、可控和可恢复。

## 约束

- 网关只能在本机运行。
- Bedrock 请求必须经过 `HTTP_PROXY`/`HTTPS_PROXY`，直连会触发 Anthropic 地区限制。
- 已输出内容的流式请求不能自动重放，否则可能产生重复内容。
- 流式请求不能设置覆盖整个响应生命周期的总超时。

## 方案

1. 请求转换层删除 Bedrock 当前不接受的 `output_config.format`；其他 `output_config` 字段继续保留。
2. AWS SDK 使用冻结的专用 HTTP Client，避免 Defaults Mode 覆盖连接策略：10 秒建连、30 秒 TCP keepalive、10 秒 TLS 握手、90 秒空闲连接回收，并提高每主机空闲连接数。响应头不设置固定超时，避免切断耗时较长的非流式推理。
3. AWS Standard Retry 保持最多 3 次，但把最大退避限制为 5 秒。SDK 只负责取得流式响应前的重试；开始消费流后不重放。
4. SSE 写出由单一 goroutine 串行完成。Bedrock 连续 15 秒暂无事件时向客户端发送 `: ping` 注释；收到事件后重新计算空闲时间。
5. 流式请求使用有界信号量，默认最多 4 个并发，排队最多 30 秒；超出后返回 `503 overloaded_error`。
6. 请求日志记录接口、是否流式、模型、结果和耗时；客户端取消记录为信息且不再尝试写错误响应，上游失败记录为错误。
7. 启动脚本在 healthz 成功后再次确认新 PID 存活，并在有 `lsof` 时确认监听端口属于新 PID，避免命中旧进程而误报成功。

## 配置

- `STREAM_HEARTBEAT_INTERVAL`，默认 `15s`
- `MAX_CONCURRENT_STREAMS`，默认 `4`
- `STREAM_QUEUE_TIMEOUT`，默认 `30s`

## 验证

- 单元测试覆盖字段过滤、配置校验、心跳、过载、无效上游状态码和请求日志路径。
- Shell 集成测试覆盖“healthz 来自旧进程但新 PID 未监听”的启动失败场景。
- 完整运行 `go test ./...`、`go test -race ./...`、`go vet ./...` 和 Linux 静态构建。
- 重启本地网关后执行真实非流式和流式请求，确认使用 `127.0.0.1:7897` 代理。
