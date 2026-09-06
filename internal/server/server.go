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
	"github.com/erickardus/ai-gateway/internal/reqlog"
	"github.com/erickardus/ai-gateway/internal/router"
	"github.com/erickardus/ai-gateway/internal/rstate"
	"github.com/erickardus/ai-gateway/internal/spend"
	"github.com/erickardus/ai-gateway/internal/sso"
	"github.com/erickardus/ai-gateway/internal/ui"
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
	// uiSessions is nil unless the admin UI is enabled; see UseUI. Its absence
	// is what leaves the /ui routes unregistered, in the same way the SSO
	// provider's does.
	uiSessions *ui.Sessions
	// traffic holds recent completed requests for the UI's traffic view. It is
	// always present and answers Enabled() for itself, so the recording path
	// needs no nil check on the hot side of a request.
	traffic *reqlog.Ring
	// sso and ssoState are nil unless SSO is configured; see UseSSO. Their
	// absence is what leaves the /sso/* routes unregistered.
	sso      *sso.Provider
	ssoState sso.Store
	// pricing maps a deployment ID to its price and whether the operator pays
	// it, resolved once at construction rather than searched per request.
	pricing map[string]deploymentPricing
	// pinnable reports, per model group, whether a prompt-prefix pin has
	// anything to decide. Fingerprinting is skipped for a group where it does
	// not: hashing the prefix of every request would buy nothing.
	pinnable map[string]bool
	// marksOpenAIPrefix reports whether any deployment reads a cache breakpoint
	// while speaking the OpenAI wire format, so a request in that format is not
	// given an annotator for a capability no upstream declares.
	marksOpenAIPrefix bool
	// mispriced records the deployments already warned about for reporting a
	// token counter their cost model does not price. The warning belongs in a
	// log once per deployment, not once per request.
	mispriced sync.Map
	// unmeasured records the deployments already warned about for answering a
	// streamed request with no usage at all. Like mispriced, it is a fact about
	// the deployment rather than the request, so it is worth one log line.
	unmeasured sync.Map
	// annotationLevel records how much of what the gateway would add each
	// deployment has been shown to accept, for the deployments that have refused
	// something. Absent means everything, which is where a deployment starts.
	//
	// It is a level rather than a flag because the annotations are not equally
	// important. A cache breakpoint is an optimization; the streamed usage
	// option is the difference between a bill and a zero. Switching both off on
	// one refusal would answer a lost optimization by losing the accounting too,
	// so a refusal drops the optional part first and the essential part only if
	// that is refused as well.
	annotationLevel sync.Map
	// annotationMu serializes demotion, which happens at most twice per
	// deployment per process. Reads stay lock-free through the sync.Map.
	annotationMu sync.Mutex
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
	marksOpenAIPrefix := false
	for i := range cfg.ModelList {
		d := &cfg.ModelList[i]
		if d.Params.Format == core.FormatOpenAI && d.Params.SupportsCacheControl {
			marksOpenAIPrefix = true
			break
		}
	}
	groups := cfg.Groups()
	pinnable := make(map[string]bool, len(groups))
	for name, group := range groups {
		pinnable[name] = worthPinning(group)
	}
	return &Server{
		cfg: cfg, auth: authn, store: store, router: rtr, log: log,
		ledger: ledger, metrics: reg, pricing: pricing, shared: shared, cache: responses,
		pinnable:          pinnable,
		marksOpenAIPrefix: marksOpenAIPrefix,
		traffic:           newTrafficRing(cfg),
	}
}

// newTrafficRing sizes the recent-request buffer.
//
// A gateway with no UI keeps no records: the buffer exists to be read by the
// traffic page, and retaining request metadata that nothing can display would
// be memory spent on nothing.
func newTrafficRing(cfg *config.Config) *reqlog.Ring {
	if !cfg.UI.Enabled {
		return reqlog.NewRing(0)
	}
	return reqlog.NewRing(cfg.UI.RequestLog())
}

// worthPinning reports whether pinning a prefix inside this group can change
// where a request lands.
//
// It is a question about the group rather than the fleet, which is the whole
// reason it is asked per group: a gateway fronting one balanced group and a
// dozen single-deployment ones would otherwise fingerprint the prompt of every
// request to all thirteen, and twelve of those have one upstream to choose from.
//
// A group of two or more is enough, because deployments within one are
// guaranteed to be distinct upstream targets. A prompt cache belongs to the
// workspace behind the credential and is scoped to one model at one endpoint, so
// two deployments could only share one by naming the same endpoint and model —
// which is refused at load as a duplicate. What remains is two spellings of one
// endpoint, which nothing here can detect and which costs some balance rather
// than any cache hits.
func worthPinning(group []*config.Deployment) bool {
	return len(group) > 1
}

// anyPinnable reports whether affinity is doing anything anywhere, which is what
// /health distinguishes from affinity merely being configured.
func (s *Server) anyPinnable() bool {
	for _, ok := range s.pinnable {
		if ok {
			return true
		}
	}
	return false
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
	// Registered unconditionally, like the other two. A gateway with no
	// hierarchy answers with an empty list, which is a truthful answer and one
	// fewer thing for a client to branch on.
	mux.HandleFunc("GET /spend/scopes", s.handleSpendScopes)
	if s.cache != nil {
		mux.HandleFunc("POST /cache/purge", s.handleCachePurge)
	}

	// Health.
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /health/liveliness", s.handleLiveliness)
	mux.HandleFunc("GET /health/liveness", s.handleLiveliness)
	mux.HandleFunc("GET /health/readiness", s.handleReadiness)

	// SSO, when an identity provider is configured. Along with the admin UI's
	// sign-in these are the only routes a browser reaches, and the only ones no
	// gateway credential guards: the
	// whole point is to issue the credential a caller does not yet have. What
	// stands in for one is the provider's own authentication, the single-use
	// state and code, and the client's PKCE verifier.
	if s.sso != nil {
		mux.HandleFunc("GET /sso/login", s.handleSSOLogin)
		mux.HandleFunc("GET /sso/callback", s.handleSSOCallback)
		mux.HandleFunc("POST /sso/exchange", s.handleSSOExchange)
		mux.HandleFunc("POST /sso/renew", s.handleSSORenew)
	}

	// Key management, master-key only.
	mux.HandleFunc("POST /key/generate", s.handleKeyGenerate)
	mux.HandleFunc("GET /key/info", s.handleKeyInfo)
	mux.HandleFunc("GET /key/list", s.handleKeyList)
	mux.HandleFunc("POST /key/update", s.handleKeyUpdate)
	mux.HandleFunc("POST /key/delete", s.handleKeyDelete)

	// The admin UI, when enabled. It is registered last because it is the only
	// thing here that is not part of the gateway's API surface.
	s.registerUI(mux)

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
