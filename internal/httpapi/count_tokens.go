package httpapi

import (
	"net/http"
	"time"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/anthropic"
)

func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	completion := requestCompletion{startedAt: time.Now(), operation: "count_tokens", result: "rejected"}
	defer s.logCompletion(r, &completion)

	prepared, ok := s.prepare(w, r)
	if !ok {
		return
	}
	completion.modelID = prepared.ModelID
	provider, ok := s.providerFor(prepared.Provider)
	if !ok {
		s.writeError(w, r, http.StatusServiceUnavailable, anthropic.ErrorTypeAPI,
			"模型所属 provider 未启用: "+prepared.Provider)
		return
	}
	count, err := provider.CountTokens(r.Context(), prepared.ModelID, prepared.Body)
	if err != nil {
		completion.result = completionResult(err)
		if completion.result == "client_canceled" {
			return
		}
		s.handleUpstreamError(w, r, err)
		return
	}
	completion.result = "success"
	writeJSON(w, http.StatusOK, map[string]int32{"input_tokens": count})
}
