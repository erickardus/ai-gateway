package router

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/provider"
)

func boolPtr(b bool) *bool { return &b }

// buildAffinityRouter is buildRouter with a prompt-cache configuration and
// optional per-deployment rate limits.
func buildAffinityRouter(t *testing.T, weights []int, pc config.PromptCacheConfig, rpm int, exec Executor, state StateStore) (*Router, []*config.Deployment) {
	t.Helper()
	r, deps := buildRouter(t, "g", weights, config.RouterConfig{Strategy: config.StrategyWeightedShuffle}, exec, state)
	r.prompt = pc
	if r.prompt.AffinityTTL == 0 {
		r.prompt.AffinityTTL = time.Minute
	}
	for _, d := range deps {
		d.RPM = rpm
	}
	return r, deps
}

func route(t *testing.T, r *Router, prefix string) *Result {
	t.Helper()
	res, err := r.Route(context.Background(), "g", &provider.Request{}, Overrides{PromptPrefix: prefix})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	res.Response.Body.Close()
	return res
}

// The point of the whole feature: a conversation must keep landing on the
// upstream already holding its prompt cache, instead of paying a cache write on
// every hop.
func TestAffinityPinsAConversationToOneDeployment(t *testing.T) {
	exec := &fakeExec{replies: map[string]error{}}
	r, _ := buildAffinityRouter(t, []int{1, 1, 1}, config.PromptCacheConfig{}, 0, exec, nil)

	first := route(t, r, "prefix-a")
	// A prefix nobody has served yet has no pin to honour. That is not a miss:
	// counting it as one would put every opening turn into the figure an
	// operator alerts on.
	if first.PromptAffinity != AffinityNew {
		t.Errorf("first request: affinity = %q, want %q", first.PromptAffinity, AffinityNew)
	}
	for i := range 40 {
		res := route(t, r, "prefix-a")
		if res.Deployment.ID() != first.Deployment.ID() {
			t.Fatalf("request %d went to %s, want the pinned %s", i, res.Deployment.ID(), first.Deployment.ID())
		}
		if res.PromptAffinity != AffinityHit {
			t.Fatalf("request %d: affinity = %q, want %q", i, res.PromptAffinity, AffinityHit)
		}
	}
}

// Pinning must not collapse into pinning everything to one upstream: distinct
// prefixes are still spread, or affinity would trade cache hits for a hot spot.
func TestAffinitySpreadsDistinctPrefixes(t *testing.T) {
	exec := &fakeExec{replies: map[string]error{}}
	r, _ := buildAffinityRouter(t, []int{1, 1, 1}, config.PromptCacheConfig{}, 0, exec, nil)

	seen := map[string]bool{}
	for i := range 60 {
		seen[route(t, r, string(rune('a'+i))).Deployment.ID()] = true
	}
	if len(seen) < 2 {
		t.Errorf("distinct prefixes all landed on one deployment: %v", seen)
	}
}

// A pin is a preference, never a constraint. A pinned deployment that has been
// ejected must be passed over exactly as if there were no pin.
func TestAffinityYieldsToACoolingDeployment(t *testing.T) {
	exec := &fakeExec{replies: map[string]error{}}
	state := NewMemState()
	r, _ := buildAffinityRouter(t, []int{1, 1}, config.PromptCacheConfig{}, 0, exec, state)

	pinned := route(t, r, "prefix-a").Deployment.ID()
	if err := state.RecordFailure(context.Background(), pinned, time.Now(), 0, time.Minute); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	res := route(t, r, "prefix-a")
	if res.Deployment.ID() == pinned {
		t.Errorf("routed to the cooling pinned deployment %s", pinned)
	}
	if res.PromptAffinity != AffinityMiss {
		t.Errorf("affinity = %q, want %q", res.PromptAffinity, AffinityMiss)
	}
}

// A pin must not let a conversation exceed a deployment's rate limit. When the
// pinned upstream is full the request goes elsewhere rather than being refused.
func TestAffinityYieldsToARateLimit(t *testing.T) {
	exec := &fakeExec{replies: map[string]error{}}
	r, _ := buildAffinityRouter(t, []int{1, 1}, config.PromptCacheConfig{}, 1, exec, nil)

	pinned := route(t, r, "prefix-a").Deployment.ID()
	res := route(t, r, "prefix-a")
	if res.Deployment.ID() == pinned {
		t.Errorf("second request reused %s despite its rpm of 1", pinned)
	}
}

