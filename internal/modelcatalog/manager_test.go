package modelcatalog

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestManagerConcurrentReadersAcrossReloads(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "models.json")
	write := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	catalog := func(id string) string {
		return fmt.Sprintf(`{"default":"current","models":[{"id":%q,"aliases":["current"]}]}`, id)
	}
	write(catalog("a"))
	manager, err := NewManager(path)
	if err != nil {
		t.Fatal(err)
	}
	old := manager.current.Load()
	var readers sync.WaitGroup
	start := make(chan struct{})
	stop := make(chan struct{})
	var stopOnce sync.Once
	stopReaders := func() { stopOnce.Do(func() { close(stop) }); readers.Wait() }
	defer stopReaders()
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			<-start
			for {
				select {
				case <-stop:
					return
				default:
				}
				if id, ok := manager.Resolve("current"); !ok || (id != "a" && id != "b") {
					t.Errorf("读取到非完整快照: %q %v", id, ok)
					return
				}
			}
		}()
	}
	close(start)
	for i := 0; i < 50; i++ {
		write(catalog([]string{"b", "a"}[i%2]))
		if err := manager.Reload(); err != nil {
			t.Fatal(err)
		}
		snapshot := manager.current.Load()
		write(`{"default":"missing","models":[{"id":"bad"}]}`)
		if err := manager.Reload(); err == nil {
			t.Fatal("无效配置应失败")
		}
		if snapshot != manager.current.Load() {
			t.Fatal("失败后不应替换快照")
		}
	}
	stopReaders()
	if id, ok := old.Resolve("current"); !ok || id != "a" {
		t.Fatal("旧快照被修改")
	}
	// 只改动测试创建的临时文件；读取失败也不能清空当前快照。
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	snapshot := manager.current.Load()
	if err := manager.Reload(); err == nil {
		t.Fatal("缺失文件应失败")
	}
	if snapshot != manager.current.Load() {
		t.Fatal("读取失败后不应替换快照")
	}
}

func TestNewManagerRejectsMissingFile(t *testing.T) {
	t.Parallel()
	if _, err := NewManager(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("缺失文件应阻止启动")
	}
}

func TestParseWhitespaceAndTrailingData(t *testing.T) {
	t.Parallel()
	good := `{"default":" alias ","models":[{"id":" arbitrary-model ","aliases":[" alias "]}]}`
	catalog, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := catalog.Resolve(" alias "); !ok || id != "arbitrary-model" {
		t.Fatalf("id=%q ok=%v", id, ok)
	}
	for _, raw := range []string{good + `{}`, good + `x`, `null`, `{"default":"x","models":[{"id":" "}]}`, `{"default":"x","models":[{"id":"x","aliases":[" "]}]}`, `{"default":"x","models":[{"id":"x","aliases":[" x "]}]}`} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatalf("无效目录被接受: %s", raw)
		}
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
