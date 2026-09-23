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
