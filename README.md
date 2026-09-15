# Bedrock Anthropic Gateway

一个独立的 Anthropic Messages API 兼容网关，将 Claude 客户端请求转发到 AWS Bedrock。网关使用 AWS SDK 默认凭证链，不保存 AWS Access Key。

## 支持接口

- `POST /v1/messages`：非流式与 SSE 流式 Messages API
- `POST /v1/messages/count_tokens`：调用 Bedrock `CountTokens`
- `GET /healthz`：存活检查

默认模型目录包含：

| 客户端名称 | Bedrock model ID |
| --- | --- |
| `opus-5`、`claude-opus-5` | `global.anthropic.claude-opus-5` |
| `opus-4-8`、`claude-opus-4-8` | `global.anthropic.claude-opus-4-8` |

完整 Bedrock model ID 也可以直接作为客户端模型名。未登记的模型会返回 `not_found_error`。

## 启动

先配置 AWS 默认凭证链。生产环境优先使用 EC2 Instance Profile、ECS Task Role 或 Web Identity；本地可以使用 `aws configure`、`AWS_PROFILE` 或环境变量。

```bash
export AWS_DEFAULT_REGION=us-east-1
export GATEWAY_API_KEY="$(openssl rand -hex 32)"
go run ./cmd/gateway
```

区域同时支持 AWS_REGION 与 AWS_DEFAULT_REGION，若两者都设置则 AWS_REGION 优先。服务默认监听 `:8080`。启动时会立即校验 `configs/models.json`，AWS 凭证由 SDK 在首次调用时从默认凭证链解析。

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
MAX_CONCURRENT_STREAMS=4      # 同时执行的 Bedrock 流式请求数
STREAM_QUEUE_TIMEOUT=30s      # 等待流式并发槽位的最长时间
```

网关在 Bedrock 暂无新事件时发送 `: ping` SSE 注释；每收到一个上游事件都会重新计算空闲时间，客户端应忽略注释。并发槽位在等待超时后返回 `503 overloaded_error`。AWS SDK 使用 10 秒建连超时、30 秒 TCP keepalive、120 秒响应头超时和最多三次重试，单次最大退避限制为 5 秒；流式响应不设置覆盖整个生成过程的总超时。

Clash Verge Rev / Mihomo 建议为 Bedrock 域名配置固定的支持地区节点，并启用：

```yaml
keep-alive-interval: 15
keep-alive-idle: 15
disable-keep-alive: false
```

## Claude 客户端

Claude Code 连接本地网关：

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080
export ANTHROPIC_AUTH_TOKEN='<与 GATEWAY_API_KEY 相同的值>'
export ANTHROPIC_MODEL=opus-5
export ANTHROPIC_DEFAULT_OPUS_MODEL=opus-5
export ANTHROPIC_DEFAULT_SONNET_MODEL=opus-4-8
export ANTHROPIC_DEFAULT_HAIKU_MODEL=opus-4-8
claude
```

此模式不要设置 `CLAUDE_CODE_USE_BEDROCK=1`，因为客户端连接的是 Anthropic 兼容网关，AWS 签名由网关完成。

通用 Anthropic SDK 可以把 `base_url` 指向 `http://127.0.0.1:8080`，并把 API key 设置成 `GATEWAY_API_KEY`。

## 动态调整模型

编辑 `configs/models.json`，每个模型配置一个真实 Bedrock model ID 和若干客户端别名：

```json
{
  "default": "opus-5",
  "models": [
    {
      "id": "global.anthropic.claude-opus-5",
      "aliases": ["claude-opus-5", "opus-5"]
    }
  ]
}
```

修改后向进程发送 `SIGHUP`：

```bash
kill -HUP <gateway-pid>
```

新配置只有在 JSON 结构、默认模型和所有别名均通过校验后才会原子生效。校验失败时进程继续使用上一版。Docker 或 Kubernetes 可以把模型文件挂载到 `/configs/models.json` 后发送 `SIGHUP`。

## Docker

```bash
docker build -t bedrock-anthropic-gateway .
docker run --rm -p 8080:8080 \
  --env-file .env \
  -v "$PWD/configs/models.json:/configs/models.json:ro" \
  bedrock-anthropic-gateway
```

本地 Docker 可在已被 .gitignore 排除的 .env 中提供 AWS 默认凭证链需要的变量。部署在 AWS 时应给容器绑定 IAM Role，不向镜像写入静态 AWS 凭证。该 Role 至少需要目标模型对应的 bedrock:InvokeModel、bedrock:InvokeModelWithResponseStream 与 bedrock:CountTokens 权限。

## 验证

```bash
go test ./...
go test -race ./...
go test -tags=integration ./internal/bedrock # 设置 BEDROCK_INTEGRATION_MODEL 后调用真实 CountTokens
go vet ./...
CGO_ENABLED=0 GOOS=linux go build -trimpath -o /tmp/bedrock-anthropic-gateway ./cmd/gateway
```

请求完成日志记录 request ID、接口、模型、是否流式、耗时和结果；上游失败另记错误详情。日志不记录请求正文、网关密钥或 AWS 凭证。
