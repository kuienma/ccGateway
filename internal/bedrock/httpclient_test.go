package bedrock

import (
	"errors"
	"testing"
	"time"
)

func TestNewHTTPClientAppliesBoundedConnectionSettings(t *testing.T) {
	t.Parallel()

	client := NewHTTPClient()
	dialer := client.GetDialer()
	transport := client.GetTransport()

	if dialer.Timeout != 10*time.Second || dialer.KeepAlive != 30*time.Second {
		t.Fatalf("dialer timeout=%s keepalive=%s", dialer.Timeout, dialer.KeepAlive)
	}
	if transport.TLSHandshakeTimeout != 10*time.Second {
		t.Fatalf("TLSHandshakeTimeout = %s", transport.TLSHandshakeTimeout)
	}
	if transport.ResponseHeaderTimeout != 120*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %s", transport.ResponseHeaderTimeout)
	}
	if transport.IdleConnTimeout != 90*time.Second {
		t.Fatalf("IdleConnTimeout = %s", transport.IdleConnTimeout)
	}
	if transport.MaxIdleConns != 100 || transport.MaxIdleConnsPerHost != 20 {
		t.Fatalf("idle conns total=%d per_host=%d", transport.MaxIdleConns, transport.MaxIdleConnsPerHost)
	}
	if client.GetTimeout() != 0 {
		t.Fatalf("流式 HTTP Client 不应设置总超时: %s", client.GetTimeout())
	}
}

func TestNewRetryerBoundsAttemptsAndBackoff(t *testing.T) {
	t.Parallel()

	retryer := NewRetryer()
	if retryer.MaxAttempts() != 3 {
		t.Fatalf("MaxAttempts = %d", retryer.MaxAttempts())
	}
	delay, err := retryer.RetryDelay(20, errors.New("unexpected EOF"))
	if err != nil {
		t.Fatalf("RetryDelay 失败: %v", err)
	}
	if delay < 0 || delay > 5*time.Second {
		t.Fatalf("RetryDelay = %s", delay)
	}
}
