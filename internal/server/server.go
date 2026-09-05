// Package server exposes the gateway's HTTP surface.
//
// The route set is dictated by what Claude Code actually calls: it posts
// inference to /v1/messages?beta=true, so patterns match on path alone; it
// probes HEAD /api/hello to warm a connection; and it may query /v1/models for
// model discovery, which must be served directly because any redirect is
// treated as failure.
package server

import (
	"log/slog"
	"net/http"

	"github.com/erickardus/ai-gateway/internal/auth"
	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/router"
)

// Server wires the gateway's dependencies to its HTTP handlers.
type Server struct {
	cfg    *config.Config
	auth   *auth.Authenticator
	store  auth.KeyStore
	router *router.Router
	log    *slog.Logger
}

// New builds a Server.
func New(cfg *config.Config, authn *auth.Authenticator, store auth.KeyStore, rtr *router.Router, log *slog.Logger) *Server {
	return &Server{cfg: cfg, auth: authn, store: store, router: rtr, log: log}
}

// Handler returns the fully wired HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Inference. Go's ServeMux matches on path, so "/v1/messages?beta=true"
	// routes here without any special handling.
	mux.HandleFunc("POST /v1/messages", s.handleMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", s.handleCountTokens)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)

	// Model discovery. Served directly at the configured base URL: Claude Code
	// treats any redirect, even http to https, as a discovery failure.
	mux.HandleFunc("GET /v1/models", s.handleModels)

	// Connection-warming probe.
	mux.HandleFunc("HEAD /api/hello", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/hello", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Health.
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /health/liveliness", s.handleLiveliness)
	mux.HandleFunc("GET /health/liveness", s.handleLiveliness)
	mux.HandleFunc("GET /health/readiness", s.handleReadiness)

	// Key management, master-key only.
	mux.HandleFunc("POST /key/generate", s.handleKeyGenerate)
	mux.HandleFunc("GET /key/info", s.handleKeyInfo)
	mux.HandleFunc("GET /key/list", s.handleKeyList)
	mux.HandleFunc("POST /key/delete", s.handleKeyDelete)

	return s.withMiddleware(mux)
}

// HTTPServer returns a configured http.Server.
//
// It deliberately sets no WriteTimeout: a write deadline applies to the whole
// response, so any value large enough for a long streaming completion is
// useless as a protection, and any useful value would sever streams mid-flight.
// ReadHeaderTimeout and IdleTimeout cover the slow-client cases instead.
func (s *Server) HTTPServer() *http.Server {
	return &http.Server{
		Addr:              s.cfg.Server.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: s.cfg.Server.ReadHeaderTimeout,
		IdleTimeout:       s.cfg.Server.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
}
