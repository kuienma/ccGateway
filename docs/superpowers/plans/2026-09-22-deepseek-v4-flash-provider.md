# DeepSeek V4 Flash 接入实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Claude Code 通过本网关调用火山方舟 Ark 上的 DeepSeek V4 Flash，复用现有鉴权、并发控制与 SSE 心跳。

**Architecture:** 协议转换完全封装在新的 ark provider 内部，对上只暴露 Anthropic 语义。现有 `Runtime.Stream` 的 `emit func([]byte) error` 语义本就是"吐一个带 `type` 字段的 Anthropic chunk"，因此 `internal/httpapi/sse.go` 不改动即可复用。模型目录按 provider 路由，Bedrock 路径行为完全不变。

**Tech Stack:** Go 1.24，标准库 `net/http` + `encoding/json`（不引入新依赖），标准库 `testing` 表驱动测试。

**Spec:** `docs/design/2026-09-22-deepseek-v4-flash-provider.md`

## Global Constraints

- 模块路径 `github.com/zhshl/bedrock-anthropic-gateway`，Go 1.24。
- **不新增第三方依赖**：Ark 走裸 `net/http`，不引 go-openai 或火山 SDK。
- 注释与错误信息用简体中文，与现有代码一致；标识符用英文。
- 测试命令：`make test`（`go test ./...`）、`make race`、`make vet`。
- **Bedrock 路径行为零变化**：每个 task 结束时 `internal/bedrock`、`internal/httpapi` 的既有测试必须全绿。
- `internal/httpapi/sse.go` **禁止修改** —— 这是本设计的验收标志（Spec §3、§9）。
- Ark 模型 ID `deepseek-v4-flash-ga-260731`，Base URL `https://ark.cn-beijing.volces.com/api/v3`。

## 前置条件

工作区当前有 6 个文件的未提交改动（`README.md`、`configs/models.json`、`internal/anthropic/request.go`、
`internal/anthropic/request_test.go`、`internal/httpapi/errors.go`、`internal/httpapi/sse.go`），其中
`request.go` 正在删除 `removeUnsupportedOutputFormat`、`models.json` 有改动。**本计划的 Task 1 与 Task 3 会改动
这两个文件，开始执行前请先提交或暂存这些改动**，否则会产生冲突。

## File Structure

新建：

| 文件 | 职责 |
| --- | --- |
| `internal/llm/provider.go` | 中立 `Provider` 接口、`Resolution` 类型、provider 名常量与白名单 |
| `internal/ark/translate/request.go` | Anthropic → OpenAI 请求转换（纯函数，无 IO） |
| `internal/ark/translate/response.go` | OpenAI → Anthropic 非流式响应转换（纯函数） |
| `internal/ark/translate/events.go` | OpenAI delta → Anthropic 事件序列状态机（核心，纯函数） |
| `internal/ark/translate/tokens.go` | 本地 token 估算 |
| `internal/ark/sse.go` | OpenAI SSE 行解析（移植 nova `llm_tools_stream.go:209-302`） |
| `internal/ark/client.go` | Ark HTTP 客户端，实现 `llm.Provider` |

修改：

| 文件 | 改动 |
| --- | --- |
| `internal/modelcatalog/parse.go:16-19,35-53` | `modelConfig` 加 `Provider`；解析并校验 |
| `internal/modelcatalog/catalog.go:6-21` | `resolutions` 值类型改 `llm.Resolution`，`Resolve` 返回它 |
| `internal/modelcatalog/manager.go:40-42` | `Resolve` 签名跟随 |
| `internal/anthropic/request.go:10-18,20-55` | `ModelResolver` 返回 `llm.Resolution`；`PreparedRequest` 加 `Provider`；仅 bedrock 注入 `anthropic_version` |
| `internal/httpapi/server.go:12-50` | `runtime` 单例改 `providers map[string]llm.Provider` |
| `internal/httpapi/messages.go:37`、`count_tokens.go:17` | 按 `prepared.Provider` 取 provider |
| `internal/httpapi/errors.go:25-63` | 错误分类去 smithy 硬耦合，改为 provider 各自给出状态码 |
| `internal/config/config.go` | 新增 `ArkAPIKey`、`ArkBaseURL`、`ArkTimeout` |
| `cmd/gateway/app.go:55-70` | 构造并注入两个 provider |
| `configs/models.json` | 登记 DeepSeek，加 `provider` 字段 |

---

### Task 1: 模型目录支持 provider 归属

把"模型名 → modelID"扩展成"模型名 → (modelID, provider)"，Bedrock 为缺省值，行为不变。

**Files:**
- Create: `internal/llm/provider.go`
- Create: `internal/llm/provider_test.go`
- Modify: `internal/modelcatalog/parse.go:16-19`（加字段）、`:35-53`（解析）
- Modify: `internal/modelcatalog/catalog.go:6-21`
- Modify: `internal/modelcatalog/manager.go:40-42`
- Test: `internal/modelcatalog/parse_test.go`（在既有 `catalog_test.go` 同包内新增用例）

**Interfaces:**
- Produces: `llm.Resolution{ModelID string; Provider string}`；常量 `llm.ProviderBedrock = "bedrock"`、
  `llm.ProviderArk = "ark"`；`llm.ValidProvider(name string) bool`；
  `modelcatalog.Catalog.Resolve(name string) (llm.Resolution, bool)`

- [ ] **Step 1: 写 llm 包的失败测试**

创建 `internal/llm/provider_test.go`：

```go
package llm

import "testing"

func TestValidProvider(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"bedrock", ProviderBedrock, true},
		{"ark", ProviderArk, true},
		{"空字符串", "", false},
		{"未知 provider", "openai", false},
		{"大小写敏感", "Bedrock", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ValidProvider(tc.input); got != tc.want {
				t.Fatalf("ValidProvider(%q) = %v，期望 %v", tc.input, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/llm/ -v`
Expected: 编译失败，`undefined: ProviderBedrock`

- [ ] **Step 3: 实现 llm 包**

创建 `internal/llm/provider.go`：

```go
// Package llm 定义与具体厂商无关的模型调用抽象。
// 该包不依赖任何其他内部包，供 modelcatalog、anthropic、httpapi 共同引用。
package llm

import "context"

const (
	// ProviderBedrock 是 AWS Bedrock 上的 Anthropic 模型，原生使用 Anthropic Messages 协议。
	ProviderBedrock = "bedrock"
	// ProviderArk 是火山方舟 Ark 网关，使用 OpenAI Chat Completions 协议，需协议转换。
	ProviderArk = "ark"
)

// Resolution 是模型名解析结果：上游模型标识与它归属的 provider。
type Resolution struct {
	ModelID  string
	Provider string
}

// ValidProvider 报告 name 是否为已登记的 provider。
func ValidProvider(name string) bool {
	switch name {
	case ProviderBedrock, ProviderArk:
		return true
	default:
		return false
	}
}

// Provider 是单个上游厂商的调用能力。
// 所有实现都必须在 Anthropic 语义下工作：body 是 Anthropic 请求体，
// Invoke 返回 Anthropic 响应体，Stream 的 emit 每次收到一个带 type 字段的
// Anthropic 流事件 chunk（这是 httpapi/sse.go 依赖的契约）。
type Provider interface {
	Invoke(ctx context.Context, modelID string, body []byte) ([]byte, error)
	Stream(ctx context.Context, modelID string, body []byte, emit func([]byte) error) error
	CountTokens(ctx context.Context, modelID string, body []byte) (int32, error)
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/llm/ -v`
Expected: PASS

- [ ] **Step 5: 写模型目录的失败测试**

在 `internal/modelcatalog/catalog_test.go` 追加：

```go
func TestParseAssignsProviderWithBedrockDefault(t *testing.T) {
	t.Parallel()

	data := []byte(`{
		"default": "opus-5",
		"models": [
			{"id": "global.anthropic.claude-opus-5", "aliases": ["opus-5"]},
			{"id": "deepseek-v4-flash-ga-260731", "provider": "ark", "aliases": ["ds-flash"]}
		]
	}`)

	catalog, err := Parse(data)
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}

	claude, ok := catalog.Resolve("opus-5")
	if !ok {
		t.Fatal("opus-5 应能解析")
	}
	if claude.ModelID != "global.anthropic.claude-opus-5" {
		t.Fatalf("ModelID = %q", claude.ModelID)
	}
	if claude.Provider != llm.ProviderBedrock {
		t.Fatalf("未指定 provider 时应缺省为 bedrock，实际 %q", claude.Provider)
	}

	deepseek, ok := catalog.Resolve("ds-flash")
	if !ok {
		t.Fatal("ds-flash 应能解析")
	}
	if deepseek.Provider != llm.ProviderArk {
		t.Fatalf("Provider = %q，期望 ark", deepseek.Provider)
	}
	if deepseek.ModelID != "deepseek-v4-flash-ga-260731" {
		t.Fatalf("ModelID = %q", deepseek.ModelID)
	}
}

func TestParseRejectsUnknownProvider(t *testing.T) {
	t.Parallel()

	data := []byte(`{
		"default": "m",
		"models": [{"id": "m", "provider": "openai"}]
	}`)

	if _, err := Parse(data); err == nil {
		t.Fatal("未知 provider 应报错")
	}
}
```

文件顶部 import 增加 `"github.com/zhshl/bedrock-anthropic-gateway/internal/llm"`。

- [ ] **Step 6: 运行测试确认失败**

Run: `go test ./internal/modelcatalog/ -run TestParse -v`
Expected: FAIL —— `json: unknown field "provider"`（因 `parse.go:24` 有 `DisallowUnknownFields`），
以及 `claude.ModelID` 取不到（`Resolve` 目前返回 string）

- [ ] **Step 7: 改 parse.go 支持并校验 provider**

`internal/modelcatalog/parse.go` 的 `modelConfig` 加字段：

```go
type modelConfig struct {
	ID       string   `json:"id"`
	Provider string   `json:"provider,omitempty"`
	Aliases  []string `json:"aliases,omitempty"`
}
```

`Parse` 里 `resolutions` 改类型，并在循环内解析 provider（替换 `parse.go:35-53` 那段）：

```go
	resolutions := make(map[string]llm.Resolution)
	for index, model := range raw.Models {
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" {
			return nil, fmt.Errorf("models[%d].id 不能为空", index)
		}
		provider := strings.TrimSpace(model.Provider)
		if provider == "" {
			// 缺省 bedrock，保持既有配置文件向后兼容。
			provider = llm.ProviderBedrock
		}
		if !llm.ValidProvider(provider) {
			return nil, fmt.Errorf("models[%d].provider 未登记: %s", index, provider)
		}
		resolution := llm.Resolution{ModelID: model.ID, Provider: provider}
		if err := registerResolution(resolutions, model.ID, resolution); err != nil {
			return nil, err
		}
		for aliasIndex, alias := range model.Aliases {
			alias = strings.TrimSpace(alias)
			if alias == "" {
				return nil, fmt.Errorf("models[%d].aliases[%d] 不能为空", index, aliasIndex)
			}
			if err := registerResolution(resolutions, alias, resolution); err != nil {
				return nil, err
			}
		}
	}
```

`registerResolution` 与 `defaultModel` 跟随改类型：

```go
func registerResolution(resolutions map[string]llm.Resolution, name string, resolution llm.Resolution) error {
	if _, exists := resolutions[name]; exists {
		return fmt.Errorf("模型名或别名重复: %s", name)
	}
	resolutions[name] = resolution
	return nil
}
```

`parse.go:55-60` 的默认模型段落改为：

```go
	defaultName := strings.TrimSpace(raw.Default)
	defaultResolution, ok := resolutions[defaultName]
	if defaultName == "" || !ok {
		return nil, fmt.Errorf("default 必须引用已登记的模型或别名")
	}
	return &Catalog{defaultModel: defaultResolution, resolutions: resolutions}, nil
```

import 增加 `"github.com/zhshl/bedrock-anthropic-gateway/internal/llm"`。

- [ ] **Step 8: 改 catalog.go 与 manager.go**

`internal/modelcatalog/catalog.go` 全文替换为：

```go
package modelcatalog

import (
	"strings"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/llm"
)

// Catalog 是只读的模型名称解析快照，由 Parse 构造。
type Catalog struct {
	defaultModel llm.Resolution
	resolutions  map[string]llm.Resolution
}

func (c *Catalog) Resolve(name string) (llm.Resolution, bool) {
	if c == nil {
		return llm.Resolution{}, false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return c.defaultModel, true
	}
	resolution, ok := c.resolutions[name]
	return resolution, ok
}
```

`internal/modelcatalog/manager.go:40-42` 改为：

```go
func (m *Manager) Resolve(name string) (llm.Resolution, bool) {
	return m.current.Load().Resolve(name)
}
```

import 增加 llm 包。

- [ ] **Step 9: 运行测试**

Run: `go test ./internal/modelcatalog/ ./internal/llm/ -v`
Expected: 新增两个用例 PASS。既有 `catalog_test.go`/`manager_test.go` 中断言 `Resolve` 返回 string 的用例会
编译失败 —— 逐个把断言改成取 `.ModelID`，不要改变它们的语义。

- [ ] **Step 10: 全量编译确认破坏面**

Run: `go build ./... 2>&1 | head -20`
Expected: `internal/anthropic` 与 `internal/httpapi` 报类型不匹配 —— 这是预期的，Task 2 修复。

- [ ] **Step 11: 提交**

```bash
git add internal/llm/ internal/modelcatalog/
git commit -m "feat(llm): 模型目录支持 provider 归属

新增 internal/llm 定义中立的 Resolution 与 Provider 抽象，
modelcatalog 解析并校验 provider 字段，未指定时缺省 bedrock
以保持既有配置向后兼容。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 2: httpapi 按 provider 路由

把 `Server` 上的单个 Bedrock runtime 换成 provider 查表，此时仍只注册 bedrock 一个实现，外部行为不变。

**Files:**
- Modify: `internal/anthropic/request.go:10-18`（接口与结构体）、`:20-55`（分流注入）
- Modify: `internal/httpapi/server.go:12-50`
- Modify: `internal/httpapi/messages.go:37`、`internal/httpapi/count_tokens.go:17`
- Modify: `internal/httpapi/sse.go` —— **不改**（`s.runtime.Stream` 调用点改为 `provider.Stream`，
  见 Step 5 的处理方式）
- Modify: `cmd/gateway/app.go:55-70`
- Test: `internal/anthropic/request_test.go`、`internal/httpapi/server_test.go`

**Interfaces:**
- Consumes: `llm.Resolution`、`llm.Provider`、`llm.ProviderBedrock`（Task 1）
- Produces: `anthropic.PreparedRequest{ModelID, Body, Stream, Provider string}`；
  `httpapi.NewServer(cfg ServerConfig, resolver anthropic.ModelResolver, providers map[string]llm.Provider, logger *slog.Logger) *Server`；
  `Server.providerFor(name string) (llm.Provider, bool)`

- [ ] **Step 1: 写 anthropic 包的失败测试**

在 `internal/anthropic/request_test.go` 追加（并把 `resolverStub` 改造成返回 `llm.Resolution`）：

```go
type resolverStub map[string]llm.Resolution

func (r resolverStub) Resolve(name string) (llm.Resolution, bool) {
	value, ok := r[name]
	return value, ok
}

