package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/anthropic"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/llm"
)

const contentTypeEventStream = "text/event-stream"

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, provider llm.Provider, prepared anthropic.PreparedRequest) error {
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
		streamDone <- provider.Stream(ctx, prepared.ModelID, prepared.Body, func(chunk []byte) error {
			copied := append([]byte(nil), chunk...)
			select {
			case chunks <- copied:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()

	heartbeat := time.NewTimer(s.streamHeartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case chunk := <-chunks:
			if err := writeStreamChunk(w, flusher, chunk); err != nil {
				s.logger.Error("写出流事件失败", "request_id", requestID(r.Context()), "error", err)
				return stopAndWaitForStream(cancel, streamDone, err)
			}
			resetTimer(heartbeat, s.streamHeartbeatInterval)
		case err := <-streamDone:
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					s.writeStreamError(w, r, flusher, err)
				}
			}
			return err
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				s.logger.Error("写出 SSE 心跳失败", "request_id", requestID(r.Context()), "error", err)
				return stopAndWaitForStream(cancel, streamDone, err)
			}
			flusher.Flush()
			heartbeat.Reset(s.streamHeartbeatInterval)
		case <-ctx.Done():
			return stopAndWaitForStream(cancel, streamDone, ctx.Err())
		}
	}
}

func stopAndWaitForStream(cancel context.CancelFunc, streamDone <-chan error, result error) error {
	cancel()
	<-streamDone
	return result
}

func resetTimer(timer *time.Timer, interval time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(interval)
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
	s.logger.Error("上游流式请求失败", "request_id", requestID(r.Context()), "error", err)
	_, kind, message := classifyUpstreamError(err)
	envelope := anthropic.ErrorEnvelope{
		Type:      errorType,
		Error:     anthropic.ErrorDetail{Type: kind, Message: message},
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
		return "", fmt.Errorf("上游流事件缺少有效 type")
	}
	return event.Type, nil
}
