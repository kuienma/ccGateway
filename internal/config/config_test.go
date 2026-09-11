package config

import (
	"testing"
	"time"
)

func TestLoadFromLookup(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		"AWS_REGION":        "us-east-1",
		"GATEWAY_API_KEY":   "gateway-secret",
		"MODEL_CONFIG_FILE": "/tmp/models.json",
		"LISTEN_ADDR":       ":9090",
		"MAX_BODY_BYTES":    "1024",
		"SHUTDOWN_TIMEOUT":  "5s",
	}

	cfg, err := LoadFromLookup(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}

	if cfg.AWSRegion != "us-east-1" {
		t.Fatalf("AWSRegion = %q", cfg.AWSRegion)
	}
	if cfg.GatewayAPIKey != "gateway-secret" {
		t.Fatalf("GatewayAPIKey = %q", cfg.GatewayAPIKey)
	}
	if cfg.ModelConfigFile != "/tmp/models.json" {
		t.Fatalf("ModelConfigFile = %q", cfg.ModelConfigFile)
	}
	if cfg.ListenAddr != ":9090" {
		t.Fatalf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.MaxBodyBytes != 1024 {
		t.Fatalf("MaxBodyBytes = %d", cfg.MaxBodyBytes)
	}
	if cfg.ShutdownTimeout != 5*time.Second {
		t.Fatalf("ShutdownTimeout = %s", cfg.ShutdownTimeout)
	}
}

func TestLoadFromLookupAcceptsAWSDefaultRegion(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		"AWS_DEFAULT_REGION": "us-west-2",
		"GATEWAY_API_KEY":    "gateway-secret",
	}
	cfg, err := LoadFromLookup(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.AWSRegion != "us-west-2" {
		t.Fatalf("AWSRegion = %q", cfg.AWSRegion)
	}
}

func TestLoadFromLookupUsesDefaults(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		"AWS_REGION":      "us-east-1",
		"GATEWAY_API_KEY": "gateway-secret",
	}

	cfg, err := LoadFromLookup(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}

	if cfg.ListenAddr != DefaultListenAddr {
		t.Fatalf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.ModelConfigFile != DefaultModelConfigFile {
		t.Fatalf("ModelConfigFile = %q", cfg.ModelConfigFile)
	}
	if cfg.MaxBodyBytes != DefaultMaxBodyBytes {
		t.Fatalf("MaxBodyBytes = %d", cfg.MaxBodyBytes)
	}
}

func TestLoadFromLookupRejectsMissingRequiredValues(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		values map[string]string
	}{
		{name: "缺少区域", values: map[string]string{"GATEWAY_API_KEY": "key"}},
		{name: "缺少网关密钥", values: map[string]string{"AWS_REGION": "us-east-1"}},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadFromLookup(func(key string) (string, bool) {
				value, ok := testCase.values[key]
				return value, ok
			})
			if err == nil {
				t.Fatal("预期配置校验失败")
			}
		})
	}
}

func TestLoadFromLookupRejectsInvalidLimits(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		"AWS_REGION":      "us-east-1",
		"GATEWAY_API_KEY": "gateway-secret",
		"MAX_BODY_BYTES":  "0",
	}

	_, err := LoadFromLookup(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err == nil {
		t.Fatal("预期 MAX_BODY_BYTES 校验失败")
	}
}
