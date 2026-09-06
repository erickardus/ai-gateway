package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erickardus/ai-gateway/internal/auth"
	"github.com/erickardus/ai-gateway/internal/cache"
	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/metrics"
	"github.com/erickardus/ai-gateway/internal/provider"
	"github.com/erickardus/ai-gateway/internal/router"
	"github.com/erickardus/ai-gateway/internal/spend"
	"github.com/erickardus/ai-gateway/internal/testutil"
)

const testVirtualKey = "sk-vk-TESTKEY"

// soloModel is the group harnessOpts.soloGroup registers: one deployment, in a
// fleet that also holds a balanced group.
const soloModel = "solo-model"

// captured records what a fake upstream received.
type captured struct {
	mu     sync.Mutex
	header http.Header
	body   []byte
	path   string
	query  string
}

func (c *captured) set(r *http.Request, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.header = r.Header.Clone()
	c.body = body
	c.path = r.URL.Path
	c.query = r.URL.RawQuery
}

func (c *captured) get() (http.Header, []byte, string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.header, c.body, c.path, c.query
}

// record wraps an upstream handler so the harness can inspect what it received,
// leaving the body readable by the handler itself.
func record(seen *captured, inner http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen.set(r, body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		inner(w, r)
	}
}

// harness builds a gateway in front of a fake upstream.
type harness struct {
	srv           *Server
	store         auth.KeyStore
	deploymentID  string
	deploymentIDs []string
	ledger        *spend.Ledger
	metrics       *metrics.Registry
	gateway       http.Handler
	upstream      *httptest.Server
	seen          *captured
	logBuf        *testutil.SyncWriter
}

type harnessOpts struct {
	authMode         string
	allowPassthrough bool
	upstream         http.HandlerFunc
	masterKey        string
	maxBodyBytes     int64
	rpmLimit         int
	maxBudget        float64
	cache            bool
	cacheScope       cache.Scope
	// extraDeployments adds further upstreams to the same model group, which is
	// what gives prompt-prefix affinity something to choose between.
	extraDeployments int
	// soloGroup registers a second model group holding a single deployment, so a
	// test can ask what a fleet does for a group with nothing to choose between
	// while another group is balanced.
	soloGroup   bool
	promptCache config.PromptCacheConfig
	// format selects the wire protocol of the deployments, defaulting to
	// anthropic. An openai group is reachable at /v1/chat/completions and is
	// named separately, since a group's deployments must share one format.
	format core.Format
	// pricing overrides the cost model applied to every api_key deployment.
	pricing *core.Pricing
	// streamUsage overrides observability.stream_usage, which decides whether a
	// streamed OpenAI-compatible request is asked to report any usage at all.
	streamUsage *bool
	// supportsCacheControl declares that the upstream reads a cache breakpoint
	// while speaking the OpenAI wire format, which is what an operator says
	// about a Qwen deployment.
	supportsCacheControl bool
	// extraAuthMode gives the extra deployments a different auth mode from the
	// first. It is what builds a mixed fleet — subscription traffic beside API
	// traffic in one group — which is the arrangement a per-deployment body
	// transform exists to serve.
	extraAuthMode core.AuthMode
	// upstreams, when set, gives each deployment its own handler — the first
	// entry serves the primary deployment and the rest serve the extras. It is
	// what lets a test model several independent providers, each holding its
	// own prompt cache, rather than one fake shared by every deployment.
	upstreams []http.HandlerFunc
}

// modelGroup is the group name the harness registers for a format. The two are
// separate because a group's deployments must all speak one protocol.
func (o harnessOpts) modelGroup() string {
	if o.format == core.FormatOpenAI {
		return "openai-gpt"
	}
	return "anthropic-claude"
}

