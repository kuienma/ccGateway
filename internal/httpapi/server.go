package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/anthropic"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/bedrock"
)

const (
	contentTypeJSON = "application/json"
	errorType       = "error"
	requestIDBytes  = 16
)

type contextKey string

const requestIDContextKey contextKey = "request-id"

type Server struct {
	apiKey       string
	maxBodyBytes int64
	resolver     anthropic.ModelResolver
	runtime      bedrock.Runtime
	logger       *slog.Logger
	handler      http.Handler
}

func NewServer(
	apiKey string,
	maxBodyBytes int64,
	resolver anthropic.ModelResolver,
	runtime bedrock.Runtime,
	logger *slog.Logger,
) *Server {
	server := &Server{
		apiKey:       apiKey,
		maxBodyBytes: maxBodyBytes,
		resolver:     resolver,
		runtime:      runtime,
		logger:       logger,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.handleHealth)
	mux.Handle("POST /v1/messages", server.authenticate(http.HandlerFunc(server.handleMessages)))
	mux.Handle("POST /v1/messages/count_tokens", server.authenticate(http.HandlerFunc(server.handleCountTokens)))
	server.handler = server.withRequestID(mux)
	return server
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := newRequestID()
		w.Header().Set("request-id", requestID)
		ctx := context.WithValue(r.Context(), requestIDContextKey, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newRequestID() string {
	value := make([]byte, requestIDBytes)
	if _, err := rand.Read(value); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(value)
}

func requestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDContextKey).(string)
	return value
}