func TestPrepareRequestSkipsAnthropicVersionForArk(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"model":"ds-flash","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`)
	resolver := resolverStub{"ds-flash": {ModelID: "deepseek-v4-flash-ga-260731", Provider: llm.ProviderArk}}

	prepared, err := PrepareRequest(raw, nil, resolver)
	if err != nil {
		t.Fatalf("准备请求失败: %v", err)
	}
	if prepared.Provider != llm.ProviderArk {
		t.Fatalf("Provider = %q，期望 ark", prepared.Provider)
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(prepared.Body, &body); err != nil {
		t.Fatalf("解析 body 失败: %v", err)
	}
	if _, exists := body["anthropic_version"]; exists {
		t.Fatal("ark 路径不应注入 anthropic_version")
	}
	if _, exists := body["model"]; exists {
		t.Fatal("body 不应包含 model")
	}
}

func TestPrepareRequestKeepsAnthropicVersionForBedrock(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"model":"opus-5","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`)
	resolver := resolverStub{"opus-5": {ModelID: "global.anthropic.claude-opus-5", Provider: llm.ProviderBedrock}}

	prepared, err := PrepareRequest(raw, nil, resolver)
	if err != nil {
		t.Fatalf("准备请求失败: %v", err)
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(prepared.Body, &body); err != nil {
		t.Fatalf("解析 body 失败: %v", err)
	}
	var version string
	if err := json.Unmarshal(body["anthropic_version"], &version); err != nil {
		t.Fatalf("anthropic_version 缺失或非字符串: %v", err)
	}
	if version != BedrockAnthropicVersion {
		t.Fatalf("anthropic_version = %q", version)
	}
}
```

既有用例里 `resolverStub{"opus-5": "global.anthropic.claude-opus-5"}` 形式的字面量，统一改成
`resolverStub{"opus-5": {ModelID: "global.anthropic.claude-opus-5", Provider: llm.ProviderBedrock}}`。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/anthropic/ -v`
Expected: 编译失败，`PreparedRequest` 无 `Provider` 字段

- [ ] **Step 3: 改 anthropic/request.go**

`internal/anthropic/request.go:10-18` 改为：

```go
type ModelResolver interface {
	Resolve(name string) (llm.Resolution, bool)
}

type PreparedRequest struct {
	ModelID  string
	Body     []byte
	Stream   bool
	Provider string
}
```

`PrepareRequest` 内 `:30-36` 的解析段与 `:43-48` 的注入段改为：

```go
	resolution, ok := resolver.Resolve(modelName)
	if !ok {
		return PreparedRequest{}, &RequestError{
			Kind:    ErrorTypeNotFound,
			Message: fmt.Sprintf("模型未登记: %s", modelName),
		}
	}
```

```go
	delete(body, "model")
	delete(body, "stream")
	// anthropic_version 与 anthropic_beta 是 Bedrock InvokeModel 的入参约定，
	// 其他 provider 由各自的转换层处理，这里不注入。
	if resolution.Provider == llm.ProviderBedrock {
		body["anthropic_version"] = mustMarshal(BedrockAnthropicVersion)
		if len(betaFeatures) > 0 {
			body["anthropic_beta"] = mustMarshal(betaFeatures)
		}
	}
```

返回语句改为：

```go
	return PreparedRequest{
		ModelID:  resolution.ModelID,
		Body:     encoded,
		Stream:   stream,
		Provider: resolution.Provider,
	}, nil
```

import 增加 llm 包。

- [ ] **Step 4: 运行 anthropic 测试**

Run: `go test ./internal/anthropic/ -v`
Expected: PASS

- [ ] **Step 5: 改 httpapi/server.go 为 provider 查表**

`internal/httpapi/server.go:12-22` 的 `runtime bedrock.Runtime` 字段改为：

```go
type Server struct {
	apiKey                  string
	maxBodyBytes            int64
	resolver                anthropic.ModelResolver
	providers               map[string]llm.Provider
	logger                  *slog.Logger
	handler                 http.Handler
	streamHeartbeatInterval time.Duration
	streamQueueTimeout      time.Duration
	streamSlots             chan struct{}
}
```

`NewServer` 签名与赋值跟随（`:33-39`）：

```go
func NewServer(cfg ServerConfig, resolver anthropic.ModelResolver, providers map[string]llm.Provider, logger *slog.Logger) *Server {
	server := &Server{
		apiKey:                  cfg.APIKey,
		maxBodyBytes:            cfg.MaxBodyBytes,
		resolver:                resolver,
		providers:               providers,
		logger:                  logger,
		streamHeartbeatInterval: cfg.StreamHeartbeatInterval,
		streamQueueTimeout:      cfg.StreamQueueTimeout,
		streamSlots:             make(chan struct{}, cfg.MaxConcurrentStreams),
	}
```

文件末尾新增查表辅助：

```go
// providerFor 返回 name 对应的 provider。模型目录已校验过 provider 合法性，
// 这里取不到说明该 provider 未在启动时注入（例如缺少凭证）。
func (s *Server) providerFor(name string) (llm.Provider, bool) {
	provider, ok := s.providers[name]
	return provider, ok
}
```

import 把 `internal/bedrock` 换成 `internal/llm`。

- [ ] **Step 6: 改两个 handler 的调用点**

`internal/httpapi/messages.go` 在 `s.prepare` 之后、分流之前插入取 provider（`:20` 之后）：

```go
	completion.modelID = prepared.ModelID
	completion.stream = prepared.Stream
	provider, ok := s.providerFor(prepared.Provider)
	if !ok {
		s.writeError(w, r, http.StatusServiceUnavailable, anthropic.ErrorTypeAPI,
			"模型所属 provider 未启用: "+prepared.Provider)
		return
	}
```

`:33` 的流式分支改为把 provider 传下去：

```go
		completion.result = completionResult(s.handleStream(w, r, provider, prepared))
```

`:37` 的非流式调用改为 `provider.Invoke(...)`：

```go
	response, err := provider.Invoke(r.Context(), prepared.ModelID, prepared.Body)
```

`internal/httpapi/count_tokens.go:17` 同样处理：

```go
	completion.modelID = prepared.ModelID
	provider, ok := s.providerFor(prepared.Provider)
	if !ok {
		s.writeError(w, r, http.StatusServiceUnavailable, anthropic.ErrorTypeAPI,
			"模型所属 provider 未启用: "+prepared.Provider)
		return
	}
	count, err := provider.CountTokens(r.Context(), prepared.ModelID, prepared.Body)
```

- [ ] **Step 7: 给 sse.go 加参数（唯一允许的改动：函数签名）**

`internal/httpapi/sse.go:16` 的签名加一个 provider 参数，函数体内 `:34` 的 `s.runtime.Stream` 改成
`provider.Stream`。**除这两处外 sse.go 的事件循环、心跳、错误处理一行不动**：

```go
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, provider llm.Provider, prepared anthropic.PreparedRequest) error {
```

```go
		streamDone <- provider.Stream(ctx, prepared.ModelID, prepared.Body, func(chunk []byte) error {
```

import 增加 llm 包。

- [ ] **Step 8: 改 cmd/gateway/app.go 注入 provider 表**

`cmd/gateway/app.go:55-63` 的 `newHTTPServer` 签名与 `NewServer` 调用改为：

```go
func newHTTPServer(cfg config.Config, models anthropic.ModelResolver, providers map[string]llm.Provider, logger *slog.Logger) *http.Server {
	api := httpapi.NewServer(httpapi.ServerConfig{
		APIKey:                  cfg.GatewayAPIKey,
		MaxBodyBytes:            cfg.MaxBodyBytes,
		StreamHeartbeatInterval: cfg.StreamHeartbeatInterval,
		MaxConcurrentStreams:    cfg.MaxConcurrentStreams,
		StreamQueueTimeout:      cfg.StreamQueueTimeout,
	},
		models, providers, logger)
```

`run` 内 `:37` 的构造改为：

```go
	providers := map[string]llm.Provider{llm.ProviderBedrock: runtime}
	server := newHTTPServer(cfg, models, providers, logger)
```

import 增加 llm 包。

- [ ] **Step 9: 修 httpapi 测试的构造调用**

`internal/httpapi` 下测试里所有 `NewServer(cfg, resolver, runtime, logger)` 调用改为
`NewServer(cfg, resolver, map[string]llm.Provider{llm.ProviderBedrock: runtime}, logger)`；
测试里的 resolver stub 返回值改成 `llm.Resolution`（`Provider: llm.ProviderBedrock`）。
不要改变任何既有断言的语义。

- [ ] **Step 10: 全量测试**

Run: `make test && make vet`
Expected: 全部 PASS。这是纯重构，行为不变。

- [ ] **Step 11: 确认 sse.go 只动了两行**

Run: `git diff --stat internal/httpapi/sse.go`
Expected: `2 insertions(+), 2 deletions(-)`（仅签名与调用点）。若超出，说明改错了方向，回退重做。

- [ ] **Step 12: 提交**

```bash
git add internal/anthropic/ internal/httpapi/ cmd/gateway/
git commit -m "refactor(httpapi): 按 provider 路由模型调用

Server 持有的单个 Bedrock runtime 改为 provider 查表，
PrepareRequest 只在 bedrock 路径注入 anthropic_version。
此次仅注册 bedrock 一个实现，外部行为不变。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 3: 请求转换 —— 消息与基础参数

纯函数转换的第一半：system 提升、文本消息、基础采样参数、image 拦截。tools 与 thinking 留到 Task 4。

**Files:**
- Create: `internal/ark/translate/types.go`
- Create: `internal/ark/translate/request.go`
- Test: `internal/ark/translate/request_test.go`

**Interfaces:**
- Produces:
  - `translate.ChatRequest`、`ChatMessage`、`ToolCall`、`FunctionCall`、`Tool`、`FunctionSpec`、
    `Thinking`、`StreamOptions`（OpenAI 侧线格式，见 Step 3）
  - `translate.BuildRequest(modelID string, anthropicBody []byte, stream bool) (*ChatRequest, *Rejection)`
  - `translate.Rejection{Kind string; Message string}` —— 转换期的客户端错误，`Kind` 取
    `anthropic.ErrorType*` 常量值

- [ ] **Step 1: 写失败测试**

创建 `internal/ark/translate/request_test.go`：

```go
package translate

import (
	"encoding/json"
	"testing"
)

func TestBuildRequestLiftsSystemAndMapsBasics(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"system": "你是助手",
		"max_tokens": 512,
		"temperature": 0.5,
		"top_p": 0.9,
		"top_k": 40,
		"stop_sequences": ["\n\n"],
		"metadata": {"user_id": "u-1"},
		"messages": [
			{"role": "user", "content": "你好"},
			{"role": "assistant", "content": [{"type": "text", "text": "在"}]}
		]
	}`)

	req, rejection := BuildRequest("deepseek-v4-flash-ga-260731", raw, true)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}

	if req.Model != "deepseek-v4-flash-ga-260731" {
		t.Fatalf("Model = %q", req.Model)
	}
	if !req.Stream || req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
		t.Fatal("流式请求应带 stream_options.include_usage")
	}
	if req.MaxTokens != 512 {
		t.Fatalf("MaxTokens = %d", req.MaxTokens)
	}
	if req.Temperature == nil || *req.Temperature != 0.5 {
		t.Fatal("temperature 应透传")
	}
	if len(req.Stop) != 1 || req.Stop[0] != "\n\n" {
		t.Fatalf("Stop = %#v", req.Stop)
	}
	if req.User != "u-1" {
		t.Fatalf("User = %q", req.User)
	}

	if len(req.Messages) != 3 {
		t.Fatalf("消息数 = %d，期望 3（system + user + assistant）", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "你是助手" {
		t.Fatalf("首条应为 system，实际 %#v", req.Messages[0])
	}
	if req.Messages[1].Role != "user" || req.Messages[1].Content != "你好" {
		t.Fatalf("Messages[1] = %#v", req.Messages[1])
	}
	if req.Messages[2].Role != "assistant" || req.Messages[2].Content != "在" {
		t.Fatalf("Messages[2] = %#v", req.Messages[2])
	}
}

func TestBuildRequestJoinsMultipleTextBlocks(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "第一段"},
			{"type": "text", "text": "第二段"}
		]}]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}
	if req.Messages[0].Content != "第一段\n\n第二段" {
		t.Fatalf("多文本块应以空行连接，实际 %q", req.Messages[0].Content)
	}
	if req.Stream {
		t.Fatal("非流式请求 Stream 应为 false")
	}
	if req.StreamOptions != nil {
		t.Fatal("非流式请求不应带 stream_options")
	}
}

func TestBuildRequestLiftsSystemBlockArray(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"system": [{"type": "text", "text": "规则一"}, {"type": "text", "text": "规则二"}],
		"max_tokens": 64,
		"messages": [{"role": "user", "content": "hi"}]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "规则一\n\n规则二" {
		t.Fatalf("system 块数组应合并，实际 %#v", req.Messages[0])
	}
}

func TestBuildRequestRejectsImageBlock(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [{"role": "user", "content": [
			{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "iVBOR"}}
		]}]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if req != nil {
		t.Fatal("含图片的请求不应返回 ChatRequest")
	}
	if rejection == nil {
		t.Fatal("含图片的请求必须被拒绝，不能静默丢弃")
	}
	if rejection.Kind != "invalid_request_error" {
		t.Fatalf("Kind = %q", rejection.Kind)
	}
	if !contains(rejection.Message, "图片") {
		t.Fatalf("错误信息应说明不支持图片，实际 %q", rejection.Message)
	}
}

func TestBuildRequestDropsUnsupportedFields(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"top_k": 40,
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "hi", "cache_control": {"type": "ephemeral"}}
		]}]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("top_k 与 cache_control 应被静默丢弃而非报错: %v", rejection)
	}

	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	for _, field := range []string{"top_k", "cache_control"} {
		if contains(string(encoded), field) {
			t.Fatalf("序列化结果不应包含 %s: %s", field, encoded)
		}
	}
}

func TestBuildRequestRejectsMalformedBody(t *testing.T) {
	t.Parallel()

	cases := []struct{ name, body string }{
		{"非对象", `[]`},
		{"messages 非数组", `{"max_tokens":1,"messages":"hi"}`},
		{"content 块缺 type", `{"max_tokens":1,"messages":[{"role":"user","content":[{"text":"hi"}]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, rejection := BuildRequest("m", []byte(tc.body), false); rejection == nil {
				t.Fatal("畸形请求体必须被拒绝")
			}
		})
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && strings.Contains(haystack, needle)
}
```

测试文件 import 增加 `"strings"`。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/ark/translate/ -v`
Expected: 编译失败，`undefined: BuildRequest`

- [ ] **Step 3: 定义线格式类型**

创建 `internal/ark/translate/types.go`：

```go
// Package translate 在 Anthropic Messages 协议与 OpenAI Chat Completions 协议之间做双向转换。
// 本包是纯函数集合，不做任何 IO，便于表驱动测试。
package translate

import "encoding/json"

// ChatRequest 是发往 Ark /chat/completions 的请求体。
type ChatRequest struct {
	Model         string         `json:"model"`
	Messages      []ChatMessage  `json:"messages"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	Stop          []string       `json:"stop,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
	Tools         []Tool         `json:"tools,omitempty"`
	ToolChoice    any            `json:"tool_choice,omitempty"`
	Thinking      *Thinking      `json:"thinking,omitempty"`
	User          string         `json:"user,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// Thinking 是火山方舟的私有字段，type 取 enabled / disabled。
type Thinking struct {
	Type string `json:"type"`
}

type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name string `json:"name"`
	// Arguments 是 JSON 文本而非对象，这是 OpenAI 协议的规定。
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function FunctionSpec `json:"function"`
}

type FunctionSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// Rejection 表示转换阶段发现的客户端错误，Kind 使用 anthropic.ErrorType* 的值。
type Rejection struct {
	Kind    string
	Message string
}

func (r *Rejection) Error() string {
	return r.Kind + ": " + r.Message
}
```

- [ ] **Step 4: 实现请求转换**

创建 `internal/ark/translate/request.go`：

```go
package translate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// blockSeparator 是多个文本块合并成单个字符串时的分隔符。
const blockSeparator = "\n\n"

const kindInvalidRequest = "invalid_request_error"

// anthropicRequest 只声明需要转换的字段，其余（top_k、cache_control 等）随解析丢弃。
type anthropicRequest struct {
	System        json.RawMessage    `json:"system"`
	Messages      []anthropicMessage `json:"messages"`
	MaxTokens     int                `json:"max_tokens"`
	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	StopSequences []string           `json:"stop_sequences"`
	Metadata      *struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicBlock 覆盖 text / image / tool_use / tool_result 四种块，
// 各自的专属字段在 Task 4 使用。
type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// BuildRequest 把 Anthropic 请求体转成 Ark 请求。
// anthropicBody 是 anthropic.PrepareRequest 处理过的 body（已摘掉 model / stream）。
func BuildRequest(modelID string, anthropicBody []byte, stream bool) (*ChatRequest, *Rejection) {
	var source anthropicRequest
	if err := json.Unmarshal(anthropicBody, &source); err != nil {
		return nil, &Rejection{Kind: kindInvalidRequest, Message: "请求体必须是 JSON 对象"}
	}

	request := &ChatRequest{
		Model:       modelID,
		MaxTokens:   source.MaxTokens,
		Temperature: source.Temperature,
		TopP:        source.TopP,
		Stop:        source.StopSequences,
		Stream:      stream,
	}
	if stream {
		// 末尾额外一个 choices 为空的 usage chunk，用于拿到真实 token 数。
		request.StreamOptions = &StreamOptions{IncludeUsage: true}
	}
	if source.Metadata != nil {
		request.User = source.Metadata.UserID
	}

	if len(source.System) > 0 {
		system, rejection := flattenText(source.System, "system")
		if rejection != nil {
			return nil, rejection
		}
		if system != "" {
			request.Messages = append(request.Messages, ChatMessage{Role: "system", Content: system})
		}
	}

	for index, message := range source.Messages {
		converted, rejection := convertMessage(index, message)
		if rejection != nil {
			return nil, rejection
		}
		request.Messages = append(request.Messages, converted...)
	}
	return request, nil
}

// convertMessage 把一条 Anthropic 消息转成一条或多条 OpenAI 消息。
// Task 4 会在此扩展 tool_use / tool_result 分支。
func convertMessage(index int, message anthropicMessage) ([]ChatMessage, *Rejection) {
	text, rejection := flattenText(message.Content, fmt.Sprintf("messages[%d].content", index))
	if rejection != nil {
		return nil, rejection
	}
	return []ChatMessage{{Role: message.Role, Content: text}}, nil
}

// flattenText 把 Anthropic 的 content（字符串或块数组）压成单个字符串。
// 遇到图片块直接拒绝；未知块类型也拒绝，避免静默丢内容。
func flattenText(raw json.RawMessage, field string) (string, *Rejection) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}

	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return "", &Rejection{Kind: kindInvalidRequest, Message: field + " 必须是字符串或块数组"}
		}
		return text, nil
	}

	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", &Rejection{Kind: kindInvalidRequest, Message: field + " 必须是字符串或块数组"}
	}

	var parts []string
	for blockIndex, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				parts = append(parts, block.Text)
			}
		case "image":
			return "", &Rejection{
				Kind:    kindInvalidRequest,
				Message: "该模型不支持图片输入，请移除消息中的图片块",
			}
		case "":
			return "", &Rejection{
				Kind:    kindInvalidRequest,
				Message: fmt.Sprintf("%s[%d] 缺少 type", field, blockIndex),
			}
		default:
			return "", &Rejection{
				Kind:    kindInvalidRequest,
				Message: fmt.Sprintf("%s[%d] 不支持的块类型: %s", field, blockIndex, block.Type),
			}
		}
	}
	return strings.Join(parts, blockSeparator), nil
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/ark/translate/ -v`
Expected: 全部 PASS

- [ ] **Step 6: 提交**

```bash
git add internal/ark/translate/
git commit -m "feat(ark): 增加请求转换的消息与基础参数部分