// After the pinned deployment fails, the retry's deployment becomes the new pin:
// a pin always names an upstream that has actually served this prefix.
func TestAffinityFollowsAFailover(t *testing.T) {
	exec := &fakeExec{replies: map[string]error{}}
	r, deps := buildAffinityRouter(t, []int{1, 1}, config.PromptCacheConfig{}, 0, exec, nil)

	pinned := route(t, r, "prefix-a").Deployment.ID()
	other := deps[0].ID()
	if other == pinned {
		other = deps[1].ID()
	}

	exec.replies[pinned] = upstreamErr(503)
	res := route(t, r, "prefix-a")
	if res.Deployment.ID() != other {
		t.Fatalf("failover went to %s, want %s", res.Deployment.ID(), other)
	}

	exec.replies = map[string]error{}
	next := route(t, r, "prefix-a")
	if next.Deployment.ID() != other {
		t.Errorf("next request went to %s, want the re-pinned %s", next.Deployment.ID(), other)
	}
	if next.PromptAffinity != AffinityHit {
		t.Errorf("affinity = %q, want %q", next.PromptAffinity, AffinityHit)
	}
}

func TestAffinityIsInertWhenThereIsNothingToChooseBetween(t *testing.T) {
	tests := []struct {
		name    string
		weights []int
		pc      config.PromptCacheConfig
		prefix  string
	}{
		{"disabled", []int{1, 1}, config.PromptCacheConfig{Affinity: boolPtr(false)}, "prefix-a"},
		{"no fingerprint", []int{1, 1}, config.PromptCacheConfig{}, ""},
		{"single deployment", []int{1}, config.PromptCacheConfig{}, "prefix-a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := &fakeExec{replies: map[string]error{}}
			state := &countingState{StateStore: NewMemState()}
			r, _ := buildAffinityRouter(t, tt.weights, tt.pc, 0, exec, state)

			res := route(t, r, tt.prefix)
			if res.PromptAffinity != AffinityOff {
				t.Errorf("affinity = %q, want it reported as off", res.PromptAffinity)
			}
			if state.reads != 0 || state.writes != 0 {
				t.Errorf("consulted affinity state %d times and wrote %d, want none of either", state.reads, state.writes)
			}
		})
	}
}

