package spend

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/erickardus/ai-gateway/internal/core"
)

// fakeHistory records what it was handed, so the decorator can be tested
// without a database.
type fakeHistory struct {
	appended []Entry
	closed   bool
}

func (f *fakeHistory) Append(_ context.Context, e Entry) error {
	f.appended = append(f.appended, e)
	return nil
}
func (f *fakeHistory) Series(context.Context, Query) ([]Bucket, error)       { return nil, nil }
func (f *fakeHistory) EachRow(context.Context, Query, func(Row) error) error { return nil }
func (f *fakeHistory) Close() error                                          { f.closed = true; return nil }

func testEntry(cost float64) Entry {
	return Entry{
		At:           time.Date(2026, 8, 14, 9, 30, 0, 0, time.UTC),
		RequestID:    "req-1",
		KeyHash:      "hash-a",
		KeyAlias:     "laptop",
		ModelGroup:   "anthropic-claude",
		DeploymentID: "anthropic-claude/primary",
		Usage:        core.Usage{InputTokens: 100, OutputTokens: 20},
		Cost:         cost,
		Billable:     true,
		Scopes:       []string{"acme/payments", "acme"},
		ScopeAliases: []string{"Payments", "Acme"},
	}
}

// One entry must reach both stores: the ledger enforces from it, the history
// reports from it, and a gateway that recorded to only one would either stop
// enforcing budgets or stop being able to explain them.
func TestWithHistoryRecordsToBoth(t *testing.T) {
	ledger := New()
	hist := &fakeHistory{}
	store := WithHistory(ledger, hist)

	if err := store.Record(context.Background(), testEntry(0.25)); err != nil {
		t.Fatalf("Record: %v", err)
	}

	spends, err := store.Spends(context.Background(), []Subject{{Kind: KindKey, ID: "hash-a"}})
	if err != nil {
		t.Fatalf("Spends: %v", err)
	}
	if spends[0] != 0.25 {
		t.Errorf("ledger spend = %v, want 0.25", spends[0])
	}
	if len(hist.appended) != 1 {
		t.Fatalf("history received %d entries, want 1", len(hist.appended))
	}
}

// A nil history must leave the store exactly as it was, so the decorator is
// something a deployment opts into rather than something every gateway pays a
// wrapper for.
func TestWithHistoryIsIdentityWhenAbsent(t *testing.T) {
	ledger := New()
	if got := WithHistory(ledger, nil); got != Store(ledger) {
		t.Error("WithHistory(store, nil) wrapped the store anyway")
	}
}

