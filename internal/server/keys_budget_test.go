package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
