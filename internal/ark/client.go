// Package ark 是火山方舟（Ark）网关的 provider 实现。
// 它对外遵守 Anthropic 语义，内部把请求与响应转换成 OpenAI Chat Completions 协议。
package ark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/anthropic"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/ark/translate"
)

// completionsPath 是 Chat Completions 的相对路径。
// Ark 的 BaseURL 已包含 /api/v3，故这里不再补版本段。
const completionsPath = "/chat/completions"

// maxErrorBodyBytes 限制读取错误响应体的长度，避免上游返回超大 body。
const maxErrorBodyBytes = 8 << 10

type Config struct {
	APIKey  string
	BaseURL string
	Timeout time.Duration
}

type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

func NewClient(cfg Config) *Client {
	return &Client{
		apiKey:  cfg.APIKey,
		baseURL: cfg.BaseURL,
		// 流式请求依赖 context 控制生命周期，这里不设 http.Client.Timeout，
		// 否则长流会被硬截断；超时通过 Config.Timeout 施加在非流式路径上。
		http: &http.Client{Transport: newTransport(cfg.Timeout)},
	}
}

func newTransport(timeout time.Duration) http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if timeout > 0 {
		// 只约束拿到响应头的时间，不约束读流式 body 的总时长。
		transport.ResponseHeaderTimeout = timeout
	}
	transport.MaxIdleConnsPerHost = 20
	return transport
}

// APIError 表示一次 Ark 调用失败，实现 llm.StatusError。
type APIError struct {
	StatusCode int
	Type       string
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ark 请求失败 (%d): %s", e.StatusCode, e.Message)
}

func (e *APIError) Status() int { return e.StatusCode }

func (e *APIError) Kind() string { return e.Type }

func (e *APIError) ClientMessage() string { return e.Message }

// rejectionError 把转换层的 Rejection 包成 llm.StatusError。
// Rejection 的输入来自客户端请求体，因此固定映射 400。
type rejectionError struct {
	rejection *translate.Rejection
}

func (e *rejectionError) Error() string { return e.rejection.Error() }

func (e *rejectionError) Status() int { return http.StatusBadRequest }

func (e *rejectionError) Kind() string { return e.rejection.Kind }

func (e *rejectionError) ClientMessage() string { return e.rejection.Message }

func (c *Client) Invoke(ctx context.Context, modelID string, body []byte) ([]byte, error) {
	response, err := c.post(ctx, modelID, body, false)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, c.errorFromResponse(response)
	}
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 Ark 响应体: %w", err)
	}
	// BuildResponse 的输入是上游响应体，出错不是客户端的责任：这里刻意返回普通
	// error 而不是 rejectionError，让 httpapi 兜底成 5xx / api_error。
	// includeThinking 由请求是否显式开启 thinking 决定，见 Task 11；
	// 当前阶段固定不透出 thinking 块。
	return translate.BuildResponse(payload, false)
}

// post 完成请求转换并发起 HTTP 调用，调用方负责关闭 Body。
func (c *Client) post(ctx context.Context, modelID string, body []byte, stream bool) (*http.Response, error) {
	chatRequest, rejection := translate.BuildRequest(modelID, body, stream)
	if rejection != nil {
		return nil, &rejectionError{rejection: rejection}
	}
	encoded, err := json.Marshal(chatRequest)
	if err != nil {
		return nil, fmt.Errorf("编码 Ark 请求: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+completionsPath,
		bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("构造 Ark 请求: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Content-Type", "application/json")
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	}

	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("调用 Ark: %w", err)
	}
	return response, nil
}

// errorFromResponse 读取错误响应体并映射成 Anthropic 错误类型。
func (c *Client) errorFromResponse(response *http.Response) error {
	payload, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBodyBytes))

	message := string(bytes.TrimSpace(payload))
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err == nil && envelope.Error.Message != "" {
		message = envelope.Error.Message
	}
	if message == "" {
		message = "Ark 请求失败"
	}
	return &APIError{
		StatusCode: response.StatusCode,
		Type:       kindForStatus(response.StatusCode),
		Message:    message,
	}
}

func kindForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return anthropic.ErrorTypeInvalidRequest
	case http.StatusUnauthorized:
		return anthropic.ErrorTypeAuthentication
	case http.StatusForbidden:
		return anthropic.ErrorTypePermission
	case http.StatusNotFound:
		return anthropic.ErrorTypeNotFound
	case http.StatusTooManyRequests:
		return anthropic.ErrorTypeRateLimit
	case http.StatusServiceUnavailable:
		return anthropic.ErrorTypeOverloaded
	default:
		return anthropic.ErrorTypeAPI
	}
}

// Stream 在 Task 10 实现。
func (c *Client) Stream(ctx context.Context, modelID string, body []byte, emit func([]byte) error) error {
	return fmt.Errorf("ark 流式调用尚未实现")
}

// CountTokens 在 Task 11 实现。
func (c *Client) CountTokens(ctx context.Context, modelID string, body []byte) (int32, error) {
	return 0, fmt.Errorf("ark token 计数尚未实现")
}
