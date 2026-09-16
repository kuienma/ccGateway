package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// requestCompletion 由单个请求持有，延迟记录最终执行状态。
type requestCompletion struct {
	startedAt time.Time
	operation string
	modelID   string
	stream    bool
	result    string
}

func (s *Server) logCompletion(r *http.Request, completion *requestCompletion) {
	s.logger.Info(
		"请求完成",
		"request_id", requestID(r.Context()),
		"operation", completion.operation,
		"model_id", completion.modelID,
		"stream", completion.stream,
		"duration_ms", time.Since(completion.startedAt).Milliseconds(),
		"result", completion.result,
	)
}

func completionResult(err error) string {
	if err == nil {
		return "success"
	}
	if errors.Is(err, context.Canceled) {
		return "client_canceled"
	}
	return "upstream_error"
}
