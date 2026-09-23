package httpapi

import (
	"errors"
	"net/http"
	"testing"

	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/anthropic"
)

func TestClassifyUpstreamErrorMapsRateLimit(t *testing.T) {
	t.Parallel()

	err := &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusTooManyRequests}},
		Err:      &smithy.GenericAPIError{Code: "ThrottlingException", Message: "请求过于频繁"},
	}
	status, kind, message := classifyUpstreamError(err)
	if status != http.StatusTooManyRequests || kind != anthropic.ErrorTypeRateLimit {
		t.Fatalf("映射结果 = %d, %s", status, kind)
	}
	if message != "请求过于频繁" {
		t.Fatalf("message = %q", message)
	}
}

func TestClassifyUpstreamErrorFallsBackToBadGatewayForMissingStatus(t *testing.T) {
	t.Parallel()

	err := &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: 0}},
		Err:      errors.New("unexpected EOF"),
	}
	status, kind, message := classifyUpstreamError(err)
	if status != http.StatusBadGateway || kind != anthropic.ErrorTypeAPI {
		t.Fatalf("映射结果 = %d, %s", status, kind)
	}
	if message != "上游请求失败" {
		t.Fatalf("message = %q", message)
	}
}

type stubStatusError struct{}

func (stubStatusError) Error() string         { return "stub" }
func (stubStatusError) Status() int           { return http.StatusTooManyRequests }
func (stubStatusError) Kind() string          { return anthropic.ErrorTypeRateLimit }
func (stubStatusError) ClientMessage() string { return "配额已用尽" }

func TestClassifyUpstreamErrorPrefersStatusError(t *testing.T) {
	t.Parallel()

	status, kind, message := classifyUpstreamError(stubStatusError{})
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d", status)
	}
	if kind != anthropic.ErrorTypeRateLimit {
		t.Fatalf("kind = %q", kind)
	}
	if message != "配额已用尽" {
		t.Fatalf("message = %q", message)
	}
}