// defaultPricing is a realistic cost model for the harness's format, so a test
// asserting on dollars is asserting against the shape of a real bill.
func (o harnessOpts) defaultPricing() core.Pricing {
	if o.pricing != nil {
		return *o.pricing
	}
	if o.format == core.FormatOpenAI {
		// Most OpenAI-compatible providers cache automatically and charge
		// nothing to write, so the default names no write price. The ones that
		// do charge — Qwen, MiniMax — are covered by tests passing their own.
		return core.Pricing{InputPer1M: 1.25, OutputPer1M: 10, CacheReadPer1M: 0.125}
	}
	return core.Pricing{InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3, CacheWritePer1M: 3.75}
}

func intPtr(n int) *int { return &n }

func newHarness(t *testing.T, opts harnessOpts) *harness {
	t.Helper()
	seen := &captured{}

	handler := opts.upstream
	if len(opts.upstreams) > 0 {
		handler = opts.upstreams[0]
	}
	if handler == nil {
		handler = func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			seen.set(r, body)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"msg_1","type":"message","usage":{"input_tokens":5,"output_tokens":9}}`)
		}
	} else {
		handler = record(seen, handler)
	}
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)

	mode := opts.authMode
	if mode == "" {
		mode = "passthrough"
	}
	format := opts.format
	if format == "" {
		format = core.FormatAnthropic
	}
	params := config.DeploymentParams{
		Format:               format,
		APIBase:              upstream.URL,
		Model:                opts.modelGroup(),
		AuthMode:             core.AuthMode(mode),
		SupportsCacheControl: opts.supportsCacheControl,
	}
	if mode == "api_key" {
		params.AuthHeader = "x-api-key"
		params.APIKey = "sk-ant-api03-SERVERSIDE"
	}

	deployment := config.Deployment{ModelName: opts.modelGroup(), Params: params, Weight: intPtr(1)}
	deployments := []config.Deployment{deployment}
	for i := 0; i < opts.extraDeployments; i++ {
		extraHandler := handler
		if i+1 < len(opts.upstreams) {
			extraHandler = record(seen, opts.upstreams[i+1])
		}
		extra := httptest.NewServer(extraHandler)
		t.Cleanup(extra.Close)
		extraParams := params
		extraParams.APIBase = extra.URL
		if opts.extraAuthMode != "" {
			extraParams.AuthMode = opts.extraAuthMode
			if opts.extraAuthMode == core.AuthModePassthrough {
				// A passthrough deployment relays the caller's own credential,
				// so it must carry none of its own.
				extraParams.AuthHeader, extraParams.AuthScheme, extraParams.APIKey = "", "", ""
			}
		}
		deployments = append(deployments, config.Deployment{
			ModelName: opts.modelGroup(), Params: extraParams, Weight: intPtr(1),
		})
	}
	if opts.soloGroup {
		deployments = append(deployments, config.Deployment{
			ModelName: soloModel, Params: params, Weight: intPtr(1),
		})
	}
	// Priced so cost accounting has something to compute. Asked per deployment
	// rather than per fleet: a passthrough deployment bills the caller's own
	// subscription, so a cost model on one is refused at load.
	for i := range deployments {
		if deployments[i].Params.AuthMode == core.AuthModeAPIKey {
			deployments[i].Cost = opts.defaultPricing()
		}
	}
	cfg := &config.Config{
		ModelList:   deployments,
		PromptCache: opts.promptCache,
		Router:      config.RouterConfig{Strategy: config.StrategyWeightedShuffle},
		VirtualKeys: config.VirtualKeysConfig{
			MasterKey:            opts.masterKey,
			HeaderNames:          []string{"x-gateway-key", "x-litellm-api-key"},
			AllowedUpstreamHosts: []string{config.HostOf(upstream.URL)},
			Keys: []config.KeySpec{{
				Key: testVirtualKey, Alias: "test-key",
				AllowPassthrough: opts.allowPassthrough, RPMLimit: opts.rpmLimit,
			}},
		},
	}
	cfg.Observability.Metrics = true
	cfg.Observability.StreamUsage = opts.streamUsage
	if opts.maxBudget > 0 {
		cfg.VirtualKeys.Keys[0].MaxBudget = opts.maxBudget
	}
	if opts.maxBodyBytes > 0 {
		cfg.Server.MaxBodyBytes = opts.maxBodyBytes
	}
	if err := config.Finalize(cfg); err != nil {
		t.Fatalf("config.Finalize: %v", err)
	}
	if opts.maxBodyBytes > 0 {
		cfg.Server.MaxBodyBytes = opts.maxBodyBytes
	}

	logBuf := testutil.NewSyncWriter()
	log := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	store := auth.NewMemStore()
	authn, err := auth.NewAuthenticator(context.Background(), store, cfg.VirtualKeys)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	client := provider.NewClient(cfg.VirtualKeys.HeaderNames, cfg.VirtualKeys.AllowedUpstreamHosts)
	rtr, err := router.New(cfg, router.NewMemState(), client, log, router.Options{Seed: 1})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	ledger := spend.New()
	reg := metrics.New()
	var responses cache.Cache
	if opts.cache {
		cfg.Cache.Enabled = true
		cfg.Cache.Scope = opts.cacheScope
		if cfg.Cache.Scope == "" {
			cfg.Cache.Scope = cache.ScopeKey
		}
		cfg.Cache.TTL = time.Minute
		cfg.Cache.MaxEntries = 100
		cfg.Cache.MaxEntryBytes = 1 << 20
		responses = cache.NewMemory(100)
	}

	srv := New(cfg, authn, store, rtr, log, ledger, reg, nil, responses)
	ids := make([]string, 0, len(cfg.ModelList))
	for i := range cfg.ModelList {
		ids = append(ids, cfg.ModelList[i].ID())
	}
	return &harness{
		srv:           srv,
		store:         store,
		deploymentID:  cfg.ModelList[0].ID(),
		deploymentIDs: ids,
		ledger:        ledger,
		metrics:       reg,
		gateway:       srv.Handler(),
		upstream:      upstream,
		seen:          seen,
		logBuf:        logBuf,
	}
}

// addKey issues a second virtual key, for tests that need two callers.
func (h *harness) addKey(t *testing.T, plaintext, alias string) {
	t.Helper()
	if err := h.store.Put(context.Background(), &core.Key{
		Hash: auth.HashKey(plaintext), Alias: alias, AllowPassthrough: true,
	}); err != nil {
		t.Fatalf("add key: %v", err)
	}
}

// setLedger rebuilds the handler around a different spend store.
func (h *harness) setLedger(t *testing.T, ledger spend.Store) {
	t.Helper()
	h.srv.ledger = ledger
	h.gateway = h.srv.Handler()
}

func (h *harness) do(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.gateway.ServeHTTP(rec, req)
	return rec
}

// claudeCodeRequest builds the request Claude Code sends on a subscription
// login: its own OAuth token in Authorization, the virtual key alongside it in
// a custom header.
func claudeCodeRequest(target, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-SUBSCRIPTION")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20,claude-code-20250219")
	req.Header.Set("user-agent", "claude-cli/2.1.230")
	req.Header.Set("x-app", "cli")
	req.Header.Set("x-gateway-key", testVirtualKey)
	req.Header.Set("content-type", "application/json")
	return req
}

// TestClaudeCodeSubscriptionPassthrough is the acceptance test for the gateway's
// primary purpose: Claude Code keeps using its claude.ai subscription while its
// traffic flows through the gateway.
func TestClaudeCodeSubscriptionPassthrough(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "passthrough", allowPassthrough: true})

	body := `{"model":"anthropic-claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	rec := h.do(t, claudeCodeRequest("/v1/messages?beta=true", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	got, gotBody, path, query := h.seen.get()

	// The subscription credential must arrive at the upstream untouched.
	if v := got.Get("Authorization"); v != "Bearer sk-ant-oat01-SUBSCRIPTION" {
		t.Errorf("upstream Authorization = %q, want the caller's OAuth token verbatim", v)
	}
	// Stripping the OAuth capability from anthropic-beta 401s the request.
	if v := got.Get("anthropic-beta"); v != "oauth-2025-04-20,claude-code-20250219" {
		t.Errorf("upstream anthropic-beta = %q, want it forwarded verbatim", v)
	}
	if v := got.Get("anthropic-version"); v != "2023-06-01" {
		t.Errorf("upstream anthropic-version = %q", v)
	}
	// The gateway's own credential must never reach the provider.
	if v := got.Get("x-gateway-key"); v != "" {
		t.Errorf("the virtual key leaked upstream: %q", v)
	}
	// The body is forwarded byte for byte when the upstream model id matches.
	if string(gotBody) != body {
		t.Errorf("body was modified.\n got: %s\nwant: %s", gotBody, body)
	}
	if path != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", path)
	}
	if query != "beta=true" {
		t.Errorf("upstream query = %q, want beta=true preserved", query)
	}

	if rec.Header().Get("x-gateway-deployment") == "" {
		t.Error("x-gateway-deployment response header missing")
	}

	// No credential material may appear in the logs.
	logs := h.logBuf.String()
	for _, secret := range []string{"sk-ant-oat01-SUBSCRIPTION", testVirtualKey} {
		if strings.Contains(logs, secret) {
			t.Errorf("a credential leaked into the logs: %q", secret)
		}
	}
}

