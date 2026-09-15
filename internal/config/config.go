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
	if raw := strings.TrimSpace(value(lookup, "MAX_BODY_BYTES")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			return Config{}, fmt.Errorf("MAX_BODY_BYTES 必须是正整数")
		}
		cfg.MaxBodyBytes = parsed
	}
	if raw := strings.TrimSpace(value(lookup, "SHUTDOWN_TIMEOUT")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			return Config{}, fmt.Errorf("SHUTDOWN_TIMEOUT 必须是正 duration")
		}
		cfg.ShutdownTimeout = parsed
	}
	if raw := strings.TrimSpace(value(lookup, "STREAM_HEARTBEAT_INTERVAL")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			return Config{}, fmt.Errorf("STREAM_HEARTBEAT_INTERVAL 必须是正 duration")
		}
		cfg.StreamHeartbeatInterval = parsed
	}
	if raw := strings.TrimSpace(value(lookup, "MAX_CONCURRENT_STREAMS")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return Config{}, fmt.Errorf("MAX_CONCURRENT_STREAMS 必须是正整数")
		}
		cfg.MaxConcurrentStreams = parsed
	}
	if raw := strings.TrimSpace(value(lookup, "STREAM_QUEUE_TIMEOUT")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			return Config{}, fmt.Errorf("STREAM_QUEUE_TIMEOUT 必须是正 duration")
		}
		cfg.StreamQueueTimeout = parsed
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
