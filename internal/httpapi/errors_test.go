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
	if message != "Bedrock 请求失败" {
		t.Fatalf("message = %q", message)
	}
}
