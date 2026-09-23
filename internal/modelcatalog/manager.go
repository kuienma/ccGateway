package modelcatalog

import (
	"fmt"
	"os"
	"sync/atomic"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/llm"
)

// Manager 管理完整校验后的不可变目录快照。
type Manager struct {
	path    string
	current atomic.Pointer[Catalog]
}

func NewManager(path string) (*Manager, error) {
	manager := &Manager{path: path}
	if err := manager.Reload(); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *Manager) Reload() error {
	data, err := os.ReadFile(m.path)
	if err != nil {
		return fmt.Errorf("读取模型配置 %s: %w", m.path, err)
	}
	catalog, err := Parse(data)
	if err != nil {
		return fmt.Errorf("校验模型配置 %s: %w", m.path, err)
	}
	// 完整配置通过校验后再原子切换，正在处理的请求继续使用旧快照。
	m.current.Store(catalog)
	return nil
}

func (m *Manager) Resolve(name string) (llm.Resolution, bool) {
	return m.current.Load().Resolve(name)
}
