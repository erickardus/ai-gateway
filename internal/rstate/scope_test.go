package rstate

import (
	"context"
	"testing"
	"time"

	"github.com/erickardus/ai-gateway/internal/limiter"
	"github.com/erickardus/ai-gateway/internal/spend"
)

// A team's budget must be one pool across the fleet, not one per replica. It is
// the same failure the per-key limits had — an allowance silently multiplied by
// the instance count — and it is worse here, because the whole reason a scope
// exists is to be a cap several callers share.
func TestScopeSpendSharedAcrossInstances(t *testing.T) {
	ctx := context.Background()
	prefix := uniquePrefix(t)
	sa, sb := newStore(t, prefix), newStore(t, prefix)
	la := NewLedger(sa, spend.New(), prefix, discard(), 2*time.Second)
	lb := NewLedger(sb, spend.New(), prefix, discard(), 2*time.Second)

	const subject = "team:acme/platform"
	// One member's request lands on the first instance.
	if err := la.Record(ctx, spend.Entry{
		KeyHash: "key-a", Cost: 3, Billable: true,
		Scopes: []string{subject}, ScopeAliases: []string{"Platform"},
	}); err != nil {
		t.Fatalf("Record on instance A: %v", err)
	}
	// Another member's lands on the second.
	if err := lb.Record(ctx, spend.Entry{
		KeyHash: "key-b", Cost: 4, Billable: true, Scopes: []string{subject},
	}); err != nil {
		t.Fatalf("Record on instance B: %v", err)
	}

	for name, l := range map[string]*Ledger{"A": la, "B": lb} {
		got, err := l.ScopeSpend(ctx, subject, 0)
		if err != nil {
			t.Fatalf("ScopeSpend on instance %s: %v", name, err)
		}
		if got != 7 {
			t.Errorf("instance %s sees %v of pooled spend, want 7 — both members' traffic", name, got)
		}
	}
}

// A scope's totals live in their own keyspace, so a scope whose id happens to
// look like a key hash cannot draw on that key's window, and neither shows up
// in the other's report.
func TestScopeAndKeySpendDoNotShareAKeyspace(t *testing.T) {
	ctx := context.Background()
	prefix := uniquePrefix(t)
	l := NewLedger(newStore(t, prefix), spend.New(), prefix, discard(), 2*time.Second)

	const collide = "acme/platform"
	if err := l.Record(ctx, spend.Entry{KeyHash: collide, Cost: 5, Billable: true}); err != nil {
		t.Fatalf("Record key: %v", err)
	}
	if err := l.Record(ctx, spend.Entry{
		KeyHash: "other", Cost: 2, Billable: true, Scopes: []string{collide},
	}); err != nil {
		t.Fatalf("Record scope: %v", err)
	}

	keySpend, err := l.KeySpend(ctx, collide, 0)
	if err != nil {
		t.Fatalf("KeySpend: %v", err)
	}
	scopeSpend, err := l.ScopeSpend(ctx, collide, 0)
	if err != nil {
		t.Fatalf("ScopeSpend: %v", err)
	}
	if keySpend != 5 {
		t.Errorf("key spend = %v, want 5 — the scope's writes must not land on it", keySpend)
	}
	if scopeSpend != 2 {
		t.Errorf("scope spend = %v, want 2 — the key's writes must not land on it", scopeSpend)
	}

	// And the reports stay separate.
	keys, err := l.Keys(ctx)
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	for _, row := range keys {
		if row.Subject == collide && row.Cost != 5 {
			t.Errorf("key report for %q = %v, want 5", row.Subject, row.Cost)
		}
	}
	scopes, err := l.Scopes(ctx)
	if err != nil {
		t.Fatalf("Scopes: %v", err)
	}
	if len(scopes) != 1 || scopes[0].Subject != collide || scopes[0].Cost != 2 {
		t.Errorf("scope report = %+v, want one row for %q costing 2", scopes, collide)
	}
}