// Revoking a key clears what it may still spend. It must not clear what it
// already spent: that record is what a chargeback is built from, and erasing it
// would make revoking a key a way to erase a month of somebody's costs.
func TestForgetClearsTheWindowAndNotTheHistory(t *testing.T) {
	ledger := New()
	hist := &fakeHistory{}
	store := WithHistory(ledger, hist)

	if err := store.Record(context.Background(), testEntry(3)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	forgetter, ok := store.(interface{ Forget(string) })
	if !ok {
		t.Fatal("the decorated store hides Forget, which the key handler reaches by assertion")
	}
	forgetter.Forget("hash-a")

	spends, err := store.Spends(context.Background(), []Subject{{Kind: KindKey, ID: "hash-a"}})
	if err != nil {
		t.Fatalf("Spends: %v", err)
	}
	if spends[0] != 0 {
		t.Errorf("spend after Forget = %v, want 0", spends[0])
	}
	if len(hist.appended) != 1 {
		t.Errorf("history holds %d entries after a revoke, want the 1 it recorded", len(hist.appended))
	}
}

// A request is charged to its key, to every scope above it, to its deployment
// and to its model group. The rollup has to reproduce exactly that, because a
// figure that counted a team once for a two-level hierarchy would understate
// every organisation in the report.
func TestRollupChargesEveryDimension(t *testing.T) {
	e := testEntry(1.5)
	rows := rollup([]Entry{e})

	got := make(map[string]dailyRow, len(rows))
	for _, r := range rows {
		got[string(r.kind)+":"+r.subject] = r
	}
	for _, want := range []string{
		"key:hash-a",
		"scope:acme/payments",
		"scope:acme",
		"deployment:anthropic-claude/primary",
		"model:anthropic-claude",
	} {
		row, ok := got[want]
		if !ok {
			t.Fatalf("rollup produced no row for %s; got %v", want, keysOf(got))
		}
		if row.Cost != 1.5 || row.Requests != 1 {
			t.Errorf("%s = %v cost over %d requests, want 1.5 over 1", want, row.Cost, row.Requests)
		}
	}
	if len(rows) != 5 {
		t.Errorf("rollup produced %d rows, want 5", len(rows))
	}
	// The scope rows carry the labels a report is read by.
	if got["scope:acme/payments"].alias != "Payments" {
		t.Errorf("scope alias = %q, want Payments", got["scope:acme/payments"].alias)
	}
}

// Entries falling in one UTC day fold into one row per subject; entries either
// side of midnight do not. This is what makes a day's upsert a handful of
// statements rather than one per request.
func TestRollupFoldsByUTCDay(t *testing.T) {
	early := testEntry(1)
	early.At = time.Date(2026, 8, 14, 23, 59, 0, 0, time.UTC)
	late := testEntry(2)
	late.At = time.Date(2026, 8, 15, 0, 1, 0, 0, time.UTC)
	same := testEntry(4)
	same.At = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	var keyRows []dailyRow
	for _, r := range rollup([]Entry{early, late, same}) {
		if r.kind == KindKey {
			keyRows = append(keyRows, r)
		}
	}
	if len(keyRows) != 2 {
		t.Fatalf("key rows = %d, want 2 (one per UTC day)", len(keyRows))
	}
	if keyRows[0].Cost != 1 {
		t.Errorf("14 August = %v, want 1", keyRows[0].Cost)
	}
	if keyRows[1].Cost != 6 || keyRows[1].Requests != 2 {
		t.Errorf("15 August = %v over %d requests, want 6 over 2", keyRows[1].Cost, keyRows[1].Requests)
	}
}

// Passthrough traffic is usage with no cost, and the rollup must keep the two
// apart: a team that ran a thousand subscription-billed requests has spent
// nothing, and a report that showed a thousand billable requests at zero cost
// would look like a pricing bug.
func TestRollupSeparatesBillableFromUsage(t *testing.T) {
	free := testEntry(0)
	free.Billable = false
	free.Cost = 0

	rows := rollup([]Entry{testEntry(2), free})
	for _, r := range rows {
		if r.kind != KindKey {
			continue
		}
		if r.Requests != 2 {
			t.Errorf("requests = %d, want 2", r.Requests)
		}
		if r.BillableRequests != 1 {
			t.Errorf("billable requests = %d, want 1", r.BillableRequests)
		}
		if r.Cost != 2 {
			t.Errorf("cost = %v, want 2", r.Cost)
		}
	}
}

func TestQueryValidate(t *testing.T) {
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)

	tests := []struct {
		name    string
		q       Query
		wantErr string
	}{
		{"a whole month by day", Query{Kind: KindScope, From: from, To: to, Interval: IntervalDay}, ""},
		{"no kind", Query{From: from, To: to}, "kind: must be"},
		{"a kind that carries no budget is still reportable", Query{Kind: KindModel, From: from, To: to}, ""},
		{"no range", Query{Kind: KindKey}, "from and to"},
		{"backwards", Query{Kind: KindKey, From: to, To: from}, "must be after from"},
		{"an interval nothing answers", Query{Kind: KindKey, From: from, To: to, Interval: "week"}, "interval: must be"},
		{"a negative limit", Query{Kind: KindKey, From: from, To: to, Limit: -1}, "limit: must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.q.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Validate: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("Validate succeeded, want an error containing %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func keysOf(m map[string]dailyRow) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
