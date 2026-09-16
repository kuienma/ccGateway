package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/anthropic"
	"github.com/zhshl/bedrock-anthropic-gateway/internal/bedrock"
)

type Server struct {
	apiKey                  string
	maxBodyBytes            int64
	resolver                anthropic.ModelResolver
	runtime                 bedrock.Runtime
	logger                  *slog.Logger
	handler                 http.Handler
	streamHeartbeatInterval time.Duration
	streamQueueTimeout      time.Duration
	streamSlots             chan struct{}
}

func NewServer(
	apiKey string,
	maxBodyBytes int64,
	resolver anthropic.ModelResolver,
	runtime bedrock.Runtime,
	logger *slog.Logger,
	streamHeartbeatInterval time.Duration,
	maxConcurrentStreams int,
	streamQueueTimeout time.Duration,
) *Server {
	server := &Server{
		apiKey:                  apiKey,
		maxBodyBytes:            maxBodyBytes,
		resolver:                resolver,
		runtime:                 runtime,
		logger:                  logger,
		streamHeartbeatInterval: streamHeartbeatInterval,
		streamQueueTimeout:      streamQueueTimeout,
		streamSlots:             make(chan struct{}, maxConcurrentStreams),
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
