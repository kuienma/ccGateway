package anthropic

import "fmt"

const (
	ErrorTypeInvalidRequest = "invalid_request_error"
	ErrorTypeAuthentication = "authentication_error"
	ErrorTypePermission     = "permission_error"
	ErrorTypeNotFound       = "not_found_error"
	ErrorTypeRateLimit      = "rate_limit_error"
	ErrorTypeAPI            = "api_error"
	ErrorTypeOverloaded     = "overloaded_error"
)

type RequestError struct {
	Kind    string
	Message string
}

func (e *RequestError) Error() string {
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

type ErrorEnvelope struct {
	Type      string      `json:"type"`
	Error     ErrorDetail `json:"error"`
	RequestID string      `json:"request_id,omitempty"`
}

type ErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}
