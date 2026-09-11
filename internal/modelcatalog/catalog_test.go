package modelcatalog

import (
	"os"
	"path/filepath"
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

func TestManagerReloadKeepsPreviousSnapshotOnFailure(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "models.json")
	initial := []byte(`{"default":"model-a","models":[{"id":"model-a","aliases":["a"]}]}`)
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatalf("写入初始目录失败: %v", err)
	}

	manager, err := NewManager(path)
	if err != nil {
		t.Fatalf("创建目录管理器失败: %v", err)
	}
	if resolved, ok := manager.Resolve("a"); !ok || resolved != "model-a" {
		t.Fatalf("初始目录解析异常: %q, %v", resolved, ok)
	}

	if err := os.WriteFile(path, []byte(`{"default":"missing","models":[{"id":"model-b"}]}`), 0o600); err != nil {
		t.Fatalf("写入无效目录失败: %v", err)
	}
	if err := manager.Reload(); err == nil {
		t.Fatal("预期 reload 失败")
	}
	if resolved, ok := manager.Resolve("a"); !ok || resolved != "model-a" {
		t.Fatalf("失败 reload 后旧快照丢失: %q, %v", resolved, ok)
	}

	updated := []byte(`{"default":"model-b","models":[{"id":"model-b","aliases":["b"]}]}`)
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		t.Fatalf("写入新目录失败: %v", err)
	}
	if err := manager.Reload(); err != nil {
		t.Fatalf("reload 新目录失败: %v", err)
	}
	if resolved, ok := manager.Resolve(""); !ok || resolved != "model-b" {
		t.Fatalf("新默认模型未生效: %q, %v", resolved, ok)
	}
}
