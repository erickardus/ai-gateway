package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/provider"
)

// The usage-based strategy balances on tokens consumed rather than requests
// served, which is the difference between spreading load and spreading cost: one
// deployment answering a few enormous requests is busier, in every sense the
// operator is billed for, than another answering many small ones.

func usageRouter(t *testing.T, deployments int, exec Executor, state StateStore) (*Router, []*config.Deployment) {
	t.Helper()
	return buildRouter(t, "g", make([]int, deployments), config.RouterConfig{Strategy: config.StrategyUsageBased}, exec, state)
}

func TestUsageBasedPicksTheLeastConsumed(t *testing.T) {
	exec := &fakeExec{replies: map[string]error{}}
	state := NewMemState()
	r, deps := usageRouter(t, 3, exec, state)
	ctx := context.Background()

	// Two deployments have carried real traffic; the third has not.
	if err := state.AddTokens(ctx, deps[0].ID(), 50_000); err != nil {
		t.Fatalf("AddTokens: %v", err)
	}
	if err := state.AddTokens(ctx, deps[1].ID(), 10_000); err != nil {
		t.Fatalf("AddTokens: %v", err)
	}

	res, err := r.Route(ctx, "g", &provider.Request{}, Overrides{})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	res.Response.Body.Close()
	if got := res.Deployment.ID(); got != deps[2].ID() {
		t.Errorf("routed to %s, want the idle %s", got, deps[2].ID())
	}
}

// The tokens a request consumes have to reach the strategy, or it balances on a
// number that never changes and every request lands in the same place.
func TestUsageRecordedByARequestSteersTheNextOne(t *testing.T) {
	exec := &fakeExec{replies: map[string]error{}}
	state := NewMemState()
	r, _ := usageRouter(t, 2, exec, state)
	ctx := context.Background()

	first, err := r.Route(ctx, "g", &provider.Request{}, Overrides{})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	first.Response.Body.Close()

	// A large reply on whichever deployment served it.
	r.RecordUsage(ctx, first.Deployment, core.Usage{
		InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 20_000, CacheWriteTokens: 2_000,
	})

	second, err := r.Route(ctx, "g", &provider.Request{}, Overrides{})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	second.Response.Body.Close()
	if second.Deployment.ID() == first.Deployment.ID() {
		t.Errorf("both requests went to %s; the tokens the first consumed did not reach the strategy",
			first.Deployment.ID())
	}

	// And the figure it steered on is every token the reply reported, cache
	// included: those are billed, so a strategy ignoring them would call a
	// deployment idle while it ran up the largest share of the invoice.
	used, err := state.TokensUsed(ctx, first.Deployment.ID())
	if err != nil {
		t.Fatalf("TokensUsed: %v", err)
	}
	if used != 23_500 {
		t.Errorf("tokens recorded = %d, want 23500 (input, output, cache read and cache write)", used)
	}
}

// A reply that reported nothing must not be recorded as usage, or a deployment
// answering unmeasured traffic would look permanently idle and attract all of
// it. That is the shape of a streamed OpenAI-compatible reply nobody asked for
// usage on.
func TestUnmeasuredRepliesAreNotRecordedAsIdle(t *testing.T) {
	exec := &fakeExec{replies: map[string]error{}}
	state := NewMemState()
	r, deps := usageRouter(t, 2, exec, state)
	ctx := context.Background()

	r.RecordUsage(ctx, deps[0], core.Usage{})

	used, err := state.TokensUsed(ctx, deps[0].ID())
	if err != nil {
		t.Fatalf("TokensUsed: %v", err)
	}
	if used != 0 {
		t.Errorf("tokens recorded = %d from an empty usage report, want 0", used)
	}
	// Nor may a nil deployment panic the accounting path, which runs after the
	// response has already been relayed.
	r.RecordUsage(ctx, nil, core.Usage{InputTokens: 10})
}

// With nothing consumed anywhere the strategy must still spread traffic. Ties
// broken by list order would send every request of a freshly started gateway to
// the first deployment.
func TestUsageBasedSpreadsAColdFleet(t *testing.T) {
	exec := &fakeExec{replies: map[string]error{}}
	r, _ := usageRouter(t, 3, exec, NewMemState())
	ctx := context.Background()

	seen := map[string]int{}
	for range 60 {
		res, err := r.Route(ctx, "g", &provider.Request{}, Overrides{})
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		res.Response.Body.Close()
		seen[res.Deployment.ID()]++
	}
	if len(seen) != 3 {
		t.Errorf("a cold fleet sent traffic to %d of 3 deployments: %v", len(seen), seen)
	}
}

