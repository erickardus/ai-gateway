package spend

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/erickardus/ai-gateway/internal/core"
)

func entry(keyHash string, cost float64, billable bool) Entry {
	return Entry{
		KeyHash: keyHash, KeyAlias: "alias-" + keyHash, DeploymentID: "dep-1",
		Usage:    core.Usage{InputTokens: 100, OutputTokens: 50, CacheReadTokens: 10, CacheWriteTokens: 5},
		Cost:     cost,
		Billable: billable,
	}
}

func TestRecordAccumulates(t *testing.T) {
	ctx := context.Background()
	l := New()
	for range 3 {
		if err := l.Record(ctx, entry("k1", 0.25, true)); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	keys, err := l.Keys(ctx)
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d keys, want 1", len(keys))
	}
	got := keys[0]
	if got.Requests != 3 || got.BillableRequests != 3 {
		t.Errorf("requests = %d/%d, want 3/3", got.Requests, got.BillableRequests)
	}
	if got.InputTokens != 300 || got.OutputTokens != 150 {
		t.Errorf("tokens = %d/%d, want 300/150", got.InputTokens, got.OutputTokens)
	}
	if got.CacheReadTokens != 30 || got.CacheWriteTokens != 15 {
		t.Errorf("cache tokens = %d/%d, want 30/15", got.CacheReadTokens, got.CacheWriteTokens)
	}
	if got.Cost < 0.749 || got.Cost > 0.751 {
		t.Errorf("cost = %v, want ~0.75", got.Cost)
	}
	if got.Alias != "alias-k1" {
		t.Errorf("alias = %q", got.Alias)
	}
}

// Passthrough traffic bills the caller's own subscription, so it must record
// usage without inventing a cost for the operator.
func TestNonBillableRecordsUsageButNoCost(t *testing.T) {
	ctx := context.Background()
	l := New()
	if err := l.Record(ctx, entry("k1", 0, false)); err != nil {
		t.Fatalf("Record: %v", err)
	}

	keys, _ := l.Keys(ctx)
	got := keys[0]
	if got.Requests != 1 {
		t.Errorf("requests = %d, want 1: usage must still be recorded", got.Requests)
	}
	if got.BillableRequests != 0 {
		t.Errorf("billable requests = %d, want 0", got.BillableRequests)
	}
	if got.Cost != 0 {
		t.Errorf("cost = %v, want 0", got.Cost)
	}
	if got.InputTokens != 100 {
		t.Errorf("input tokens = %d, want 100", got.InputTokens)
	}
}

func TestKeySpendWindowRollsOver(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		l := New()
		const window = time.Hour

		l.Record(ctx, entry("k1", 5, true))
		spent, err := l.KeySpend(ctx, "k1", window)
		if err != nil {
			t.Fatalf("KeySpend: %v", err)
		}
		if spent != 5 {
			t.Fatalf("spent = %v, want 5", spent)
		}

		// Still within the window.
		time.Sleep(window - time.Minute)
		if spent, _ := l.KeySpend(ctx, "k1", window); spent != 5 {
			t.Fatalf("spent = %v, want 5 before the window rolls", spent)
		}

		// Past it: the budget starts fresh.
		time.Sleep(2 * time.Minute)
		if spent, _ := l.KeySpend(ctx, "k1", window); spent != 0 {
			t.Errorf("spent = %v, want 0 after the window rolls over", spent)
		}
	})
}

func TestKeySpendLifetimeWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		l := New()
		l.Record(ctx, entry("k1", 2, true))
		time.Sleep(1000 * time.Hour)
		// A zero window means the key's whole lifetime; nothing resets.
		if spent, _ := l.KeySpend(ctx, "k1", 0); spent != 2 {
			t.Errorf("spent = %v, want 2 with no window", spent)
		}
	})
}

func TestUnknownKeyHasNoSpend(t *testing.T) {
	spent, err := New().KeySpend(context.Background(), "never-seen", time.Hour)
	if err != nil || spent != 0 {
		t.Errorf("got %v, %v; want 0, nil", spent, err)
	}
}

func TestFilePersistenceRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nested", "spend.json")

	l, err := NewFileLedger(path)
	if err != nil {
		t.Fatalf("NewFileLedger: %v", err)
	}
	l.Record(ctx, entry("k1", 1.5, true))
	if err := l.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %v, want 0600", perm)
	}

	// Budgets must survive a restart, or one would hand every key a fresh
	// allowance on every deploy.
	reopened, err := NewFileLedger(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	spent, _ := reopened.KeySpend(ctx, "k1", 0)
	if spent != 1.5 {
		t.Errorf("spend after restart = %v, want 1.5", spent)
	}

	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "alias-k1") == false {
		t.Error("alias was not persisted")
	}
}

func TestForgetDropsKey(t *testing.T) {
	ctx := context.Background()
	l := New()
	l.Record(ctx, entry("k1", 1, true))
	l.Forget("k1")
	if spent, _ := l.KeySpend(ctx, "k1", 0); spent != 0 {
		t.Errorf("spend = %v, want 0 after Forget", spent)
	}
	keys, _ := l.Keys(ctx)
	if len(keys) != 0 {
		t.Errorf("got %d keys, want 0", len(keys))
	}
}

