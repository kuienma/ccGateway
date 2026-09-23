package modelcatalog

import (
	"os"
	"path/filepath"
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

func TestUsesProviderReportsCatalogMembership(t *testing.T) {
	t.Parallel()

	bedrockOnly, err := Parse([]byte(`{"default":"a","models":[{"id":"a"}]}`))
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	if !bedrockOnly.UsesProvider(llm.ProviderBedrock) {
		t.Fatal("纯 bedrock 目录应报告使用 bedrock")
	}
	if bedrockOnly.UsesProvider(llm.ProviderArk) {
		t.Fatal("未登记 ark 模型时不应报告使用 ark")
	}

	mixed, err := Parse([]byte(`{"default":"a","models":[{"id":"a"},{"id":"b","provider":"ark"}]}`))
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	if !mixed.UsesProvider(llm.ProviderArk) || !mixed.UsesProvider(llm.ProviderBedrock) {
		t.Fatal("混合目录应同时报告两个 provider")
	}

	var absent *Catalog
	if absent.UsesProvider(llm.ProviderArk) {
		t.Fatal("nil 目录不应报告使用任何 provider")
	}
}

// TestRepositoryModelsConfigIsValid 直接读仓库里真实的 configs/models.json，
// 守住「Bedrock 路径零变化」这条全局约束：真实配置必须能解析，
// 且原有的 Anthropic 模型在未显式声明 provider 时仍缺省落到 bedrock。
func TestRepositoryModelsConfigIsValid(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("..", "..", "configs", "models.json"))
	if err != nil {
		t.Fatalf("读取真实模型配置失败: %v", err)
	}
	catalog, err := Parse(data)
	if err != nil {
		t.Fatalf("真实模型配置无法解析: %v", err)
	}

	for _, name := range []string{
		"global.anthropic.claude-opus-5", "claude-opus-5", "opus-5",
		"global.anthropic.claude-opus-4-8", "claude-opus-4-8", "opus-4-8",
	} {
		resolved, ok := catalog.Resolve(name)
		if !ok {
			t.Fatalf("真实配置中 %q 应可解析", name)
		}
		if resolved.Provider != llm.ProviderBedrock {
			t.Fatalf("%q 的 provider = %q，Anthropic 模型必须缺省落到 bedrock", name, resolved.Provider)
		}
	}

	// 缺省模型（空模型名）也必须仍然是 bedrock 上的模型。
	defaultResolution, ok := catalog.Resolve("")
	if !ok {
		t.Fatal("真实配置应有可解析的缺省模型")
	}
	if defaultResolution.Provider != llm.ProviderBedrock {
		t.Fatalf("缺省模型 provider = %q，期望 bedrock", defaultResolution.Provider)
	}

	// 登记方向同样要守：有人把 "provider":"ark" 误改成 bedrock（或删掉该字段，
	// 从而缺省成 bedrock）时，上面的断言不会响，必须在这里拦住。
	for _, name := range []string{"deepseek-v4-flash-ga-260731", "deepseek-v4-flash", "ds-flash"} {
		resolved, ok := catalog.Resolve(name)
		if !ok {
			t.Fatalf("真实配置中 %q 应可解析", name)
		}
		if resolved.Provider != llm.ProviderArk {
			t.Fatalf("%q 的 provider = %q，期望 ark", name, resolved.Provider)
		}
		if resolved.ModelID != "deepseek-v4-flash-ga-260731" {
			t.Fatalf("%q 的 ModelID = %q", name, resolved.ModelID)
		}
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
