package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/anthropic"
)

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	completion := requestCompletion{startedAt: time.Now(), operation: "messages", result: "rejected"}
	defer s.logCompletion(r, &completion)

	prepared, ok := s.prepare(w, r)
	if !ok {
		return
	}
	completion.modelID = prepared.ModelID
	completion.stream = prepared.Stream
	if prepared.Stream {
		if err := s.acquireStream(r.Context()); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				completion.result = "client_canceled"
				return
			}
			completion.result = "overloaded"
			s.writeError(w, r, http.StatusServiceUnavailable, anthropic.ErrorTypeOverloaded, "流式请求并发已满")
			return
		}
		defer s.releaseStream()
		completion.result = completionResult(s.handleStream(w, r, prepared))
		return
	}

	response, err := s.runtime.Invoke(r.Context(), prepared.ModelID, prepared.Body)
	if err != nil {
		completion.result = completionResult(err)
		if completion.result == "client_canceled" {
			return
		}
		s.handleUpstreamError(w, r, err)
		return
	}
	completion.result = "success"
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(response); err != nil {
		s.logger.Error("写出响应失败", "request_id", requestID(r.Context()), "error", err)
	}
}
