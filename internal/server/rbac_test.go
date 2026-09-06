package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/spend"
)

// teamRBAC declares one organisation holding one team, with the team carrying
// whatever cap the test is about.
func teamRBAC(budget float64) config.RBACConfig {
	return config.RBACConfig{Organizations: []config.OrgConfig{{
		ID: "acme",
		Teams: []config.TeamConfig{{
			ID: "platform",
			ScopeLimits: config.ScopeLimits{
				Alias:     "Platform Engineering",
				MaxBudget: budget,
			},
		}},
	}}}
}

const secondKey = "sk-vk-SECONDKEY"

// The end-to-end version of the guarantee the hierarchy exists for: two
// developers on one team spend one budget. The first exhausts it through real
// traffic, and the second is refused on their first request.
func TestTeamBudgetIsExhaustedByItsMembersTogether(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, upstream: usageUpstream,
		masterKey: "sk-master", rbac: teamRBAC(0.02), keyScope: "acme/platform",
	})
	h.addScopedKey(t, secondKey, "colleague", "acme/platform")

	body := `{"model":"anthropic-claude","messages":[]}`
	if rec := h.do(t, claudeCodeRequest("/v1/messages", body)); rec.Code != http.StatusOK {
		t.Fatalf("first member's request = %d, want 200", rec.Code)
	}

	// The second key has spent nothing of its own and carries no cap of its
	// own. Only the pool can refuse it.
	req := claudeCodeRequest("/v1/messages", body)
	req.Header.Set("x-gateway-key", secondKey)
	rec := h.do(t, req)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("second member's request = %d, want 402; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "acme/platform") {
		t.Errorf("the refusal should name the team, got %s", rec.Body.String())
	}
}

// A pool is its own subject in the ledger, so a team's spend can be read
// directly rather than inferred by summing the keys that happen to be in it
// today.
func TestSpendScopesReportsThePool(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, upstream: usageUpstream,
		masterKey: "sk-master", rbac: teamRBAC(0), keyScope: "acme/platform",
	})
	h.addScopedKey(t, secondKey, "colleague", "acme/platform")

	body := `{"model":"anthropic-claude","messages":[]}`
	if rec := h.do(t, claudeCodeRequest("/v1/messages", body)); rec.Code != http.StatusOK {
		t.Fatalf("request = %d, want 200", rec.Code)
	}
	req := claudeCodeRequest("/v1/messages", body)
	req.Header.Set("x-gateway-key", secondKey)
	if rec := h.do(t, req); rec.Code != http.StatusOK {
		t.Fatalf("second request = %d, want 200", rec.Code)
	}

	rows := h.spendScopes(t)
	bySubject := map[string]spend.Summary{}
	for _, r := range rows {
		bySubject[r.Subject] = r
	}

	team, ok := bySubject["team:acme/platform"]
	if !ok {
		t.Fatalf("no row for the team; got %v", rows)
	}
	if team.Requests != 2 {
		t.Errorf("team requests = %d, want 2 — both members' traffic", team.Requests)
	}
	if team.Alias != "Platform Engineering" {
		t.Errorf("team alias = %q, want the configured label", team.Alias)
	}
	if team.Cost <= 0 {
		t.Errorf("team cost = %v, want the pooled spend", team.Cost)
	}

	org, ok := bySubject["organization:acme"]
	if !ok {
		t.Fatalf("no row for the organisation; got %v", rows)
	}
	if org.Requests != team.Requests || org.Cost != team.Cost {
		t.Errorf("the organisation should carry everything beneath it: org %+v, team %+v", org.Totals, team.Totals)
	}

	// The per-key rows are unchanged: each key still reports only its own.
	keys := h.spendKeys(t)
	for _, r := range keys {
		if r.Requests != 1 {
			t.Errorf("key %q reports %d requests, want 1 — a pool must not be summed into its members", r.Subject, r.Requests)
		}
	}
}

// A gateway with no hierarchy answers the endpoint truthfully rather than
// failing, so a client need not know whether one is configured.
func TestSpendScopesIsEmptyWithoutAHierarchy(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, upstream: usageUpstream, masterKey: "sk-master",
	})
	if rec := h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`)); rec.Code != http.StatusOK {
		t.Fatalf("request = %d, want 200", rec.Code)
	}
	if rows := h.spendScopes(t); len(rows) != 0 {
		t.Errorf("want no scope rows, got %v", rows)
	}
}

// A team's model allowlist binds every key beneath it, whatever the key says.
func TestTeamModelAllowlistBindsItsKeys(t *testing.T) {
	rbac := config.RBACConfig{Organizations: []config.OrgConfig{{
		ID: "acme",
		Teams: []config.TeamConfig{{
			ID:          "platform",
			ScopeLimits: config.ScopeLimits{Models: []string{soloModel}},
		}},
	}}}
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, upstream: usageUpstream,
		masterKey: "sk-master", rbac: rbac, keyScope: "acme/platform", soloGroup: true,
	})

	// The key itself names no models, so without the team it could call this.
	rec := h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
	// The refusal names the level that withheld it. "This key is not permitted"
	// is true however the chain reached that answer, but it leaves a caller
	// whose key does list the model with nowhere to go — the obvious next step,
	// widening the key's allowlist, changes nothing.
	if !strings.Contains(rec.Body.String(), "acme/platform") {
		t.Errorf("the refusal should name the team that withheld it, got %s", rec.Body.String())
	}
	if logs := h.logBuf.String(); !strings.Contains(logs, "acme/platform") {
		t.Errorf("the log should name the scope too; got %s", logs)
	}

	// The model the team does allow still works, so the allowlist narrows
	// rather than blocks.
	if rec := h.do(t, claudeCodeRequest("/v1/messages",
		`{"model":"`+soloModel+`","messages":[]}`)); rec.Code != http.StatusOK {
		t.Fatalf("the model the team allows = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// /key/generate refuses a scope that is not declared, rather than handing back
// a key that fails later.
func TestKeyGenerateRefusesAnUnknownScope(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "api_key", masterKey: "sk-master", upstream: usageUpstream, rbac: teamRBAC(0),
	})
	req := httptest.NewRequest(http.MethodPost, "/key/generate",
		strings.NewReader(`{"alias":"new","scope":"acme/pltform"}`))
	req.Header.Set("x-gateway-key", "sk-master")
	rec := h.do(t, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/key/generate",
		strings.NewReader(`{"alias":"new","scope":"acme/platform"}`))
	req.Header.Set("x-gateway-key", "sk-master")
	if rec := h.do(t, req); rec.Code != http.StatusCreated {
		t.Fatalf("a declared scope should be accepted, got %d: %s", rec.Code, rec.Body.String())
	}
}

// spendScopes reads GET /spend/scopes with the master key.
func (h *harness) spendScopes(t *testing.T) []spend.Summary {
	t.Helper()
	return h.spendRows(t, "/spend/scopes")
}

// spendKeys reads GET /spend/keys with the master key.
func (h *harness) spendKeys(t *testing.T) []spend.Summary {
	t.Helper()
	return h.spendRows(t, "/spend/keys")
}

func (h *harness) spendRows(t *testing.T, path string) []spend.Summary {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("x-gateway-key", "sk-master")
	rec := h.do(t, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
	}
	var got struct {
		Entries []spend.Summary `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %s: %v (body %s)", path, err, rec.Body.String())
	}
	return got.Entries
}
