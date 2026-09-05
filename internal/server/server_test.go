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

	"github.com/erickardus/ai-gateway/internal/auth"
	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/provider"
	"github.com/erickardus/ai-gateway/internal/router"
	"github.com/erickardus/ai-gateway/internal/testutil"
)

const testVirtualKey = "sk-vk-TESTKEY"

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

// harness builds a gateway in front of a fake upstream.
type harness struct {
	gateway  http.Handler
	upstream *httptest.Server
	seen     *captured
	logBuf   *testutil.SyncWriter
}

type harnessOpts struct {
	authMode         string
	allowPassthrough bool
	upstream         http.HandlerFunc
	masterKey        string
	maxBodyBytes     int64
	rpmLimit         int
}

func intPtr(n int) *int { return &n }

func newHarness(t *testing.T, opts harnessOpts) *harness {
	t.Helper()
	seen := &captured{}

	handler := opts.upstream
	if handler == nil {
		handler = func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			seen.set(r, body)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"msg_1","type":"message","usage":{"input_tokens":5,"output_tokens":9}}`)
		}
	} else {
		inner := handler
		handler = func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			seen.set(r, body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			inner(w, r)
		}
	}
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)

	mode := opts.authMode
	if mode == "" {
		mode = "passthrough"
	}
	params := config.DeploymentParams{
		Format:   core.FormatAnthropic,
		APIBase:  upstream.URL,
		Model:    "anthropic-claude",
		AuthMode: core.AuthMode(mode),
	}
	if mode == "api_key" {
		params.AuthHeader = "x-api-key"
		params.APIKey = "sk-ant-api03-SERVERSIDE"
	}

	cfg := &config.Config{
		ModelList: []config.Deployment{{ModelName: "anthropic-claude", Params: params, Weight: intPtr(1)}},
		Router:    config.RouterConfig{Strategy: config.StrategyWeightedShuffle},
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

	return &harness{
		gateway:  New(cfg, authn, store, rtr, log).Handler(),
		upstream: upstream,
		seen:     seen,
		logBuf:   logBuf,
	}
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

	s := &Server{cfg: &config.Config{}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
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
	s := &Server{cfg: &config.Config{}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
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