// A key without allow_passthrough must not reach a passthrough deployment.
func TestPassthroughRequiresKeyPermission(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "passthrough", allowPassthrough: false})
	rec := h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
}

// In api_key mode the caller's credential is replaced, never forwarded.
func TestAPIKeyModeDoesNotLeakCallerCredential(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "api_key", allowPassthrough: true})
	rec := h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got, _, _, _ := h.seen.get()
	if v := got.Get("x-api-key"); v != "sk-ant-api03-SERVERSIDE" {
		t.Errorf("x-api-key = %q, want the deployment's own credential", v)
	}
	if v := got.Get("Authorization"); v != "" {
		t.Errorf("the caller's subscription token leaked upstream: %q", v)
	}
}

func TestUnauthenticatedRequestRejected(t *testing.T) {
	h := newHarness(t, harnessOpts{allowPassthrough: true})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"anthropic-claude"}`))
	// Only a subscription token, which is never a gateway credential.
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-SUBSCRIPTION")
	if rec := h.do(t, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestUnknownModelRejected(t *testing.T) {
	h := newHarness(t, harnessOpts{allowPassthrough: true})
	rec := h.do(t, claudeCodeRequest("/v1/messages", `{"model":"no-such-model","messages":[]}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

// Upstream errors are relayed verbatim: Claude Code matches on the upstream's
// own wording to decide whether to retry with a capability disabled.
func TestUpstreamErrorRelayedVerbatim(t *testing.T) {
	upstreamBody := `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 250000 tokens > 200000 maximum"}}`
	h := newHarness(t, harnessOpts{
		allowPassthrough: true,
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, upstreamBody)
		},
	})
	rec := h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if rec.Body.String() != upstreamBody {
		t.Errorf("error body was rewritten.\n got: %s\nwant: %s", rec.Body.String(), upstreamBody)
	}
}

