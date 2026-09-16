package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"
)

// 防止抽取公共日志逻辑时丢失提前返回路径、请求关联或流式标记。
func TestCompletionLogAcrossEndpoints(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []struct{ path, operation string }{
		{"/v1/messages", "messages"},
		{"/v1/messages/count_tokens", "count_tokens"},
	} {
		for _, tc := range []struct {
			name, body, result, model string
			err                       error
			status                    int
			stream                    bool
		}{
			{name: "success", body: `{"model":"opus-5","messages":[]}`, result: "success", model: "model-id", status: 200},
			{name: "stream", body: `{"model":"opus-5","messages":[],"stream":true}`, result: "success", model: "model-id", status: 200, stream: true},
			{name: "invalid_json", body: `{`, result: "rejected", status: 400},
			{name: "unknown_model", body: `{"model":"unknown","messages":[]}`, result: "rejected", status: 404},
			{name: "upstream_error", body: `{"model":"opus-5","messages":[]}`, result: "upstream_error", model: "model-id", err: errors.New("upstream reset"), status: 502},
			{name: "canceled", body: `{"model":"opus-5","messages":[]}`, result: "client_canceled", model: "model-id", err: context.Canceled, status: 200},
		} {
			t.Run(endpoint.operation+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				var logs bytes.Buffer
				server := NewServer(ServerConfig{APIKey: testGatewayKey, MaxBodyBytes: 1 << 20, StreamHeartbeatInterval: time.Hour, MaxConcurrentStreams: 1, StreamQueueTimeout: time.Second},
					resolverStub{"opus-5": "model-id"}, &runtimeStub{response: []byte(`{"type":"message"}`), tokenCount: 12, err: tc.err}, slog.New(slog.NewJSONHandler(&logs, nil)))
				response := performRequest(server, http.MethodPost, endpoint.path, []byte(tc.body), map[string]string{"x-api-key": testGatewayKey})
				if response.Code != tc.status {
					t.Fatalf("status = %d, want %d", response.Code, tc.status)
				}
				decoder := json.NewDecoder(&logs)
				completions := 0
				for {
					var record struct {
						Message   string `json:"msg"`
						RequestID string `json:"request_id"`
						Operation string `json:"operation"`
						ModelID   string `json:"model_id"`
						Stream    *bool  `json:"stream"`
						Duration  *int64 `json:"duration_ms"`
						Result    string `json:"result"`
					}
					err := decoder.Decode(&record)
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					if record.Message != "请求完成" {
						continue
					}
					completions++
					if record.RequestID == "" || record.RequestID != response.Header().Get("request-id") {
						t.Fatalf("请求 ID 不一致: %+v", record)
					}
					wantStream := tc.stream && endpoint.operation == "messages"
					if record.Operation != endpoint.operation || record.ModelID != tc.model || record.Result != tc.result || record.Stream == nil || *record.Stream != wantStream {
						t.Fatalf("完成日志不符合预期: %+v", record)
					}
					if record.Duration == nil || *record.Duration < 0 {
						t.Fatalf("缺少有效耗时: %+v", record)
					}
				}
				if completions != 1 {
					t.Fatalf("完成日志数量 = %d, want 1", completions)
				}
			})
		}
	}
}
