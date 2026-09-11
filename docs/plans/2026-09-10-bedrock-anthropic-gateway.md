# Bedrock Anthropic Gateway Implementation Plan

Goal: 构建一个可独立部署、兼容 Anthropic Messages API 的 AWS Bedrock Go 网关。

Architecture: net/http 负责路由、鉴权和 SSE；internal/bedrock 封装 AWS SDK v2；internal/anthropic 负责请求、响应和错误协议转换。Bedrock 客户端通过最小接口注入，测试只替换外部 AWS 边界并断言真实 HTTP 行为。

Tech Stack: Go 1.24、AWS SDK for Go v2、标准库 net/http、Docker。

Spec: docs/design/2026-09-10-bedrock-anthropic-gateway.md

## 全局约束

- 不记录提示词、响应正文、AWS 凭证或网关密钥。
- GATEWAY_API_KEY 与 AWS_REGION 缺失时启动失败。
- 模型只允许 Global Opus 5 和 Global Opus 4.8。
- 生产代码文件控制在 500 行内。
- 所有行为先写失败测试，再写最小实现。

## 任务

### Task 1：配置与模型白名单

Files: go.mod、internal/config/config.go、internal/config/config_test.go。

步骤：先测试缺失配置、默认值、非法映射和固定模型映射；确认失败；实现 Config.Load；确认通过。

### Task 2：Anthropic 请求转换

Files: internal/anthropic/request.go、request_test.go、error.go。

步骤：先测试别名、完整 ID、未知模型、字段删除、版本和 beta 注入；确认失败；实现转换；确认通过。

### Task 3：Bedrock 适配器

Files: internal/bedrock/client.go、client_test.go。

步骤：先测试普通调用、EventStream chunk、异常事件和取消；确认失败；实现 AWS SDK v2 adapter；确认通过。

### Task 4：HTTP Messages 与 SSE

Files: internal/httpapi/server.go、auth.go、messages.go、sse.go 及对应测试。

步骤：先测试健康检查、两种认证、请求限制、普通响应、SSE 和错误映射；确认失败；实现 handlers；确认通过。

### Task 5：Token 统计与进程生命周期

Files: internal/httpapi/count_tokens.go、count_tokens_test.go、cmd/gateway/main.go。

步骤：先测试统计成功和上游不支持；确认失败；实现 handler、AWS 装配和优雅退出；确认通过。

### Task 6：容器与交付

Files: Dockerfile、.dockerignore、.gitignore、.env.example、README.md、Makefile、real_aws_integration_test.go。

步骤：添加显式启用的真实 AWS 测试；补充 Claude Code、IAM、Docker 使用说明；运行 test、race、vet、Linux build、Docker build 和凭证扫描；提交。


动态模型目录：模型与别名从 MODEL_CONFIG_FILE 加载，收到 SIGHUP 后原子热更新；新配置无效时继续保留上一版快照。
