package anthropic

import (
	"encoding/json"
	"fmt"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/llm"
)

const BedrockAnthropicVersion = "bedrock-2023-05-31"

type ModelResolver interface {
	Resolve(name string) (llm.Resolution, bool)
}

type PreparedRequest struct {
	ModelID  string
	Body     []byte
	Stream   bool
	Provider string
}

func PrepareRequest(raw []byte, betaFeatures []string, resolver ModelResolver) (PreparedRequest, *RequestError) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		return PreparedRequest{}, invalidRequest("请求体必须是 JSON 对象")
	}

	modelName, requestErr := optionalString(body, "model")
	if requestErr != nil {
		return PreparedRequest{}, requestErr
	}
	resolution, ok := resolver.Resolve(modelName)
	if !ok {
		return PreparedRequest{}, &RequestError{
			Kind:    ErrorTypeNotFound,
			Message: fmt.Sprintf("模型未登记: %s", modelName),
		}
	}

	stream, requestErr := optionalBool(body, "stream")
	if requestErr != nil {
		return PreparedRequest{}, requestErr
	}

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

	encoded, err := json.Marshal(body)
	if err != nil {
		return PreparedRequest{}, invalidRequest("请求体无法编码")
	}
	return PreparedRequest{
		ModelID:  resolution.ModelID,
		Body:     encoded,
		Stream:   stream,
		Provider: resolution.Provider,
	}, nil
}

func optionalString(body map[string]json.RawMessage, key string) (string, *RequestError) {
	raw, exists := body[key]
	if !exists {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", invalidRequest(key + " 必须是字符串")
	}
	return value, nil
}

func optionalBool(body map[string]json.RawMessage, key string) (bool, *RequestError) {
	raw, exists := body[key]
	if !exists {
		return false, nil
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, invalidRequest(key + " 必须是布尔值")
	}
	return value, nil
}

func invalidRequest(message string) *RequestError {
	return &RequestError{Kind: ErrorTypeInvalidRequest, Message: message}
}

func mustMarshal(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}
