package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

func TestHealthDoesNotRequireAuthentication(t *testing.T) {
	t.Parallel()

	server := newTestServer(&runtimeStub{}, resolverStub{})
	response := performRequest(server, http.MethodGet, "/healthz", nil, nil)

	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("health response = %d %s", response.Code, response.Body.String())
	}
}