// A pin is an optimization. Losing one costs a cache write, not a request.
func TestAffinitySurvivesAFailingStateStore(t *testing.T) {
	exec := &fakeExec{replies: map[string]error{}}
	state := &countingState{StateStore: NewMemState(), fail: true}
	r, _ := buildAffinityRouter(t, []int{1, 1}, config.PromptCacheConfig{}, 0, exec, state)

	res, err := r.Route(context.Background(), "g", &provider.Request{}, Overrides{PromptPrefix: "prefix-a"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	res.Response.Body.Close()
	if res.Deployment == nil {
		t.Fatal("no deployment served the request")
	}
}

// countingState counts affinity traffic, and can fail it on demand.
type countingState struct {
	StateStore
	reads, writes int
	fail          bool
}

func (c *countingState) Affinity(ctx context.Context, fingerprint string) (string, bool, error) {
	c.reads++
	if c.fail {
		return "", false, errors.New("state unavailable")
	}
	return c.StateStore.Affinity(ctx, fingerprint)
}

func (c *countingState) SetAffinity(ctx context.Context, fingerprint, id string, ttl time.Duration) error {
	c.writes++
	if c.fail {
		return errors.New("state unavailable")
	}
	return c.StateStore.SetAffinity(ctx, fingerprint, id, ttl)
}

// Pins must be scoped to the model group. Deployment IDs already carry the
// group name, but a fallback hop that shared a pin would consult state for a
// deployment that cannot serve it.
func TestAffinityIsScopedToTheModelGroup(t *testing.T) {
	exec := &fakeExec{replies: map[string]error{}}
	state := NewMemState()
	r, _ := buildAffinityRouter(t, []int{1, 1}, config.PromptCacheConfig{}, 0, exec, state)

	served := route(t, r, "prefix-a").Deployment.ID()
	got, ok, err := state.Affinity(context.Background(), "g\x00prefix-a")
	if err != nil || !ok {
		t.Fatalf("Affinity: got=%q ok=%v err=%v", got, ok, err)
	}
	if got != served {
		t.Errorf("pin = %s, want %s", got, served)
	}
	if _, ok, _ := state.Affinity(context.Background(), "prefix-a"); ok {
		t.Error("a pin was stored under an unscoped key")
	}
}

func TestMemStateAffinityExpires(t *testing.T) {
	state := NewMemState()
	now := time.Unix(0, 0)
	state.now = func() time.Time { return now }
	ctx := context.Background()

	if err := state.SetAffinity(ctx, "fp", "dep-1", time.Minute); err != nil {
		t.Fatalf("SetAffinity: %v", err)
	}
	if id, ok, _ := state.Affinity(ctx, "fp"); !ok || id != "dep-1" {
		t.Fatalf("Affinity = %q, %v; want dep-1, true", id, ok)
	}

	now = now.Add(time.Minute)
	if id, ok, _ := state.Affinity(ctx, "fp"); ok {
		t.Errorf("Affinity = %q after the ttl elapsed, want a miss", id)
	}
}

func TestMemStateAffinityIgnoresANonPositiveTTL(t *testing.T) {
	state := NewMemState()
	if err := state.SetAffinity(context.Background(), "fp", "dep-1", 0); err != nil {
		t.Fatalf("SetAffinity: %v", err)
	}
	if _, ok, _ := state.Affinity(context.Background(), "fp"); ok {
		t.Error("a pin with no ttl was stored")
	}
}

// Memory must stay flat under an unbounded stream of distinct prompts, and a
// pin demoted by that churn must still be served: a busy conversation should
// not lose its cache because unrelated traffic filled the map.
func TestMemStateAffinityBoundsMemory(t *testing.T) {
	state := NewMemState()
	ctx := context.Background()

	for i := range maxAffinityEntries + 10 {
		if err := state.SetAffinity(ctx, "fp-"+strconv.Itoa(i), "dep-1", time.Hour); err != nil {
			t.Fatalf("SetAffinity: %v", err)
		}
	}
	if got := len(state.affinityCur) + len(state.affinityPrev); got > 2*maxAffinityEntries {
		t.Errorf("holding %d pins, want at most %d", got, 2*maxAffinityEntries)
	}
	if len(state.affinityCur) >= maxAffinityEntries {
		t.Errorf("live generation was never rotated: %d entries", len(state.affinityCur))
	}
	// The newest pin, and one demoted to the older generation, are both served.
	if _, ok, _ := state.Affinity(ctx, "fp-"+strconv.Itoa(maxAffinityEntries+9)); !ok {
		t.Error("the newest pin was dropped")
	}
	if _, ok, _ := state.Affinity(ctx, "fp-"+strconv.Itoa(maxAffinityEntries-1)); !ok {
		t.Error("a pin demoted to the older generation was dropped")
	}
}

// Pins are read and written from every request goroutine, and the two-generation
// map rotates under that traffic.
func TestMemStateAffinityIsConcurrencySafe(t *testing.T) {
	state := NewMemState()
	ctx := context.Background()

	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				key := "fp-" + strconv.Itoa(w*500+i)
				if err := state.SetAffinity(ctx, key, "dep-1", time.Minute); err != nil {
					t.Errorf("SetAffinity: %v", err)
					return
				}
				if _, _, err := state.Affinity(ctx, key); err != nil {
					t.Errorf("Affinity: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// "No pin yet" and "a pin was passed over" are different facts, and only the
// second is a problem worth alerting on.
func TestAffinityDistinguishesANewPrefixFromAPassedOverPin(t *testing.T) {
	exec := &fakeExec{replies: map[string]error{}}
	state := NewMemState()
	r, _ := buildAffinityRouter(t, []int{1, 1}, config.PromptCacheConfig{}, 0, exec, state)

	if got := route(t, r, "prefix-a").PromptAffinity; got != AffinityNew {
		t.Errorf("unpinned prefix: affinity = %q, want %q", got, AffinityNew)
	}
	if got := route(t, r, "prefix-a").PromptAffinity; got != AffinityHit {
		t.Errorf("pinned prefix: affinity = %q, want %q", got, AffinityHit)
	}

	// Eject the pinned deployment: now a pin exists and cannot be honoured.
	pinned, _, err := state.Affinity(context.Background(), "g\x00prefix-a")
	if err != nil {
		t.Fatalf("Affinity: %v", err)
	}
	if err := state.RecordFailure(context.Background(), pinned, time.Now(), 0, time.Minute); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if got := route(t, r, "prefix-a").PromptAffinity; got != AffinityMiss {
		t.Errorf("passed-over pin: affinity = %q, want %q", got, AffinityMiss)
	}
}