system 提升为 system message、文本块合并、采样参数透传，
图片块显式拒绝而非静默丢弃，top_k 与 cache_control 随解析丢弃。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 4: 请求转换 —— 工具与 thinking 策略

补齐 `BuildRequest` 的另一半：tools 定义、`tool_use`/`tool_result` 块、`tool_choice`，以及 thinking 的保守策略
（Spec §6）。

**Files:**
- Modify: `internal/ark/translate/request.go`（扩展 `anthropicRequest`、`convertMessage`，新增工具相关函数）
- Test: `internal/ark/translate/request_tools_test.go`

**Interfaces:**
- Consumes: `BuildRequest`、`Rejection`、`anthropicBlock`（Task 3）
- Produces: 无新导出符号，`BuildRequest` 行为扩展

- [ ] **Step 1: 写工具转换的失败测试**

创建 `internal/ark/translate/request_tools_test.go`：

```go
package translate

import (
	"encoding/json"
	"testing"
)

func TestBuildRequestConvertsToolDefinitions(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [{"role": "user", "content": "北京天气"}],
		"tools": [{
			"name": "get_weather",
			"description": "查询天气",
			"input_schema": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}
		}]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}
	if len(req.Tools) != 1 {
		t.Fatalf("工具数 = %d", len(req.Tools))
	}
	tool := req.Tools[0]
	if tool.Type != "function" {
		t.Fatalf("Type = %q，期望 function", tool.Type)
	}
	if tool.Function.Name != "get_weather" || tool.Function.Description != "查询天气" {
		t.Fatalf("Function = %#v", tool.Function)
	}

	var schema map[string]any
	if err := json.Unmarshal(tool.Function.Parameters, &schema); err != nil {
		t.Fatalf("parameters 应是合法 JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Fatalf("input_schema 应原样搬到 parameters，实际 %#v", schema)
	}
}

func TestBuildRequestConvertsToolUseAndToolResult(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [
			{"role": "user", "content": "北京天气"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "我查一下"},
				{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "北京"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "18度晴"}
			]}
		]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}

	if len(req.Messages) != 3 {
		t.Fatalf("消息数 = %d，期望 3", len(req.Messages))
	}

	assistant := req.Messages[1]
	if assistant.Role != "assistant" || assistant.Content != "我查一下" {
		t.Fatalf("assistant 文本应保留，实际 %#v", assistant)
	}
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("tool_calls 数 = %d", len(assistant.ToolCalls))
	}
	call := assistant.ToolCalls[0]
	if call.ID != "toolu_1" || call.Type != "function" || call.Function.Name != "get_weather" {
		t.Fatalf("ToolCall = %#v", call)
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
		t.Fatalf("arguments 必须是 JSON 文本: %v", err)
	}
	if input["city"] != "北京" {
		t.Fatalf("input 应序列化进 arguments，实际 %q", call.Function.Arguments)
	}

	result := req.Messages[2]
	if result.Role != "tool" {
		t.Fatalf("tool_result 应转成 role=tool，实际 %q", result.Role)
	}
	if result.ToolCallID != "toolu_1" {
		t.Fatalf("ToolCallID = %q", result.ToolCallID)
	}
	if result.Content != "18度晴" {
		t.Fatalf("Content = %q", result.Content)
	}
}

func TestBuildRequestSplitsMultipleToolResults(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [{"role": "user", "content": [
			{"type": "tool_result", "tool_use_id": "t1", "content": "r1"},
			{"type": "tool_result", "tool_use_id": "t2", "content": "r2"}
		]}]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("两个 tool_result 应拆成两条 message，实际 %d 条", len(req.Messages))
	}
	if req.Messages[0].ToolCallID != "t1" || req.Messages[1].ToolCallID != "t2" {
		t.Fatalf("tool_call_id 顺序错误: %#v", req.Messages)
	}
}

func TestBuildRequestFlattensToolResultBlockContent(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [{"role": "user", "content": [
			{"type": "tool_result", "tool_use_id": "t1", "content": [{"type": "text", "text": "结果"}]}
		]}]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}
	if req.Messages[0].Content != "结果" {
		t.Fatalf("tool_result 的块数组 content 应压平，实际 %q", req.Messages[0].Content)
	}
}

func TestBuildRequestDropsHistoricalThinkingBlocks(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "内部推理", "signature": "sig-abc"},
				{"type": "text", "text": "答案"}
			]}
		]
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("历史 thinking 块应被丢弃而非报错: %v", rejection)
	}
	if req.Messages[1].Content != "答案" {
		t.Fatalf("thinking 内容不应进入 content，实际 %q", req.Messages[1].Content)
	}
}

func TestBuildRequestToolChoice(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  any
	}{
		{"auto", `{"type":"auto"}`, "auto"},
		{"any", `{"type":"any"}`, "required"},
		{"none", `{"type":"none"}`, "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw := []byte(`{"max_tokens":64,"messages":[{"role":"user","content":"hi"}],"tool_choice":` + tc.input + `}`)
			req, rejection := BuildRequest("m", raw, false)
			if rejection != nil {
				t.Fatalf("不应被拒绝: %v", rejection)
			}
			if req.ToolChoice != tc.want {
				t.Fatalf("ToolChoice = %#v，期望 %#v", req.ToolChoice, tc.want)
			}
		})
	}
}

func TestBuildRequestToolChoiceSpecificTool(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_tokens": 64,
		"messages": [{"role": "user", "content": "hi"}],
		"tool_choice": {"type": "tool", "name": "get_weather"}
	}`)

	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("不应被拒绝: %v", rejection)
	}
	encoded, err := json.Marshal(req.ToolChoice)
	if err != nil {
		t.Fatalf("序列化 tool_choice 失败: %v", err)
	}
	want := `{"type":"function","function":{"name":"get_weather"}}`
	if string(encoded) != want {
		t.Fatalf("ToolChoice = %s，期望 %s", encoded, want)
	}
}

func TestBuildRequestThinkingPolicy(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		thinking string
		want     string
	}{
		{"未传时主动关闭", ``, "disabled"},
		{"显式 disabled", `,"thinking":{"type":"disabled"}`, "disabled"},
		{"显式 enabled", `,"thinking":{"type":"enabled","budget_tokens":2048}`, "enabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw := []byte(`{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]` + tc.thinking + `}`)
			req, rejection := BuildRequest("m", raw, false)
			if rejection != nil {
				t.Fatalf("不应被拒绝: %v", rejection)
			}
			if req.Thinking == nil {
				t.Fatal("Thinking 必须显式设置，不能留空让 Ark 用默认值")
			}
			if req.Thinking.Type != tc.want {
				t.Fatalf("Thinking.Type = %q，期望 %q", req.Thinking.Type, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/ark/translate/ -run 'Tool|Thinking' -v`
Expected: FAIL —— `req.Tools` 为空、`ToolCalls` 为空、`req.Thinking` 为 nil

- [ ] **Step 3: 扩展输入结构体**

`internal/ark/translate/request.go` 的 `anthropicRequest` 增加三个字段：

```go
	Tools      []anthropicTool `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`
	Thinking   *struct {
		Type string `json:"type"`
	} `json:"thinking"`
```

同文件新增：

```go
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}
```

`anthropicBlock` 增加 thinking 块的字段：

```go
	Thinking string `json:"thinking"`
```

- [ ] **Step 4: 在 BuildRequest 里接上 tools、tool_choice 与 thinking**

在 `BuildRequest` 的 `request.User` 赋值之后、system 处理之前插入：

```go
	for index, tool := range source.Tools {
		if strings.TrimSpace(tool.Name) == "" {
			return nil, &Rejection{
				Kind:    kindInvalidRequest,
				Message: fmt.Sprintf("tools[%d].name 不能为空", index),
			}
		}
		request.Tools = append(request.Tools, Tool{
			Type: "function",
			Function: FunctionSpec{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.InputSchema,
			},
		})
	}

	toolChoice, rejection := convertToolChoice(source.ToolChoice)
	if rejection != nil {
		return nil, rejection
	}
	request.ToolChoice = toolChoice

	// Ark 不传 thinking 时默认开启思考，而 Anthropic 协议在客户端未请求时
	// 不应返回 thinking 块，因此这里始终显式下发。
	request.Thinking = &Thinking{Type: "disabled"}
	if source.Thinking != nil && source.Thinking.Type == "enabled" {
		request.Thinking.Type = "enabled"
	}
```

同文件新增 `convertToolChoice`：

```go
// convertToolChoice 把 Anthropic 的 tool_choice 转成 OpenAI 形态。
// auto → "auto"，any → "required"，none → "none"，tool → 指定函数。
func convertToolChoice(raw json.RawMessage) (any, *Rejection) {
	if len(strings.TrimSpace(string(raw))) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return nil, &Rejection{Kind: kindInvalidRequest, Message: "tool_choice 必须是对象"}
	}
	switch choice.Type {
	case "auto":
		return "auto", nil
	case "any":
		return "required", nil
	case "none":
		return "none", nil
	case "tool":
		if choice.Name == "" {
			return nil, &Rejection{Kind: kindInvalidRequest, Message: "tool_choice.name 不能为空"}
		}
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": choice.Name},
		}, nil
	default:
		return nil, &Rejection{
			Kind:    kindInvalidRequest,
			Message: "tool_choice.type 不支持: " + choice.Type,
		}
	}
}
```

> 注意：`tool_choice` 为指定工具时用 `map[string]any` 而非匿名 struct，测试断言的序列化结果
> `{"type":"function","function":{"name":"get_weather"}}` 依赖 Go 对 map 键的字典序输出 ——
> `function` 排在 `type` 前会导致断言失败。实现时请改用具名 struct 保证字段顺序：
>
> ```go
> type toolChoiceFunction struct {
> 	Type     string `json:"type"`
> 	Function struct {
> 		Name string `json:"name"`
> 	} `json:"function"`
> }
> ```
>
> 构造后返回该 struct 值。

- [ ] **Step 5: 重写 convertMessage 支持工具块**

把 Task 3 的 `convertMessage` 整体替换为：

```go
// convertMessage 把一条 Anthropic 消息转成一条或多条 OpenAI 消息。
// tool_result 块必须各自成为独立的 role=tool 消息；
// tool_use 块合并进同一条 assistant 消息的 tool_calls。
func convertMessage(index int, message anthropicMessage) ([]ChatMessage, *Rejection) {
	field := fmt.Sprintf("messages[%d].content", index)
	trimmed := strings.TrimSpace(string(message.Content))

	// 纯字符串 content 没有块结构，直接走文本路径。
	if trimmed == "" || trimmed == "null" || trimmed[0] == '"' {
		text, rejection := flattenText(message.Content, field)
		if rejection != nil {
			return nil, rejection
		}
		return []ChatMessage{{Role: message.Role, Content: text}}, nil
	}

	var blocks []anthropicBlock
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		return nil, &Rejection{Kind: kindInvalidRequest, Message: field + " 必须是字符串或块数组"}
	}

	var (
		texts     []string
		toolCalls []ToolCall
		results   []ChatMessage
	)
	for blockIndex, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				texts = append(texts, block.Text)
			}
		case "thinking", "redacted_thinking":
			// 历史推理内容不回传给 Ark，丢弃。
		case "tool_use":
			if block.ID == "" || block.Name == "" {
				return nil, &Rejection{
					Kind:    kindInvalidRequest,
					Message: fmt.Sprintf("%s[%d] tool_use 缺少 id 或 name", field, blockIndex),
				}
			}
			arguments := strings.TrimSpace(string(block.Input))
			if arguments == "" || arguments == "null" {
				arguments = "{}"
			}
			toolCalls = append(toolCalls, ToolCall{
				ID:       block.ID,
				Type:     "function",
				Function: FunctionCall{Name: block.Name, Arguments: arguments},
			})
		case "tool_result":
			if block.ToolUseID == "" {
				return nil, &Rejection{
					Kind:    kindInvalidRequest,
					Message: fmt.Sprintf("%s[%d] tool_result 缺少 tool_use_id", field, blockIndex),
				}
			}
			content, rejection := flattenText(block.Content, fmt.Sprintf("%s[%d].content", field, blockIndex))
			if rejection != nil {
				return nil, rejection
			}
			results = append(results, ChatMessage{
				Role:       "tool",
				Content:    content,
				ToolCallID: block.ToolUseID,
			})
		case "image":
			return nil, &Rejection{
				Kind:    kindInvalidRequest,
				Message: "该模型不支持图片输入，请移除消息中的图片块",
			}
		case "":
			return nil, &Rejection{
				Kind:    kindInvalidRequest,
				Message: fmt.Sprintf("%s[%d] 缺少 type", field, blockIndex),
			}
		default:
			return nil, &Rejection{
				Kind:    kindInvalidRequest,
				Message: fmt.Sprintf("%s[%d] 不支持的块类型: %s", field, blockIndex, block.Type),
			}
		}
	}

	var converted []ChatMessage
	// 有文本或工具调用时才产出主消息；纯 tool_result 消息不产出空的 user 消息。
	if len(texts) > 0 || len(toolCalls) > 0 {
		converted = append(converted, ChatMessage{
			Role:      message.Role,
			Content:   strings.Join(texts, blockSeparator),
			ToolCalls: toolCalls,
		})
	}
	return append(converted, results...), nil
}
```

- [ ] **Step 6: 运行全部转换测试**

Run: `go test ./internal/ark/translate/ -v`
Expected: 全部 PASS（含 Task 3 的用例）

- [ ] **Step 7: 提交**

```bash
git add internal/ark/translate/
git commit -m "feat(ark): 请求转换补齐工具与 thinking 策略

tools[].input_schema 搬到 function.parameters，tool_use 合并进
assistant.tool_calls，tool_result 各自拆成独立 role=tool 消息，
历史 thinking 块丢弃。thinking 始终显式下发：仅客户端明确请求
enabled 时开启，其余一律 disabled。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 5: 非流式响应转换

把 Ark 的 `chat.completion` 响应转成 Anthropic Messages 响应。真实响应样本见 Spec §2（实测 `reasoning_content`
与 `content` 同时出现在 `message` 里）。

**Files:**
- Create: `internal/ark/translate/response.go`
- Test: `internal/ark/translate/response_test.go`

**Interfaces:**
- Consumes: `Rejection`（Task 3）
- Produces:
  - `translate.ChatResponse`、`ChatChoice`、`ChatResponseMessage`、`Usage`（Ark 侧线格式）
  - `translate.BuildResponse(payload []byte, includeThinking bool) ([]byte, error)` ——
    返回可直接写给客户端的 Anthropic 响应 JSON

- [ ] **Step 1: 写失败测试**

创建 `internal/ark/translate/response_test.go`：

