package modelcatalog

import (
	"testing"
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
		if !ok || resolved != expected {
			t.Fatalf("Resolve(%q) = %q, %v", input, resolved, ok)
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
