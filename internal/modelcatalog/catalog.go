package modelcatalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
)

type fileConfig struct {
	Default string        `json:"default"`
	Models  []modelConfig `json:"models"`
}

type modelConfig struct {
	ID      string   `json:"id"`
	Aliases []string `json:"aliases,omitempty"`
}

type Catalog struct {
	defaultModel string
	resolutions  map[string]string
}

func Parse(data []byte) (*Catalog, error) {
	var raw fileConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("解析模型配置: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	if len(raw.Models) == 0 {
		return nil, fmt.Errorf("models 至少包含一个模型")
	}

	resolutions := make(map[string]string)
	for index, model := range raw.Models {
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" {
			return nil, fmt.Errorf("models[%d].id 不能为空", index)
		}
		if err := registerResolution(resolutions, model.ID, model.ID); err != nil {
			return nil, err
		}
		for aliasIndex, alias := range model.Aliases {
			alias = strings.TrimSpace(alias)
			if alias == "" {
				return nil, fmt.Errorf("models[%d].aliases[%d] 不能为空", index, aliasIndex)
			}
			if err := registerResolution(resolutions, alias, model.ID); err != nil {
				return nil, err
			}
		}
	}

	defaultName := strings.TrimSpace(raw.Default)
	defaultModel, ok := resolutions[defaultName]
	if defaultName == "" || !ok {
		return nil, fmt.Errorf("default 必须引用已登记的模型或别名")
	}
	return &Catalog{defaultModel: defaultModel, resolutions: resolutions}, nil
}

func (c *Catalog) Resolve(name string) (string, bool) {
	if c == nil {
		return "", false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return c.defaultModel, true
	}
	modelID, ok := c.resolutions[name]
	return modelID, ok
}

func registerResolution(resolutions map[string]string, name, modelID string) error {
	if _, exists := resolutions[name]; exists {
		return fmt.Errorf("模型名或别名重复: %s", name)
	}
	resolutions[name] = modelID
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("解析模型配置尾部: %w", err)
	}
	return fmt.Errorf("模型配置只能包含一个 JSON 对象")
}

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

func (m *Manager) Resolve(name string) (string, bool) {
	return m.current.Load().Resolve(name)
}
