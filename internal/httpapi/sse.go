package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/anthropic"
)

const contentTypeEventStream = "text/event-stream"

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, prepared anthropic.PreparedRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, r, http.StatusInternalServerError, anthropic.ErrorTypeAPI, "当前 HTTP 服务不支持流式响应")
		return
	}

	w.Header().Set("Content-Type", contentTypeEventStream)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	err := s.runtime.Stream(r.Context(), prepared.ModelID, prepared.Body, func(chunk []byte) error {
		eventType, err := streamEventType(chunk)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, chunk); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	})
	if err == nil {
		return
	}

	s.logger.Error("Bedrock 流式请求失败", "request_id", requestID(r.Context()), "error", err)
	envelope := anthropic.ErrorEnvelope{
		Type:      errorType,
		Error:     anthropic.ErrorDetail{Type: anthropic.ErrorTypeAPI, Message: "Bedrock 流式请求失败"},
		RequestID: requestID(r.Context()),
	}
	encoded, marshalErr := json.Marshal(envelope)
	if marshalErr == nil {
		_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", encoded)
		flusher.Flush()
	}
}

func streamEventType(chunk []byte) (string, error) {
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(chunk, &event); err != nil || event.Type == "" {
		return "", fmt.Errorf("Bedrock 流事件缺少有效 type")
	}
	return event.Type, nil
}
