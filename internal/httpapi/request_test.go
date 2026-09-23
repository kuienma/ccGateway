package httpapi

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/llm"
)

func TestMessagesRejectsUnknownModel(t *testing.T) {
	t.Parallel()

	server := newTestServer(&runtimeStub{}, resolverStub{})
	headers := map[string]string{"x-api-key": testGatewayKey}
	response := performRequest(server, http.MethodPost, "/v1/messages", []byte(`{"model":"unknown"}`), headers)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	assertErrorType(t, response.Body.Bytes(), "not_found_error")
}

func TestMessagesEnforcesBodyLimit(t *testing.T) {
	t.Parallel()

	server := NewServer(ServerConfig{APIKey: testGatewayKey, MaxBodyBytes: 8, StreamHeartbeatInterval: time.Hour, MaxConcurrentStreams: 4, StreamQueueTimeout: time.Second},
		resolverStub{}, map[string]llm.Provider{llm.ProviderBedrock: &runtimeStub{}}, discardLogger())
	headers := map[string]string{"x-api-key": testGatewayKey}
	response := performRequest(server, http.MethodPost, "/v1/messages", bytes.Repeat([]byte("x"), 9), headers)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestRejectedRequestsNeverCallUpstream(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		for _, tc := range []struct {
			name, body string
			status     int
		}{
			{"unknown_model", `{"model":"missing"}`, 404},
			{"invalid_json", `{`, 400},
			{"invalid_stream", `{"stream":"yes"}`, 400},
			{"invalid_model", `{"model":123}`, 400},
			{"not_object", `[]`, 400},
			{"too_large", strings.Repeat("x", 65), 413},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				runtime := &runtimeStub{}
				cfg := ServerConfig{APIKey: testGatewayKey, MaxBodyBytes: 64, StreamHeartbeatInterval: time.Hour, MaxConcurrentStreams: 1, StreamQueueTimeout: time.Second}
				response := performRequest(NewServer(cfg, resolverStub{"": "id"}, map[string]llm.Provider{llm.ProviderBedrock: runtime}, discardLogger()), http.MethodPost, path, []byte(tc.body), map[string]string{"x-api-key": testGatewayKey})
				if response.Code != tc.status {
					t.Fatalf("status=%d, want %d", response.Code, tc.status)
				}
				if runtime.body != nil {
					t.Fatalf("拒绝请求却调用上游: %s", runtime.body)
				}
				assertRequestID(t, response)
			})
		}
	}
}
