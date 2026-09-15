package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/anthropic"
)

const contentTypeEventStream = "text/event-stream"

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, prepared anthropic.PreparedRequest) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, r, http.StatusInternalServerError, anthropic.ErrorTypeAPI, "当前 HTTP 服务不支持流式响应")
		return fmt.Errorf("当前 HTTP 服务不支持流式响应")
	}

	w.Header().Set("Content-Type", contentTypeEventStream)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	chunks := make(chan []byte)
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- s.runtime.Stream(ctx, prepared.ModelID, prepared.Body, func(chunk []byte) error {
			copied := append([]byte(nil), chunk...)
			select {
			case chunks <- copied:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()

	ticker := time.NewTicker(s.streamHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case chunk := <-chunks:
			if err := writeStreamChunk(w, flusher, chunk); err != nil {
				s.logger.Error("写出流事件失败", "request_id", requestID(r.Context()), "error", err)
				return err
			}
		case err := <-streamDone:
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					s.writeStreamError(w, r, flusher, err)
				}
			}
			return err
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				s.logger.Error("写出 SSE 心跳失败", "request_id", requestID(r.Context()), "error", err)
				return err
			}
			flusher.Flush()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func writeStreamChunk(w http.ResponseWriter, flusher http.Flusher, chunk []byte) error {
	eventType, err := streamEventType(chunk)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, chunk); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func (s *Server) writeStreamError(w http.ResponseWriter, r *http.Request, flusher http.Flusher, err error) {
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
