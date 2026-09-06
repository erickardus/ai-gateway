package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/ui"
)

const testMasterKey = "sk-master-UITEST"

func newUIHarness(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, harnessOpts{
		authMode:         "api_key",
		masterKey:        testMasterKey,
		allowPassthrough: true,
		ui:               true,
	})
}

// signIn exchanges the master key for a session cookie.
func (h *harness) signIn(t *testing.T) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/ui/api/session",
		strings.NewReader(`{"master_key":"`+testMasterKey+`"}`))
	rec := h.do(t, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("sign in: status %d, body %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == ui.CookieName {
			return c
		}
	}
	t.Fatal("sign in returned no session cookie")
	return nil
}

// uiGet issues an authenticated GET against the UI's API.
func (h *harness) uiGet(t *testing.T, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(cookie)
	return h.do(t, req)
}

func (h *harness) uiPost(t *testing.T, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.AddCookie(cookie)
	req.Header.Set(ui.CSRFHeader, "1")
	return h.do(t, req)
}

// The cookie must be scoped to /ui so the browser never attaches it to an
// inference request. This is the structural half of keeping the console's
// credential out of the inference plane — the other half is that
// ExtractGatewayKey reads headers only.
func TestUISessionCookieIsScopedToTheConsole(t *testing.T) {
	h := newUIHarness(t)
	cookie := h.signIn(t)

	if cookie.Path != "/ui" {
		t.Fatalf("session cookie Path = %q, want /ui", cookie.Path)
	}
	if !cookie.HttpOnly {
		t.Fatal("session cookie is readable by JavaScript")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie SameSite = %v, want Strict", cookie.SameSite)
	}
}

