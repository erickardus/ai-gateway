package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/erickardus/ai-gateway/internal/auth"
	"github.com/erickardus/ai-gateway/internal/core"
)

// A key minted at runtime must be able to carry a budget.
//
// The README's own quickstart mints keys through this endpoint, so it is the
// path most keys take — and the request body accepted max_budget silently,
// dropping it in the JSON decoder and handing back a 201 for an uncapped key.
// Enforcement lives in the ledger and keys on the hash alone, so it never knew
// the difference between a key declared in the config and one issued here.
func TestGeneratedKeyCarriesItsBudget(t *testing.T) {
	h := newHarness(t, harnessOpts{masterKey: "sk-master-SECRET"})

	body := `{"alias":"capped","models":["anthropic-claude"],"max_budget":12.5,"budget_duration":"24h"}`
	req := httptest.NewRequest(http.MethodPost, "/key/generate", strings.NewReader(body))
	req.Header.Set("x-gateway-key", "sk-master-SECRET")
	rec := h.do(t, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("/key/generate = %d, body = %s", rec.Code, rec.Body.String())
	}

	var created struct {
		Info struct {
			Hash           string  `json:"hash"`
			MaxBudget      float64 `json:"max_budget"`
			BudgetDuration int64   `json:"budget_duration"`
		} `json:"info"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Info.MaxBudget != 12.5 {
		t.Errorf("max_budget = %v, want 12.5", created.Info.MaxBudget)
	}
	if want := int64(24 * time.Hour); created.Info.BudgetDuration != want {
		t.Errorf("budget_duration = %v, want %v", created.Info.BudgetDuration, want)
	}

	// And the stored key agrees, so enforcement sees the cap rather than only
	// the response echoing it back.
	stored, err := h.store.Get(t.Context(), created.Info.Hash)
	if err != nil {
		t.Fatalf("get stored key: %v", err)
	}
	if stored.MaxBudget != 12.5 || stored.BudgetDuration != 24*time.Hour {
		t.Errorf("stored key budget = %v over %v, want 12.5 over 24h", stored.MaxBudget, stored.BudgetDuration)
	}
}

func TestGeneratedKeyBudgetValidation(t *testing.T) {
	h := newHarness(t, harnessOpts{masterKey: "sk-master-SECRET"})

	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "a window with no cap limits nothing",
			body: `{"budget_duration":"24h"}`,
			want: "budget_duration requires max_budget",
		},
		{
			name: "an unparseable window",
			body: `{"max_budget":5,"budget_duration":"soon"}`,
			want: "budget_duration must be a positive Go duration string",
		},
		{
			name: "a negative cap",
			body: `{"max_budget":-1}`,
			want: "max_budget must not be negative",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/key/generate", strings.NewReader(tt.body))
			req.Header.Set("x-gateway-key", "sk-master-SECRET")
			rec := h.do(t, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("/key/generate = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.want) {
				t.Errorf("error = %s, want it to mention %q", rec.Body.String(), tt.want)
			}
		})
	}
}

// A lifetime budget is the documented meaning of an omitted window, so it must
// remain reachable rather than being caught by the "window with no cap" rule.
func TestGeneratedKeyAcceptsALifetimeBudget(t *testing.T) {
	h := newHarness(t, harnessOpts{masterKey: "sk-master-SECRET"})

	req := httptest.NewRequest(http.MethodPost, "/key/generate", strings.NewReader(`{"max_budget":5}`))
	req.Header.Set("x-gateway-key", "sk-master-SECRET")
	rec := h.do(t, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("/key/generate = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// A key's hash is what its spend, its budget window and its rate-limit counters
// are addressed by, so raising a budget must be an edit rather than a
// revoke-and-reissue — which would reset the window it was spending against and
// hand its holder a new secret to install everywhere.
func TestKeyUpdatePreservesIdentity(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "api_key", masterKey: testMasterKey, maxBudget: 0.01})

	before := h.keyInfo(t, auth.HashKey(testVirtualKey))
	body := `{"hash":"` + auth.HashKey(testVirtualKey) + `","max_budget":5,"alias":"renamed"}`
	req := httptest.NewRequest(http.MethodPost, "/key/update", strings.NewReader(body))
	req.Header.Set("x-gateway-key", testMasterKey)
	rec := h.do(t, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	after := h.keyInfo(t, auth.HashKey(testVirtualKey))
	if after.Hash != before.Hash {
		t.Fatalf("hash changed from %q to %q", before.Hash, after.Hash)
	}
	if after.MaxBudget != 5 || after.Alias != "renamed" {
		t.Fatalf("update did not apply: %+v", after)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Fatal("update rewrote the creation time")
	}
}

// Omitted fields must be left alone. A partial update built on zero values
// would silently reset whatever the caller did not mention.
func TestKeyUpdateLeavesOmittedFieldsAlone(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "api_key", masterKey: testMasterKey, rpmLimit: 42})

	body := `{"hash":"` + auth.HashKey(testVirtualKey) + `","blocked":true}`
	req := httptest.NewRequest(http.MethodPost, "/key/update", strings.NewReader(body))
	req.Header.Set("x-gateway-key", testMasterKey)
	if rec := h.do(t, req); rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}

	after := h.keyInfo(t, auth.HashKey(testVirtualKey))
	if !after.Blocked {
		t.Fatal("blocked was not applied")
	}
	if after.RPMLimit != 42 {
		t.Fatalf("rpm_limit = %d, want the untouched 42", after.RPMLimit)
	}
	if after.Alias != "test-key" {
		t.Fatalf("alias = %q, want the untouched test-key", after.Alias)
	}
}

// The window-without-a-cap rule is checked against the merged key, not the
// request: clearing a budget on a key that already has a window has to be
// refused for the same reason setting both at once is.
func TestKeyUpdateRefusesAWindowWithNoCap(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "api_key", masterKey: testMasterKey})

	set := `{"hash":"` + auth.HashKey(testVirtualKey) + `","max_budget":5,"budget_duration":"720h"}`
	req := httptest.NewRequest(http.MethodPost, "/key/update", strings.NewReader(set))
	req.Header.Set("x-gateway-key", testMasterKey)
	if rec := h.do(t, req); rec.Code != http.StatusOK {
		t.Fatalf("setting a budget: status %d, body %s", rec.Code, rec.Body.String())
	}

	clear := `{"hash":"` + auth.HashKey(testVirtualKey) + `","max_budget":0}`
	req = httptest.NewRequest(http.MethodPost, "/key/update", strings.NewReader(clear))
	req.Header.Set("x-gateway-key", testMasterKey)
	rec := h.do(t, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("clearing the cap but not the window: status %d, want 400", rec.Code)
	}
}

func TestKeyUpdateRequiresTheMasterKey(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "api_key", masterKey: testMasterKey})
	body := `{"hash":"` + auth.HashKey(testVirtualKey) + `","blocked":true}`
	req := httptest.NewRequest(http.MethodPost, "/key/update", strings.NewReader(body))
	req.Header.Set("x-gateway-key", testVirtualKey)
	if rec := h.do(t, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
}

// keyInfo reads one key's stored metadata through the management endpoint.
func (h *harness) keyInfo(t *testing.T, hash string) core.Key {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/key/info?hash="+hash, nil)
	req.Header.Set("x-gateway-key", testMasterKey)
	rec := h.do(t, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("key info: status %d, body %s", rec.Code, rec.Body.String())
	}
	var key core.Key
	if err := json.Unmarshal(rec.Body.Bytes(), &key); err != nil {
		t.Fatalf("decode key: %v", err)
	}
	return key
}