// reserveRefuser reports capacity in every check the candidate filter makes, and
// then refuses the reservation for one deployment. That is what losing a race
// for the last slot looks like from the router's side: the deployment was
// admissible when it was chosen and full by the time it was claimed.
type reserveRefuser struct {
	StateStore
	refuse string
}

func (r *reserveRefuser) Reserve(ctx context.Context, id string, rpm, tpm int) (bool, error) {
	if id == r.refuse {
		return false, nil
	}
	return r.StateStore.Reserve(ctx, id, rpm, tpm)
}

// A deployment that fills between being chosen and being claimed must cost the
// request another candidate, not the request itself.
func TestARaceForTheLastSlotFallsToAnotherDeployment(t *testing.T) {
	exec := &fakeExec{replies: map[string]error{}}
	base := NewMemState()
	refuser := &reserveRefuser{StateStore: base}
	r, deps := buildRouter(t, "g", []int{1, 1}, config.RouterConfig{Strategy: config.StrategyWeightedShuffle}, exec, refuser)
	// Deployment IDs are derived once the config is finalized, so the one to
	// refuse is only knowable after the router exists.
	full := deps[0].ID()
	refuser.refuse = full
	// A deployment with no limits never consults the store, so the race being
	// modelled only exists where capacity is configured at all.
	for _, d := range deps {
		d.RPM = 100
	}

	res, err := r.Route(context.Background(), "g", &provider.Request{}, Overrides{})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	res.Response.Body.Close()
	if res.Deployment.ID() == full {
		t.Errorf("routed to %s, which refused the reservation", full)
	}
}

// A pinned deployment that fills is passed over the same way: the pin is a
// preference, and losing the race is one more reason it cannot be honoured.
func TestAPinnedDeploymentThatFillsIsPassedOver(t *testing.T) {
	exec := &fakeExec{replies: map[string]error{}}
	base := NewMemState()
	ctx := context.Background()

	refuser := &reserveRefuser{StateStore: base}
	r, deps := buildAffinityRouter(t, []int{1, 1}, config.PromptCacheConfig{}, 0, exec, refuser)
	full := deps[0].ID()
	refuser.refuse = full
	for _, d := range deps {
		d.RPM = 100
	}
	if err := base.SetAffinity(ctx, "g\x00prefix-a", full, time.Minute); err != nil {
		t.Fatalf("SetAffinity: %v", err)
	}

	res, err := r.Route(ctx, "g", &provider.Request{}, Overrides{PromptPrefix: "prefix-a"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	res.Response.Body.Close()
	if res.Deployment.ID() == full {
		t.Errorf("routed to the pinned %s, which refused the reservation", full)
	}
	if res.PromptAffinity != AffinityMiss {
		t.Errorf("affinity = %q, want miss: the pin existed and something else served the request", res.PromptAffinity)
	}
}

// When every candidate fills, the request is refused rather than served by a
// deployment with no capacity for it.
func TestNoCapacityAnywhereIsRefused(t *testing.T) {
	exec := &fakeExec{replies: map[string]error{}}
	refuser := &reserveRefuser{StateStore: NewMemState()}
	r, deps := buildRouter(t, "g", []int{1}, config.RouterConfig{Strategy: config.StrategyWeightedShuffle}, exec, refuser)
	refuser.refuse = deps[0].ID()
	deps[0].RPM = 100

	_, err := r.Route(context.Background(), "g", &provider.Request{}, Overrides{})
	if !errors.Is(err, core.ErrNoHealthyDeployment) {
		t.Errorf("err = %v, want ErrNoHealthyDeployment", err)
	}
}

// Every configured strategy has to name itself and route, because /health
// reports the name and an operator reads it to confirm the gateway is balancing
// the way they configured it. A strategy that routed but reported the wrong name
// would send them looking in the wrong place for a load problem.
func TestEveryStrategyNamesItselfAndRoutes(t *testing.T) {
	for _, name := range config.KnownStrategies {
		t.Run(name, func(t *testing.T) {
			exec := &fakeExec{replies: map[string]error{}}
			r, _ := buildRouter(t, "g", []int{1, 1, 1},
				config.RouterConfig{Strategy: name}, exec, NewMemState())

			if got := r.strategy.Name(); got != name {
				t.Errorf("strategy reports %q, configured as %q", got, name)
			}

			res, err := r.Route(context.Background(), "g", &provider.Request{}, Overrides{})
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			res.Response.Body.Close()
			if res.Deployment == nil {
				t.Fatal("routed to no deployment")
			}
		})
	}
}
