package config

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestLoadFromLookup(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		"AWS_REGION":                "us-east-1",
		"GATEWAY_API_KEY":           "gateway-secret",
		"MODEL_CONFIG_FILE":         "/tmp/models.json",
		"LISTEN_ADDR":               ":9090",
		"MAX_BODY_BYTES":            "1024",
		"SHUTDOWN_TIMEOUT":          "5s",
		"STREAM_HEARTBEAT_INTERVAL": "2s",
		"MAX_CONCURRENT_STREAMS":    "6",
		"STREAM_QUEUE_TIMEOUT":      "7s",
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
	if cfg.StreamHeartbeatInterval != 2*time.Second {
		t.Fatalf("StreamHeartbeatInterval = %s", cfg.StreamHeartbeatInterval)
	}
	if cfg.MaxConcurrentStreams != 6 {
		t.Fatalf("MaxConcurrentStreams = %d", cfg.MaxConcurrentStreams)
	}
	if cfg.StreamQueueTimeout != 7*time.Second {
		t.Fatalf("StreamQueueTimeout = %s", cfg.StreamQueueTimeout)
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
	if cfg.StreamHeartbeatInterval != DefaultStreamHeartbeatInterval {
		t.Fatalf("StreamHeartbeatInterval = %s", cfg.StreamHeartbeatInterval)
	}
	if cfg.MaxConcurrentStreams != DefaultMaxConcurrentStreams {
		t.Fatalf("MaxConcurrentStreams = %d", cfg.MaxConcurrentStreams)
	}
	if cfg.StreamQueueTimeout != DefaultStreamQueueTimeout {
		t.Fatalf("StreamQueueTimeout = %s", cfg.StreamQueueTimeout)
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

func TestLoadFromLookupRejectsInvalidStabilitySettings(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		key   string
		value string
	}{
		{name: "心跳间隔", key: "STREAM_HEARTBEAT_INTERVAL", value: "0s"},
		{name: "流并发数", key: "MAX_CONCURRENT_STREAMS", value: "0"},
		{name: "排队超时", key: "STREAM_QUEUE_TIMEOUT", value: "invalid"},
	}
	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			values := map[string]string{
				"AWS_REGION":      "us-east-1",
				"GATEWAY_API_KEY": "gateway-secret",
				testCase.key:      testCase.value,
			}
			_, err := LoadFromLookup(func(key string) (string, bool) {
				value, ok := values[key]
				return value, ok
			})
			if err == nil {
				t.Fatal("预期稳定性配置校验失败")
			}
		})
	}
}

