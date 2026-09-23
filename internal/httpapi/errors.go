package httpapi

import (
	"errors"
	"net/http"

	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/anthropic"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/llm"
)

const errorType = "error"

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, kind, message string) {
	writeJSON(w, status, anthropic.ErrorEnvelope{
		Type: errorType,
		Error: anthropic.ErrorDetail{
			Type:    kind,
			Message: message,
		},
		RequestID: requestID(r.Context()),
	})
}

func (s *Server) handleUpstreamError(w http.ResponseWriter, r *http.Request, err error) {
	status, kind, message := classifyUpstreamError(err)
	s.logger.Error("上游请求失败", "request_id", requestID(r.Context()), "status", status, "error", err)
	s.writeError(w, r, status, kind, message)
}

func classifyUpstreamError(err error) (int, string, string) {
	// provider 自带分类时优先采用，避免 HTTP 层耦合具体厂商 SDK。
	var statusErr llm.StatusError
	if errors.As(err, &statusErr) {
		return statusErr.Status(), statusErr.Kind(), statusErr.ClientMessage()
	}

	status := http.StatusBadGateway
	kind := anthropic.ErrorTypeAPI
	message := "上游请求失败"

	var responseError *smithyhttp.ResponseError
	if errors.As(err, &responseError) {
		responseStatus := responseError.HTTPStatusCode()
		if responseStatus >= 100 && responseStatus <= 599 {
			status = responseStatus
		}
	}
	switch status {
	case http.StatusBadRequest:
		kind = anthropic.ErrorTypeInvalidRequest
	case http.StatusUnauthorized:
		kind = anthropic.ErrorTypeAuthentication
	case http.StatusForbidden:
		kind = anthropic.ErrorTypePermission
	case http.StatusNotFound:
		kind = anthropic.ErrorTypeNotFound
	case http.StatusTooManyRequests:
		kind = anthropic.ErrorTypeRateLimit
	case http.StatusServiceUnavailable:
		kind = anthropic.ErrorTypeOverloaded
	}

	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		message = apiError.ErrorMessage()
	}
	return status, kind, message
}