```go
package translate

import (
	"encoding/json"
	"testing"
)

// 取自 2026-09-22 对 Ark 的真实响应（见设计文档 §2）。
const arkTextResponse = `{
	"choices": [{
		"finish_reason": "stop",
		"index": 0,
		"message": {"content": "ok", "reasoning_content": "We need answer with only two characters ok.", "role": "assistant"}
	}],
	"created": 1790133454,
	"id": "021790133452915006c460d0b2d36d003716bcb3522c0d185f955",
	"model": "deepseek-v4-flash-ga-260731",
	"object": "chat.completion",
	"usage": {"completion_tokens": 11, "prompt_tokens": 89, "total_tokens": 100,
		"completion_tokens_details": {"reasoning_tokens": 9}}
}`

func decodeAnthropicResponse(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("结果必须是合法 JSON: %v", err)
	}
	return decoded
}

func TestBuildResponseMapsTextAndUsage(t *testing.T) {
	t.Parallel()

	out, err := BuildResponse([]byte(arkTextResponse), false)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	decoded := decodeAnthropicResponse(t, out)

	if decoded["type"] != "message" {
		t.Fatalf("type = %v，期望 message", decoded["type"])
	}
	if decoded["role"] != "assistant" {
		t.Fatalf("role = %v", decoded["role"])
	}
	if decoded["id"] != "021790133452915006c460d0b2d36d003716bcb3522c0d185f955" {
		t.Fatalf("id 应沿用上游，实际 %v", decoded["id"])
	}
	if decoded["model"] != "deepseek-v4-flash-ga-260731" {
		t.Fatalf("model = %v", decoded["model"])
	}
	if decoded["stop_reason"] != "end_turn" {
		t.Fatalf("finish_reason=stop 应映射为 end_turn，实际 %v", decoded["stop_reason"])
	}

	content, ok := decoded["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("includeThinking=false 时只应有一个 text 块，实际 %#v", decoded["content"])
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "ok" {
		t.Fatalf("content[0] = %#v", block)
	}

	usage := decoded["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 89 {
		t.Fatalf("input_tokens = %v", usage["input_tokens"])
	}
	if usage["output_tokens"].(float64) != 11 {
		t.Fatalf("output_tokens = %v", usage["output_tokens"])
	}
}

func TestBuildResponseEmitsThinkingBlockFirst(t *testing.T) {
	t.Parallel()

	out, err := BuildResponse([]byte(arkTextResponse), true)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	content := decodeAnthropicResponse(t, out)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("应有 thinking + text 两块，实际 %d 块", len(content))
	}
	first := content[0].(map[string]any)
	if first["type"] != "thinking" {
		t.Fatalf("thinking 块必须排在最前，实际首块 %v", first["type"])
	}
	if first["thinking"] != "We need answer with only two characters ok." {
		t.Fatalf("thinking 内容 = %v", first["thinking"])
	}
	if content[1].(map[string]any)["type"] != "text" {
		t.Fatalf("第二块应为 text，实际 %v", content[1])
	}
}

func TestBuildResponseMapsToolCalls(t *testing.T) {
	t.Parallel()

	payload := `{
		"id": "resp-1",
		"model": "m",
		"choices": [{
			"finish_reason": "tool_calls",
			"index": 0,
			"message": {"content": "", "role": "assistant", "tool_calls": [
				{"id": "call_1", "type": "function", "index": 0,
				 "function": {"name": "get_weather", "arguments": "{\"city\": \"北京\"}"}}
			]}
		}],
		"usage": {"completion_tokens": 20, "prompt_tokens": 100}
	}`

	out, err := BuildResponse([]byte(payload), false)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	decoded := decodeAnthropicResponse(t, out)

	if decoded["stop_reason"] != "tool_use" {
		t.Fatalf("finish_reason=tool_calls 应映射为 tool_use，实际 %v", decoded["stop_reason"])
	}
	content := decoded["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content 为空字符串时不应产出 text 块，实际 %#v", content)
	}
	block := content[0].(map[string]any)
	if block["type"] != "tool_use" || block["id"] != "call_1" || block["name"] != "get_weather" {
		t.Fatalf("tool_use 块 = %#v", block)
	}
	input := block["input"].(map[string]any)
	if input["city"] != "北京" {
		t.Fatalf("arguments 应解析成 input 对象，实际 %#v", input)
	}
}

func TestBuildResponseStopReasonMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		finish string
		want   string
	}{
		{"stop", "end_turn"},
		{"tool_calls", "tool_use"},
		{"length", "max_tokens"},
		{"content_filter", "end_turn"},
		{"", "end_turn"},
	}
	for _, tc := range cases {
		t.Run(tc.finish, func(t *testing.T) {
			t.Parallel()
			payload := `{"id":"r","model":"m","choices":[{"finish_reason":"` + tc.finish +
				`","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
			out, err := BuildResponse([]byte(payload), false)
			if err != nil {
				t.Fatalf("转换失败: %v", err)
			}
			if got := decodeAnthropicResponse(t, out)["stop_reason"]; got != tc.want {
				t.Fatalf("finish_reason=%q → %v，期望 %q", tc.finish, got, tc.want)
			}
		})
	}
}

func TestBuildResponseRejectsEmptyChoices(t *testing.T) {
	t.Parallel()

	if _, err := BuildResponse([]byte(`{"id":"r","model":"m","choices":[]}`), false); err == nil {
		t.Fatal("choices 为空必须报错，不能返回空响应给客户端")
	}
}

func TestBuildResponseKeepsMalformedToolArgumentsVisible(t *testing.T) {
	t.Parallel()

	payload := `{"id":"r","model":"m","choices":[{"finish_reason":"tool_calls",
		"message":{"role":"assistant","content":"","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"f","arguments":"{不是JSON"}}]}}],
		"usage":{"prompt_tokens":1,"completion_tokens":1}}`

	if _, err := BuildResponse([]byte(payload), false); err == nil {
		t.Fatal("工具参数不是合法 JSON 时必须报错，不能静默产出损坏的 input")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/ark/translate/ -run BuildResponse -v`
Expected: 编译失败，`undefined: BuildResponse`

- [ ] **Step 3: 实现响应转换**

创建 `internal/ark/translate/response.go`：

```go
package translate

import (
	"encoding/json"
	"fmt"
)

// ChatResponse 是 Ark /chat/completions 的非流式响应体。
type ChatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   *Usage       `json:"usage"`
}

type ChatChoice struct {
	Index        int                 `json:"index"`
	FinishReason string              `json:"finish_reason"`
	Message      ChatResponseMessage `json:"message"`
}

type ChatResponseMessage struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content"`
	ToolCalls        []ToolCall `json:"tool_calls"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// anthropicResponse 是回给客户端的 Anthropic Messages 响应。
type anthropicResponse struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Role         string           `json:"role"`
	Model        string           `json:"model"`
	Content      []responseBlock  `json:"content"`
	StopReason   string           `json:"stop_reason"`
	StopSequence *string          `json:"stop_sequence"`
	Usage        anthropicUsage   `json:"usage"`
}

type responseBlock struct {
	Type string `json:"type"`
	// text 块
	Text string `json:"text,omitempty"`
	// thinking 块
	Thinking string `json:"thinking,omitempty"`
	// tool_use 块
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// MapStopReason 把 OpenAI 的 finish_reason 映射成 Anthropic 的 stop_reason。
// 未知值一律按 end_turn 处理，避免客户端收到不认识的枚举。
func MapStopReason(finishReason string) string {
	switch finishReason {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

// BuildResponse 把 Ark 非流式响应转成 Anthropic 响应。
// includeThinking 为真时把 reasoning_content 作为 thinking 块透出（排在最前）。
func BuildResponse(payload []byte, includeThinking bool) ([]byte, error) {
	var source ChatResponse
	if err := json.Unmarshal(payload, &source); err != nil {
		return nil, fmt.Errorf("解析 Ark 响应: %w", err)
	}
	if len(source.Choices) == 0 {
		return nil, fmt.Errorf("Ark 响应不含 choices")
	}
	choice := source.Choices[0]

	result := anthropicResponse{
		ID:         source.ID,
		Type:       "message",
		Role:       "assistant",
		Model:      source.Model,
		StopReason: MapStopReason(choice.FinishReason),
	}
	if source.Usage != nil {
		result.Usage = anthropicUsage{
			InputTokens:  source.Usage.PromptTokens,
			OutputTokens: source.Usage.CompletionTokens,
		}
	}

	// Anthropic 要求 thinking 块排在其他块之前。
	if includeThinking && choice.Message.ReasoningContent != "" {
		result.Content = append(result.Content, responseBlock{
			Type:     "thinking",
			Thinking: choice.Message.ReasoningContent,
		})
	}
	// Ark 在纯工具调用时 content 是空字符串，不能产出空 text 块。
	if choice.Message.Content != "" {
		result.Content = append(result.Content, responseBlock{
			Type: "text",
			Text: choice.Message.Content,
		})
	}
	for index, call := range choice.Message.ToolCalls {
		input := json.RawMessage(call.Function.Arguments)
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		if !json.Valid(input) {
			return nil, fmt.Errorf("tool_calls[%d].function.arguments 不是合法 JSON", index)
		}
		result.Content = append(result.Content, responseBlock{
			Type:  "tool_use",
			ID:    call.ID,
			Name:  call.Function.Name,
			Input: input,
		})
	}
	// content 为空数组时保持 [] 而不是 null，Claude Code 按数组解析。
	if result.Content == nil {
		result.Content = []responseBlock{}
	}
	return json.Marshal(result)
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/ark/translate/ -v`
Expected: 全部 PASS

- [ ] **Step 5: 提交**

```bash
git add internal/ark/translate/
git commit -m "feat(ark): 增加非流式响应转换

choices[0] 转 Anthropic content 块数组，thinking 块排最前，
tool_calls 的 arguments 解析为 input（非法 JSON 直接报错），
finish_reason 映射 stop_reason，content 为空字符串时不产出空 text 块。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 6: Ark 客户端与非流式接线

第一个可端到端验证的里程碑：`curl` 发非流式 `/v1/messages` 能打通 DeepSeek。

> Claude Code 始终使用流式请求，**本任务完成后 Claude Code 还不可用**，验收靠 curl。Claude Code 可用是
> Task 10 的里程碑。

**Files:**
- Create: `internal/ark/client.go`
- Create: `internal/ark/client_test.go`
- Modify: `internal/llm/provider.go`（新增 `StatusError` 接口）
- Modify: `internal/httpapi/errors.go:25-63`（优先用 `StatusError`，保留 smithy 兜底）
- Modify: `internal/config/config.go`
- Modify: `cmd/gateway/app.go`
- Modify: `configs/models.json`
- Modify: `.env.example`
- Test: `internal/config/config_test.go`（既有文件追加）

**Interfaces:**
- Consumes: `llm.Provider`（Task 1）、`translate.BuildRequest`（Task 3/4）、
  `translate.BuildResponse`（Task 5）
- Produces:
  - `llm.StatusError` 接口：`Status() int`、`Kind() string`、`ClientMessage() string`
  - `ark.Config{APIKey, BaseURL string; Timeout time.Duration}`
  - `ark.NewClient(cfg Config) *Client`，`*Client` 实现 `llm.Provider`
  - `ark.APIError`（实现 `llm.StatusError`）
  - `config.Config` 新增字段 `ArkAPIKey`、`ArkBaseURL`、`ArkTimeout`

- [ ] **Step 1: 给 llm 包加 StatusError 接口**

在 `internal/llm/provider.go` 末尾追加：

```go
// StatusError 由 provider 的错误实现，向 HTTP 层给出应回给客户端的状态码与错误类型。
// httpapi 优先按此接口分类，避免 HTTP 层耦合具体厂商的 SDK 错误类型。
type StatusError interface {
	error
	// Status 返回应回给客户端的 HTTP 状态码。
	Status() int
	// Kind 返回 Anthropic 错误类型（anthropic.ErrorType* 的值）。
	Kind() string
	// ClientMessage 返回可安全暴露给客户端的错误描述。
	ClientMessage() string
}
```

- [ ] **Step 2: 写 Ark 客户端的失败测试**

创建 `internal/ark/client_test.go`：

```go
package ark

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/llm"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewClient(Config{APIKey: "test-key", BaseURL: server.URL, Timeout: 5 * time.Second})
}

func TestInvokeTranslatesBothDirections(t *testing.T) {
	t.Parallel()

	var received map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("请求路径 = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &received); err != nil {
			t.Errorf("上游收到的 body 不是合法 JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r1","model":"m","choices":[{"finish_reason":"stop",
			"message":{"role":"assistant","content":"你好"}}],
			"usage":{"prompt_tokens":5,"completion_tokens":2}}`))
	})

	out, err := client.Invoke(context.Background(), "deepseek-v4-flash-ga-260731",
		[]byte(`{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("Invoke 失败: %v", err)
	}

	if received["model"] != "deepseek-v4-flash-ga-260731" {
		t.Fatalf("上游 model = %v", received["model"])
	}
	if received["stream"] != nil && received["stream"] != false {
		t.Fatalf("非流式请求不应带 stream=true，实际 %v", received["stream"])
	}

	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("返回值不是合法 JSON: %v", err)
	}
	if decoded["type"] != "message" {
		t.Fatalf("应返回 Anthropic 格式响应，实际 %#v", decoded)
	}
	content := decoded["content"].([]any)
	if content[0].(map[string]any)["text"] != "你好" {
		t.Fatalf("content = %#v", content)
	}
}

func TestInvokeRejectsImageBeforeCallingUpstream(t *testing.T) {
	t.Parallel()

	called := false
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	body := []byte(`{"max_tokens":64,"messages":[{"role":"user","content":[
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"x"}}]}]}`)
	_, err := client.Invoke(context.Background(), "m", body)
	if err == nil {
		t.Fatal("含图片的请求必须报错")
	}
	if called {
		t.Fatal("转换阶段被拒绝的请求不应发往上游")
	}

	var statusErr llm.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("错误应实现 llm.StatusError，实际 %T", err)
	}
	if statusErr.Status() != http.StatusBadRequest {
		t.Fatalf("Status = %d，期望 400", statusErr.Status())
	}
	if statusErr.Kind() != "invalid_request_error" {
		t.Fatalf("Kind = %q", statusErr.Kind())
	}
}

func TestInvokeMapsUpstreamErrorStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		status   int
		wantKind string
	}{
		{"401", http.StatusUnauthorized, "authentication_error"},
		{"403", http.StatusForbidden, "permission_error"},
		{"429", http.StatusTooManyRequests, "rate_limit_error"},
		{"400", http.StatusBadRequest, "invalid_request_error"},
		{"500", http.StatusInternalServerError, "api_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":{"code":"x","message":"上游拒绝"}}`))
			})

			_, err := client.Invoke(context.Background(), "m",
				[]byte(`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
			if err == nil {
				t.Fatal("上游非 2xx 必须报错")
			}
			var statusErr llm.StatusError
			if !errors.As(err, &statusErr) {
				t.Fatalf("错误应实现 llm.StatusError，实际 %T", err)
			}
			if statusErr.Status() != tc.status {
				t.Fatalf("Status = %d，期望 %d", statusErr.Status(), tc.status)
			}
			if statusErr.Kind() != tc.wantKind {
				t.Fatalf("Kind = %q，期望 %q", statusErr.Kind(), tc.wantKind)
			}
			if !strings.Contains(statusErr.ClientMessage(), "上游拒绝") {
				t.Fatalf("ClientMessage 应含上游描述，实际 %q", statusErr.ClientMessage())
			}
		})
	}
}

func TestInvokePropagatesContextCancel(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Invoke(ctx, "m", []byte(`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应透传 context.Canceled，实际 %v", err)
	}
}
```

- [ ] **Step 3: 运行测试确认失败**

Run: `go test ./internal/ark/ -v`
Expected: 编译失败，`undefined: NewClient`

- [ ] **Step 4: 实现 Ark 客户端**

创建 `internal/ark/client.go`：

```go
// Package ark 是火山方舟（Ark）网关的 provider 实现。
// 它对外遵守 Anthropic 语义，内部把请求与响应转换成 OpenAI Chat Completions 协议。
package ark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/anthropic"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/ark/translate"
)

// completionsPath 是 Chat Completions 的相对路径。
// Ark 的 BaseURL 已包含 /api/v3，故这里不再补版本段。
const completionsPath = "/chat/completions"

// maxErrorBodyBytes 限制读取错误响应体的长度，避免上游返回超大 body。
const maxErrorBodyBytes = 8 << 10

type Config struct {
	APIKey  string
	BaseURL string
	Timeout time.Duration
}

type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

func NewClient(cfg Config) *Client {
	return &Client{
		apiKey:  cfg.APIKey,
		baseURL: cfg.BaseURL,
		// 流式请求依赖 context 控制生命周期，这里不设 http.Client.Timeout，
		// 否则长流会被硬截断；超时通过 Config.Timeout 施加在非流式路径上。
		http: &http.Client{Transport: newTransport(cfg.Timeout)},
	}
}

func newTransport(timeout time.Duration) http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if timeout > 0 {
		// 只约束拿到响应头的时间，不约束读流式 body 的总时长。
		transport.ResponseHeaderTimeout = timeout
	}
	transport.MaxIdleConnsPerHost = 20
	return transport
}

// APIError 表示一次 Ark 调用失败，实现 llm.StatusError。
type APIError struct {
	StatusCode int
	Type       string
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ark 请求失败 (%d): %s", e.StatusCode, e.Message)
}

func (e *APIError) Status() int { return e.StatusCode }

func (e *APIError) Kind() string { return e.Type }

func (e *APIError) ClientMessage() string { return e.Message }

// rejectionError 把转换层的 Rejection 包成 llm.StatusError。
type rejectionError struct {
	rejection *translate.Rejection
}

func (e *rejectionError) Error() string { return e.rejection.Error() }

func (e *rejectionError) Status() int { return http.StatusBadRequest }

func (e *rejectionError) Kind() string { return e.rejection.Kind }

func (e *rejectionError) ClientMessage() string { return e.rejection.Message }

func (c *Client) Invoke(ctx context.Context, modelID string, body []byte) ([]byte, error) {
	response, err := c.post(ctx, modelID, body, false)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, c.errorFromResponse(response)
	}
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 Ark 响应体: %w", err)
	}
	// includeThinking 由请求是否显式开启 thinking 决定，见 Task 11；
	// 当前阶段固定不透出 thinking 块。
	return translate.BuildResponse(payload, false)
}

// post 完成请求转换并发起 HTTP 调用，调用方负责关闭 Body。
func (c *Client) post(ctx context.Context, modelID string, body []byte, stream bool) (*http.Response, error) {
	chatRequest, rejection := translate.BuildRequest(modelID, body, stream)
	if rejection != nil {
		return nil, &rejectionError{rejection: rejection}
	}
	encoded, err := json.Marshal(chatRequest)
	if err != nil {
		return nil, fmt.Errorf("编码 Ark 请求: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+completionsPath,
		bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("构造 Ark 请求: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Content-Type", "application/json")
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	}

	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("调用 Ark: %w", err)
	}
	return response, nil
}

// errorFromResponse 读取错误响应体并映射成 Anthropic 错误类型。
func (c *Client) errorFromResponse(response *http.Response) error {
	payload, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBodyBytes))

	message := string(bytes.TrimSpace(payload))
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err == nil && envelope.Error.Message != "" {
		message = envelope.Error.Message
	}
	if message == "" {
		message = "Ark 请求失败"
	}
	return &APIError{
		StatusCode: response.StatusCode,
		Type:       kindForStatus(response.StatusCode),
		Message:    message,
	}
}

func kindForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return anthropic.ErrorTypeInvalidRequest
	case http.StatusUnauthorized:
		return anthropic.ErrorTypeAuthentication
	case http.StatusForbidden:
		return anthropic.ErrorTypePermission
	case http.StatusNotFound:
		return anthropic.ErrorTypeNotFound
	case http.StatusTooManyRequests:
		return anthropic.ErrorTypeRateLimit
	case http.StatusServiceUnavailable:
		return anthropic.ErrorTypeOverloaded
	default:
		return anthropic.ErrorTypeAPI
	}
}

// Stream 在 Task 10 实现。
func (c *Client) Stream(ctx context.Context, modelID string, body []byte, emit func([]byte) error) error {
	return fmt.Errorf("ark 流式调用尚未实现")
}

// CountTokens 在 Task 11 实现。
func (c *Client) CountTokens(ctx context.Context, modelID string, body []byte) (int32, error) {
	return 0, fmt.Errorf("ark token 计数尚未实现")
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/ark/ -v`
Expected: 全部 PASS

- [ ] **Step 6: 让 httpapi 优先按 StatusError 分类**

`internal/httpapi/errors.go` 的 `classifyUpstreamError` 开头插入 StatusError 分支，并把日志与兜底文案
去掉 Bedrock 字样：

```go
func classifyUpstreamError(err error) (int, string, string) {
	// provider 自带分类时优先采用，避免 HTTP 层耦合具体厂商 SDK。
	var statusErr llm.StatusError
	if errors.As(err, &statusErr) {
		return statusErr.Status(), statusErr.Kind(), statusErr.ClientMessage()
	}

	status := http.StatusBadGateway
	kind := anthropic.ErrorTypeAPI
	message := "上游请求失败"
	// …以下保留既有的 smithy 分支不变…
```

`handleUpstreamError`（`:25-29`）的日志文案改为中立描述：

```go
	s.logger.Error("上游请求失败", "request_id", requestID(r.Context()), "status", status, "error", err)
```

import 增加 llm 包。

- [ ] **Step 7: 给既有 errors_test.go 补一个 StatusError 用例**

在 `internal/httpapi/errors_test.go` 追加：

```go
type stubStatusError struct{}

func (stubStatusError) Error() string         { return "stub" }
func (stubStatusError) Status() int           { return http.StatusTooManyRequests }
func (stubStatusError) Kind() string          { return anthropic.ErrorTypeRateLimit }
func (stubStatusError) ClientMessage() string { return "配额已用尽" }

func TestClassifyUpstreamErrorPrefersStatusError(t *testing.T) {
	t.Parallel()

	status, kind, message := classifyUpstreamError(stubStatusError{})
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d", status)
	}
	if kind != anthropic.ErrorTypeRateLimit {
		t.Fatalf("kind = %q", kind)
	}
	if message != "配额已用尽" {
		t.Fatalf("message = %q", message)
	}
}
```

- [ ] **Step 8: 加配置项**

`internal/config/config.go` 的常量区、结构体与加载逻辑分别追加：

```go
	DefaultArkBaseURL = "https://ark.cn-beijing.volces.com/api/v3"
	DefaultArkTimeout = 120 * time.Second
```

```go
	ArkAPIKey  string
	ArkBaseURL string
	ArkTimeout time.Duration
```

`LoadFromLookup` 的初始化块里加：

```go
		ArkAPIKey:  strings.TrimSpace(value(lookup, "ARK_API_KEY")),
		ArkBaseURL: valueOrDefault(lookup, "ARK_BASE_URL", DefaultArkBaseURL),
		ArkTimeout: DefaultArkTimeout,
```

在 `StreamQueueTimeout` 校验之后追加：

```go
	if cfg.ArkTimeout, err = positiveDuration(lookup, "ARK_TIMEOUT", cfg.ArkTimeout); err != nil {
		return Config{}, err
	}
```

> `ARK_API_KEY` 允许为空 —— 只有 `models.json` 里登记了 ark 模型时才必填，该校验放在 Step 9 的启动装配处，
> 这样只用 Bedrock 的部署无需提供 Ark 凭证。

在 `internal/config/config_test.go` 追加：

```go
func TestLoadFromLookupArkDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFromLookup(stubLookup(map[string]string{
		"AWS_REGION":      "us-east-1",
		"GATEWAY_API_KEY": "k",
	}))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.ArkBaseURL != DefaultArkBaseURL {
		t.Fatalf("ArkBaseURL = %q", cfg.ArkBaseURL)
	}
	if cfg.ArkAPIKey != "" {
		t.Fatal("未设置 ARK_API_KEY 时应为空而不是报错")
	}
	if cfg.ArkTimeout != DefaultArkTimeout {
		t.Fatalf("ArkTimeout = %v", cfg.ArkTimeout)
	}
}
```

> `stubLookup` 是该测试文件中既有的辅助函数；若命名不同，沿用文件内现有写法。

- [ ] **Step 9: 装配 ark provider 并校验凭证**

`cmd/gateway/app.go` 的 `run` 里，`providers` 构造替换为：

```go
	providers, err := buildProviders(cfg, models, runtime)
	if err != nil {
		return err
	}
	server := newHTTPServer(cfg, models, providers, logger)
```

同文件新增：

```go
// buildProviders 按模型目录实际用到的 provider 装配实现。
// 目录里登记了 ark 模型却没有凭证时直接启动失败，不留到首个请求才报错。
func buildProviders(cfg config.Config, models *modelcatalog.Manager, runtime bedrock.Runtime) (map[string]llm.Provider, error) {
	providers := map[string]llm.Provider{llm.ProviderBedrock: runtime}
	if !models.UsesProvider(llm.ProviderArk) {
		return providers, nil
	}
	if cfg.ArkAPIKey == "" {
		return nil, fmt.Errorf("模型配置登记了 ark 模型，必须设置 ARK_API_KEY")
	}
	providers[llm.ProviderArk] = ark.NewClient(ark.Config{
		APIKey:  cfg.ArkAPIKey,
		BaseURL: cfg.ArkBaseURL,
		Timeout: cfg.ArkTimeout,
	})
	return providers, nil
}
```

`run` 内 `newRuntime` 之后的 `models` 变量已是 `*modelcatalog.Manager`，可直接传入。
import 增加 `fmt`、`internal/ark`、`internal/llm`。

`internal/modelcatalog` 新增查询方法（`catalog.go` 与 `manager.go` 各一个）：

```go
// UsesProvider 报告目录中是否存在归属该 provider 的模型。
func (c *Catalog) UsesProvider(provider string) bool {
	if c == nil {
		return false
	}
	for _, resolution := range c.resolutions {
		if resolution.Provider == provider {
			return true
		}
	}
	return false
}
```

```go
func (m *Manager) UsesProvider(provider string) bool {
	return m.current.Load().UsesProvider(provider)
}
```

- [ ] **Step 10: 登记模型与环境变量样例**

`configs/models.json` 的 `models` 数组追加（保留既有两个 Anthropic 模型不动）：

```json
    {
      "id": "deepseek-v4-flash-ga-260731",
      "provider": "ark",
      "aliases": [
        "deepseek-v4-flash",
        "ds-flash"
      ]
    }
```

`.env.example` 追加：

```
# 火山方舟 Ark（仅当 models.json 登记了 provider=ark 的模型时必填）
ARK_API_KEY=
ARK_BASE_URL=https://ark.cn-beijing.volces.com/api/v3
ARK_TIMEOUT=120s
```

- [ ] **Step 11: 全量测试与构建**

Run: `make test && make vet && make build`
Expected: 全部通过

- [ ] **Step 12: 真机验证非流式打通**

启动网关（`ARK_API_KEY` 取 nova 的 `DOUBAO_ARK_API_KEY` 值），然后：

```bash
curl -s -X POST http://127.0.0.1:8080/v1/messages \
  -H "x-api-key: $GATEWAY_API_KEY" \
  -H "content-type: application/json" \
  -d '{"model":"ds-flash","max_tokens":64,"messages":[{"role":"user","content":"只回复 ok"}]}' | jq .
```

Expected: 返回 `{"type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],
"stop_reason":"end_turn","usage":{...}}`。若返回 `authentication_error`，检查 `ARK_API_KEY`。

再验证工具调用闭环：

```bash
curl -s -X POST http://127.0.0.1:8080/v1/messages \
  -H "x-api-key: $GATEWAY_API_KEY" -H "content-type: application/json" \
  -d '{"model":"ds-flash","max_tokens":256,
       "messages":[{"role":"user","content":"北京天气怎么样？请调用工具。"}],
       "tools":[{"name":"get_weather","description":"查询天气",
                 "input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]}' | jq .
```

Expected: `stop_reason` 为 `tool_use`，`content` 含一个 `tool_use` 块且 `input` 是对象（不是字符串）。

- [ ] **Step 13: 提交**

```bash
git add internal/ark/ internal/llm/ internal/httpapi/ internal/config/ internal/modelcatalog/ cmd/gateway/ configs/models.json .env.example
git commit -m "feat(ark): 接入火山方舟 provider 的非流式调用

新增 ark provider（裸 net/http，无新依赖），装配时校验
登记了 ark 模型必须提供 ARK_API_KEY。httpapi 错误分类改为优先
采用 provider 自带的 llm.StatusError，保留 smithy 兜底。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 7: OpenAI SSE 行解析

把 Ark 的 SSE 字节流切成结构化 chunk。参考 nova `apps/worker-go/internal/service/llm_tools_stream.go:209-302`。

**Files:**
- Create: `internal/ark/translate/chunk.go`（chunk 线格式，状态机与解析器共用）
- Create: `internal/ark/sse.go`
- Test: `internal/ark/sse_test.go`

**Interfaces:**
- Produces:
  - `translate.StreamChunk`、`StreamChoice`、`StreamDelta`、`StreamToolCall`
  - `ark.scanSSE(r io.Reader, onChunk func(translate.StreamChunk) error) (sawDone bool, err error)`
    （包内小写，仅 `client.go` 使用）

- [ ] **Step 1: 写失败测试**

创建 `internal/ark/sse_test.go`：

```go
package ark

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/ark/translate"
)

// 取自 2026-09-22 对 Ark 的真实流式响应（见设计文档 §2）。
const arkToolStream = `data: {"choices":[{"delta":{"content":"","reasoning_content":"The","role":"assistant"},"index":0}],"id":"s1","model":"m","object":"chat.completion.chunk","usage":null}

data: {"choices":[{"delta":{"content":"","role":"assistant","tool_calls":[{"function":{"arguments":"","name":"get_weather"},"id":"call_1","index":0,"type":"function"}]},"index":0}],"id":"s1","model":"m","object":"chat.completion.chunk","usage":null}

data: {"choices":[{"delta":{"content":"","role":"assistant","tool_calls":[{"function":{"arguments":"{\"city\": \""},"index":0}]},"index":0}],"id":"s1","model":"m","object":"chat.completion.chunk","usage":null}

data: {"choices":[{"delta":{"content":"","role":"assistant"},"finish_reason":"tool_calls","index":0}],"id":"s1","model":"m","object":"chat.completion.chunk","usage":{"completion_tokens":81,"prompt_tokens":397}}

data: {"choices":[],"id":"s1","model":"m","object":"chat.completion.chunk","usage":{"completion_tokens":81,"prompt_tokens":397}}

data: [DONE]
`

func TestScanSSEParsesRealStream(t *testing.T) {
	t.Parallel()

	var chunks []translate.StreamChunk
	sawDone, err := scanSSE(strings.NewReader(arkToolStream), func(chunk translate.StreamChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !sawDone {
		t.Fatal("应识别到 [DONE]")
	}
	if len(chunks) != 5 {
		t.Fatalf("chunk 数 = %d，期望 5（[DONE] 不产出 chunk）", len(chunks))
	}

	if chunks[0].Choices[0].Delta.ReasoningContent != "The" {
		t.Fatalf("首个 chunk 的 reasoning_content = %q", chunks[0].Choices[0].Delta.ReasoningContent)
	}

	first := chunks[1].Choices[0].Delta.ToolCalls[0]
	if first.ID != "call_1" || first.Function.Name != "get_weather" || first.Index != 0 {
		t.Fatalf("工具首发 chunk = %#v", first)
	}
	if first.Function.Arguments != "" {
		t.Fatalf("首发 chunk 的 arguments 应为空，实际 %q", first.Function.Arguments)
	}

	fragment := chunks[2].Choices[0].Delta.ToolCalls[0]
	if fragment.Function.Arguments != `{"city": "` {
		t.Fatalf("参数分片应原样保留转义后的文本，实际 %q", fragment.Function.Arguments)
	}
	if fragment.ID != "" {
		t.Fatal("后续分片不带 id，解析器不应凭空补值")
	}

	if chunks[3].Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q", chunks[3].Choices[0].FinishReason)
	}
	if chunks[4].Usage == nil || chunks[4].Usage.PromptTokens != 397 {
		t.Fatalf("末尾 usage chunk = %#v", chunks[4].Usage)
	}
	if len(chunks[4].Choices) != 0 {
		t.Fatal("末尾 usage chunk 的 choices 应为空")
	}
}

func TestScanSSEIgnoresCommentsAndBlankLines(t *testing.T) {
	t.Parallel()

	stream := ": ping\n\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"index\":0}]}\n\ndata: [DONE]\n"
	var count int
	sawDone, err := scanSSE(strings.NewReader(stream), func(translate.StreamChunk) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !sawDone || count != 1 {
		t.Fatalf("sawDone=%v count=%d，期望 true/1", sawDone, count)
	}
}

func TestScanSSEReportsMissingDone(t *testing.T) {
	t.Parallel()

	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"index\":0}]}\n\n"
	sawDone, err := scanSSE(strings.NewReader(stream), func(translate.StreamChunk) error { return nil })
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if sawDone {
		t.Fatal("流里没有 [DONE] 时 sawDone 必须为 false")
	}
}

func TestScanSSERejectsMalformedChunk(t *testing.T) {
	t.Parallel()

	stream := "data: {不是JSON\n\n"
	if _, err := scanSSE(strings.NewReader(stream), func(translate.StreamChunk) error { return nil }); err == nil {
		t.Fatal("畸形 chunk 必须报错，不能静默跳过")
	}
}

func TestScanSSEPropagatesCallbackError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("下游中止")
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"index\":0}]}\n\n"
	_, err := scanSSE(strings.NewReader(stream), func(translate.StreamChunk) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("回调错误应透传，实际 %v", err)
	}
}

func TestScanSSEHandlesLongLine(t *testing.T) {
	t.Parallel()

	// 工具参数可能很长，单行远超 bufio.Scanner 的 64KiB 默认上限。
	long := strings.Repeat("a", 200*1024)
	stream := fmt.Sprintf(
		"data: {\"choices\":[{\"delta\":{\"content\":\"\",\"tool_calls\":[{\"function\":{\"arguments\":%q},\"index\":0}]},\"index\":0}]}\n\ndata: [DONE]\n",
		long)

	var got string
	if _, err := scanSSE(strings.NewReader(stream), func(chunk translate.StreamChunk) error {
		if len(chunk.Choices) > 0 && len(chunk.Choices[0].Delta.ToolCalls) > 0 {
			got = chunk.Choices[0].Delta.ToolCalls[0].Function.Arguments
		}
		return nil
	}); err != nil {
		t.Fatalf("超长行解析失败: %v", err)
	}
	if len(got) != len(long) {
		t.Fatalf("超长参数被截断：得到 %d 字节，期望 %d", len(got), len(long))
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/ark/ -run ScanSSE -v`
Expected: 编译失败，`undefined: scanSSE`

- [ ] **Step 3: 定义 chunk 线格式**

创建 `internal/ark/translate/chunk.go`：

```go
package translate

// StreamChunk 是 Ark 流式响应的单个 chunk（object=chat.completion.chunk）。
type StreamChunk struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
	// Usage 仅在末尾若干 chunk 出现（需请求时带 stream_options.include_usage）。
	Usage *Usage `json:"usage"`
}

type StreamChoice struct {
	Index        int         `json:"index"`
	FinishReason string      `json:"finish_reason"`
	Delta        StreamDelta `json:"delta"`
}

// StreamDelta 是一个增量片段。
// 注意 Content 恒为存在的字符串字段，纯思考阶段它是空串而非缺失 ——
// 判断是否要开 text 块必须看内容是否非空，不能看字段是否存在。
type StreamDelta struct {
	Role             string           `json:"role"`
	Content          string           `json:"content"`
	ReasoningContent string           `json:"reasoning_content"`
	ToolCalls        []StreamToolCall `json:"tool_calls"`
}

// StreamToolCall 的 Index 是工具在本次回复中的序号（非 Anthropic 的 content block 序号）。
// 首发分片带 ID 与 Function.Name，后续分片只有 Function.Arguments。
type StreamToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
		// Arguments 是 JSON 文本的一个片段，可能断在转义符或多字节字符中间。
		Arguments string `json:"arguments"`
	} `json:"function"`
}
```

- [ ] **Step 4: 实现 SSE 行解析**

创建 `internal/ark/sse.go`：

```go
package ark

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/ark/translate"
)

const (
	dataPrefix = "data:"
	doneMarker = "[DONE]"
	// 工具参数可能很长，单行上限放宽到 4MiB。
	maxSSELineBytes = 4 << 20
	initialSSEBuf   = 64 << 10
)

// scanSSE 逐行解析 OpenAI 兼容的 SSE 流，对每个 data chunk 调用 onChunk。
// 返回是否见到 [DONE]，供调用方判断流是否完整结束。
func scanSSE(r io.Reader, onChunk func(translate.StreamChunk) error) (bool, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, initialSSEBuf), maxSSELineBytes)

	sawDone := false
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		// 空行是事件分隔符，以 ':' 开头的是注释（含心跳），都跳过。
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, dataPrefix) {
			// 忽略 event: / id: 等其他字段，Ark 当前不使用。
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(line, dataPrefix))
		if payload == "" {
			continue
		}
		if payload == doneMarker {
			sawDone = true
			continue
		}

		var chunk translate.StreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return sawDone, fmt.Errorf("解析 Ark 流事件: %w", err)
		}
		if err := onChunk(chunk); err != nil {
			return sawDone, err
		}
	}
	if err := scanner.Err(); err != nil {
		return sawDone, fmt.Errorf("读取 Ark 流: %w", err)
	}
	return sawDone, nil
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/ark/ -v`
Expected: 全部 PASS

- [ ] **Step 6: 提交**

```bash
git add internal/ark/
git commit -m "feat(ark): 增加 OpenAI 兼容 SSE 行解析

