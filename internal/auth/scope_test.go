package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/spend"
)

// testHierarchy builds acme → platform → gateway, letting the caller adjust any
// level before the parents are wired.
func testHierarchy(t *testing.T, adjust func(org, team, project *core.Scope)) map[string]*core.Scope {
	t.Helper()
	org := &core.Scope{ID: "acme", Kind: core.ScopeOrganization}
	team := &core.Scope{ID: "acme/platform", Kind: core.ScopeTeam}
	project := &core.Scope{ID: "acme/platform/gateway", Kind: core.ScopeProject}
	if adjust != nil {
		adjust(org, team, project)
	}
	team.Parent, project.Parent = org, team
	return map[string]*core.Scope{org.ID: org, team.ID: team, project.ID: project}
}

// authenticatorWithScopes builds an Authenticator holding the named keys, each
// attached to a scope.
func authenticatorWithScopes(t *testing.T, scopes map[string]*core.Scope, keys ...config.KeySpec) *Authenticator {
	t.Helper()
	a, err := NewAuthenticator(context.Background(), NewMemStore(), config.VirtualKeysConfig{
		HeaderNames: []string{"x-gateway-key"},
		Keys:        keys,
	}, scopes)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	return a
}

// A team budget is a pool, which is the whole reason the hierarchy exists. Two
// developers on one team draw down one cap: what the first spends is subtracted
// from what the second may spend, where two keys carrying the same role would
// each have received a private copy of it.
func TestTeamBudgetIsSharedBetweenItsKeys(t *testing.T) {
	ctx := context.Background()
	scopes := testHierarchy(t, func(_, team, _ *core.Scope) {
		team.MaxBudget = 10
	})
	a := authenticatorWithScopes(t, scopes,
		config.KeySpec{Key: "sk-vk-alice", Alias: "alice", Scope: "acme/platform"},
		config.KeySpec{Key: "sk-vk-bob", Alias: "bob", Scope: "acme/platform"},
	)
	ledger := spend.New()

	alice := authContext(t, a, "sk-vk-alice")
	bob := authContext(t, a, "sk-vk-bob")

	// Neither has spent anything, so both may proceed.
	for _, ac := range []*Context{alice, bob} {
		if err := a.CheckBudget(ctx, ac, ledger); err != nil {
			t.Fatalf("CheckBudget(%s) before any spend: %v", ac.Key.Alias, err)
		}
	}

	// Alice exhausts the team's budget. Her own key has no cap of its own, so
	// nothing but the pool can refuse anyone.
	if err := ledger.Record(ctx, spend.Entry{
		KeyHash: alice.Key.Hash, Cost: 11, Billable: true,
		Scopes: alice.ScopeSubjects(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if err := a.CheckBudget(ctx, alice, ledger); !errors.Is(err, core.ErrBudgetExceeded) {
		t.Fatalf("CheckBudget(alice) after overspending: got %v, want ErrBudgetExceeded", err)
	}
	// The point of the test: Bob spent nothing, and is refused anyway.
	err := a.CheckBudget(ctx, bob, ledger)
	if !errors.Is(err, core.ErrBudgetExceeded) {
		t.Fatalf("CheckBudget(bob) after alice overspent: got %v, want ErrBudgetExceeded", err)
	}
	if !strings.Contains(err.Error(), "acme/platform") {
		t.Errorf("error should name the team whose pool refused the request, got %q", err)
	}
}

// A key outside the hierarchy is unaffected by anyone's pool, so adding a team
// does not quietly cap the keys that were never put in one.
func TestUnscopedKeyIgnoresPools(t *testing.T) {
	ctx := context.Background()
	scopes := testHierarchy(t, func(_, team, _ *core.Scope) { team.MaxBudget = 10 })
	a := authenticatorWithScopes(t, scopes,
		config.KeySpec{Key: "sk-vk-scoped", Alias: "scoped", Scope: "acme/platform"},
		config.KeySpec{Key: "sk-vk-loose", Alias: "loose"},
	)
	ledger := spend.New()

	scoped := authContext(t, a, "sk-vk-scoped")
	loose := authContext(t, a, "sk-vk-loose")
	if loose.Scope != nil {
		t.Fatalf("a key naming no scope should resolve to none, got %v", loose.Scope)
	}
	if err := ledger.Record(ctx, spend.Entry{
		KeyHash: scoped.Key.Hash, Cost: 50, Billable: true, Scopes: scoped.ScopeSubjects(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := a.CheckBudget(ctx, loose, ledger); err != nil {
		t.Fatalf("an unscoped key must not be charged against a pool it is not in: %v", err)
	}
}

// The innermost cap that binds is the one reported, so the message names the
// level whose owner can act on it.
func TestBudgetRefusalNamesTheInnermostScope(t *testing.T) {
	ctx := context.Background()
	scopes := testHierarchy(t, func(org, team, project *core.Scope) {
		org.MaxBudget = 100
		team.MaxBudget = 50
		project.MaxBudget = 5
	})
	a := authenticatorWithScopes(t, scopes,
		config.KeySpec{Key: "sk-vk-dev", Alias: "dev", Scope: "acme/platform/gateway"})
	ledger := spend.New()
	ac := authContext(t, a, "sk-vk-dev")

	if err := ledger.Record(ctx, spend.Entry{
		KeyHash: ac.Key.Hash, Cost: 6, Billable: true, Scopes: ac.ScopeSubjects(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	err := a.CheckBudget(ctx, ac, ledger)
	if !errors.Is(err, core.ErrBudgetExceeded) {
		t.Fatalf("got %v, want ErrBudgetExceeded", err)
	}
	if !strings.Contains(err.Error(), "acme/platform/gateway") {
		t.Errorf("want the project named, got %q", err)
	}
	if strings.Contains(err.Error(), "shared budget") && strings.Contains(err.Error(), "50.0000") {
		t.Errorf("the team's cap has not been reached and should not be reported: %q", err)
	}
}

// Every level records the request, so an organisation's cap is reached by the
// traffic of every team beneath it and not only by keys attached to it directly.
func TestSpendReachesEveryLevelOfTheChain(t *testing.T) {
	ctx := context.Background()
	scopes := testHierarchy(t, func(org, _, _ *core.Scope) { org.MaxBudget = 10 })
	a := authenticatorWithScopes(t, scopes,
		config.KeySpec{Key: "sk-vk-dev", Alias: "dev", Scope: "acme/platform/gateway"})
	ledger := spend.New()
	ac := authContext(t, a, "sk-vk-dev")

	subjects := ac.ScopeSubjects()
	want := []string{"project:acme/platform/gateway", "team:acme/platform", "organization:acme"}
	if len(subjects) != len(want) {
		t.Fatalf("ScopeSubjects() = %v, want %v", subjects, want)
	}
	for i := range want {
		if subjects[i] != want[i] {
			t.Fatalf("ScopeSubjects()[%d] = %q, want %q (innermost first)", i, subjects[i], want[i])
		}
	}

	if err := ledger.Record(ctx, spend.Entry{
		KeyHash: ac.Key.Hash, Cost: 11, Billable: true, Scopes: subjects,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// The project and team declare no cap; the organisation two levels up does.
	err := a.CheckBudget(ctx, ac, ledger)
	if !errors.Is(err, core.ErrBudgetExceeded) {
		t.Fatalf("got %v, want the organisation's cap to bind: %v", err, err)
	}
	if !strings.Contains(err.Error(), "organization") {
		t.Errorf("want the organisation named, got %q", err)
	}
}

// A key cannot widen its scope: the chain is an intersection, so listing a
// model the team withholds grants nothing.
func TestScopeNarrowsWhatAKeyMayCall(t *testing.T) {
	scopes := testHierarchy(t, func(org, team, _ *core.Scope) {
		org.Models = []string{"anthropic-claude", "anthropic-claude-haiku"}
		team.Models = []string{"anthropic-claude-haiku"}
	})
	a := authenticatorWithScopes(t, scopes, config.KeySpec{
		Key: "sk-vk-dev", Alias: "dev", Scope: "acme/platform",
		// The key asks for more than its team allows.
		Models: []string{"anthropic-claude", "anthropic-claude-haiku"},
	})
	ac := authContext(t, a, "sk-vk-dev")

	if err := a.AuthorizeModel(ac, "anthropic-claude-haiku"); err != nil {
		t.Fatalf("the model both levels allow should be permitted: %v", err)
	}
	err := a.AuthorizeModel(ac, "anthropic-claude")
	if !errors.Is(err, core.ErrModelNotAllowed) {
		t.Fatalf("got %v, want ErrModelNotAllowed", err)
	}
	if !strings.Contains(err.Error(), "acme/platform") {
		t.Errorf("the error should name the scope that withheld the model, got %q", err)
	}

	// A scope naming no models abstains rather than granting everything: the
	// organisation above still binds.
	if err := a.AuthorizeModel(ac, "gpt"); !errors.Is(err, core.ErrModelNotAllowed) {
		t.Errorf("a model no level lists must stay refused, got %v", err)
	}
}

// Blocking a team blocks every key beneath it at once, which is what suspending
// a team has to mean when its members hold keys the operator never saw.
func TestBlockingAScopeBlocksItsKeys(t *testing.T) {
	scopes := testHierarchy(t, func(_, team, _ *core.Scope) { team.Blocked = true })
	a := authenticatorWithScopes(t, scopes,
		config.KeySpec{Key: "sk-vk-under", Alias: "under", Scope: "acme/platform/gateway"},
		config.KeySpec{Key: "sk-vk-beside", Alias: "beside", Scope: "acme"},
	)
	if _, err := a.Authenticate(context.Background(), header("sk-vk-under")); !errors.Is(err, core.ErrKeyBlocked) {
		t.Fatalf("a key under a blocked team: got %v, want ErrKeyBlocked", err)
	}
	// A sibling elsewhere in the hierarchy is unaffected.
	if _, err := a.Authenticate(context.Background(), header("sk-vk-beside")); err != nil {
		t.Fatalf("a key outside the blocked team should still authenticate: %v", err)
	}
}

// A key naming a scope that is not configured is refused rather than treated as
// unscoped. The key store outlives the config that produced it, so the failure
// mode being avoided is a renamed team silently dropping the cap its members
// were issued under.
func TestKeyNamingAnUnknownScopeIsRefused(t *testing.T) {
	a := authenticatorWithScopes(t, testHierarchy(t, nil),
		config.KeySpec{Key: "sk-vk-orphan", Alias: "orphan", Scope: "acme/retired"})
	_, err := a.Authenticate(context.Background(), header("sk-vk-orphan"))
	if !errors.Is(err, core.ErrKeyInvalid) {
		t.Fatalf("got %v, want ErrKeyInvalid", err)
	}
	if !strings.Contains(err.Error(), "acme/retired") {
		t.Errorf("the error should name the missing scope, got %q", err)
	}
}

// A scope's rpm is one allowance its members share, not one each.
func TestScopeRateLimitIsShared(t *testing.T) {
	ctx := context.Background()
	scopes := testHierarchy(t, func(_, team, _ *core.Scope) { team.RPMLimit = 2 })
	a := authenticatorWithScopes(t, scopes,
		config.KeySpec{Key: "sk-vk-alice", Alias: "alice", Scope: "acme/platform"},
		config.KeySpec{Key: "sk-vk-bob", Alias: "bob", Scope: "acme/platform"},
	)
	alice := authContext(t, a, "sk-vk-alice")
	bob := authContext(t, a, "sk-vk-bob")

	if err := a.Admit(ctx, alice); err != nil {
		t.Fatalf("alice's first request: %v", err)
	}
	if err := a.Admit(ctx, bob); err != nil {
		t.Fatalf("bob's first request, within the team's allowance of 2: %v", err)
	}
	// The third request across both keys exceeds the shared allowance.
	err := a.Admit(ctx, alice)
	if !errors.Is(err, core.ErrRateLimited) {
		t.Fatalf("got %v, want ErrRateLimited once the team's shared rpm is spent", err)
	}
	if !strings.Contains(err.Error(), "acme/platform") {
		t.Errorf("the error should name the scope holding the limit, got %q", err)
	}
}

// A scope's tpm is fed by every member's tokens, or the shared allowance would
// admit requests it never counted.
func TestScopeTokensAccumulateAcrossKeys(t *testing.T) {
	ctx := context.Background()
	scopes := testHierarchy(t, func(_, team, _ *core.Scope) { team.TPMLimit = 100 })
	a := authenticatorWithScopes(t, scopes,
		config.KeySpec{Key: "sk-vk-alice", Alias: "alice", Scope: "acme/platform"},
		config.KeySpec{Key: "sk-vk-bob", Alias: "bob", Scope: "acme/platform"},
	)
	alice := authContext(t, a, "sk-vk-alice")
	bob := authContext(t, a, "sk-vk-bob")

	if err := a.Admit(ctx, alice); err != nil {
		t.Fatalf("alice: %v", err)
	}
	a.RecordTokens(ctx, alice, 150)

	// Bob has consumed nothing himself, but the team's token allowance is gone.
	if err := a.Admit(ctx, bob); !errors.Is(err, core.ErrRateLimited) {
		t.Fatalf("got %v, want bob refused by the team's exhausted tpm", err)
	}
}

// A request refused by an outer scope must leave no trace on the inner
// allowances it was checked against first.
//
// This is the correctness half of reserving the chain in one operation. Charging
// each level in turn would let a team's refusal keep the increment already made
// to the key's own window, so a key would be counted for requests the gateway
// never served — and a caller sitting against a team limit would burn their
// personal allowance doing nothing.
func TestRefusalByAnOuterScopeLeavesInnerCountersUntouched(t *testing.T) {
	ctx := context.Background()
	scopes := testHierarchy(t, func(_, team, _ *core.Scope) { team.RPMLimit = 1 })
	a := authenticatorWithScopes(t, scopes, config.KeySpec{
		Key: "sk-vk-dev", Alias: "dev", Scope: "acme/platform", RPMLimit: 100,
	})
	ac := authContext(t, a, "sk-vk-dev")

	local, ok := a.limits.(*LocalLimiter)
	if !ok {
		t.Fatalf("expected the default local limiter, got %T", a.limits)
	}

	if err := a.Admit(ctx, ac); err != nil {
		t.Fatalf("first request: %v", err)
	}
	used, _ := local.Snapshot(ac.Key.Hash)
	if used != 1 {
		t.Fatalf("after one admitted request the key has used %d, want 1", used)
	}

	// The team's allowance is spent. Several refusals must not move the key's
	// own counter at all.
	for i := range 5 {
		if err := a.Admit(ctx, ac); !errors.Is(err, core.ErrRateLimited) {
			t.Fatalf("refused request %d: got %v, want ErrRateLimited", i+1, err)
		}
	}
	used, _ = local.Snapshot(ac.Key.Hash)
	if used != 1 {
		t.Errorf("the key's window moved to %d across 5 refusals, want 1: a refused request must charge nothing", used)
	}
}

// The refusal names the level holding the limit, and the key's own limit is
// still reported as the key's.
func TestRateLimitRefusalNamesTheRightLevel(t *testing.T) {
	ctx := context.Background()

	t.Run("the key's own limit", func(t *testing.T) {
		scopes := testHierarchy(t, nil)
		a := authenticatorWithScopes(t, scopes, config.KeySpec{
			Key: "sk-vk-dev", Alias: "dev", Scope: "acme/platform", RPMLimit: 1,
		})
		ac := authContext(t, a, "sk-vk-dev")
		if err := a.Admit(ctx, ac); err != nil {
			t.Fatalf("first: %v", err)
		}
		err := a.Admit(ctx, ac)
		if !errors.Is(err, core.ErrRateLimited) {
			t.Fatalf("got %v, want ErrRateLimited", err)
		}
		if strings.Contains(err.Error(), "acme") {
			t.Errorf("a key's own limit should not be reported as a scope's: %q", err)
		}
	})

	t.Run("an organisation two levels up", func(t *testing.T) {
		scopes := testHierarchy(t, func(org, _, _ *core.Scope) { org.RPMLimit = 1 })
		a := authenticatorWithScopes(t, scopes, config.KeySpec{
			Key: "sk-vk-dev", Alias: "dev", Scope: "acme/platform/gateway",
		})
		ac := authContext(t, a, "sk-vk-dev")
		if err := a.Admit(ctx, ac); err != nil {
			t.Fatalf("first: %v", err)
		}
		err := a.Admit(ctx, ac)
		if !errors.Is(err, core.ErrRateLimited) {
			t.Fatalf("got %v, want ErrRateLimited", err)
		}
		if !strings.Contains(err.Error(), "organization \"acme\"") {
			t.Errorf("want the organisation named, got %q", err)
		}
	})
}

// countingLedger reports how many times the ledger was asked, so the batched
// read can be held to one call however deep the chain is.
type countingLedger struct {
	*spend.Ledger
	calls int
	last  []spend.Subject
}

func (l *countingLedger) Spends(ctx context.Context, subjects []spend.Subject) ([]float64, error) {
	l.calls++
	l.last = subjects
	return l.Ledger.Spends(ctx, subjects)
}

// Every budget in the chain is read in one call. Asking per level would be a
// round trip each on the hot path of every request.
func TestBudgetChainIsReadInOneCall(t *testing.T) {
	ctx := context.Background()
	scopes := testHierarchy(t, func(org, team, project *core.Scope) {
		org.MaxBudget, team.MaxBudget, project.MaxBudget = 100, 50, 10
	})
	a := authenticatorWithScopes(t, scopes, config.KeySpec{
		Key: "sk-vk-dev", Alias: "dev", Scope: "acme/platform/gateway",
		MaxBudget: 5, BudgetDuration: time.Hour,
	})
	ledger := &countingLedger{Ledger: spend.New()}
	ac := authContext(t, a, "sk-vk-dev")

	if err := a.CheckBudget(ctx, ac, ledger); err != nil {
		t.Fatalf("CheckBudget: %v", err)
	}
	if ledger.calls != 1 {
		t.Errorf("the ledger was asked %d times, want 1 for the whole chain", ledger.calls)
	}
	// The key plus three scopes, innermost first — the order the refusal
	// message depends on.
	want := []spend.Subject{
		{Kind: spend.KindKey, ID: ac.Key.SpendSubject(), Window: time.Hour},
		{Kind: spend.KindScope, ID: "project:acme/platform/gateway"},
		{Kind: spend.KindScope, ID: "team:acme/platform"},
		{Kind: spend.KindScope, ID: "organization:acme"},
	}
	if len(ledger.last) != len(want) {
		t.Fatalf("asked about %d subjects, want %d: %+v", len(ledger.last), len(want), ledger.last)
	}
	for i := range want {
		if ledger.last[i].ID != want[i].ID || ledger.last[i].Kind != want[i].Kind {
			t.Errorf("subject %d = %v %q, want %v %q", i,
				ledger.last[i].Kind, ledger.last[i].ID, want[i].Kind, want[i].ID)
		}
	}
}

// A gateway with no hierarchy and no key budget asks nothing at all.
func TestNoBudgetsAsksTheLedgerNothing(t *testing.T) {
	a := authenticatorWithScopes(t, nil, config.KeySpec{Key: "sk-vk-dev", Alias: "dev"})
	ledger := &countingLedger{Ledger: spend.New()}
	ac := authContext(t, a, "sk-vk-dev")

	if err := a.CheckBudget(context.Background(), ac, ledger); err != nil {
		t.Fatalf("CheckBudget: %v", err)
	}
	if ledger.calls != 0 {
		t.Errorf("the ledger was asked %d times, want 0 where nothing has a cap", ledger.calls)
	}
}