func TestPricingCost(t *testing.T) {
	p := core.Pricing{InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.30, CacheWritePer1M: 3.75}
	u := core.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 1_000_000, CacheWriteTokens: 1_000_000}
	if got := p.Cost(u); got != 22.05 {
		t.Errorf("Cost = %v, want 22.05", got)
	}
	if !(core.Pricing{}).Zero() {
		t.Error("empty pricing should report Zero")
	}
	if (core.Pricing{}).Cost(u) != 0 {
		t.Error("unpriced deployment should cost 0")
	}
	// Cache reads are an order of magnitude cheaper than input; a cost model
	// that ignored them would be badly wrong on Claude Code traffic.
	cacheHeavy := core.Usage{InputTokens: 1000, CacheReadTokens: 200_000}
	if got := p.Cost(cacheHeavy); got > 0.07 {
		t.Errorf("cache-heavy cost = %v, unexpectedly high", got)
	}
}

func TestConcurrentRecord(t *testing.T) {
	ctx := context.Background()
	l := New()
	done := make(chan struct{})
	for range 50 {
		go func() {
			for range 20 {
				l.Record(ctx, entry("shared", 0.01, true))
			}
			done <- struct{}{}
		}()
	}
	for range 50 {
		<-done
	}
	keys, _ := l.Keys(ctx)
	if keys[0].Requests != 1000 {
		t.Errorf("requests = %d, want 1000", keys[0].Requests)
	}
}

// Savings are net of the prompt cache's write premium, so an entry can report a
// negative one: a request that wrote a cache and read nothing back cost more
// than it would have uncached. The ledger has to carry that sign rather than
// treat it as an absent value, since a run of them summing towards zero is the
// report that caching is not paying for itself.
func TestSavingsAccumulateWithTheirSign(t *testing.T) {
	ctx := context.Background()
	l := New()

	for _, savings := range []float64{2.50, -0.75, -0.25, 1.00} {
		e := entry("k1", 0.10, true)
		e.CacheSavings = savings
		if err := l.Record(ctx, e); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	rows, err := l.Keys(ctx)
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if got := rows[0].CacheSavings; got < 2.4999 || got > 2.5001 {
		t.Errorf("cache savings = %v, want 2.50", got)
	}
}

// A workload that only ever writes caches reports a negative total, which is the
// whole point of the figure being net: it says prompt caching is costing this
// operator money, and no other number in the ledger does.
func TestAWorkloadThatOnlyWritesReportsALoss(t *testing.T) {
	ctx := context.Background()
	l := New()

	for range 4 {
		e := entry("k1", 0.10, true)
		e.CacheSavings = -0.30
		if err := l.Record(ctx, e); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	rows, _ := l.Keys(ctx)
	if got := rows[0].CacheSavings; got > -1.19 || got < -1.21 {
		t.Errorf("cache savings = %v, want about -1.20", got)
	}
	if rows[0].Cost <= 0 {
		t.Error("cost must stay positive: the request was still charged for")
	}
}

// Non-billable traffic invents no savings for the same reason it invents no
// cost. A passthrough deployment's cache activity was paid for by the caller's
// own subscription.
func TestNonBillableRecordsNoSavings(t *testing.T) {
	ctx := context.Background()
	l := New()

	e := entry("k1", 0, false)
	e.CacheSavings = 9.99
	if err := l.Record(ctx, e); err != nil {
		t.Fatalf("Record: %v", err)
	}

	rows, _ := l.Keys(ctx)
	if rows[0].CacheSavings != 0 {
		t.Errorf("cache savings = %v, want 0 for passthrough traffic", rows[0].CacheSavings)
	}
	if rows[0].CacheReadTokens == 0 {
		t.Error("usage must still be recorded, only the money is not")
	}
}

// TestFilePersistenceCarriesEveryFigure is the restart the budget depends on.
//
// The round-trip test above proves cost survives. Everything else in Totals has
// to as well: a reopened ledger that lost its cache counters would report a
// deployment as having done no caching, and one that lost the sign on savings
// would turn a loss back into nothing.
func TestFilePersistenceCarriesEveryFigure(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "spend.json")

	l, err := NewFileLedger(path)
	if err != nil {
		t.Fatalf("NewFileLedger: %v", err)
	}
	e := entry("k1", 1.25, true)
	e.CacheSavings = -0.5
	if err := l.Record(ctx, e); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := l.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	before, err := l.Keys(ctx)
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	reopened, err := NewFileLedger(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	after, err := reopened.Keys(ctx)
	if err != nil {
		t.Fatalf("Keys after restart: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("got %d rows after restart, want 1", len(after))
	}
	// Compared with the window start set aside: it survives as a wall-clock
	// instant, and a reloaded one carries no monotonic reading for == to match.
	if !before[0].WindowStart.Equal(after[0].WindowStart) {
		t.Errorf("window start = %v after restart, want %v", after[0].WindowStart, before[0].WindowStart)
	}
	beforeTotals, afterTotals := before[0].Totals, after[0].Totals
	beforeTotals.WindowStart, afterTotals.WindowStart = time.Time{}, time.Time{}
	if beforeTotals != afterTotals {
		t.Errorf("totals changed across a restart:\nbefore %+v\nafter  %+v", beforeTotals, afterTotals)
	}
	if after[0].CacheSavings != -0.5 {
		t.Errorf("cache savings = %v after restart, want -0.5", after[0].CacheSavings)
	}

	// Deployment rows are persisted too, or /spend would report an empty fleet
	// after every deploy while keys still showed their spend.
	deployments, err := reopened.Deployments(ctx)
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	if len(deployments) != 1 || deployments[0].Subject != "dep-1" {
		t.Errorf("deployment rows after restart = %+v, want one for dep-1", deployments)
	}
}