chunk 线格式与解析器分离，单行上限放宽到 4MiB 以容纳长工具参数，
注释行与空行跳过，畸形 chunk 直接报错不静默跳过，
并返回是否见到 [DONE] 供断流判断。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 8: 事件状态机 —— 消息生命周期与 thinking / text

本任务与 Task 9 是整个计划的核心。状态机把 Ark 的扁平 delta 流重组成 Anthropic 的嵌套事件序列。

**Files:**
- Create: `internal/ark/translate/events.go`
- Test: `internal/ark/translate/events_test.go`

**Interfaces:**
- Consumes: `StreamChunk`、`Usage`、`MapStopReason`（Task 5/7）
- Produces:
  - `translate.NewEventEmitter(modelID string, includeThinking bool, emit func([]byte) error) *EventEmitter`
  - `(*EventEmitter).Consume(chunk StreamChunk) error`
  - `(*EventEmitter).Finish(sawDone bool) error`

**关键约束**（Spec §5）：

1. Anthropic 的 `index` 是**全局递增的 content block 序号**，与 OpenAI 的 `tool_calls[].index` 不是一回事。
2. `delta.content` 恒为存在的字符串，纯思考阶段是空串 —— **必须判非空**才开 text 块。
3. 每个 `content_block_start` 必须有配对的 `content_block_stop`。

