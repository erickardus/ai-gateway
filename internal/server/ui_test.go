package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/erickardus/ai-gateway/internal/audit"
	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/reqlog"
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
	for _, path := range []string{
		"/ui/api/overview", "/ui/api/keys", "/ui/api/traffic", "/ui/api/spend/keys",
		"/ui/api/analytics", "/ui/api/audit", "/ui/api/config", "/ui/api/spend/export",
	} {
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

// analyticsBody is the analytics response as a test reads it.
type analyticsBody struct {
	Window        string `json:"window"`
	BucketSeconds int    `json:"bucket_seconds"`
	From          string `json:"from"`
	To            string `json:"to"`
	Series        []struct {
		Start        time.Time `json:"start"`
		Requests     int       `json:"requests"`
		Errors       int       `json:"errors"`
		Rejected     int       `json:"rejected"`
		LatencyP50MS float64   `json:"latency_p50_ms"`
	} `json:"series"`
	Totals struct {
		Requests  int `json:"requests"`
		Errors    int `json:"errors"`
		Rejected  int `json:"rejected"`
		CacheHits int `json:"cache_hits"`
	} `json:"totals"`
	Latency struct {
		P50MS float64 `json:"p50_ms"`
		P95MS float64 `json:"p95_ms"`
		MaxMS float64 `json:"max_ms"`
	} `json:"latency"`
	Outcomes []struct {
		Outcome string `json:"outcome"`
		Count   int    `json:"count"`
	} `json:"outcomes"`
	RejectReasons []struct {
		Reason string `json:"reason"`
		Count  int    `json:"count"`
	} `json:"reject_reasons"`
	Groups []struct {
		Name     string `json:"name"`
		Requests int    `json:"requests"`
	} `json:"groups"`
	Keys []struct {
		Name     string `json:"name"`
		Requests int    `json:"requests"`
	} `json:"keys"`
	Note string `json:"note"`
}

func fetchAnalytics(t *testing.T, h *harness, cookie *http.Cookie, query string) analyticsBody {
	t.Helper()
	rec := h.uiGet(t, "/ui/api/analytics"+query, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("analytics: status %d, body %s", rec.Code, rec.Body.String())
	}
	var body analyticsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// The series must cover the whole window even where nothing happened. A chart
// that drops empty buckets draws a quiet hour as a straight line between the
// two minutes either side of it, which is the opposite of what an operator
// looking for an outage needs to see.
func TestUIAnalyticsBucketsAreContiguousAcrossTheWindow(t *testing.T) {
	h := newUIHarness(t)
	cookie := h.signIn(t)

	now := time.Now().UTC()
	// Two requests half an hour apart, in a window of twelve five-minute
	// buckets: whatever they land in, most of the series has nothing in it.
	h.srv.traffic.Add(reqlog.Record{
		ID: "a", At: now.Add(-32 * time.Minute), ModelGroup: "anthropic-claude",
		Deployment: h.deploymentID, Outcome: "success", LatencyMS: 120,
	})
	h.srv.traffic.Add(reqlog.Record{
		ID: "b", At: now.Add(-2 * time.Minute), ModelGroup: "anthropic-claude",
		Deployment: h.deploymentID, Outcome: "success", LatencyMS: 140,
	})

	body := fetchAnalytics(t, h, cookie, "?window=1h&buckets=12")
	if len(body.Series) != 12 {
		t.Fatalf("got %d buckets, want 12", len(body.Series))
	}
	if body.BucketSeconds != 300 {
		t.Fatalf("bucket_seconds = %d, want 300", body.BucketSeconds)
	}
	for i := 1; i < len(body.Series); i++ {
		gap := body.Series[i].Start.Sub(body.Series[i-1].Start)
		if gap != 5*time.Minute {
			t.Fatalf("bucket %d starts %v after the one before it, want 5m", i, gap)
		}
	}
	if body.Totals.Requests != 2 {
		t.Fatalf("totals.requests = %d, want the two records", body.Totals.Requests)
	}
	empty := 0
	for _, b := range body.Series {
		if b.Requests == 0 {
			empty++
		}
	}
	if empty != 10 {
		t.Fatalf("%d empty buckets, want the ten the traffic did not fall in", empty)
	}
	if !strings.Contains(body.Note, "per-process") {
		t.Fatalf("note does not say the buffer is per-process: %q", body.Note)
	}
}

// A window is clamped rather than refused, and the response says which window
// it actually covered: a chart drawn against the window it asked for would put
// a day of traffic on a week's axis.
func TestUIAnalyticsClampsTheWindowAndBucketCount(t *testing.T) {
	h := newUIHarness(t)
	cookie := h.signIn(t)

	body := fetchAnalytics(t, h, cookie, "?window=168h&buckets=5000")
	if len(body.Series) != 240 {
		t.Fatalf("got %d buckets, want the 240 cap", len(body.Series))
	}
	from, err := time.Parse(time.RFC3339, body.From)
	if err != nil {
		t.Fatalf("from is not RFC 3339: %v", err)
	}
	to, err := time.Parse(time.RFC3339, body.To)
	if err != nil {
		t.Fatalf("to is not RFC 3339: %v", err)
	}
	if covered := to.Sub(from); covered != 24*time.Hour {
		t.Fatalf("covered %v, want the 24h cap", covered)
	}
}

// A refusal is decided in microseconds and arrives in volume, so counting it in
// the latency distribution reports a p50 of nothing at all on a gateway whose
// upstream is slow. Only requests that reached a deployment may be measured.
func TestUIAnalyticsLatencyExcludesRequestsThatReachedNoUpstream(t *testing.T) {
	h := newUIHarness(t)
	cookie := h.signIn(t)

	now := time.Now().UTC()
	h.srv.traffic.Add(reqlog.Record{
		ID: "served", At: now.Add(-time.Minute), ModelGroup: "anthropic-claude",
		Deployment: h.deploymentID, KeyAlias: "test-key", Outcome: "success",
		LatencyMS: 900,
	})
	for i := range 9 {
		h.srv.traffic.Add(reqlog.Record{
			ID: "refused" + strconv.Itoa(i), At: now.Add(-time.Minute),
			ModelGroup: "anthropic-claude", KeyAlias: "test-key",
			Outcome: "rejected", RejectReason: "model_forbidden", LatencyMS: 1,
		})
	}

	body := fetchAnalytics(t, h, cookie, "?window=10m&buckets=12")
	if body.Totals.Requests != 10 || body.Totals.Rejected != 9 {
		t.Fatalf("totals = %+v, want 10 requests of which 9 rejected", body.Totals)
	}
	// Nine of ten records are one-millisecond refusals, so any percentile that
	// counted them would sit at 1.
	if body.Latency.P50MS != 900 || body.Latency.MaxMS != 900 {
		t.Fatalf("latency = %+v, want the served request's 900ms alone", body.Latency)
	}
	for _, b := range body.Series {
		if b.Requests > 0 && b.LatencyP50MS != 900 {
			t.Fatalf("bucket latency p50 = %v, want 900", b.LatencyP50MS)
		}
	}
	if len(body.RejectReasons) != 1 || body.RejectReasons[0].Reason != "model_forbidden" ||
		body.RejectReasons[0].Count != 9 {
		t.Fatalf("reject_reasons = %+v", body.RejectReasons)
	}
	// A refusal reached no deployment, so it belongs to the caller and the group
	// it named but to no deployment row.
	if len(body.Keys) != 1 || body.Keys[0].Requests != 10 {
		t.Fatalf("keys = %+v, want the one caller with all ten", body.Keys)
	}
	if len(body.Groups) != 1 || body.Groups[0].Requests != 10 {
		t.Fatalf("groups = %+v", body.Groups)
	}
}

// A cache hit is recorded against the sentinel deployment "cache", so a test
// for "reached a deployment" admits it — and it answered in microseconds
// without calling anyone. Counting it would make a gateway look faster the more
// often it was asked the same question twice, which is the one thing latency
// must not report.
func TestUIAnalyticsLatencyExcludesCacheHits(t *testing.T) {
	h := newUIHarness(t)
	cookie := h.signIn(t)

	now := time.Now().UTC()
	h.srv.traffic.Add(reqlog.Record{
		ID: "served", At: now.Add(-time.Minute), ModelGroup: "anthropic-claude",
		Deployment: h.deploymentID, KeyAlias: "test-key", Outcome: "success",
		LatencyMS: 900,
	})
	for i := range 9 {
		h.srv.traffic.Add(reqlog.Record{
			ID: "hit" + strconv.Itoa(i), At: now.Add(-time.Minute),
			ModelGroup: "anthropic-claude", KeyAlias: "test-key",
			Deployment: "cache", Outcome: "cache_hit", LatencyMS: 0.2,
		})
	}

	body := fetchAnalytics(t, h, cookie, "?window=10m&buckets=12")
	if body.Totals.Requests != 10 || body.Totals.CacheHits != 9 {
		t.Fatalf("totals = %+v, want 10 requests of which 9 cache hits", body.Totals)
	}
	if body.Latency.P50MS != 900 || body.Latency.MaxMS != 900 {
		t.Fatalf("latency = %+v, want the one upstream call's 900ms alone", body.Latency)
	}
	for _, b := range body.Series {
		if b.Requests > 0 && b.LatencyP50MS != 900 {
			t.Fatalf("bucket latency p50 = %v, want 900", b.LatencyP50MS)
		}
	}
}

// A latency percentile over nothing must be zero rather than absent or wrong,
// and a window with no traffic is the ordinary state of a quiet gateway.
func TestUIAnalyticsAnswersAnEmptyWindow(t *testing.T) {
	h := newUIHarness(t)
	cookie := h.signIn(t)

	body := fetchAnalytics(t, h, cookie, "")
	if len(body.Series) != 60 || body.BucketSeconds != 60 {
		t.Fatalf("default window: %d buckets of %ds, want 60 of 60s", len(body.Series), body.BucketSeconds)
	}
	if body.Totals.Requests != 0 || body.Latency.P50MS != 0 {
		t.Fatalf("an empty window reported %+v / %+v", body.Totals, body.Latency)
	}
	// Empty collections must encode as arrays: the console maps over them, and a
	// null is a branch it would have to carry on every list.
	if body.Outcomes == nil || body.Groups == nil || body.Keys == nil || body.RejectReasons == nil {
		t.Fatal("an empty window returned nulls where the console expects empty arrays")
	}
}

// The stdout sink writes to a stream it cannot read back. That is a feature
// being off, not a failure, so it answers 200 and says which sinks would make
// the log visible.
func TestUIAuditReportsAnUnreadableSink(t *testing.T) {
	h := newUIHarness(t)
	cookie := h.signIn(t)
	h.srv.UseAudit(audit.NewWriterSink(io.Discard))

	rec := h.uiGet(t, "/ui/api/audit", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Readable bool   `json:"readable"`
		Sink     string `json:"sink"`
		Count    int    `json:"count"`
		Sealed   bool   `json:"sealed"`
		Note     string `json:"note"`
		Records  []any  `json:"records"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Readable {
		t.Fatal("a stdout sink reported itself readable")
	}
	if body.Sink != "stdout" || body.Sealed {
		t.Fatalf("body = %+v", body)
	}
	if body.Records == nil {
		t.Fatal("records is null; the console renders an empty table from an array")
	}
	if !strings.Contains(body.Note, "file") || !strings.Contains(body.Note, "postgres") {
		t.Fatalf("note does not name the sinks that can be read: %q", body.Note)
	}
}

// The console could not see the audit log at all before this endpoint existed,
// which made the one artefact an auditor asks for the one thing only somebody
// with shell access could read.
func TestUIAuditReadsTheFileChain(t *testing.T) {
	h := newUIHarness(t)
	cookie := h.signIn(t)

	path := filepath.Join(t.TempDir(), "audit.log")
	sink, err := audit.OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	t.Cleanup(func() { sink.Close() })
	h.srv.cfg.Audit.Sink = config.AuditSinkFile
	h.srv.cfg.Audit.Path = path
	h.srv.UseAudit(sink)

	for _, action := range []string{audit.ActionKeyGenerate, audit.ActionKeyDelete} {
		if _, err := sink.Record(context.Background(), audit.Event{
			Action: action, Actor: audit.MasterKeyActor(),
			TargetKind: audit.TargetKey, Target: "hash-1", Outcome: audit.OutcomeSuccess,
		}); err != nil {
			t.Fatalf("record %s: %v", action, err)
		}
	}

	rec := h.uiGet(t, "/ui/api/audit", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Readable        bool           `json:"readable"`
		Sink            string         `json:"sink"`
		Count           int            `json:"count"`
		Sealed          bool           `json:"sealed"`
		Verified        *bool          `json:"verified"`
		VerifiedThrough uint64         `json:"verified_through"`
		VerifyError     string         `json:"verify_error"`
		Records         []audit.Record `json:"records"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Readable || body.Sink != "file" || body.Count != 2 {
		t.Fatalf("body = %+v", body)
	}
	// Newest first, which is the order the question "what just happened" is
	// asked in.
	if body.Records[0].Action != audit.ActionKeyDelete || body.Records[0].Seq != 2 {
		t.Fatalf("first record = %+v, want the newest", body.Records[0])
	}
	if body.Records[1].Seq != 1 {
		t.Fatalf("second record = %+v, want seq 1", body.Records[1])
	}
	if body.Verified == nil || !*body.Verified || body.VerifiedThrough != 2 || body.VerifyError != "" {
		t.Fatalf("a file chain the gateway just wrote did not verify: %+v", body)
	}
	if body.Sealed {
		t.Fatal("a healthy chain reported itself sealed")
	}

	// The limit bounds what one page fetches, newest end first.
	rec = h.uiGet(t, "/ui/api/audit?limit=1", cookie)
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Count != 1 || body.Records[0].Seq != 2 {
		t.Fatalf("limit=1 returned %+v, want only the newest record", body.Records)
	}
}

// A chain that no longer verifies is the state nothing else surfaces until
// somebody tries to administer something, so the console has to show it.
func TestUIAuditReportsASealedChain(t *testing.T) {
	h := newUIHarness(t)
	cookie := h.signIn(t)

	path := filepath.Join(t.TempDir(), "audit.log")
	sink, err := audit.OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := sink.Record(context.Background(), audit.Event{
		Action: audit.ActionKeyGenerate, Actor: audit.MasterKeyActor(),
		Outcome: audit.OutcomeSuccess,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	sink.Close()

	// Edit the record in place, which is what the hash is there to catch.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(path, bytes.Replace(raw, []byte(`"outcome":"success"`),
		[]byte(`"outcome":"refused"`), 1), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	reopened, err := audit.OpenFile(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
	h.srv.cfg.Audit.Sink = config.AuditSinkFile
	h.srv.cfg.Audit.Path = path
	h.srv.UseAudit(reopened)

	rec := h.uiGet(t, "/ui/api/audit", cookie)
	var body struct {
		Sealed     bool   `json:"sealed"`
		SealReason string `json:"seal_reason"`
		Verified   *bool  `json:"verified"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Sealed || body.SealReason == "" {
		t.Fatalf("an edited chain reported %+v", body)
	}
	if body.Verified == nil || *body.Verified {
		t.Fatalf("an edited chain reported verified = %v", body.Verified)
	}
}

// The configuration holds the master key, every upstream API key and three
// connection strings with passwords in them. A page that reports what this
// instance is configured to do must report none of them.
func TestUIConfigNeverLeaksASecret(t *testing.T) {
	h := newUIHarness(t)
	cookie := h.signIn(t)

	rec := h.uiGet(t, "/ui/api/config", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	for _, secret := range []string{testMasterKey, "sk-ant-api03-SERVERSIDE"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("the config view leaked %q: %s", secret, raw)
		}
	}

	var body struct {
		Strategy string `json:"strategy"`
		Groups   []struct {
			Name                   string   `json:"name"`
			Format                 string   `json:"format"`
			Deployments            []string `json:"deployments"`
			Fallbacks              []string `json:"fallbacks"`
			ContextWindowFallbacks []string `json:"context_window_fallbacks"`
		} `json:"groups"`
		Router map[string]any `json:"router"`
		Keys   struct {
			StoreKind    string   `json:"store_kind"`
			MasterKeySet bool     `json:"master_key_set"`
			HeaderNames  []string `json:"header_names"`
		} `json:"keys"`
		Audit struct {
			Enabled  bool   `json:"enabled"`
			Sink     string `json:"sink"`
			Readable bool   `json:"readable"`
		} `json:"audit"`
		Limits struct {
			RequestLogSize int `json:"request_log_size"`
		} `json:"limits"`
		ResponseCache map[string]any `json:"response_cache"`
		RBAC          struct {
			Enabled bool `json:"enabled"`
		} `json:"rbac"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// A boolean is the whole of what may be said about the master key, and it
	// still has to be said: it decides whether this console exists at all.
	if !body.Keys.MasterKeySet {
		t.Fatal("master_key_set is false on a gateway with a master key")
	}
	if body.Strategy != config.StrategyWeightedShuffle {
		t.Fatalf("strategy = %q", body.Strategy)
	}
	if len(body.Groups) != 1 || body.Groups[0].Name != "anthropic-claude" {
		t.Fatalf("groups = %+v", body.Groups)
	}
	if body.Groups[0].Format != string(core.FormatAnthropic) {
		t.Fatalf("group format = %q", body.Groups[0].Format)
	}
	if len(body.Groups[0].Deployments) != 1 || body.Groups[0].Deployments[0] != h.deploymentID {
		t.Fatalf("group deployments = %+v", body.Groups[0].Deployments)
	}
	if body.Groups[0].Fallbacks == nil || body.Groups[0].ContextWindowFallbacks == nil {
		t.Fatal("a group with no fallbacks returned null rather than an empty list")
	}
	if body.Router["num_retries"] != float64(config.DefaultNumRetries) {
		t.Fatalf("router = %+v", body.Router)
	}
	if body.Limits.RequestLogSize != h.srv.traffic.Cap() {
		t.Fatalf("request_log_size = %d, want the ring's capacity", body.Limits.RequestLogSize)
	}
	// Nothing is recording, and the page says so rather than implying a chain
	// exists that the console simply cannot read.
	if body.Audit.Enabled || body.Audit.Sink != "none" || body.Audit.Readable {
		t.Fatalf("audit = %+v on a gateway with no sink attached", body.Audit)
	}
	if _, ok := body.ResponseCache["entries"]; ok {
		t.Fatal("a gateway with no response cache reported an entry count")
	}
}

// The in-process cache can say how full it is, and a page about what this
// instance is doing should say so.
func TestUIConfigReportsTheResponseCacheSize(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "api_key", masterKey: testMasterKey, ui: true, cache: true,
	})
	cookie := h.signIn(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"anthropic-claude","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-gateway-key", testVirtualKey)
	if rec := h.do(t, req); rec.Code != http.StatusOK {
		t.Fatalf("inference: status %d, body %s", rec.Code, rec.Body.String())
	}

	rec := h.uiGet(t, "/ui/api/config", cookie)
	var body struct {
		ResponseCache struct {
			Enabled bool `json:"enabled"`
			Entries int  `json:"entries"`
		} `json:"response_cache"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.ResponseCache.Enabled || body.ResponseCache.Entries != 1 {
		t.Fatalf("response_cache = %+v, want one stored entry", body.ResponseCache)
	}
}

// The export is the artefact that goes to whoever does chargeback. Reaching it
// through the console session is what stops the master key being handed out so
// somebody can run a curl.
func TestUISpendExportIsServedThroughTheSession(t *testing.T) {
	h := newUIHarness(t)
	cookie := h.signIn(t)

	// With no history configured both surfaces answer the same 404, which is the
	// honest answer rather than the one an unknown path gives.
	rec := h.uiGet(t, "/ui/api/spend/export", cookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404 with no spend history: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "spend_history") {
		t.Fatalf("the refusal does not say what to configure: %s", rec.Body.String())
	}
}
