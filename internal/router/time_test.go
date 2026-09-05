package router

import (
	"context"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/limiter"
	"github.com/erickardus/ai-gateway/internal/provider"
)

// These tests run inside a synctest bubble, where time is virtual and advances
// only when every goroutine is blocked. That makes cooldown expiry, window
// rollover and retry backoff exactly reproducible, with no real sleeping.

func TestCooldownEjectsAndRecovers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		state := NewMemState()
		const (
			id           = "dep-1"
			allowedFails = 2
			period       = 30 * time.Second
		)

		// Failures up to and including the allowance must not eject.
		for i := 1; i <= allowedFails; i++ {
			if err := state.RecordFailure(ctx, id, time.Now(), allowedFails, period); err != nil {
				t.Fatalf("RecordFailure: %v", err)
			}
			if cooling, _ := state.InCooldown(ctx, id, time.Now()); cooling {
				t.Fatalf("ejected after %d failures, allowed_fails is %d", i, allowedFails)
			}
		}

		// Exceeding it ejects.
		if err := state.RecordFailure(ctx, id, time.Now(), allowedFails, period); err != nil {
			t.Fatalf("RecordFailure: %v", err)
		}
		if cooling, _ := state.InCooldown(ctx, id, time.Now()); !cooling {
			t.Fatal("not ejected after exceeding allowed_fails")
		}

		// Still ejected just before the period elapses.
		time.Sleep(period - time.Second)
		if cooling, _ := state.InCooldown(ctx, id, time.Now()); !cooling {
			t.Fatal("recovered early")
		}

		// Recovered once it has.
		time.Sleep(2 * time.Second)
		if cooling, _ := state.InCooldown(ctx, id, time.Now()); cooling {
			t.Fatal("still ejected after the cooldown period elapsed")
		}

		// Recovery resets the tally, so a single later failure must not
		// immediately re-eject.
		if err := state.RecordFailure(ctx, id, time.Now(), allowedFails, period); err != nil {
			t.Fatalf("RecordFailure: %v", err)
		}
		if cooling, _ := state.InCooldown(ctx, id, time.Now()); cooling {
			t.Fatal("re-ejected on the first failure after recovery: the tally did not reset")
		}
	})
}

func TestRateLimitWindowRollsOver(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lim := limiter.New()
		const rpm = 3

		for i := range rpm {
			if !lim.Reserve("k", rpm, 0) {
				t.Fatalf("request %d rejected while under the limit", i+1)
			}
		}
		if lim.Reserve("k", rpm, 0) {
			t.Fatal("admitted a request beyond the limit")
		}

		// Still limited just short of the window.
		time.Sleep(limiter.Window - time.Second)
		if lim.Reserve("k", rpm, 0) {
			t.Fatal("window rolled over early")
		}

		// A full window after the first request, capacity is back.
		time.Sleep(2 * time.Second)
		if !lim.Reserve("k", rpm, 0) {
			t.Fatal("window did not roll over")
		}
	})
}

func TestTokenLimitEnforced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lim := limiter.New()
		const tpm = 1000

		if !lim.Reserve("k", 0, tpm) {
			t.Fatal("first request rejected")
		}
		lim.AddTokens("k", 1200) // the response overshot the budget
		if lim.Reserve("k", 0, tpm) {
			t.Fatal("admitted a request after the token budget was exhausted")
		}

		time.Sleep(limiter.Window + time.Second)
		if !lim.Reserve("k", 0, tpm) {
			t.Fatal("token budget did not reset with the window")
		}
	})
}