- [ ] **Step 1: 写失败测试**

创建 `internal/ark/translate/events_test.go`：

```go
package translate

import (
	"encoding/json"
	"testing"
)

// collectEvents 收集状态机产出的事件，按 type 顺序返回，便于断言序列。
type eventLog struct {
	events []map[string]any
}

func (l *eventLog) emit(payload []byte) error {
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return err
	}
	l.events = append(l.events, decoded)
	return nil
}

func (l *eventLog) types() []string {
	result := make([]string, 0, len(l.events))
	for _, event := range l.events {
		result = append(result, event["type"].(string))
	}
	return result
}

func (l *eventLog) ofType(t *testing.T, kind string) []map[string]any {
	t.Helper()
	var result []map[string]any
	for _, event := range l.events {
		if event["type"] == kind {
			result = append(result, event)
		}
	}
	return result
}

func assertTypes(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("事件序列长度 = %d，期望 %d\n实际: %v\n期望: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("事件[%d] = %q，期望 %q\n实际: %v\n期望: %v", i, got[i], want[i], got, want)
		}
	}
}

func textChunk(content string) StreamChunk {
	return StreamChunk{
		ID:      "s1",
		Model:   "m",
		Choices: []StreamChoice{{Index: 0, Delta: StreamDelta{Role: "assistant", Content: content}}},
	}
}

func reasoningChunk(reasoning string) StreamChunk {
	return StreamChunk{
		ID:    "s1",
		Model: "m",
		Choices: []StreamChoice{{Index: 0, Delta: StreamDelta{
			Role: "assistant", Content: "", ReasoningContent: reasoning,
		}}},
	}
}

func finishChunk(reason string, prompt, completion int) StreamChunk {
	return StreamChunk{
		ID:      "s1",
		Model:   "m",
		Choices: []StreamChoice{{Index: 0, FinishReason: reason, Delta: StreamDelta{Role: "assistant"}}},
		Usage:   &Usage{PromptTokens: prompt, CompletionTokens: completion},
	}
}

func TestEventEmitterTextOnlySequence(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("deepseek-v4-flash-ga-260731", false, log.emit)

	for _, chunk := range []StreamChunk{textChunk("你"), textChunk("好"), finishChunk("stop", 10, 2)} {
		if err := emitter.Consume(chunk); err != nil {
			t.Fatalf("Consume 失败: %v", err)
		}
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	assertTypes(t, log.types(), []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	})

	start := log.events[0]["message"].(map[string]any)
	if start["role"] != "assistant" || start["type"] != "message" {
		t.Fatalf("message_start.message = %#v", start)
	}
	if start["model"] != "deepseek-v4-flash-ga-260731" {
		t.Fatalf("model = %v", start["model"])
	}
	if start["id"] == "" || start["id"] == nil {
		t.Fatal("message_start 必须带 id")
	}

	block := log.events[1]["content_block"].(map[string]any)
	if block["type"] != "text" {
		t.Fatalf("首块应为 text，实际 %v", block["type"])
	}
	if log.events[1]["index"].(float64) != 0 {
		t.Fatalf("首块 index 应为 0，实际 %v", log.events[1]["index"])
	}

	delta := log.events[2]["delta"].(map[string]any)
	if delta["type"] != "text_delta" || delta["text"] != "你" {
		t.Fatalf("delta = %#v", delta)
	}

	messageDelta := log.events[5]
	if messageDelta["delta"].(map[string]any)["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %#v", messageDelta["delta"])
	}
	usage := messageDelta["usage"].(map[string]any)
	if usage["output_tokens"].(float64) != 2 {
		t.Fatalf("output_tokens = %v", usage["output_tokens"])
	}
	if usage["input_tokens"].(float64) != 10 {
		t.Fatalf("message_delta 应补上真实 input_tokens，实际 %v", usage["input_tokens"])
	}
}

func TestEventEmitterSkipsEmptyContent(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	// 纯思考阶段的 chunk：content 是空串，reasoning 未开启透出。
	for _, chunk := range []StreamChunk{reasoningChunk("思考"), textChunk(""), finishChunk("stop", 5, 1)} {
		if err := emitter.Consume(chunk); err != nil {
			t.Fatalf("Consume 失败: %v", err)
		}
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	// 全程没有非空内容，不应开任何 content block。
	assertTypes(t, log.types(), []string{"message_start", "message_delta", "message_stop"})
}

func TestEventEmitterThinkingThenText(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", true, log.emit)

	chunks := []StreamChunk{
		reasoningChunk("先"), reasoningChunk("想"),
		textChunk("答"), finishChunk("stop", 8, 3),
	}
	for _, chunk := range chunks {
		if err := emitter.Consume(chunk); err != nil {
			t.Fatalf("Consume 失败: %v", err)
		}
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	assertTypes(t, log.types(), []string{
		"message_start",
		"content_block_start",  // thinking
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",   // thinking 关闭
		"content_block_start",  // text
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	})

	thinkingStart := log.events[1]
	if thinkingStart["content_block"].(map[string]any)["type"] != "thinking" {
		t.Fatalf("首块应为 thinking，实际 %#v", thinkingStart["content_block"])
	}
	if thinkingStart["index"].(float64) != 0 {
		t.Fatalf("thinking 块 index = %v", thinkingStart["index"])
	}

	thinkingDelta := log.events[2]["delta"].(map[string]any)
	if thinkingDelta["type"] != "thinking_delta" || thinkingDelta["thinking"] != "先" {
		t.Fatalf("thinking_delta = %#v", thinkingDelta)
	}

	textStart := log.events[5]
	if textStart["content_block"].(map[string]any)["type"] != "text" {
		t.Fatalf("第二块应为 text，实际 %#v", textStart["content_block"])
	}
	if textStart["index"].(float64) != 1 {
		t.Fatalf("text 块 index 应为 1（全局递增），实际 %v", textStart["index"])
	}
}

func TestEventEmitterDropsReasoningWhenDisabled(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	for _, chunk := range []StreamChunk{reasoningChunk("不该出现"), textChunk("答"), finishChunk("stop", 1, 1)} {
		if err := emitter.Consume(chunk); err != nil {
			t.Fatalf("Consume 失败: %v", err)
		}
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	for _, event := range log.events {
		if encoded, _ := json.Marshal(event); string(encoded) != "" {
			if block, ok := event["content_block"].(map[string]any); ok && block["type"] == "thinking" {
				t.Fatal("includeThinking=false 时不应产出 thinking 块")
			}
		}
	}
	assertTypes(t, log.types(), []string{
		"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	})
}

func TestEventEmitterIgnoresUsageOnlyChunk(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	if err := emitter.Consume(textChunk("hi")); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	if err := emitter.Consume(finishChunk("stop", 7, 4)); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	// 末尾纯 usage chunk：choices 为空，不应产出任何事件。
	before := len(log.events)
	if err := emitter.Consume(StreamChunk{ID: "s1", Model: "m", Usage: &Usage{PromptTokens: 7, CompletionTokens: 4}}); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	if len(log.events) != before {
		t.Fatalf("纯 usage chunk 不应产出事件，新增了 %d 个", len(log.events)-before)
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}
}

func TestEventEmitterStopReasonMapping(t *testing.T) {
	t.Parallel()

	cases := []struct{ finish, want string }{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"tool_calls", "tool_use"},
	}
	for _, tc := range cases {
		t.Run(tc.finish, func(t *testing.T) {
			t.Parallel()
			log := &eventLog{}
			emitter := NewEventEmitter("m", false, log.emit)
			if err := emitter.Consume(textChunk("x")); err != nil {
				t.Fatalf("Consume 失败: %v", err)
			}
			if err := emitter.Consume(finishChunk(tc.finish, 1, 1)); err != nil {
				t.Fatalf("Consume 失败: %v", err)
			}
			if err := emitter.Finish(true); err != nil {
				t.Fatalf("Finish 失败: %v", err)
			}
			deltas := log.ofType(t, "message_delta")
			if len(deltas) != 1 {
				t.Fatalf("message_delta 数 = %d", len(deltas))
			}
			if got := deltas[0]["delta"].(map[string]any)["stop_reason"]; got != tc.want {
				t.Fatalf("stop_reason = %v，期望 %q", got, tc.want)
			}
		})
	}
}

func TestEventEmitterFinishWithoutFinishReasonFails(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)
	if err := emitter.Consume(textChunk("半句")); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}

	// 没收到 finish_reason 就结束 —— 上游断流，必须报错让上层按可重试处理。
	if err := emitter.Finish(true); err == nil {
		t.Fatal("缺少 finish_reason 时 Finish 必须报错")
	}
}

func TestEventEmitterFinishBeforeAnyChunkFails(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)
	if err := emitter.Finish(false); err == nil {
		t.Fatal("一个 chunk 都没收到时 Finish 必须报错")
	}
	if len(log.events) != 0 {
		t.Fatal("没有 chunk 时不应产出任何事件")
	}
}

func TestEventEmitterConsumeAfterFinishIsIgnored(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)
	if err := emitter.Consume(textChunk("x")); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	if err := emitter.Consume(finishChunk("stop", 1, 1)); err != nil {
		t.Fatalf("Consume 失败: %v", err)
	}
	before := len(log.events)
	// 上游在 finish 之后又发内容 chunk：忽略，不能再开块（message_stop 已发出）。
	if err := emitter.Consume(textChunk("多余")); err != nil {
		t.Fatalf("finish 后的多余 chunk 应被忽略而非报错: %v", err)
	}
	if len(log.events) != before {
		t.Fatal("message_stop 之后不应再产出事件")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/ark/translate/ -run EventEmitter -v`
Expected: 编译失败，`undefined: NewEventEmitter`

- [ ] **Step 3: 实现状态机**

创建 `internal/ark/translate/events.go`：

```go
package translate

import (
	"encoding/json"
	"fmt"
)

// 块类型。
const (
	blockKindThinking = "thinking"
	blockKindText     = "text"
	blockKindToolUse  = "tool_use"
)

// EventEmitter 把 Ark 的扁平 delta 流重组成 Anthropic 的事件序列。
//
// 不变式：
//   - index 是全局递增的 content block 序号，与 OpenAI 的 tool_calls[].index 无关；
//   - 每个 content_block_start 都有配对的 content_block_stop；
//   - message_start 必为首个事件，message_stop 必为末个事件。
//
// 实测 Ark 会先吐完 reasoning_content 才开始 content / tool_calls，
// 恰好满足 Anthropic 要求 thinking 块排在最前，因此无需缓冲重排。
type EventEmitter struct {
	emit            func([]byte) error
	modelID         string
	includeThinking bool

	started   bool
	finished  bool
	messageID string

	nextIndex int
	openKind  string
	openIndex int

	// toolBlocks 把 OpenAI 的 tool_calls[].index 映射到 Anthropic 的 block index。
	toolBlocks map[int]int

	inputTokens  int
	outputTokens int
}

func NewEventEmitter(modelID string, includeThinking bool, emit func([]byte) error) *EventEmitter {
	return &EventEmitter{
		emit:            emit,
		modelID:         modelID,
		includeThinking: includeThinking,
		toolBlocks:      make(map[int]int),
	}
}

// Consume 处理一个上游 chunk，按需产出 Anthropic 事件。
func (e *EventEmitter) Consume(chunk StreamChunk) error {
	if e.finished {
		// message_stop 之后上游仍在发数据：忽略，避免产出非法事件。
		return nil
	}
	if chunk.Usage != nil {
		e.inputTokens = chunk.Usage.PromptTokens
		e.outputTokens = chunk.Usage.CompletionTokens
	}
	if len(chunk.Choices) == 0 {
		// 末尾的纯 usage chunk。
		return nil
	}
	if err := e.ensureStarted(chunk); err != nil {
		return err
	}

	choice := chunk.Choices[0]
	if err := e.consumeReasoning(choice.Delta.ReasoningContent); err != nil {
		return err
	}
	if err := e.consumeText(choice.Delta.Content); err != nil {
		return err
	}
	if err := e.consumeToolCalls(choice.Delta.ToolCalls); err != nil {
		return err
	}
	if choice.FinishReason != "" {
		return e.complete(MapStopReason(choice.FinishReason))
	}
	return nil
}

// Finish 在上游流结束后收尾。sawDone 表示是否见到 [DONE]。
func (e *EventEmitter) Finish(sawDone bool) error {
	if e.finished {
		return nil
	}
	if !e.started {
		return fmt.Errorf("Ark 流未产出任何事件")
	}
	// 收到内容但没收到 finish_reason，说明流被截断。
	// 此时已有 content_block_start 发出，客户端在等后续事件，必须报错让上层按可重试处理。
	return fmt.Errorf("Ark 流意外结束：缺少 finish_reason（sawDone=%v）", sawDone)
}

func (e *EventEmitter) ensureStarted(chunk StreamChunk) error {
	if e.started {
		return nil
	}
	e.started = true
	e.messageID = "msg_" + chunk.ID
	if chunk.ID == "" {
		e.messageID = "msg_ark"
	}
	model := chunk.Model
	if model == "" {
		model = e.modelID
	}
	// 上游只在流末尾给 usage，这里先填 0，结束时在 message_delta 里补真实值。
	return e.write(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            e.messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func (e *EventEmitter) consumeReasoning(reasoning string) error {
	if reasoning == "" || !e.includeThinking {
		return nil
	}
	if err := e.openBlockOfKind(blockKindThinking, nil); err != nil {
		return err
	}
	return e.write(map[string]any{
		"type":  "content_block_delta",
		"index": e.openIndex,
		"delta": map[string]any{"type": "thinking_delta", "thinking": reasoning},
	})
}

// consumeText 只在内容非空时开块 —— Ark 的 delta.content 恒存在，
// 纯思考阶段是空串，凭字段存在开块会产出空的 text 块并破坏事件序列。
func (e *EventEmitter) consumeText(content string) error {
	if content == "" {
		return nil
	}
	if err := e.openBlockOfKind(blockKindText, nil); err != nil {
		return err
	}
	return e.write(map[string]any{
		"type":  "content_block_delta",
		"index": e.openIndex,
		"delta": map[string]any{"type": "text_delta", "text": content},
	})
}

// consumeToolCalls 在 Task 9 实现。
func (e *EventEmitter) consumeToolCalls(calls []StreamToolCall) error {
	return nil
}

// openBlockOfKind 确保当前打开的块是指定类型；类型不同则先关闭旧块再开新块。
// contentBlock 为 nil 时按 kind 生成默认的空块体。
func (e *EventEmitter) openBlockOfKind(kind string, contentBlock map[string]any) error {
	if e.openKind == kind && kind != blockKindToolUse {
		return nil
	}
	if err := e.closeOpenBlock(); err != nil {
		return err
	}
	if contentBlock == nil {
		switch kind {
		case blockKindThinking:
			contentBlock = map[string]any{"type": blockKindThinking, "thinking": ""}
		case blockKindText:
			contentBlock = map[string]any{"type": blockKindText, "text": ""}
		default:
			return fmt.Errorf("缺少 %s 块的块体", kind)
		}
	}
	e.openKind = kind
	e.openIndex = e.nextIndex
	e.nextIndex++
	return e.write(map[string]any{
		"type":          "content_block_start",
		"index":         e.openIndex,
		"content_block": contentBlock,
	})
}

func (e *EventEmitter) closeOpenBlock() error {
	if e.openKind == "" {
		return nil
	}
	index := e.openIndex
	e.openKind = ""
	return e.write(map[string]any{"type": "content_block_stop", "index": index})
}

// complete 关闭当前块并收尾整条消息。
func (e *EventEmitter) complete(stopReason string) error {
	if err := e.closeOpenBlock(); err != nil {
		return err
	}
	// input_tokens 在 message_start 时还拿不到，这里一并给出真实值。
	if err := e.write(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{
			"input_tokens":  e.inputTokens,
			"output_tokens": e.outputTokens,
		},
	}); err != nil {
		return err
	}
	e.finished = true
	return e.write(map[string]any{"type": "message_stop"})
}

func (e *EventEmitter) write(event map[string]any) error {
	encoded, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("编码 Anthropic 事件: %w", err)
	}
	return e.emit(encoded)
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/ark/translate/ -run EventEmitter -v`
Expected: 全部 PASS

- [ ] **Step 5: 全量测试**

Run: `make test`
Expected: 全部 PASS

- [ ] **Step 6: 提交**