// The session must not authenticate anything outside the console — not the
// inference path, and not the master-key management endpoints it mirrors.
func TestUISessionDoesNotAuthenticateTheGatewayAPI(t *testing.T) {
	h := newUIHarness(t)
	cookie := h.signIn(t)

	for _, path := range []string{"/key/list", "/spend/keys", "/health"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		if rec := h.do(t, req); rec.Code == http.StatusOK {
			t.Fatalf("%s accepted a UI session cookie as authentication", path)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"anthropic-claude","messages":[]}`))
	req.AddCookie(cookie)
	if rec := h.do(t, req); rec.Code == http.StatusOK {
		t.Fatal("/v1/messages accepted a UI session cookie as authentication")
	}
}

func TestUIRejectsWrongMasterKey(t *testing.T) {
	h := newUIHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/ui/api/session",
		strings.NewReader(`{"master_key":"sk-master-WRONG"}`))
	rec := h.do(t, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("a refused sign-in still set a cookie")
	}
}

func TestUIRequiresASession(t *testing.T) {
	h := newUIHarness(t)
	for _, path := range []string{"/ui/api/overview", "/ui/api/keys", "/ui/api/traffic", "/ui/api/spend/keys"} {
		rec := h.do(t, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without a session: status %d, want 401", path, rec.Code)
		}
	}
}

// SameSite is a browser policy; the header is a property of the request. A
// mutation must carry both.
func TestUIMutationRequiresTheCSRFHeader(t *testing.T) {
	h := newUIHarness(t)
	cookie := h.signIn(t)

	req := httptest.NewRequest(http.MethodPost, "/ui/api/keys/generate", strings.NewReader(`{"alias":"x"}`))
	req.AddCookie(cookie)
	if rec := h.do(t, req); rec.Code != http.StatusForbidden {
		t.Fatalf("a mutation without %s: status %d, want 403", ui.CSRFHeader, rec.Code)
	}

	if rec := h.uiPost(t, "/ui/api/keys/generate", `{"alias":"x"}`, cookie); rec.Code != http.StatusCreated {
		t.Fatalf("a mutation with the header: status %d, body %s", rec.Code, rec.Body.String())
	}
}

// A gateway with no UI configured must serve no browser surface at all — not
// even a sign-in page to guess a master key against.
func TestUIRoutesAreUnregisteredWhenDisabled(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "api_key", masterKey: testMasterKey})
	for _, path := range []string{"/ui/", "/ui/api/overview"} {
		rec := h.do(t, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s with the UI disabled: status %d, want 404", path, rec.Code)
		}
	}
	rec := h.do(t, httptest.NewRequest(http.MethodPost, "/ui/api/session",
		strings.NewReader(`{"master_key":"`+testMasterKey+`"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("sign-in with the UI disabled: status %d, want 404", rec.Code)
	}
}

func TestUIOverviewComposesOneAnswer(t *testing.T) {
	h := newUIHarness(t)
	cookie := h.signIn(t)

	rec := h.uiGet(t, "/ui/api/overview", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Status             string `json:"status"`
		Strategy           string `json:"strategy"`
		HealthyDeployments int    `json:"healthy_deployments"`
		Deployments        []struct {
			Deployment string `json:"deployment"`
			AuthMode   string `json:"auth_mode"`
		} `json:"deployments"`
		Keys     map[string]int `json:"keys"`
		Features map[string]bool
		Traffic  struct {
			Capacity int `json:"capacity"`
		} `json:"traffic"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "healthy" || body.HealthyDeployments != 1 {
		t.Fatalf("overview reported %s with %d healthy", body.Status, body.HealthyDeployments)
	}
	if len(body.Deployments) != 1 || body.Deployments[0].AuthMode != "api_key" {
		t.Fatalf("deployments = %+v", body.Deployments)
	}
	if body.Keys["total"] != 1 {
		t.Fatalf("keys total = %d, want 1", body.Keys["total"])
	}
	if body.Traffic.Capacity == 0 {
		t.Fatal("traffic buffer reports no capacity with the UI enabled")
	}
}

// A completed request must appear in the traffic view with the routing decision
// attached, since that decision is invisible once the response is sent.
func TestUITrafficRecordsCompletedRequests(t *testing.T) {
	h := newUIHarness(t)
	cookie := h.signIn(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"anthropic-claude","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-gateway-key", testVirtualKey)
	if rec := h.do(t, req); rec.Code != http.StatusOK {
		t.Fatalf("inference: status %d, body %s", rec.Code, rec.Body.String())
	}

	rec := h.uiGet(t, "/ui/api/traffic", cookie)
	var body struct {
		Records []struct {
			ModelGroup string `json:"model_group"`
			Deployment string `json:"deployment"`
			Outcome    string `json:"outcome"`
			KeyAlias   string `json:"key_alias"`
			Billable   bool   `json:"billable"`
			Usage      struct {
				InputTokens int `json:"input_tokens"`
			} `json:"usage"`
		} `json:"records"`
		Held int `json:"held"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Held != 1 || len(body.Records) != 1 {
		t.Fatalf("traffic held %d records, want 1: %s", body.Held, rec.Body.String())
	}
	got := body.Records[0]
	if got.ModelGroup != "anthropic-claude" || got.Outcome != "success" || got.KeyAlias != "test-key" {
		t.Fatalf("record = %+v", got)
	}
	if !got.Billable || got.Usage.InputTokens != 5 {
		t.Fatalf("record did not carry accounting: %+v", got)
	}
}

// The traffic buffer must stay empty when the UI is off: it exists to be read
// by the console, and retaining metadata nothing can display is memory spent
// for nothing.
func TestTrafficIsNotRecordedWithoutTheUI(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "api_key", masterKey: testMasterKey})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"anthropic-claude","messages":[]}`))
	req.Header.Set("x-gateway-key", testVirtualKey)
	h.do(t, req)

	if h.srv.traffic.Len() != 0 {
		t.Fatalf("traffic ring holds %d records with the UI disabled", h.srv.traffic.Len())
	}
}

// The keys view joins the key store to the ledger, because a key's budget and
// the spend it is measured against live in different places and are addressed
// by different subjects.
func TestUIKeysJoinsSpendToBudget(t *testing.T) {
	h := newUIHarness(t)
	cookie := h.signIn(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"anthropic-claude","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-gateway-key", testVirtualKey)
	h.do(t, req)

	rec := h.uiGet(t, "/ui/api/keys", cookie)
	var body struct {
		Keys []struct {
			Alias        string  `json:"alias"`
			SpendSubject string  `json:"spend_subject"`
			Spend        float64 `json:"spend"`
		} `json:"keys"`
		Groups []string `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Keys) != 1 {
		t.Fatalf("got %d keys, want 1", len(body.Keys))
	}
	if body.Keys[0].Spend <= 0 {
		t.Fatalf("key spend = %v, want the request's cost", body.Keys[0].Spend)
	}
	if body.Keys[0].SpendSubject == "" {
		t.Fatal("no spend subject reported, so the UI cannot group a person's devices")
	}
	if len(body.Groups) == 0 {
		t.Fatal("no model groups offered to the key form")
	}
}

