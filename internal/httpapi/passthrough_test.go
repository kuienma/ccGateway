package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/anthropic"
)

func TestOutputFormatAndUpstreamErrorsPassThrough(t *testing.T) {
	t.Parallel()
	const outputConfig = `{"format":{"type":"json_schema","schema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}},"effort":"high"}`
	for _, upstream := range []struct {
		status              int
		code, message, kind string
	}{
		{400, "ValidationException", "output_config.format: Extra inputs are not permitted", anthropic.ErrorTypeInvalidRequest},
		{503, "ServiceUnavailableException", "Model service is temporarily unavailable", anthropic.ErrorTypeOverloaded},
	} {
		for _, flow := range []struct {
			name, path      string
			stream, started bool
		}{
			{"messages", "/v1/messages", false, false},
			{"count_tokens", "/v1/messages/count_tokens", false, false},
			{"stream_before_chunk", "/v1/messages", true, false},
			{"stream_after_chunk", "/v1/messages", true, true},
		} {
			t.Run(fmt.Sprintf("%s/%d", flow.name, upstream.status), func(t *testing.T) {
				runtime := &runtimeStub{err: fmt.Errorf("调用 Bedrock: %w", &smithyhttp.ResponseError{
					Response: &smithyhttp.Response{Response: &http.Response{StatusCode: upstream.status}},
					Err:      &smithy.GenericAPIError{Code: upstream.code, Message: upstream.message},
				})}
				if flow.started {
					runtime.streamChunks = [][]byte{[]byte(`{"type":"message_start"}`)}
				}
				body := []byte(fmt.Sprintf(`{"model":"opus-5","max_tokens":64,"messages":[{"role":"user","content":"Return an answer."}],"stream":%t,"output_config":%s}`, flow.stream, outputConfig))
				response := performRequest(newTestServer(runtime, resolverStub{"opus-5": "model-id"}), http.MethodPost, flow.path, body, map[string]string{"x-api-key": testGatewayKey})
				var forwarded map[string]json.RawMessage
				if err := json.Unmarshal(runtime.body, &forwarded); err != nil {
					t.Fatal(err)
				}
				if string(forwarded["output_config"]) != outputConfig {
					t.Errorf("output_config 未原样透传: %s", runtime.body)
				}
				payload := response.Body.String()
				wantStatus := upstream.status
				if flow.stream {
					wantStatus = http.StatusOK // SSE 响应已开始，错误通过事件返回。
					prefix := "event: error\ndata: "
					if flow.started {
						prefix = "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" + prefix
					}
					if !strings.HasPrefix(payload, prefix) {
						t.Fatalf("SSE 错误事件或顺序错误: %s", payload)
					}
					payload = strings.TrimPrefix(payload, prefix)
				}
				if response.Code != wantStatus {
					t.Errorf("status=%d, want=%d", response.Code, wantStatus)
				}
				var envelope anthropic.ErrorEnvelope
				if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
					t.Fatal(err)
				}
				if envelope.Type != "error" || envelope.Error.Type != upstream.kind || envelope.Error.Message != upstream.message {
					t.Errorf("上游错误信息未保留: %+v", envelope)
				}
				if envelope.RequestID == "" || envelope.RequestID != response.Header().Get("request-id") {
					t.Errorf("request ID 未关联: %+v", envelope)
				}
			})
		}
	}
}