```bash
git add internal/ark/translate/
git commit -m "feat(ark): 增加流式事件状态机的消息与文本路径

把 Ark 的扁平 delta 重组成 Anthropic 事件序列，index 全局递增，
每个 content_block_start 都配对 stop。delta.content 恒存在，
故只在非空时开 text 块。上游只在末尾给 usage，input_tokens 在
message_delta 补齐。缺少 finish_reason 视为断流报错。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 9: 事件状态机 —— 工具调用与并行 index 映射

填充 Task 8 留下的 `consumeToolCalls`。这是 Claude Code 能否真正可用的关键路径。

**Files:**
- Modify: `internal/ark/translate/events.go`（实现 `consumeToolCalls`、新增 `openToolBlock`）
- Test: `internal/ark/translate/events_tools_test.go`

**Interfaces:**
- Consumes: `EventEmitter`、`StreamToolCall`（Task 7/8）
- Produces: 无新导出符号

**设计取舍（重要）**：`arguments` 分片按到达顺序**流式**转发为 `input_json_delta`，不做缓冲。
实测 Ark 是分组吐出的（reasoning 全部 → tool 0 全部 → tool 1 全部），不存在交错。
若真机出现交错（某工具的分片到达时它的块已被后来的工具关闭），状态机**报错而非产出损坏序列**。
备选方案：改为缓冲每个 tool 的完整参数、在 `complete` 时按 index 顺序一次性产出块 —— 因为 Claude Code
要等 `tool_use` 块完整才执行工具，缓冲不损失功能，只损失参数的打字机效果。**只有真机确认交错时才切换。**

- [ ] **Step 1: 写失败测试**

创建 `internal/ark/translate/events_tools_test.go`：

```go
package translate

import "testing"

// toolStartChunk 是工具首发分片：带 id、name 与空 arguments。
func toolStartChunk(toolIndex int, id, name string) StreamChunk {
	call := StreamToolCall{Index: toolIndex, ID: id, Type: "function"}
	call.Function.Name = name
	return StreamChunk{
		ID:      "s1",
		Model:   "m",
		Choices: []StreamChoice{{Index: 0, Delta: StreamDelta{Role: "assistant", ToolCalls: []StreamToolCall{call}}}},
	}
}

// toolArgChunk 是参数分片：只有 index 与 arguments。
func toolArgChunk(toolIndex int, arguments string) StreamChunk {
	call := StreamToolCall{Index: toolIndex}
	call.Function.Arguments = arguments
	return StreamChunk{
		ID:      "s1",
		Model:   "m",
		Choices: []StreamChoice{{Index: 0, Delta: StreamDelta{Role: "assistant", ToolCalls: []StreamToolCall{call}}}},
	}
}

func TestEventEmitterSingleToolCall(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	// 复刻实测的真实分片边界（见设计文档 §2）。
	chunks := []StreamChunk{
		toolStartChunk(0, "call_1", "get_weather"),
		toolArgChunk(0, `{"city": "`),
		toolArgChunk(0, "北京"),
		toolArgChunk(0, `"`),
		toolArgChunk(0, "}"),
		finishChunk("tool_calls", 397, 81),
	}
	for _, chunk := range chunks {
		if err := emitter.Consume(chunk); err != nil {
			t.Fatalf("Consume 失败: %v", err)
		}
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	assertTypes(t, log.types(), []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_delta",
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	})

	block := log.events[1]["content_block"].(map[string]any)
	if block["type"] != "tool_use" || block["id"] != "call_1" || block["name"] != "get_weather" {
		t.Fatalf("tool_use 块 = %#v", block)
	}
	if _, ok := block["input"].(map[string]any); !ok {
		t.Fatalf("tool_use 块的 input 应是空对象，实际 %#v", block["input"])
	}

	// 四个分片顺序拼起来必须还原成完整 JSON。
	var joined string
	for _, event := range log.ofType(t, "content_block_delta") {
		delta := event["delta"].(map[string]any)
		if delta["type"] != "input_json_delta" {
			t.Fatalf("delta.type = %v，期望 input_json_delta", delta["type"])
		}
		joined += delta["partial_json"].(string)
	}
	if joined != `{"city": "北京"}` {
		t.Fatalf("分片拼接 = %q，期望 {\"city\": \"北京\"}", joined)
	}

	if log.ofType(t, "message_delta")[0]["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatal("stop_reason 应为 tool_use")
	}
}

func TestEventEmitterParallelToolCalls(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	chunks := []StreamChunk{
		toolStartChunk(0, "call_a", "get_weather"),
		toolArgChunk(0, `{"city":"北京"}`),
		toolStartChunk(1, "call_b", "get_weather"),
		toolArgChunk(1, `{"city":"上海"}`),
		finishChunk("tool_calls", 400, 90),
	}
	for _, chunk := range chunks {
		if err := emitter.Consume(chunk); err != nil {
			t.Fatalf("Consume 失败: %v", err)
		}
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	assertTypes(t, log.types(), []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_stop", // 工具 0
		"content_block_start", "content_block_delta", "content_block_stop", // 工具 1
		"message_delta", "message_stop",
	})

	first := log.events[1]
	second := log.events[4]
	if first["index"].(float64) != 0 || second["index"].(float64) != 1 {
		t.Fatalf("两个工具块的 index 应为 0 和 1，实际 %v 与 %v", first["index"], second["index"])
	}
	if first["content_block"].(map[string]any)["id"] != "call_a" {
		t.Fatalf("工具 0 的 id = %v", first["content_block"].(map[string]any)["id"])
	}
	if second["content_block"].(map[string]any)["id"] != "call_b" {
		t.Fatalf("工具 1 的 id = %v", second["content_block"].(map[string]any)["id"])
	}

	// 两个块的参数不能串到对方的 index 上。
	deltas := log.ofType(t, "content_block_delta")
	if deltas[0]["index"].(float64) != 0 || deltas[1]["index"].(float64) != 1 {
		t.Fatalf("参数分片的 index 串了: %v / %v", deltas[0]["index"], deltas[1]["index"])
	}
}

func TestEventEmitterTextThenToolUsesIncreasingIndex(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	chunks := []StreamChunk{
		textChunk("我查一下"),
		toolStartChunk(0, "call_1", "get_weather"),
		toolArgChunk(0, "{}"),
		finishChunk("tool_calls", 10, 5),
	}
	for _, chunk := range chunks {
		if err := emitter.Consume(chunk); err != nil {
			t.Fatalf("Consume 失败: %v", err)
		}
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	assertTypes(t, log.types(), []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_stop", // text
		"content_block_start", "content_block_delta", "content_block_stop", // tool_use
		"message_delta", "message_stop",
	})
	if log.events[1]["content_block"].(map[string]any)["type"] != "text" {
		t.Fatal("首块应为 text")
	}
	if log.events[4]["index"].(float64) != 1 {
		t.Fatalf("tool_use 块 index 应为 1，实际 %v", log.events[4]["index"])
	}
}

func TestEventEmitterThinkingThenToolUse(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", true, log.emit)

	chunks := []StreamChunk{
		reasoningChunk("要查天气"),
		toolStartChunk(0, "call_1", "get_weather"),
		toolArgChunk(0, "{}"),
		finishChunk("tool_calls", 10, 5),
	}
	for _, chunk := range chunks {
		if err := emitter.Consume(chunk); err != nil {
			t.Fatalf("Consume 失败: %v", err)
		}
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	if log.events[1]["content_block"].(map[string]any)["type"] != blockKindThinking {
		t.Fatal("thinking 块必须排在 tool_use 之前")
	}
	if log.events[4]["content_block"].(map[string]any)["type"] != blockKindToolUse {
		t.Fatalf("第二块应为 tool_use，实际 %#v", log.events[4]["content_block"])
	}
}

func TestEventEmitterOrphanArgumentFragmentFails(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	// 没有首发分片就来参数分片：协议被破坏，必须报错而不是猜一个块。
	if err := emitter.Consume(toolArgChunk(0, `{"a":1}`)); err == nil {
		t.Fatal("孤儿参数分片必须报错")
	}
}

func TestEventEmitterInterleavedToolFragmentsFail(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	chunks := []StreamChunk{
		toolStartChunk(0, "call_a", "f"),
		toolArgChunk(0, `{"x":`),
		toolStartChunk(1, "call_b", "g"), // 工具 0 的块在此被关闭
	}
	for _, chunk := range chunks {
		if err := emitter.Consume(chunk); err != nil {
			t.Fatalf("Consume 失败: %v", err)
		}
	}

	// 回头再给工具 0 的分片 —— 它的块已关闭，无法再追加，必须报错。
	if err := emitter.Consume(toolArgChunk(0, `1}`)); err == nil {
		t.Fatal("交错的参数分片必须报错（详见任务说明的备选缓冲方案）")
	}
}

func TestEventEmitterIncompleteToolStartFails(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", false, log.emit)

	// 有 id 但没 name 的首发分片：无法构造合法的 tool_use 块。
	call := StreamToolCall{Index: 0, ID: "call_1", Type: "function"}
	chunk := StreamChunk{
		ID:      "s1",
		Model:   "m",
		Choices: []StreamChoice{{Index: 0, Delta: StreamDelta{ToolCalls: []StreamToolCall{call}}}},
	}
	if err := emitter.Consume(chunk); err == nil {
		t.Fatal("缺 name 的工具首发分片必须报错")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/ark/translate/ -run 'Tool' -v`
Expected: FAIL —— `consumeToolCalls` 目前是空实现，产出的事件序列只有 message_start / message_delta / message_stop

- [ ] **Step 3: 实现工具路径**

把 `internal/ark/translate/events.go` 里 Task 8 留下的空实现替换为：

```go
// consumeToolCalls 处理工具调用分片。
//
// 首发分片带 ID 与 Function.Name，据此开一个新的 tool_use 块并登记
// OpenAI tool index → Anthropic block index 的映射；
// 后续分片只有 Function.Arguments，按映射追加 input_json_delta。
//
// 参数分片原样转发，绝不在中途解析 —— 分片会断在 JSON 转义符或多字节
// UTF-8 字符中间，只有全部拼完才是合法 JSON。
func (e *EventEmitter) consumeToolCalls(calls []StreamToolCall) error {
	for _, call := range calls {
		if call.ID != "" || call.Function.Name != "" {
			if err := e.openToolBlock(call); err != nil {
				return err
			}
		}
		if call.Function.Arguments == "" {
			continue
		}
		index, ok := e.toolBlocks[call.Index]
		if !ok {
			return fmt.Errorf("收到 tool_calls[%d] 的参数分片但缺少首发分片", call.Index)
		}
		// 该工具的块必须仍处于打开状态，否则说明上游把多个工具的分片交错发送，
		// 当前的流式实现无法处理（备选方案见实施计划 Task 9 说明）。
		if e.openKind != blockKindToolUse || e.openIndex != index {
			return fmt.Errorf("tool_calls[%d] 的参数分片在其内容块关闭后到达，上游分片存在交错", call.Index)
		}
		if err := e.write(map[string]any{
			"type":  "content_block_delta",
			"index": index,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": call.Function.Arguments,
			},
		}); err != nil {
			return err
		}
	}
	return nil
}

// openToolBlock 为一个工具调用开启 tool_use 块，并登记 index 映射。
func (e *EventEmitter) openToolBlock(call StreamToolCall) error {
	if call.ID == "" || call.Function.Name == "" {
		return fmt.Errorf("tool_calls[%d] 首发分片缺少 id 或 name", call.Index)
	}
	if _, exists := e.toolBlocks[call.Index]; exists {
		return fmt.Errorf("tool_calls[%d] 出现重复的首发分片", call.Index)
	}
	if err := e.openBlockOfKind(blockKindToolUse, map[string]any{
		"type":  blockKindToolUse,
		"id":    call.ID,
		"name":  call.Function.Name,
		"input": map[string]any{},
	}); err != nil {
		return err
	}
	e.toolBlocks[call.Index] = e.openIndex
	return nil
}
```

> `openBlockOfKind` 在 Task 8 已对 `blockKindToolUse` 做了特例：即使当前打开的块也是 tool_use，
> 也要关旧开新（每个工具一个独立块）。若 Task 8 的实现漏了这一点，本任务的并行用例会失败 ——
> 检查那句 `if e.openKind == kind && kind != blockKindToolUse`。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/ark/translate/ -v`
Expected: 全部 PASS（含 Task 8 的用例）

- [ ] **Step 5: 加一条不变式回归测试**

在 `internal/ark/translate/events_tools_test.go` 追加，用于挡住未来改动破坏配对关系：

```go
func TestEventEmitterBlockStartStopAlwaysPaired(t *testing.T) {
	t.Parallel()

	log := &eventLog{}
	emitter := NewEventEmitter("m", true, log.emit)

	chunks := []StreamChunk{
		reasoningChunk("想"), textChunk("说"),
		toolStartChunk(0, "c0", "f"), toolArgChunk(0, "{}"),
		toolStartChunk(1, "c1", "g"), toolArgChunk(1, "{}"),
		finishChunk("tool_calls", 1, 1),
	}
	for _, chunk := range chunks {
		if err := emitter.Consume(chunk); err != nil {
			t.Fatalf("Consume 失败: %v", err)
		}
	}
	if err := emitter.Finish(true); err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	var depth int
	var seenIndexes []float64
	for _, event := range log.events {
		switch event["type"] {
		case "content_block_start":
			depth++
			if depth != 1 {
				t.Fatalf("块不可嵌套，当前深度 %d", depth)
			}
			seenIndexes = append(seenIndexes, event["index"].(float64))
		case "content_block_stop":
			depth--
			if depth != 0 {
				t.Fatalf("content_block_stop 未配对，当前深度 %d", depth)
			}
		}
	}
	if depth != 0 {
		t.Fatal("流结束时仍有未关闭的内容块")
	}
	for i, index := range seenIndexes {
		if int(index) != i {
			t.Fatalf("块 index 必须从 0 起严格递增，实际 %v", seenIndexes)
		}
	}
	if log.types()[0] != "message_start" {
		t.Fatal("message_start 必须是首个事件")
	}
	if last := log.types()[len(log.types())-1]; last != "message_stop" {
		t.Fatalf("message_stop 必须是末个事件，实际 %q", last)
	}
}
```

- [ ] **Step 6: 运行并全量测试**

Run: `go test ./internal/ark/... -v && make test`
Expected: 全部 PASS

- [ ] **Step 7: 提交**

```bash
git add internal/ark/translate/
git commit -m "feat(ark): 事件状态机支持工具调用与并行 index 映射

首发分片开 tool_use 块并登记 OpenAI tool index 到 Anthropic block
index 的映射，参数分片原样转发为 input_json_delta 不中途解析。
孤儿分片、重复首发、分片交错一律报错而非产出损坏序列。
新增不变式回归测试：块不嵌套、start/stop 配对、index 严格递增。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 10: 流式接线 —— Claude Code 可用

把 SSE 解析与状态机接进 `ark.Client.Stream`。**本任务完成后 Claude Code 即可实际使用 DeepSeek。**

**Files:**
- Modify: `internal/ark/client.go`（实现 `Stream`）
- Test: `internal/ark/stream_test.go`

**Interfaces:**
- Consumes: `scanSSE`（Task 7）、`translate.NewEventEmitter`（Task 8/9）
- Produces: `(*ark.Client).Stream` 的真实实现

- [ ] **Step 1: 写失败测试**

创建 `internal/ark/stream_test.go`：

```go
package ark

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/llm"
)

func streamingHandler(payload string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/event-stream" {
			http.Error(w, "缺少 Accept 头", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(payload))
	}
}

