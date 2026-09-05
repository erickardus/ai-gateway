package router

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/provider"
)

// On a passthrough deployment the credential is the caller's own, so a 401 or
// 429 describes that caller. Counting it would let one developer's expired
// subscription token eject the shared upstream for everyone else.
func TestPassthroughCallerErrorsDoNotEjectDeployment(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests} {
		err := &core.UpstreamError{StatusCode: status}
		if coolable(err, core.AuthModePassthrough) {
			t.Errorf("status %d on a passthrough deployment must not count toward ejection", status)
		}
		if !coolable(err, core.AuthModeAPIKey) && status != http.StatusForbidden {
			t.Errorf("status %d on an api_key deployment should count toward ejection", status)
		}
	}
	// A genuine upstream fault still ejects, in either mode.
	for _, mode := range []core.AuthMode{core.AuthModePassthrough, core.AuthModeAPIKey} {
		if !coolable(&core.UpstreamError{StatusCode: http.StatusBadGateway}, mode) {
			t.Errorf("a 502 must count toward ejection in %s mode", mode)
		}
	}
}

// The gateway does not translate between wire formats, so an ingress must never
// reach a deployment of the other format.
func TestFormatIsolation(t *testing.T) {
	cfg := &config.Config{Router: config.RouterConfig{Strategy: config.StrategyWeightedShuffle}}
	for _, f := range []core.Format{core.FormatAnthropic, core.FormatOpenAI} {
		cfg.ModelList = append(cfg.ModelList, config.Deployment{
			ModelName: string(f) + "-group",
			Params: config.DeploymentParams{
				Format: f, APIBase: "https://up.example.com", Model: string(f) + "-group",
				AuthMode: core.AuthModeAPIKey, AuthHeader: "x-api-key", APIKey: "k",
			},
		})
	}
	if err := config.Finalize(cfg); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	exec := &fakeExec{replies: map[string]error{}}
	r, err := New(cfg, NewMemState(), exec, discardLogger(), Options{Seed: 3})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// An OpenAI ingress must not reach the anthropic group.
	_, err = r.Route(context.Background(), "anthropic-group",
		&provider.Request{Format: core.FormatOpenAI}, Overrides{})
	if !errors.Is(err, core.ErrNoHealthyDeployment) {
		t.Errorf("openai ingress into an anthropic group: got %v, want ErrNoHealthyDeployment", err)
	}
	if n := len(exec.callList()); n != 0 {
		t.Errorf("the mismatched request reached the upstream %d times", n)
	}

	// The matching format still routes.
	res, err := r.Route(context.Background(), "anthropic-group",
		&provider.Request{Format: core.FormatAnthropic}, Overrides{})
	if err != nil {
		t.Fatalf("matching format failed to route: %v", err)
	}
	res.Response.Body.Close()
}

// stream_timeout bounds time to the first chunk. Attaching it to the request
// context would sever the body mid-stream once the deadline passed.
func TestStreamTimeoutDoesNotTruncateLongStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rc := config.RouterConfig{
			Strategy:      config.StrategyWeightedShuffle,
			Timeout:       10 * time.Minute,
			StreamTimeout: 2 * time.Second,
			Cooldown:      config.CooldownConfig{Period: time.Minute},
			Backoff:       config.BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond},
		}
		// Headers arrive quickly; the body then streams well past StreamTimeout.
		exec := &fakeExec{replies: map[string]error{}, hold: 500 * time.Millisecond}
		r, _ := buildRouter(t, "g", []int{1}, rc, exec, nil)

		res, err := r.Route(context.Background(), "g",
			&provider.Request{Stream: true, Format: core.FormatAnthropic}, Overrides{})
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		defer res.Response.Body.Close()

		// Simulate a long completion: far longer than stream_timeout.
		time.Sleep(30 * time.Second)

		buf := make([]byte, 64)
		if _, err := res.Response.Body.Read(buf); err != nil && !errors.Is(err, context.Canceled) {
			// A real body would still be readable; the fake returns EOF.
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("the stream was severed by stream_timeout after headers had arrived: %v", err)
			}
		}
	})
}

