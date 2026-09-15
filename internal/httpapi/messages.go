package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/anthropic"
)

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	modelID := ""
	stream := false
	result := "rejected"
	defer func() {
		s.logger.Info(
			"请求完成",
			"request_id", requestID(r.Context()),
			"operation", "messages",
			"model_id", modelID,
			"stream", stream,
			"duration_ms", time.Since(startedAt).Milliseconds(),
			"result", result,
		)
	}()

	prepared, ok := s.prepare(w, r)
	if !ok {
		return
	}
	modelID = prepared.ModelID
	stream = prepared.Stream
	if prepared.Stream {
		if !s.acquireStream(r.Context()) {
			result = "overloaded"
			s.writeError(w, r, http.StatusServiceUnavailable, anthropic.ErrorTypeOverloaded, "流式请求并发已满")
			return
		}
		defer s.releaseStream()
		result = completionResult(s.handleStream(w, r, prepared))
		return
	}

	response, err := s.runtime.Invoke(r.Context(), prepared.ModelID, prepared.Body)
	if err != nil {
		result = completionResult(err)
		s.handleUpstreamError(w, r, err)
		return
	}
	result = "success"
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(response); err != nil {
		s.logger.Error("写出响应失败", "request_id", requestID(r.Context()), "error", err)
	}
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

func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	modelID := ""
	result := "rejected"
	defer func() {
		s.logger.Info(
			"请求完成",
			"request_id", requestID(r.Context()),
			"operation", "count_tokens",
			"model_id", modelID,
			"stream", false,
			"duration_ms", time.Since(startedAt).Milliseconds(),
			"result", result,
		)
	}()

	prepared, ok := s.prepare(w, r)
	if !ok {
		return
	}
	modelID = prepared.ModelID
	count, err := s.runtime.CountTokens(r.Context(), prepared.ModelID, prepared.Body)
	if err != nil {
		result = completionResult(err)
		s.handleUpstreamError(w, r, err)
		return
	}
	result = "success"
	writeJSON(w, http.StatusOK, map[string]int32{"input_tokens": count})
}

func (s *Server) prepare(w http.ResponseWriter, r *http.Request) (anthropic.PreparedRequest, bool) {
	raw, err := readBody(w, r, s.maxBodyBytes)
	if err != nil {
		status := http.StatusBadRequest
		message := "读取请求体失败"
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			status = http.StatusRequestEntityTooLarge
			message = "请求体超过大小限制"
		}
		s.writeError(w, r, status, anthropic.ErrorTypeInvalidRequest, message)
		return anthropic.PreparedRequest{}, false
	}

	prepared, requestErr := anthropic.PrepareRequest(raw, betaFeatures(r.Header.Values("anthropic-beta")), s.resolver)
	if requestErr != nil {
		status := http.StatusBadRequest
		if requestErr.Kind == anthropic.ErrorTypeNotFound {
			status = http.StatusNotFound
		}
		s.writeError(w, r, status, requestErr.Kind, requestErr.Message)
		return anthropic.PreparedRequest{}, false
	}
	return prepared, true
}

func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

func betaFeatures(values []string) []string {
	var result []string
	for _, value := range values {
		for _, feature := range strings.Split(value, ",") {
			if feature = strings.TrimSpace(feature); feature != "" {
				result = append(result, feature)
			}
		}
	}
	return result
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