func collectStream(t *testing.T, client *Client, body string) []map[string]any {
	t.Helper()
	var events []map[string]any
	err := client.Stream(context.Background(), "m", []byte(body), func(chunk []byte) error {
		var decoded map[string]any
		if err := json.Unmarshal(chunk, &decoded); err != nil {
			return err
		}
		events = append(events, decoded)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream 失败: %v", err)
	}
	return events
}

func TestStreamRequestsStreamingAndEmitsAnthropicEvents(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, streamingHandler(arkToolStream))
	events := collectStream(t, client, `{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)

	if len(events) == 0 {
		t.Fatal("未产出任何事件")
	}
	if events[0]["type"] != "message_start" {
		t.Fatalf("首个事件 = %v，期望 message_start", events[0]["type"])
	}
	if last := events[len(events)-1]["type"]; last != "message_stop" {
		t.Fatalf("末个事件 = %v，期望 message_stop", last)
	}

	// arkToolStream 是一次工具调用，应产出 tool_use 块与 tool_use 停止原因。
	var sawToolUse bool
	for _, event := range events {
		if event["type"] != "content_block_start" {
			continue
		}
		if event["content_block"].(map[string]any)["type"] == "tool_use" {
			sawToolUse = true
		}
	}
	if !sawToolUse {
		t.Fatal("应产出 tool_use 内容块")
	}

	// 每个事件都必须带 type —— httpapi/sse.go 依赖它生成 SSE 事件名。
	for i, event := range events {
		if _, ok := event["type"].(string); !ok {
			t.Fatalf("事件[%d] 缺少 type: %#v", i, event)
		}
	}
}

func TestStreamDoesNotEmitThinkingByDefault(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, streamingHandler(arkToolStream))
	events := collectStream(t, client, `{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)

	for _, event := range events {
		if event["type"] != "content_block_start" {
			continue
		}
		if event["content_block"].(map[string]any)["type"] == "thinking" {
			t.Fatal("客户端未请求 thinking 时不应产出 thinking 块")
		}
	}
}

func TestStreamReportsTruncatedStream(t *testing.T) {
	t.Parallel()

	// 有内容但没有 finish_reason，也没有 [DONE]。
	truncated := `data: {"choices":[{"delta":{"content":"半","role":"assistant"},"index":0}],"id":"s","model":"m"}

`
	client := newTestClient(t, streamingHandler(truncated))
	err := client.Stream(context.Background(), "m",
		[]byte(`{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`),
		func([]byte) error { return nil })
	if err == nil {
		t.Fatal("断流必须报错，不能当成正常结束")
	}
	if !strings.Contains(err.Error(), "finish_reason") {
		t.Fatalf("错误信息应指出缺少 finish_reason，实际 %v", err)
	}
}

func TestStreamMapsUpstreamErrorBeforeStreaming(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"限流"}}`))
	})

	err := client.Stream(context.Background(), "m",
		[]byte(`{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`),
		func([]byte) error { return nil })
	if err == nil {
		t.Fatal("上游 429 必须报错")
	}
	var statusErr llm.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("错误应实现 llm.StatusError，实际 %T", err)
	}
	if statusErr.Status() != http.StatusTooManyRequests {
		t.Fatalf("Status = %d", statusErr.Status())
	}
}

func TestStreamRejectsImageBeforeCallingUpstream(t *testing.T) {
	t.Parallel()

	called := false
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { called = true })

	body := []byte(`{"max_tokens":64,"messages":[{"role":"user","content":[
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"x"}}]}]}`)
	if err := client.Stream(context.Background(), "m", body, func([]byte) error { return nil }); err == nil {
		t.Fatal("含图片的流式请求必须报错")
	}
	if called {
		t.Fatal("被拒绝的请求不应发往上游")
	}
}

func TestStreamPropagatesEmitError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("客户端断开")
	client := newTestClient(t, streamingHandler(arkToolStream))
	err := client.Stream(context.Background(), "m",
		[]byte(`{"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`),
		func([]byte) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("emit 错误应透传，实际 %v", err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/ark/ -run Stream -v`
Expected: FAIL —— `ark 流式调用尚未实现`

- [ ] **Step 3: 实现 Stream**

把 `internal/ark/client.go` 里 Task 6 留下的占位实现替换为：

```go
func (c *Client) Stream(ctx context.Context, modelID string, body []byte, emit func([]byte) error) error {
	response, err := c.post(ctx, modelID, body, true)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode > 299 {
		return c.errorFromResponse(response)
	}

	// includeThinking 在 Task 11 接上客户端的 thinking 开关，当前一律不透出。
	emitter := translate.NewEventEmitter(modelID, false, emit)
	sawDone, err := scanSSE(response.Body, emitter.Consume)
	if err != nil {
		return err
	}
	return emitter.Finish(sawDone)
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/ark/... -v`
Expected: 全部 PASS

- [ ] **Step 5: 全量测试与构建**

Run: `make test && make race && make vet && make build`
Expected: 全部通过

- [ ] **Step 6: 真机验证流式**

启动网关后：

```bash
curl -N -s -X POST http://127.0.0.1:8080/v1/messages \
  -H "x-api-key: $GATEWAY_API_KEY" -H "content-type: application/json" \
  -d '{"model":"ds-flash","stream":true,"max_tokens":256,
       "messages":[{"role":"user","content":"北京天气怎么样？请调用工具。"}],
       "tools":[{"name":"get_weather","description":"查询天气",
                 "input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]}'
```

Expected: 看到 `event: message_start`、`event: content_block_start`(tool_use)、多个
`event: content_block_delta`(input_json_delta)、`event: content_block_stop`、
`event: message_delta`(stop_reason=tool_use)、`event: message_stop`。

- [ ] **Step 7: 用 Claude Code 实跑**

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8080 ANTHROPIC_AUTH_TOKEN=$GATEWAY_API_KEY \
  claude --model ds-flash
```

让它做一件需要工具的事，例如"列出当前目录下的 Go 文件并统计行数"。

Expected: 工具调用正常执行、结果正常回传、多轮循环不中断。
若出现协议错误，记录 Claude Code 的报错原文与网关日志的 `request_id`，据此定位是哪个事件不合规。

- [ ] **Step 8: 提交**

```bash
git add internal/ark/
git commit -m "feat(ark): 接通流式调用

SSE 解析与事件状态机接进 Stream，上游非 2xx 先行映射为
llm.StatusError，断流按缺少 finish_reason 报错。
至此 Claude Code 可通过网关使用 DeepSeek V4 Flash。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 11: token 计数、max_tokens 钳制与 thinking 透出

补齐三处收尾能力。

**Files:**
- Create: `internal/ark/translate/tokens.go`
- Modify: `internal/ark/translate/request.go`（`max_tokens` 钳制、导出 `WantsThinking`）
- Modify: `internal/ark/client.go`（`CountTokens` 实现、`Invoke`/`Stream` 接上 thinking 开关）
- Test: `internal/ark/translate/tokens_test.go`

**Interfaces:**
- Consumes: `BuildRequest`、`EventEmitter`、`BuildResponse`
- Produces:
  - `translate.EstimateTokens(anthropicBody []byte) (int32, error)`
  - `translate.WantsThinking(anthropicBody []byte) bool`
  - `translate.MaxOutputTokens = 32768`

- [ ] **Step 1: 写失败测试**

创建 `internal/ark/translate/tokens_test.go`：

```go
package translate

import "testing"

func TestEstimateTokensCountsAllTextSources(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"system": "你是助手",
		"max_tokens": 64,
		"messages": [
			{"role": "user", "content": "北京今天天气怎么样"},
			{"role": "assistant", "content": [{"type": "text", "text": "我查一下"}]}
		]
	}`)

	count, err := EstimateTokens(body)
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	if count <= 0 {
		t.Fatalf("估算值应为正数，实际 %d", count)
	}

	// 中文约 1 token/字：上述约 20 个汉字，估算值不该低于字数的一半。
	if count < 10 {
		t.Fatalf("中文估算明显偏低: %d", count)
	}
}

func TestEstimateTokensIsConservative(t *testing.T) {
	t.Parallel()

	short := []byte(`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	long := []byte(`{"max_tokens":8,"messages":[{"role":"user","content":"` +
		"这是一段明显更长的中文输入用于验证估算随长度单调增加" + `"}]}`)

	shortCount, err := EstimateTokens(short)
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	longCount, err := EstimateTokens(long)
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	if longCount <= shortCount {
		t.Fatalf("更长输入的估算值应更大: %d vs %d", longCount, shortCount)
	}
}

func TestEstimateTokensIncludesToolDefinitions(t *testing.T) {
	t.Parallel()

	withoutTools := []byte(`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	withTools := []byte(`{"max_tokens":8,"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"get_weather","description":"查询指定城市的天气",
		"input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]}`)

	bare, err := EstimateTokens(withoutTools)
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	tooled, err := EstimateTokens(withTools)
	if err != nil {
		t.Fatalf("估算失败: %v", err)
	}
	// 工具定义要占上下文，实测同样的输入带 tools 后 prompt_tokens 从 89 涨到 397。
	if tooled <= bare {
		t.Fatalf("带工具定义的估算值应更大: %d vs %d", tooled, bare)
	}
}

func TestEstimateTokensRejectsMalformedBody(t *testing.T) {
	t.Parallel()

	if _, err := EstimateTokens([]byte(`[]`)); err == nil {
		t.Fatal("畸形请求体应报错")
	}
}

func TestWantsThinking(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want bool
	}{
		{"未传", `{"max_tokens":8,"messages":[]}`, false},
		{"enabled", `{"max_tokens":8,"messages":[],"thinking":{"type":"enabled","budget_tokens":1024}}`, true},
		{"disabled", `{"max_tokens":8,"messages":[],"thinking":{"type":"disabled"}}`, false},
		{"畸形", `{`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := WantsThinking([]byte(tc.body)); got != tc.want {
				t.Fatalf("WantsThinking = %v，期望 %v", got, tc.want)
			}
		})
	}
}

func TestBuildRequestClampsMaxTokens(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"max_tokens":999999,"messages":[{"role":"user","content":"hi"}]}`)
	req, rejection := BuildRequest("m", raw, false)
	if rejection != nil {
		t.Fatalf("超限的 max_tokens 应钳制而非报错: %v", rejection)
	}
	if req.MaxTokens != MaxOutputTokens {
		t.Fatalf("MaxTokens = %d，期望钳制到 %d", req.MaxTokens, MaxOutputTokens)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/ark/translate/ -run 'Estimate|WantsThinking|Clamp' -v`
Expected: 编译失败，`undefined: EstimateTokens`

- [ ] **Step 3: 实现估算与 thinking 探测**

创建 `internal/ark/translate/tokens.go`：

```go
package translate

import (
	"encoding/json"
	"unicode"
)

// MaxOutputTokens 是该模型的输出上限。
// DeepSeek V4 Flash 是推理模型，思考部分也计入输出，故上限取 32K。
const MaxOutputTokens = 32768

// 估算系数：CJK 字符约 1 token/字，其余字符约 4 字符/token。
const asciiCharsPerToken = 4

// EstimateTokens 本地估算 Anthropic 请求体的输入 token 数。
//
// Ark 没有 count_tokens 端点，这里只能估算。刻意偏向高估：
// Claude Code 用这个值做上下文预算，低估会导致真实请求超限被上游拒绝。
func EstimateTokens(anthropicBody []byte) (int32, error) {
	var source anthropicRequest
	if err := json.Unmarshal(anthropicBody, &source); err != nil {
		return 0, err
	}

	var text string
	if system, rejection := flattenText(source.System, "system"); rejection == nil {
		text += system
	}
	for index, message := range source.Messages {
		// 图片等不支持的块在 BuildRequest 阶段会被拒绝，这里忽略转换失败，
		// 只要尽力统计文本量即可。
		if content, rejection := flattenText(message.Content, "messages"); rejection == nil {
			text += content
		}
		_ = index
	}
	// 工具定义要进上下文，且占比可观：实测同一输入带一个工具后
	// prompt_tokens 从 89 涨到 397，故按原始 JSON 长度计入。
	for _, tool := range source.Tools {
		text += tool.Name + tool.Description + string(tool.InputSchema)
	}

	return int32(estimateTextTokens(text)) + perMessageOverhead*int32(len(source.Messages)), nil
}

// perMessageOverhead 是每条消息的固定开销（角色标记、分隔符等）。
const perMessageOverhead = 4

func estimateTextTokens(text string) int {
	var cjk, other int
	for _, r := range text {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			cjk++
			continue
		}
		other++
	}
	// other 向上取整，避免短输入被估成 0。
	return cjk + (other+asciiCharsPerToken-1)/asciiCharsPerToken
}

// WantsThinking 报告客户端是否显式请求了 thinking。
// 请求体畸形时返回 false —— 真正的报错由 BuildRequest 负责。
func WantsThinking(anthropicBody []byte) bool {
	var source struct {
		Thinking *struct {
			Type string `json:"type"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(anthropicBody, &source); err != nil {
		return false
	}
	return source.Thinking != nil && source.Thinking.Type == "enabled"
}
```

- [ ] **Step 4: 在 BuildRequest 里钳制 max_tokens**

`internal/ark/translate/request.go` 里 `request` 初始化之后插入：

```go
	// 超过模型上限的 max_tokens 直接钳制，不报错 —— Claude Code 常按
	// Anthropic 模型的上限填值，拒绝会让请求白失败。
	if request.MaxTokens > MaxOutputTokens {
		request.MaxTokens = MaxOutputTokens
	}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/ark/translate/ -v`
Expected: 全部 PASS

- [ ] **Step 6: 客户端接上 CountTokens 与 thinking 开关**

`internal/ark/client.go` 的 `CountTokens` 占位实现替换为：

```go
// CountTokens 本地估算输入 token 数。Ark 没有对应端点，
// 估算偏向高估，避免 Claude Code 低估上下文导致真实请求超限。
func (c *Client) CountTokens(_ context.Context, _ string, body []byte) (int32, error) {
	count, err := translate.EstimateTokens(body)
	if err != nil {
		return 0, &rejectionError{rejection: &translate.Rejection{
			Kind:    anthropic.ErrorTypeInvalidRequest,
			Message: "请求体无法解析，无法估算 token 数",
		}}
	}
	return count, nil
}
```

`Invoke` 与 `Stream` 里的 `includeThinking` 改为读客户端意图：

```go
	return translate.BuildResponse(payload, translate.WantsThinking(body))
```

```go
	emitter := translate.NewEventEmitter(modelID, translate.WantsThinking(body), emit)
```

- [ ] **Step 7: 补客户端层测试**

在 `internal/ark/stream_test.go` 追加：

```go
func TestStreamEmitsThinkingWhenRequested(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, streamingHandler(arkToolStream))
	events := collectStream(t, client,
		`{"max_tokens":64,"thinking":{"type":"enabled","budget_tokens":1024},
		  "messages":[{"role":"user","content":"hi"}]}`)

	var sawThinking bool
	for _, event := range events {
		if event["type"] != "content_block_start" {
			continue
		}
		if event["content_block"].(map[string]any)["type"] == "thinking" {
			sawThinking = true
		}
	}
	if !sawThinking {
		t.Fatal("客户端请求 thinking 时应透出 thinking 块")
	}
	// thinking 块必须是第一个内容块。
	for _, event := range events {
		if event["type"] != "content_block_start" {
			continue
		}
		if event["content_block"].(map[string]any)["type"] != "thinking" {
			t.Fatal("thinking 块必须排在其他内容块之前")
		}
		break
	}
}

func TestCountTokensEstimatesLocally(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("CountTokens 不应发起上游请求")
	})

	count, err := client.CountTokens(context.Background(), "m",
		[]byte(`{"max_tokens":64,"messages":[{"role":"user","content":"北京今天天气怎么样"}]}`))
	if err != nil {
		t.Fatalf("CountTokens 失败: %v", err)
	}
	if count <= 0 {
		t.Fatalf("估算值 = %d", count)
	}
}
```

- [ ] **Step 8: 全量测试与真机验证**

Run: `make test && make race && make vet`

真机验证 count_tokens：

```bash
curl -s -X POST http://127.0.0.1:8080/v1/messages/count_tokens \
  -H "x-api-key: $GATEWAY_API_KEY" -H "content-type: application/json" \
  -d '{"model":"ds-flash","messages":[{"role":"user","content":"北京今天天气怎么样"}]}' | jq .
```

Expected: `{"input_tokens": <正整数>}`

- [ ] **Step 9: 提交**

```bash
git add internal/ark/
git commit -m "feat(ark): 补齐 token 估算、max_tokens 钳制与 thinking 透出

Ark 无 count_tokens 端点，改为本地估算并刻意高估，避免
Claude Code 低估上下文导致真实请求超限。输出上限钳制到 32K。
thinking 块按客户端是否显式请求决定是否透出。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 12: 文档与端到端回归

**Files:**
- Modify: `README.md`
- Modify: `docs/design/2026-09-22-deepseek-v4-flash-provider.md`（回填实测结果，更新状态）

- [ ] **Step 1: 更新 README**

在 README 的配置说明处补充 Ark 相关内容：模型登记方式（`provider: "ark"`）、`ARK_API_KEY`
等环境变量、以及"该模型不支持图片输入、count_tokens 为本地估算"两条限制。

- [ ] **Step 2: 跑完整回归**

Run: `make test && make race && make vet && make build`
Expected: 全部通过

- [ ] **Step 3: 确认 sse.go 的改动仍只有签名那两行**

Run: `git diff --stat HEAD~11 -- internal/httpapi/sse.go`
Expected: `2 insertions(+), 2 deletions(-)`。若超出，说明中途破坏了本设计的核心约束，需复核。

- [ ] **Step 4: Bedrock 回归**

用 Claude Code 连网关跑一次 Bedrock 模型（`--model opus-5`），确认原有链路未受影响。

- [ ] **Step 5: 回填设计文档**

把 §11「未决问题」逐条改为实测结论：

1. thinking 块缺 `signature` 时 Claude Code 是否报错 —— 填 Task 10/11 的实跑结果
2. DeepSeek 工具调用在 Claude Code 长链路下的稳定性 —— 填试用观察
3. Ark 的限流与重试语义 —— 填观察到的限流表现

把文档头部状态从「待评审」改为「已实施」。

- [ ] **Step 6: 提交**

```bash
git add README.md docs/design/2026-09-22-deepseek-v4-flash-provider.md
git commit -m "docs(ark): 补充 DeepSeek 接入说明并回填实测结论

README 增加 ark provider 的配置方式与已知限制，
设计文档的未决问题改为实测结论。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

## 自查记录

按 writing-plans 的自查清单核对本计划：

**Spec 覆盖**：Spec §4 模块划分 → Task 1/2/6；§5 流式事件映射 → Task 7/8/9；§6 请求转换与 thinking 策略
→ Task 3/4/11；§7 不支持能力与降级（图片拦截、count_tokens、max_tokens 钳制）→ Task 3/11；§8 配置
→ Task 6；§9 测试策略的六类用例分散在 Task 3-10，其中"sse.go 不改"的验收在 Task 2 Step 11 与 Task 12
Step 3 各查一次。**已知缺口**：Spec §7 提到 `cache_control` 丢弃，由 Task 3 的
`TestBuildRequestDropsUnsupportedFields` 覆盖；Spec §11 的三个未决问题在 Task 12 Step 5 回填。

**类型一致性**：`llm.Resolution`（Task 1）→ Task 2 消费；`translate.Rejection`（Task 3）→ Task 6 包成
`rejectionError`；`translate.StreamChunk`（Task 7）→ Task 8/9 状态机与 Task 10 的 `scanSSE` 回调；
`MapStopReason`（Task 5）→ Task 8 复用，未重复定义；`blockKindToolUse` 常量在 Task 8 定义、Task 9 与
测试引用。

**已修正的坑**：Task 4 的 `tool_choice` 若用 `map[string]any` 会因键序输出导致断言失败，已在该步骤标注改用
具名 struct；Task 9 依赖 Task 8 的 `openBlockOfKind` 对 tool_use 的特例，已在步骤里注明检查点。