// A slow upstream must still be cut off before headers arrive.
func TestStreamTimeoutStillBoundsTimeToFirstChunk(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rc := config.RouterConfig{
			Strategy:      config.StrategyWeightedShuffle,
			Timeout:       10 * time.Minute,
			StreamTimeout: time.Second,
			Cooldown:      config.CooldownConfig{Period: time.Minute},
			Backoff:       config.BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond},
			NumRetries:    intPtr(0),
		}
		// Headers never arrive within the stream timeout.
		exec := &fakeExec{replies: map[string]error{}, hold: 30 * time.Second}
		r, _ := buildRouter(t, "g", []int{1}, rc, exec, nil)

		start := time.Now()
		_, err := r.Route(context.Background(), "g",
			&provider.Request{Stream: true, Format: core.FormatAnthropic}, Overrides{})
		if err == nil {
			t.Fatal("expected the header deadline to fire")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("header deadline took %v to fire, want ~1s", elapsed)
		}
	})
}

// A large or malformed override must never disable the deadline entirely.
func TestTimeoutOverrideIsClamped(t *testing.T) {
	for _, v := range []string{"1e30", "1e300", "9e18", "99999999999", "-5", "0", "abc", "NaN", "Inf"} {
		h := http.Header{}
		h.Set("x-litellm-timeout", v)
		o := OverridesFromHeaders(h)
		if o.Timeout != nil && *o.Timeout <= 0 {
			t.Errorf("x-litellm-timeout %q produced a non-positive duration %v, which reads as no deadline at all", v, *o.Timeout)
		}
		if o.Timeout != nil && *o.Timeout > maxOverrideTimeout {
			t.Errorf("x-litellm-timeout %q produced %v, beyond the %v ceiling", v, *o.Timeout, maxOverrideTimeout)
		}
	}
}

// The two timeout headers are distinct knobs and must not overwrite each other.
func TestTimeoutHeadersAreIndependent(t *testing.T) {
	h := http.Header{}
	h.Set("x-litellm-timeout", "600")
	h.Set("x-litellm-stream-timeout", "5")
	o := OverridesFromHeaders(h)

	if o.Timeout == nil || *o.Timeout != 600*time.Second {
		t.Errorf("Timeout = %v, want 600s", o.Timeout)
	}
	if o.StreamTimeout == nil || *o.StreamTimeout != 5*time.Second {
		t.Errorf("StreamTimeout = %v, want 5s", o.StreamTimeout)
	}
}

// An always-failing deployment records no latency sample; it must not therefore
// be treated as infinitely fast and starve the healthy one.
func TestLatencyBasedDoesNotStarveHealthyDeployment(t *testing.T) {
	ctx := context.Background()
	state := NewMemState()
	cfg := &config.Config{Router: config.RouterConfig{Strategy: config.StrategyLatencyBased, LowestLatencyBuffer: 0.2}}
	for range 2 {
		cfg.ModelList = append(cfg.ModelList, config.Deployment{
			ModelName: "g",
			Params: config.DeploymentParams{
				Format: core.FormatAnthropic, APIBase: "https://up.example.com", Model: "g",
				AuthMode: core.AuthModeAPIKey, AuthHeader: "x-api-key", APIKey: "k",
			},
		})
	}
	if err := config.Finalize(cfg); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	deps := cfg.Groups()["g"]

	// One healthy deployment with real samples; one that has only ever failed.
	for range 20 {
		_ = state.BeginRequest(ctx, deps[0].ID())
		_ = state.EndRequest(ctx, deps[0].ID(), 50*time.Millisecond, true)
		_ = state.BeginRequest(ctx, deps[1].ID())
		_ = state.EndRequest(ctx, deps[1].ID(), 0, false)
	}

	strategy := LatencyBased{Buffer: 0.2}
	counts := map[string]int{}
	r, _ := New(cfg, state, &fakeExec{replies: map[string]error{}}, discardLogger(), Options{Seed: 11})
	for range 1000 {
		dep, err := strategy.Pick(ctx, deps, state, r.rand())
		if err != nil {
			t.Fatalf("Pick: %v", err)
		}
		counts[dep.ID()]++
	}
	if counts[deps[0].ID()] == 0 {
		t.Fatal("the healthy deployment was never selected: an unsampled failing deployment starved it")
	}
	if share := float64(counts[deps[0].ID()]) / 1000; share < 0.3 {
		t.Errorf("healthy deployment took only %.2f of traffic; it should dominate", share)
	}
}