func TestLoadFromLookupBoundaries(t *testing.T) {
	t.Parallel()
	defaults, err := LoadFromLookup(lookupValues(map[string]string{"AWS_REGION": "us-east-1", "GATEWAY_API_KEY": "key"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"MAX_BODY_BYTES", "MAX_CONCURRENT_STREAMS", "SHUTDOWN_TIMEOUT", "STREAM_HEARTBEAT_INTERVAL", "STREAM_QUEUE_TIMEOUT"} {
		for _, raw := range []string{"", " \t ", "0", "-1", "invalid", "9223372036854775808"} {
			t.Run(key+"/"+raw, func(t *testing.T) {
				values := map[string]string{"AWS_REGION": "us-east-1", "GATEWAY_API_KEY": "key", key: raw}
				cfg, err := LoadFromLookup(lookupValues(values))
				if strings.TrimSpace(raw) == "" {
					if err != nil || cfg != defaults {
						t.Fatalf("空白应使用默认值: %+v %v", cfg, err)
					}
				} else {
					suffix := " 必须是正 duration"
					if key == "MAX_BODY_BYTES" || key == "MAX_CONCURRENT_STREAMS" {
						suffix = " 必须是正整数"
					}
					if err == nil || err.Error() != key+suffix || cfg != (Config{}) {
						t.Fatalf("非法值未保留校验语义: %+v %v", cfg, err)
					}
				}
			})
		}
	}
}

func TestLoadFromLookupTrimsAndPrefersAWSRegion(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"AWS_REGION": " us-east-1 ", "AWS_DEFAULT_REGION": "us-west-2", "GATEWAY_API_KEY": " key ",
		"LISTEN_ADDR": " :9090 ", "MODEL_CONFIG_FILE": " models.json ", "MAX_BODY_BYTES": " +9223372036854775807 ",
		"MAX_CONCURRENT_STREAMS": " +2 ", "SHUTDOWN_TIMEOUT": " 1ns ", "STREAM_HEARTBEAT_INTERVAL": " 2m ", "STREAM_QUEUE_TIMEOUT": " 1.5s ",
	}
	cfg, err := LoadFromLookup(lookupValues(values))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AWSRegion != "us-east-1" || cfg.GatewayAPIKey != "key" || cfg.ListenAddr != ":9090" || cfg.ModelConfigFile != "models.json" || cfg.MaxBodyBytes != math.MaxInt64 || cfg.MaxConcurrentStreams != 2 || cfg.ShutdownTimeout != time.Nanosecond || cfg.StreamHeartbeatInterval != 2*time.Minute || cfg.StreamQueueTimeout != 1500*time.Millisecond {
		t.Fatalf("cfg = %+v", cfg)
	}
	values["AWS_REGION"] = " \t "
	cfg, err = LoadFromLookup(lookupValues(values))
	if err != nil || cfg.AWSRegion != "us-west-2" {
		t.Fatalf("区域 fallback = %+v %v", cfg, err)
	}
	for _, key := range []string{"SHUTDOWN_TIMEOUT", "STREAM_HEARTBEAT_INTERVAL", "STREAM_QUEUE_TIMEOUT"} {
		for _, raw := range []string{"-1ns", "0s", "9223372036854775808ns"} {
			copy := map[string]string{"AWS_REGION": "r", "GATEWAY_API_KEY": "k", key: raw}
			if _, err := LoadFromLookup(lookupValues(copy)); err == nil {
				t.Fatalf("%s=%q 应被拒绝", key, raw)
			}
		}
	}
}

func TestLoadFromLookupArkDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFromLookup(lookupValues(map[string]string{
		"AWS_REGION":      "us-east-1",
		"GATEWAY_API_KEY": "k",
	}))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.ArkBaseURL != DefaultArkBaseURL {
		t.Fatalf("ArkBaseURL = %q", cfg.ArkBaseURL)
	}
	if cfg.ArkAPIKey != "" {
		t.Fatal("未设置 ARK_API_KEY 时应为空而不是报错")
	}
	if cfg.ArkTimeout != DefaultArkTimeout {
		t.Fatalf("ArkTimeout = %v", cfg.ArkTimeout)
	}
}

func TestLoadFromLookupArkOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFromLookup(lookupValues(map[string]string{
		"AWS_REGION":      "us-east-1",
		"GATEWAY_API_KEY": "k",
		"ARK_API_KEY":     " ark-secret ",
		"ARK_BASE_URL":    " https://example.test/api/v3 ",
		"ARK_TIMEOUT":     "30s",
	}))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.ArkAPIKey != "ark-secret" {
		t.Fatalf("ArkAPIKey = %q", cfg.ArkAPIKey)
	}
	if cfg.ArkBaseURL != "https://example.test/api/v3" {
		t.Fatalf("ArkBaseURL = %q", cfg.ArkBaseURL)
	}
	if cfg.ArkTimeout != 30*time.Second {
		t.Fatalf("ArkTimeout = %v", cfg.ArkTimeout)
	}
	if _, err := LoadFromLookup(lookupValues(map[string]string{
		"AWS_REGION":      "us-east-1",
		"GATEWAY_API_KEY": "k",
		"ARK_TIMEOUT":     "0s",
	})); err == nil {
		t.Fatal("ARK_TIMEOUT=0s 应被拒绝")
	}
}

func lookupValues(values map[string]string) LookupFunc {
	return func(key string) (string, bool) { value, ok := values[key]; return value, ok }
}