// A pooled window resets on the first request after it elapses, the same way a
// key's does, so a monthly team budget starts fresh rather than never.
func TestScopeBudgetWindowRolls(t *testing.T) {
	ctx := context.Background()
	prefix := uniquePrefix(t)
	l := NewLedger(newStore(t, prefix), spend.New(), prefix, discard(), 2*time.Second)

	const subject = "team:acme/platform"
	if err := l.Record(ctx, spend.Entry{KeyHash: "k", Cost: 9, Billable: true, Scopes: []string{subject}}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got, err := l.ScopeSpend(ctx, subject, time.Hour); err != nil || got != 9 {
		t.Fatalf("ScopeSpend inside the window = %v, %v; want 9", got, err)
	}
	// A window already elapsed by the time it is read.
	got, err := l.ScopeSpend(ctx, subject, time.Nanosecond)
	if err != nil {
		t.Fatalf("ScopeSpend: %v", err)
	}
	if got != 0 {
		t.Errorf("ScopeSpend after the window elapsed = %v, want 0", got)
	}
}

// The chain is reserved atomically across the fleet: a request the team refuses
// leaves no increment on the key's own shared window either.
func TestReserveAllIsAtomicAcrossInstances(t *testing.T) {
	ctx := context.Background()
	prefix := uniquePrefix(t)
	a := newStore(t, prefix).KeyLimiter()
	b := newStore(t, prefix).KeyLimiter()

	claims := func() []limiter.Claim {
		return []limiter.Claim{
			{Subject: "key-hash", RPM: 100},
			{Subject: "team:acme/platform", RPM: 1},
		}
	}

	// One instance takes the team's only slot.
	if got, err := a.ReserveAll(ctx, claims()); err != nil || got != -1 {
		t.Fatalf("first reservation = %d, %v; want -1", got, err)
	}
	// The other instance sees it gone — the pool is shared.
	got, err := b.ReserveAll(ctx, claims())
	if err != nil {
		t.Fatalf("ReserveAll: %v", err)
	}
	if got != 1 {
		t.Fatalf("second instance = %d, want 1 (the team refused)", got)
	}

	// And the key's own window kept only the admitted request. Five refusals
	// must add nothing to it.
	for range 5 {
		if _, err := b.ReserveAll(ctx, claims()); err != nil {
			t.Fatalf("ReserveAll: %v", err)
		}
	}
	used, _ := a.Snapshot(ctx, "key-hash")
	if used != 1 {
		t.Errorf("the key's shared window is at %d, want 1: a refused request must charge nothing", used)
	}
}

// A chain declaring no limits anywhere is the common case, and it must not cost
// a round trip to discover that.
func TestReserveAllSkipsRedisWhenNothingIsLimited(t *testing.T) {
	ctx := context.Background()
	prefix := uniquePrefix(t)
	s := newStore(t, prefix)
	k := s.KeyLimiter()

	got, err := k.ReserveAll(ctx, []limiter.Claim{
		{Subject: "key-hash"}, {Subject: "team:acme/platform"},
	})
	if err != nil || got != -1 {
		t.Fatalf("ReserveAll with no limits = %d, %v; want -1", got, err)
	}
	if used, _ := k.Snapshot(ctx, "key-hash"); used != 0 {
		t.Errorf("an unlimited chain wrote a counter anyway: %d", used)
	}
}

// Every budget in the chain is read in one pipeline, and each subject keeps its
// own window.
func TestSpendsReadsTheChainTogether(t *testing.T) {
	ctx := context.Background()
	prefix := uniquePrefix(t)
	l := NewLedger(newStore(t, prefix), spend.New(), prefix, discard(), 2*time.Second)

	if err := l.Record(ctx, spend.Entry{
		KeyHash: "key-hash", Cost: 3, Billable: true,
		Scopes: []string{"team:acme/platform", "organization:acme"},
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, err := l.Spends(ctx, []spend.Subject{
		{Kind: spend.KindKey, ID: "key-hash", Window: time.Hour},
		{Kind: spend.KindScope, ID: "team:acme/platform", Window: time.Hour},
		{Kind: spend.KindScope, ID: "organization:acme"},
		{Kind: spend.KindScope, ID: "team:never-used"},
	})
	if err != nil {
		t.Fatalf("Spends: %v", err)
	}
	want := []float64{3, 3, 3, 0}
	if len(got) != len(want) {
		t.Fatalf("Spends returned %d values, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Spends[%d] = %v, want %v", i, got[i], want[i])
		}
	}

	// A window already elapsed reads as zero and clears, without disturbing the
	// subjects read alongside it.
	got, err = l.Spends(ctx, []spend.Subject{
		{Kind: spend.KindScope, ID: "team:acme/platform", Window: time.Nanosecond},
		{Kind: spend.KindScope, ID: "organization:acme", Window: time.Hour},
	})
	if err != nil {
		t.Fatalf("Spends: %v", err)
	}
	if got[0] != 0 {
		t.Errorf("the elapsed window = %v, want 0", got[0])
	}
	if got[1] != 3 {
		t.Errorf("a subject read alongside it = %v, want 3 — its own window has not elapsed", got[1])
	}
}
