package modelcatalog

import (
	"strings"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/llm"
)

// Catalog 是只读的模型名称解析快照，由 Parse 构造。
type Catalog struct {
	defaultModel llm.Resolution
	resolutions  map[string]llm.Resolution
}

func (c *Catalog) Resolve(name string) (llm.Resolution, bool) {
	if c == nil {
		return llm.Resolution{}, false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return c.defaultModel, true
	}
	resolution, ok := c.resolutions[name]
	return resolution, ok
}
