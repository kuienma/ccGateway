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

// ServerConfig 仅包含 HTTP 层使用的基础配置，运行依赖单独注入。
type ServerConfig struct {
	APIKey                  string
	MaxBodyBytes            int64
	StreamHeartbeatInterval time.Duration
	MaxConcurrentStreams    int
	StreamQueueTimeout      time.Duration
}

func NewServer(cfg ServerConfig, resolver anthropic.ModelResolver, runtime bedrock.Runtime, logger *slog.Logger) *Server {
	server := &Server{
		apiKey:                  cfg.APIKey,
		maxBodyBytes:            cfg.MaxBodyBytes,
		resolver:                resolver,
		runtime:                 runtime,
		logger:                  logger,
		streamHeartbeatInterval: cfg.StreamHeartbeatInterval,
		streamQueueTimeout:      cfg.StreamQueueTimeout,
		streamSlots:             make(chan struct{}, cfg.MaxConcurrentStreams),
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
