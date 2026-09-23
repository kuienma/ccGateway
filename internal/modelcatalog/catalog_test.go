package modelcatalog

import (
	"testing"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/llm"
)

func TestParseAndResolveAliases(t *testing.T) {
	t.Parallel()

	catalog, err := Parse([]byte(`{
		"default": "opus-5",
		"models": [
			{
				"id": "global.anthropic.claude-opus-5",
				"aliases": ["claude-opus-5", "opus-5"]
			},
			{
				"id": "global.anthropic.claude-opus-4-8",
				"aliases": ["claude-opus-4-8", "opus-4-8"]
			}
		]
	}`))
	if err != nil {
		t.Fatalf("解析模型目录失败: %v", err)
	}

	testCases := map[string]string{
		"":                                 "global.anthropic.claude-opus-5",
		"opus-5":                           "global.anthropic.claude-opus-5",
		"claude-opus-4-8":                  "global.anthropic.claude-opus-4-8",
		"global.anthropic.claude-opus-4-8": "global.anthropic.claude-opus-4-8",
	}
	for input, expected := range testCases {
		resolved, ok := catalog.Resolve(input)
		if !ok || resolved.ModelID != expected {
			t.Fatalf("Resolve(%q) = %q, %v", input, resolved.ModelID, ok)
		}
	}

	if _, ok := catalog.Resolve("unknown"); ok {
		t.Fatal("未登记模型不应被解析")
	}
}

func TestParseRejectsInvalidCatalog(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		body string
	}{
		{name: "空模型列表", body: `{"default":"opus-5","models":[]}`},
		{name: "默认模型不存在", body: `{"default":"missing","models":[{"id":"model-a"}]}`},
		{name: "别名冲突", body: `{"default":"model-a","models":[{"id":"model-a","aliases":["same"]},{"id":"model-b","aliases":["same"]}]}`},
		{name: "模型 ID 冲突", body: `{"default":"model-a","models":[{"id":"model-a"},{"id":"model-a"}]}`},
		{name: "未知字段", body: `{"default":"model-a","models":[{"id":"model-a"}],"extra":true}`},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse([]byte(testCase.body)); err == nil {
				t.Fatal("预期模型目录校验失败")
			}
		})
	}
}

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