func TestStreamingRelay(t *testing.T) {
	h := newHarness(t, harnessOpts{
		allowPassthrough: true,
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			rc := http.NewResponseController(w)
			io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
			rc.Flush()
			io.WriteString(w, ": keep-alive comment\n\n")
			io.WriteString(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
			rc.Flush()
			io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":4,\"output_tokens\":11}}\n\n")
			rc.Flush()
		},
	})

	rec := h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","stream":true,"messages":[]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	out := rec.Body.String()
	for _, want := range []string{"message_start", ": keep-alive comment", "event: ping", "message_delta"} {
		if !strings.Contains(out, want) {
			t.Errorf("relayed stream is missing %q:\n%s", want, out)
		}
	}
}

func TestHelloProbe(t *testing.T) {
	h := newHarness(t, harnessOpts{allowPassthrough: true})
	if rec := h.do(t, httptest.NewRequest(http.MethodHead, "/api/hello", nil)); rec.Code != http.StatusOK {
		t.Errorf("HEAD /api/hello = %d, want 200", rec.Code)
	}
}

func TestLivelinessNeedsNoCredential(t *testing.T) {
	h := newHarness(t, harnessOpts{allowPassthrough: true})
	for _, path := range []string{"/health/liveliness", "/health/liveness"} {
		if rec := h.do(t, httptest.NewRequest(http.MethodGet, path, nil)); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}
}

