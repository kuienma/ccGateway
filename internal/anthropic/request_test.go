package anthropic

import (
	"encoding/json"
	"testing"
)

type resolverStub map[string]string

func (r resolverStub) Resolve(name string) (string, bool) {
	value, ok := r[name]
	return value, ok
}

func TestPrepareRequestPreservesFieldsAndMapsModel(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"model": "opus-5",
		"max_tokens": 512,
		"messages": [{"role":"user","content":"hello"}],
		"tools": [{"name":"weather","description":"lookup","input_schema":{"type":"object"}}],
		"stream": true
	}`)
	resolver := resolverStub{"opus-5": "global.anthropic.claude-opus-5"}

	prepared, err := PrepareRequest(raw, []string{"beta-a", "beta-b"}, resolver)
	if err != nil {
		t.Fatalf("准备请求失败: %v", err)
	}
	if prepared.ModelID != "global.anthropic.claude-opus-5" {
		t.Fatalf("ModelID = %q", prepared.ModelID)
	}
	if !prepared.Stream {
		t.Fatal("stream 应为 true")
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(prepared.Body, &body); err != nil {
		t.Fatalf("解析 Bedrock body 失败: %v", err)
	}
	if _, exists := body["model"]; exists {
		t.Fatal("Bedrock body 不应包含 model")
	}
	if _, exists := body["stream"]; exists {
		t.Fatal("Bedrock body 不应包含 stream")
	}
	if _, exists := body["tools"]; !exists {
		t.Fatal("未知或扩展字段应原样保留")
	}
	assertJSONString(t, body["anthropic_version"], BedrockAnthropicVersion)

	var betas []string
	if err := json.Unmarshal(body["anthropic_beta"], &betas); err != nil {
		t.Fatalf("anthropic_beta 解析失败: %v", err)
	}
	if len(betas) != 2 || betas[0] != "beta-a" || betas[1] != "beta-b" {
		t.Fatalf("anthropic_beta = %#v", betas)
	}
}

func TestPrepareRequestUsesDefaultModel(t *testing.T) {
	t.Parallel()

	resolver := resolverStub{"": "global.anthropic.claude-opus-5"}
	prepared, err := PrepareRequest([]byte(`{"max_tokens":1,"messages":[]}`), nil, resolver)
	if err != nil {
		t.Fatalf("准备请求失败: %v", err)
	}
	if prepared.ModelID != "global.anthropic.claude-opus-5" {
		t.Fatalf("ModelID = %q", prepared.ModelID)
	}
}

func TestPrepareRequestRejectsUnknownModel(t *testing.T) {
	t.Parallel()

	_, err := PrepareRequest(
		[]byte(`{"model":"unknown","max_tokens":1,"messages":[]}`),
		nil,
		resolverStub{},
	)
	if err == nil || err.Kind != ErrorTypeNotFound {
		t.Fatalf("预期 not_found_error，得到 %#v", err)
	}
}

func TestPrepareRequestRejectsInvalidJSONAndStreamType(t *testing.T) {
	t.Parallel()

	testCases := [][]byte{
		[]byte(`{"model":`),
		[]byte(`{"model":"opus-5","stream":"yes"}`),
	}
	for _, body := range testCases {
		if _, err := PrepareRequest(body, nil, resolverStub{"opus-5": "model"}); err == nil || err.Kind != ErrorTypeInvalidRequest {
			t.Fatalf("预期 invalid_request_error，得到 %#v", err)
		}
	}
}

func assertJSONString(t *testing.T, raw json.RawMessage, expected string) {
	t.Helper()
	var actual string
	if err := json.Unmarshal(raw, &actual); err != nil {
		t.Fatalf("字符串解析失败: %v", err)
	}
	if actual != expected {
		t.Fatalf("字符串 = %q，预期 %q", actual, expected)
	}
}
