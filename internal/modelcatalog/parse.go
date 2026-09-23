package modelcatalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/llm"
)

type fileConfig struct {
	Default string        `json:"default"`
	Models  []modelConfig `json:"models"`
}

type modelConfig struct {
	ID       string   `json:"id"`
	Provider string   `json:"provider,omitempty"`
	Aliases  []string `json:"aliases,omitempty"`
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

	resolutions := make(map[string]llm.Resolution)
	for index, model := range raw.Models {
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" {
			return nil, fmt.Errorf("models[%d].id 不能为空", index)
		}
		provider := strings.TrimSpace(model.Provider)
		if provider == "" {
			// 缺省 bedrock，保持既有配置文件向后兼容。
			provider = llm.ProviderBedrock
		}
		if !llm.ValidProvider(provider) {
			return nil, fmt.Errorf("models[%d].provider 未登记: %s", index, provider)
		}
		resolution := llm.Resolution{ModelID: model.ID, Provider: provider}
		if err := registerResolution(resolutions, model.ID, resolution); err != nil {
			return nil, err
		}
		for aliasIndex, alias := range model.Aliases {
			alias = strings.TrimSpace(alias)
			if alias == "" {
				return nil, fmt.Errorf("models[%d].aliases[%d] 不能为空", index, aliasIndex)
			}
			if err := registerResolution(resolutions, alias, resolution); err != nil {
				return nil, err
			}
		}
	}

	defaultName := strings.TrimSpace(raw.Default)
	defaultResolution, ok := resolutions[defaultName]
	if defaultName == "" || !ok {
		return nil, fmt.Errorf("default 必须引用已登记的模型或别名")
	}
	return &Catalog{defaultModel: defaultResolution, resolutions: resolutions}, nil
}

func registerResolution(resolutions map[string]llm.Resolution, name string, resolution llm.Resolution) error {
	if _, exists := resolutions[name]; exists {
		return fmt.Errorf("模型名或别名重复: %s", name)
	}
	resolutions[name] = resolution
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
