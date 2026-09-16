# Bedrock Anthropic Gateway 设计

## 目标

提供一个可独立部署的 Go 网关，让 Claude Code 与支持自定义 Anthropic Base URL 的客户端，通过 Anthropic Messages API 调用 AWS Bedrock。客户端只持有网关密钥，AWS 凭证仅存在于网关运行环境。

## 模型目录

模型不硬编码在 Go 源码中。进程从 MODEL_CONFIG_FILE 指向的 JSON 文件加载 Bedrock model ID、客户端别名和默认模型。初始目录包含 global.anthropic.claude-opus-5 与 global.anthropic.claude-opus-4-8，默认使用 Opus 5。

收到 SIGHUP 时先读取并完整校验新目录，再原子替换内存快照。无效配置不会影响上一版；未登记模型在调用 AWS 前拒绝。这样可以在不重新编译、不终止现有请求的情况下增删模型、调整别名或切换默认模型。

## 对外协议

- POST /v1/messages：兼容 Anthropic Messages 请求，支持普通 JSON 和 stream=true 的 SSE。
- POST /v1/messages/count_tokens：返回 Anthropic 形态的 input_tokens。
- GET /healthz：进程存活检查，不访问 AWS。
- 认证接受 x-api-key 与 Authorization: Bearer。
- 错误返回 Anthropic 形态的 type/error 对象。

## Bedrock 转换

网关从请求体删除 model 和 stream，注入 anthropic_version=bedrock-2023-05-31，将 anthropic-beta 请求头转换为 anthropic_beta。非流式调用 InvokeModel；流式调用 InvokeModelWithResponseStream，并把 Bedrock EventStream chunk 中的 Anthropic JSON转换为标准 SSE event/data 帧。

Token 统计调用 Bedrock CountTokens。若目标模型或区域不支持，返回明确上游能力错误，不使用字符数估算。

## 安全与运维

- AWS SDK v2 默认凭证链，支持 IAM Role、AWS Profile、SSO 和临时凭证；区域接受 AWS_REGION 或 AWS_DEFAULT_REGION。
- GATEWAY_API_KEY 必填并使用恒定时间比较。
- 默认请求体上限 20 MiB。
- 完成日志每个进入 Messages/CountTokens handler 的请求记录一次 request_id、operation、model_id、stream、duration_ms、result；上游错误另记 error，非流式错误同时记录 status，不额外承诺独立 AWS request ID 字段，不记录正文或凭证。
- 支持 HTTP 超时、客户端取消、优雅退出。
- Docker 镜像使用非 root 用户运行。

## 验收场景

1. 无密钥或错误密钥返回 401。
2. 初始别名分别映射到正确 Global Bedrock ID，SIGHUP 后新目录原子生效。
3. 未知模型、非法 JSON、超大请求在访问 AWS 前拒绝。
4. 普通响应保持 Anthropic Messages JSON。
5. EventStream 正确转换成 Claude Code 可消费的 SSE。
6. 客户端断开会取消 AWS 请求。
7. Bedrock 4xx、限流和 5xx转换为 Anthropic 错误。
8. Go 单测、race、vet、Linux 静态构建及 Docker 构建通过。

## 2026-09-16 职责整理

当前启动装配与生命周期分离在 cmd/gateway/app.go、lifecycle.go；HTTP 基础参数使用 ServerConfig，resolver/runtime/logger 显式注入。模型目录由 parse.go 完整校验、catalog.go 只读解析、manager.go 加载并原子替换。热更新监听具有 context 退出路径。退出时取消热更新，HTTP Shutdown 与后台任务清理共用一个从退出开始计算的超时 context（使用既有 SHUTDOWN_TIMEOUT），保持在途请求的优雅完成语义，不累计多轮超时。期限耗尽返回 DeadlineExceeded 并保留原始服务／Shutdown 错误；同步文件读取可能仍未结束，不能将该路径视为全部任务已回收。所有返回路径均取消信号订阅，由 main 按既有错误退出路径终止进程。

执行与验证结果见 `../reports/2026-09-16-project-refactor.md`。此前设计与计划中的部署、真实 AWS 调用不属于本轮验收范围。
