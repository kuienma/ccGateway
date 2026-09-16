package modelcatalog

import "strings"

// Catalog 是只读的模型名称解析快照，由 Parse 构造。
type Catalog struct {
	defaultModel string
	resolutions  map[string]string
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