// A cooled-down deployment must be skipped, and become selectable again once
// its cooldown expires.
func TestRouterSkipsCooledDeploymentThenRecovers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rc := config.RouterConfig{
			Strategy:   config.StrategyWeightedShuffle,
			NumRetries: intPtr(1),
			Cooldown:   config.CooldownConfig{AllowedFails: intPtr(0), Period: 30 * time.Second},
			Backoff:    config.BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond},
		}
		exec := &fakeExec{replies: map[string]error{}}
		state := NewMemState()
		r, deps := buildRouter(t, "g", []int{1, 1}, rc, exec, state)

		// The first deployment fails with a 503, which is cooldown-eligible.
		exec.replies[deps[0].ID()] = upstreamErr(http.StatusServiceUnavailable)

		res, err := r.Route(context.Background(), "g", &provider.Request{}, Overrides{})
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		res.Response.Body.Close()
		if cooling, _ := state.InCooldown(context.Background(), deps[0].ID(), time.Now()); !cooling {
			t.Fatal("the failing deployment was not ejected")
		}

		// While it is ejected, everything goes to the healthy one.
		for range 10 {
			res, err := r.Route(context.Background(), "g", &provider.Request{}, Overrides{})
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if res.Deployment.ID() == deps[0].ID() {
				t.Fatal("routed to an ejected deployment")
			}
			res.Response.Body.Close()
		}

		// After the cooldown it becomes selectable again.
		time.Sleep(31 * time.Second)
		delete(exec.replies, deps[0].ID())
		if cooling, _ := state.InCooldown(context.Background(), deps[0].ID(), time.Now()); cooling {
			t.Fatal("still ejected after the cooldown period")
		}
	})
}

// With another healthy deployment available there is nothing to wait for, so a
// retry must be immediate rather than backing off.
func TestNoBackoffWhenAHealthyPeerExists(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rc := config.RouterConfig{
			Strategy: config.StrategyWeightedShuffle, NumRetries: intPtr(2),
			Cooldown: config.CooldownConfig{AllowedFails: intPtr(100), Period: time.Minute},
			// A backoff long enough that any wait would be unmistakable.
			Backoff: config.BackoffConfig{Initial: 10 * time.Second, Max: 60 * time.Second},
		}
		exec := &fakeExec{replies: map[string]error{}}
		r, deps := buildRouter(t, "g", []int{1, 1}, rc, exec, nil)
		exec.replies[deps[0].ID()] = upstreamErr(http.StatusInternalServerError)

		start := time.Now()
		res, err := r.Route(context.Background(), "g", &provider.Request{}, Overrides{})
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		res.Response.Body.Close()

		if elapsed := time.Since(start); elapsed >= time.Second {
			t.Errorf("retry waited %v; with a healthy peer available it must retry immediately", elapsed)
		}
	})
}

// With only one deployment there is nothing else to try, so backoff applies.
func TestBackoffAppliesWhenNothingElseIsHealthy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rc := config.RouterConfig{
			Strategy: config.StrategyWeightedShuffle, NumRetries: intPtr(1),
			Cooldown: config.CooldownConfig{AllowedFails: intPtr(100), Period: time.Minute},
			Backoff:  config.BackoffConfig{Initial: 5 * time.Second, Max: 30 * time.Second, Jitter: floatPtr(0)},
		}
		exec := &fakeExec{replies: map[string]error{}}
		r, deps := buildRouter(t, "g", []int{1}, rc, exec, nil)
		exec.replies[deps[0].ID()] = upstreamErr(http.StatusInternalServerError)

		start := time.Now()
		if _, err := r.Route(context.Background(), "g", &provider.Request{}, Overrides{}); err == nil {
			t.Fatal("expected failure")
		}
		if elapsed := time.Since(start); elapsed < 5*time.Second {
			t.Errorf("retried after %v; with no healthy peer it must back off", elapsed)
		}
	})
}

// An upstream Retry-After within a sane range takes precedence over the
// computed backoff.
func TestRetryAfterHeaderHonoured(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rc := config.RouterConfig{
			Strategy: config.StrategyWeightedShuffle, NumRetries: intPtr(1),
			Cooldown: config.CooldownConfig{AllowedFails: intPtr(100), Period: time.Minute},
			Backoff:  config.BackoffConfig{Initial: time.Second, Max: 2 * time.Second, Jitter: floatPtr(0)},
		}
		exec := &fakeExec{replies: map[string]error{}}
		r, deps := buildRouter(t, "g", []int{1}, rc, exec, nil)

		h := http.Header{}
		h.Set("Retry-After", "20")
		exec.replies[deps[0].ID()] = &core.UpstreamError{
			StatusCode: http.StatusTooManyRequests, Header: h, Deployment: deps[0].ID(),
		}

		start := time.Now()
		if _, err := r.Route(context.Background(), "g", &provider.Request{}, Overrides{}); err == nil {
			t.Fatal("expected failure")
		}
		if elapsed := time.Since(start); elapsed < 20*time.Second {
			t.Errorf("waited %v, want at least the 20s the upstream asked for", elapsed)
		}
	})
}
