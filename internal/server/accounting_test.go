package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// usageUpstream replies with a realistic Anthropic usage block, including the
// cache counters Claude Code traffic actually carries.
func usageUpstream(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"id":"msg_1","type":"message","usage":{"input_tokens":1000,"output_tokens":500,"cache_read_input_tokens":20000,"cache_creation_input_tokens":2000}}`)
}

func TestSpendRecordedForBillableDeployment(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "api_key", allowPassthrough: true, upstream: usageUpstream})

	rec := h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	keys, err := h.ledger.Keys(context.Background())
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d key summaries, want 1", len(keys))
	}
	got := keys[0]
	if got.InputTokens != 1000 || got.OutputTokens != 500 {
		t.Errorf("tokens = %d/%d, want 1000/500", got.InputTokens, got.OutputTokens)
	}
	if got.CacheReadTokens != 20000 || got.CacheWriteTokens != 2000 {
		t.Errorf("cache tokens = %d/%d, want 20000/2000", got.CacheReadTokens, got.CacheWriteTokens)
	}
	// input 1000@3 + output 500@15 + cacheRead 20000@0.30 + cacheWrite 2000@3.75
	// = 0.003 + 0.0075 + 0.006 + 0.0075 = 0.024
	if got.Cost < 0.0239 || got.Cost > 0.0241 {
		t.Errorf("cost = %v, want ~0.024", got.Cost)
	}
	if got.BillableRequests != 1 {
		t.Errorf("billable requests = %d, want 1", got.BillableRequests)
	}
}

// Passthrough traffic bills the caller's subscription, so the operator's ledger
// must show usage but no cost.
func TestPassthroughRecordsUsageWithoutCost(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "passthrough", allowPassthrough: true, upstream: usageUpstream})

	if rec := h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	keys, _ := h.ledger.Keys(context.Background())
	got := keys[0]
	if got.Requests != 1 {
		t.Errorf("requests = %d, want 1", got.Requests)
	}
	if got.InputTokens != 1000 {
		t.Errorf("usage must still be recorded, got %d input tokens", got.InputTokens)
	}
	if got.Cost != 0 {
		t.Errorf("cost = %v, want 0: the caller's subscription was billed, not the operator", got.Cost)
	}
	if got.BillableRequests != 0 {
		t.Errorf("billable requests = %d, want 0", got.BillableRequests)
	}
}

func TestBudgetEnforced(t *testing.T) {
	// A budget small enough that one request exhausts it.
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, upstream: usageUpstream, maxBudget: 0.02,
	})

	if rec := h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`)); rec.Code != http.StatusOK {
		t.Fatalf("first request = %d, want 200", rec.Code)
	}

	// The next one is refused: 402, because retrying sooner will not help.
	rec := h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("second request = %d, want 402; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "budget") {
		t.Errorf("error body should mention the budget: %s", rec.Body.String())
	}
}

// A budget must not apply to traffic the operator does not pay for.
func TestBudgetIgnoresPassthroughTraffic(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "passthrough", allowPassthrough: true, upstream: usageUpstream, maxBudget: 0.0001,
	})
	for i := range 5 {
		if rec := h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`)); rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200: subscription traffic must not consume an operator budget", i+1, rec.Code)
		}
	}
}

func TestSpendEndpointsRequireMasterKey(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "api_key", allowPassthrough: true, upstream: usageUpstream, masterKey: "sk-master-X"})
	h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))

	for _, path := range []string{"/spend/keys", "/spend/deployments"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("x-gateway-key", testVirtualKey)
		if rec := h.do(t, req); rec.Code != http.StatusUnauthorized {
			t.Errorf("virtual key on %s = %d, want 401", path, rec.Code)
		}

		req = httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("x-gateway-key", "sk-master-X")
		rec := h.do(t, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("master key on %s = %d", path, rec.Code)
		}
		var payload struct {
			Count     int     `json:"count"`
			TotalCost float64 `json:"total_cost"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if payload.Count != 1 {
			t.Errorf("%s count = %d, want 1", path, payload.Count)
		}
		if payload.TotalCost <= 0 {
			t.Errorf("%s total_cost = %v, want > 0", path, payload.TotalCost)
		}
	}
}

func TestMetricsEndpoint(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "api_key", allowPassthrough: true, upstream: usageUpstream})
	h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))
	// A rejected request too, so the reason label is exercised.
	h.do(t, claudeCodeRequest("/v1/messages", `{"model":"nope","messages":[]}`))

	rec := h.do(t, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}
	out := rec.Body.String()
	for _, want := range []string{
		`gateway_requests_total{model="anthropic-claude"`,
		`outcome="success"`,
		`gateway_tokens_total{model="anthropic-claude"`,
		`gateway_cost_total{model="anthropic-claude"`,
		`gateway_rejections_total{model="nope",outcome="model_unknown"} 1`,
		`gateway_request_duration_seconds_bucket`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output is missing %q:\n%s", want, out)
		}
	}
	// No credential may appear in a scrape.
	if strings.Contains(out, testVirtualKey) || strings.Contains(out, "sk-ant-oat01") {
		t.Error("a credential leaked into the metrics output")
	}
}

// In-flight must return to zero once requests complete.
func TestInFlightSettlesToZero(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "api_key", allowPassthrough: true, upstream: usageUpstream})
	for range 5 {
		h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))
	}
	rec := h.do(t, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, "gateway_in_flight") && !strings.HasSuffix(line, " 0") {
			t.Errorf("in-flight did not settle: %q", line)
		}
	}
}
