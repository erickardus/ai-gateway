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
	"sync"

	"github.com/erickardus/ai-gateway/internal/auth"
	"github.com/erickardus/ai-gateway/internal/cache"
	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/metrics"
	"github.com/erickardus/ai-gateway/internal/router"
	"github.com/erickardus/ai-gateway/internal/rstate"
	"github.com/erickardus/ai-gateway/internal/spend"
)

// Server wires the gateway's dependencies to its HTTP handlers.
type Server struct {
	cfg     *config.Config
	auth    *auth.Authenticator
	store   auth.KeyStore
	router  *router.Router
	log     *slog.Logger
	ledger  spend.Store
	metrics *metrics.Registry
	// shared is set when state is shared across instances, for readiness
	// reporting. It is nil in single-instance deployments.
	shared *rstate.Store
	// cache is nil when response caching is disabled.
	cache cache.Cache
	// pricing maps a deployment ID to its price and whether the operator pays
	// it, resolved once at construction rather than searched per request.
	pricing map[string]deploymentPricing
	// balanced reports whether any model group holds more than one deployment.
	// Prompt-prefix fingerprinting is skipped when none does: with a single
	// upstream per group every request already lands on the same prompt cache,
	// so hashing the prefix of every request would buy nothing.
	balanced bool
	// mispriced records the deployments already warned about for reporting a
	// token counter their cost model does not price. The warning belongs in a
	// log once per deployment, not once per request.
	mispriced sync.Map
}

// deploymentPricing is what one deployment costs the operator.
type deploymentPricing struct {
	price core.Pricing
	// billable is false for a passthrough deployment, where the caller's own
	// subscription is charged rather than the operator's account.
	billable bool
}

// New builds a Server. ledger and reg may be nil, which disables spend
// accounting and metrics respectively.
func New(cfg *config.Config, authn *auth.Authenticator, store auth.KeyStore, rtr *router.Router, log *slog.Logger, ledger spend.Store, reg *metrics.Registry, shared *rstate.Store, responses cache.Cache) *Server {
	pricing := make(map[string]deploymentPricing, len(cfg.ModelList))
	for i := range cfg.ModelList {
		d := &cfg.ModelList[i]
		pricing[d.ID()] = deploymentPricing{
			price:    d.Cost,
			billable: d.Params.AuthMode != core.AuthModePassthrough,
		}
	}
	balanced := false
	for _, group := range cfg.Groups() {
		if len(group) > 1 {
			balanced = true
			break
		}
	}
	return &Server{
		cfg: cfg, auth: authn, store: store, router: rtr, log: log,
		ledger: ledger, metrics: reg, pricing: pricing, shared: shared, cache: responses,
		balanced: balanced,
	}
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

	// Observability.
	if s.cfg.Observability.Metrics {
		mux.HandleFunc("GET /metrics", s.handleMetrics)
	}
	mux.HandleFunc("GET /spend/keys", s.handleSpendKeys)
	mux.HandleFunc("GET /spend/deployments", s.handleSpendDeployments)
	if s.cache != nil {
		mux.HandleFunc("POST /cache/purge", s.handleCachePurge)
	}

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
