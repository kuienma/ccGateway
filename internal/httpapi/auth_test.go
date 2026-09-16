package httpapi

import (
	"net/http"
	"testing"
)

func TestMessagesRequiresGatewayAuthentication(t *testing.T) {
	t.Parallel()

	server := newTestServer(&runtimeStub{}, resolverStub{})
	response := performRequest(server, http.MethodPost, "/v1/messages", []byte(`{}`), nil)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	assertErrorType(t, response.Body.Bytes(), "authentication_error")
}

func TestAuthenticationPriorityAcrossEndpoints(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		for _, tc := range []struct {
			name, key, auth string
			status          int
		}{
			{"key_wins", testGatewayKey, "Bearer wrong", 200},
			{"wrong_key_wins", "wrong", "Bearer " + testGatewayKey, 401},
			{"blank_key_falls_back", " \t ", "Bearer " + testGatewayKey, 200},
			{"bearer_case_sensitive", "", "bearer " + testGatewayKey, 401},
			{"trimmed_key", " " + testGatewayKey + " ", "", 200},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				runtime := &runtimeStub{response: []byte(`{}`)}
				server := newTestServer(runtime, resolverStub{"": "model-id"})
				response := performRequest(server, http.MethodPost, path, []byte(`{}`), map[string]string{"x-api-key": tc.key, "Authorization": tc.auth})
				if response.Code != tc.status {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				if tc.status == 401 {
					if runtime.body != nil {
						t.Fatal("认证失败仍调用上游")
					}
					assertRequestID(t, response)
				}
			})
		}
	}
}