// Claude Code treats any redirect on /v1/models as a discovery failure.
func TestModelsServedDirectlyWithoutRedirect(t *testing.T) {
	h := newHarness(t, harnessOpts{allowPassthrough: true})
	req := httptest.NewRequest(http.MethodGet, "/v1/models?limit=1000", nil)
	req.Header.Set("x-gateway-key", testVirtualKey)
	rec := h.do(t, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("/v1/models redirected to %q; Claude Code treats that as discovery failure", loc)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Data) != 1 || payload.Data[0].ID != "anthropic-claude" {
		t.Errorf("unexpected model list: %s", rec.Body.String())
	}
}

func TestKeyEndpointsRequireMasterKey(t *testing.T) {
	h := newHarness(t, harnessOpts{allowPassthrough: true, masterKey: "sk-master-SECRET"})

	// A valid virtual key is not sufficient for management endpoints.
	req := httptest.NewRequest(http.MethodPost, "/key/generate", strings.NewReader(`{}`))
	req.Header.Set("x-gateway-key", testVirtualKey)
	if rec := h.do(t, req); rec.Code != http.StatusUnauthorized {
		t.Errorf("virtual key on /key/generate = %d, want 401", rec.Code)
	}

	// The master key mints a key, returned exactly once.
	req = httptest.NewRequest(http.MethodPost, "/key/generate", strings.NewReader(`{"alias":"ci","models":["claude-*"]}`))
	req.Header.Set("x-gateway-key", "sk-master-SECRET")
	rec := h.do(t, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("master key on /key/generate = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Key  string `json:"key"`
		Info struct {
			Hash  string `json:"hash"`
			Alias string `json:"alias"`
		} `json:"info"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(created.Key, "sk-vk-") {
		t.Errorf("issued key = %q, want an sk-vk- prefix", created.Key)
	}
	if created.Info.Hash == "" || created.Info.Alias != "ci" {
		t.Errorf("unexpected key info: %+v", created.Info)
	}
	if strings.Contains(rec.Body.String(), created.Info.Hash) && created.Info.Hash == created.Key {
		t.Error("the stored hash must not equal the plaintext key")
	}
}

// A panic after the response is committed must not append an error envelope
// into the body the client is already parsing.
func TestPanicAfterCommitDoesNotCorruptResponse(t *testing.T) {
	h := newHarness(t, harnessOpts{allowPassthrough: true})
	srv := h.gateway

	// Drive the middleware chain directly with a handler that commits then panics.
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		panic("boom after commit")
	})
	_ = srv

	s := &Server{cfg: &config.Config{}, log: slog.New(slog.DiscardHandler)}
	rec := httptest.NewRecorder()
	s.withMiddleware(inner).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))

	body := rec.Body.String()
	if strings.Contains(body, "internal_error") {
		t.Errorf("an error envelope was appended to a committed response:\n%s", body)
	}
	if !strings.Contains(body, "message_start") {
		t.Errorf("the already-written bytes were lost:\n%s", body)
	}
}

// A panic before anything is written must still produce a clean 500.
func TestPanicBeforeCommitReturns500(t *testing.T) {
	s := &Server{cfg: &config.Config{}, log: slog.New(slog.DiscardHandler)}
	inner := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom early") })

	rec := httptest.NewRecorder()
	s.withMiddleware(inner).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "internal_error") {
		t.Errorf("body = %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "boom early") {
		t.Error("the panic value leaked to the client")
	}
}

// disable_fallbacks is a gateway directive; forwarding it would make an upstream
// that rejects unknown fields 400 every request that uses the feature.
func TestDisableFallbacksStrippedFromUpstreamBody(t *testing.T) {
	h := newHarness(t, harnessOpts{allowPassthrough: true})
	body := `{"model":"anthropic-claude","disable_fallbacks":true,"max_tokens":10}`
	rec := h.do(t, claudeCodeRequest("/v1/messages", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	_, gotBody, _, _ := h.seen.get()
	if strings.Contains(string(gotBody), "disable_fallbacks") {
		t.Errorf("the gateway directive was forwarded upstream: %s", gotBody)
	}
	if !strings.Contains(string(gotBody), `"max_tokens":10`) {
		t.Errorf("sibling fields were lost: %s", gotBody)
	}
}

// /health discloses the upstream topology, so it must require a key.
func TestHealthRequiresAuthentication(t *testing.T) {
	h := newHarness(t, harnessOpts{allowPassthrough: true})

	rec := h.do(t, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous GET /health = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "api_base") {
		t.Error("the upstream topology leaked to an unauthenticated caller")
	}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("x-gateway-key", testVirtualKey)
	if rec := h.do(t, req); rec.Code != http.StatusOK {
		t.Errorf("authenticated GET /health = %d, want 200", rec.Code)
	}
}

// The body-size limit is the only guard against a memory-exhaustion request, so
// it needs a test that actually exceeds it.
func TestOversizedBodyIsRejected(t *testing.T) {
	h := newHarness(t, harnessOpts{allowPassthrough: true, maxBodyBytes: 512})
	big := strings.Repeat("x", 4096)
	req := claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","pad":"`+big+`"}`)

	rec := h.do(t, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body = %s", rec.Code, rec.Body.String())
	}
	if n := len(h.seen.body); n != 0 {
		t.Error("an oversized body was forwarded upstream")
	}
}

// Rate limit must be charged only to requests the gateway will actually
// dispatch, or a key is billed for its own rejected requests.
func TestRateLimitNotChargedForRejectedRequests(t *testing.T) {
	h := newHarness(t, harnessOpts{allowPassthrough: true, rpmLimit: 3})

	// Requests for an unknown model must not consume the key's budget.
	for range 10 {
		rec := h.do(t, claudeCodeRequest("/v1/messages", `{"model":"no-such-model","messages":[]}`))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	}
	// Model discovery must not consume it either.
	for range 5 {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("x-gateway-key", testVirtualKey)
		if rec := h.do(t, req); rec.Code != http.StatusOK {
			t.Fatalf("/v1/models = %d", rec.Code)
		}
	}

	// The full budget must still be available for real requests.
	for i := range 3 {
		rec := h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("dispatched request %d = %d, want 200; budget was consumed by rejected requests", i+1, rec.Code)
		}
	}
	// And the limit still applies.
	if rec := h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`)); rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 once the budget is spent", rec.Code)
	}
}

// The access log must identify the calling key without the credential itself
// ever appearing in a log line.
func TestAccessLogIdentifiesKeyWithoutLeakingIt(t *testing.T) {
	h := newHarness(t, harnessOpts{allowPassthrough: true})
	h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))

	logs := h.logBuf.String()
	if !strings.Contains(logs, "test-key") {
		t.Errorf("the access log does not identify the calling key:\n%s", logs)
	}
	for _, secret := range []string{testVirtualKey, "sk-ant-oat01-SUBSCRIPTION"} {
		if strings.Contains(logs, secret) {
			t.Errorf("a credential leaked into the logs: %q", secret)
		}
	}
}
