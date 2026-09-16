package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultListenAddr              = ":8080"
	DefaultModelConfigFile         = "configs/models.json"
	DefaultMaxBodyBytes            = int64(20 << 20)
	DefaultShutdownTimeout         = 15 * time.Second
	DefaultStreamHeartbeatInterval = 15 * time.Second
	DefaultMaxConcurrentStreams    = 4
	DefaultStreamQueueTimeout      = 30 * time.Second
)

type Config struct {
	ListenAddr              string
	AWSRegion               string
	GatewayAPIKey           string
	ModelConfigFile         string
	MaxBodyBytes            int64
	ShutdownTimeout         time.Duration
	StreamHeartbeatInterval time.Duration
	MaxConcurrentStreams    int
	StreamQueueTimeout      time.Duration
}

type LookupFunc func(string) (string, bool)

func Load() (Config, error) {
	return LoadFromLookup(os.LookupEnv)
}

func LoadFromLookup(lookup LookupFunc) (Config, error) {
	cfg := Config{
		ListenAddr:              valueOrDefault(lookup, "LISTEN_ADDR", DefaultListenAddr),
		AWSRegion:               awsRegion(lookup),
		GatewayAPIKey:           strings.TrimSpace(value(lookup, "GATEWAY_API_KEY")),
		ModelConfigFile:         valueOrDefault(lookup, "MODEL_CONFIG_FILE", DefaultModelConfigFile),
		MaxBodyBytes:            DefaultMaxBodyBytes,
		ShutdownTimeout:         DefaultShutdownTimeout,
		StreamHeartbeatInterval: DefaultStreamHeartbeatInterval,
		MaxConcurrentStreams:    DefaultMaxConcurrentStreams,
		StreamQueueTimeout:      DefaultStreamQueueTimeout,
	}

	if cfg.AWSRegion == "" {
		return Config{}, fmt.Errorf("AWS_REGION 或 AWS_DEFAULT_REGION 不能为空")
	}
	if cfg.GatewayAPIKey == "" {
		return Config{}, fmt.Errorf("GATEWAY_API_KEY 不能为空")
	}
	var err error
	if cfg.MaxBodyBytes, err = positiveInteger(lookup, "MAX_BODY_BYTES", cfg.MaxBodyBytes, 64); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = positiveDuration(lookup, "SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout); err != nil {
		return Config{}, err
	}
	if cfg.StreamHeartbeatInterval, err = positiveDuration(lookup, "STREAM_HEARTBEAT_INTERVAL", cfg.StreamHeartbeatInterval); err != nil {
		return Config{}, err
	}
	streams, err := positiveInteger(lookup, "MAX_CONCURRENT_STREAMS", int64(cfg.MaxConcurrentStreams), strconv.IntSize)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxConcurrentStreams = int(streams)
	if cfg.StreamQueueTimeout, err = positiveDuration(lookup, "STREAM_QUEUE_TIMEOUT", cfg.StreamQueueTimeout); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func awsRegion(lookup LookupFunc) string {
	if region := strings.TrimSpace(value(lookup, "AWS_REGION")); region != "" {
		return region
	}
	return strings.TrimSpace(value(lookup, "AWS_DEFAULT_REGION"))
}

func value(lookup LookupFunc, key string) string {
	result, _ := lookup(key)
	return result
}

func valueOrDefault(lookup LookupFunc, key, fallback string) string {
	result := strings.TrimSpace(value(lookup, key))
	if result == "" {
		return fallback
	}
	return result
}

// bitSize 保留 int 与 int64 配置各自的溢出边界。
func positiveInteger(lookup LookupFunc, key string, fallback int64, bitSize int) (int64, error) {
	raw := strings.TrimSpace(value(lookup, key))
	if raw == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(raw, 10, bitSize)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s 必须是正整数", key)
	}
	return parsed, nil
}

func positiveDuration(lookup LookupFunc, key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(value(lookup, key))
	if raw == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s 必须是正 duration", key)
	}
	return parsed, nil
}