// A refusal is the record an operator most needs — a budget exhausted, a
// blocked key, a model that is not in the group they thought — and the metrics
// counter that tallies rejections cannot say whose.
func TestUITrafficRecordsRejections(t *testing.T) {
	h := newUIHarness(t)
	cookie := h.signIn(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"no-such-group","messages":[]}`))
	req.Header.Set("x-gateway-key", testVirtualKey)
	if rec := h.do(t, req); rec.Code == http.StatusOK {
		t.Fatal("an unknown model group was accepted")
	}

	rec := h.uiGet(t, "/ui/api/traffic", cookie)
	var body struct {
		Records []struct {
			Outcome      string `json:"outcome"`
			RejectReason string `json:"reject_reason"`
			KeyAlias     string `json:"key_alias"`
			Deployment   string `json:"deployment"`
			Billable     bool   `json:"billable"`
		} `json:"records"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Records) != 1 {
		t.Fatalf("got %d records, want the rejection: %s", len(body.Records), rec.Body.String())
	}
	got := body.Records[0]
	if got.Outcome != "rejected" || got.RejectReason == "" {
		t.Fatalf("record = %+v, want a rejection carrying its reason", got)
	}
	if got.KeyAlias != "test-key" {
		t.Fatalf("key alias = %q; a rejection that cannot name the caller is not worth recording", got.KeyAlias)
	}
	// A refusal reached no upstream, so it must not look like passthrough
	// traffic that was billed elsewhere.
	if got.Deployment != "" || got.Billable {
		t.Fatalf("a rejection reported a deployment or billability: %+v", got)
	}
}

// The scope ledger is keyed by Scope.SpendSubject() — "team:acme/platform" —
// not by the bare id. Reading it by id alone missed every row, so a team at its
// cap rendered as having spent nothing while its keys were about to be refused.
func TestUIScopesReportPooledSpend(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode:  "api_key",
		masterKey: testMasterKey,
		ui:        true,
		keyScope:  "acme/platform",
		rbac: config.RBACConfig{Organizations: []config.OrgConfig{{
			ID:          "acme",
			ScopeLimits: config.ScopeLimits{Alias: "Acme"},
			Teams: []config.TeamConfig{{
				ID:          "platform",
				ScopeLimits: config.ScopeLimits{Alias: "Platform", MaxBudget: 100},
			}},
		}}},
	})
	cookie := h.signIn(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"anthropic-claude","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-gateway-key", testVirtualKey)
	if rec := h.do(t, req); rec.Code != http.StatusOK {
		t.Fatalf("inference: status %d, body %s", rec.Code, rec.Body.String())
	}

	rec := h.uiGet(t, "/ui/api/scopes", cookie)
	var body struct {
		Scopes []struct {
			ID    string  `json:"id"`
			Kind  string  `json:"kind"`
			Spend float64 `json:"spend"`
			Keys  int     `json:"keys"`
		} `json:"scopes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Scopes) != 2 {
		t.Fatalf("got %d scopes, want the org and the team", len(body.Scopes))
	}
	for _, scope := range body.Scopes {
		if scope.Spend <= 0 {
			t.Fatalf("scope %q reported %v spend; the pool should carry the request's whole cost",
				scope.ID, scope.Spend)
		}
	}
}

// A cache hit is a served request and belongs in the traffic view. It must not
// carry a cost: the answer was paid for once, when it was first fetched.
func TestUITrafficRecordsCacheHits(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "api_key", masterKey: testMasterKey, ui: true, cache: true,
	})
	cookie := h.signIn(t)

	body := `{"model":"anthropic-claude","messages":[{"role":"user","content":"same"}]}`
	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("x-gateway-key", testVirtualKey)
		if rec := h.do(t, req); rec.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
		}
	}

	rec := h.uiGet(t, "/ui/api/traffic", cookie)
	var got struct {
		Records []struct {
			Outcome  string  `json:"outcome"`
			Cost     float64 `json:"cost"`
			Billable bool    `json:"billable"`
		} `json:"records"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Records) != 2 {
		t.Fatalf("got %d records, want the miss and the hit: %s", len(got.Records), rec.Body.String())
	}
	hit := got.Records[0]
	if hit.Outcome != "cache_hit" {
		t.Fatalf("newest record is %q, want cache_hit", hit.Outcome)
	}
	if hit.Cost != 0 || hit.Billable {
		t.Fatalf("a cache hit was billed: %+v", hit)
	}
}

// An unauthenticated refusal is counted but not kept: it is decided before any
// credential is verified, so recording it would let anyone with a socket evict
// the buffer an operator opened the page to read.
func TestUnauthenticatedRejectionsDoNotFillTheRing(t *testing.T) {
	h := newUIHarness(t)

	for range 20 {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"anthropic-claude","messages":[]}`))
		req.Header.Set("x-gateway-key", "sk-vk-NOTAKEY")
		h.do(t, req)
	}

	if n := h.srv.traffic.Len(); n != 0 {
		t.Fatalf("traffic ring holds %d unauthenticated rejections, want 0", n)
	}
	// The counter still carries them, so the signal is not lost.
	var buf bytes.Buffer
	if _, err := h.metrics.WriteTo(&buf); err != nil {
		t.Fatalf("write metrics: %v", err)
	}
	if !strings.Contains(buf.String(), `outcome="unauthenticated"`) {
		t.Fatal("unauthenticated rejections were dropped from the metrics too")
	}
}
