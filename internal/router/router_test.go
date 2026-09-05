package router

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/provider"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// fakeExec records which deployments were called and replies per deployment.
type fakeExec struct {
	mu      sync.Mutex
	calls   []string
	replies map[string]error // nil means success
	hold    time.Duration
}

func (f *fakeExec) Do(ctx context.Context, dep *config.Deployment, _ *provider.Request) (*provider.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, dep.ID())
	err, ok := f.replies[dep.ID()]
	hold := f.hold
	f.mu.Unlock()

	if hold > 0 {
		select {
		case <-time.After(hold):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if ok && err != nil {
		return nil, err
	}
	return &provider.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("{}")),
		Deployment: dep.ID(),
	}, nil
}

func (f *fakeExec) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// buildRouter assembles a router over n deployments in one model group.
func buildRouter(t *testing.T, group string, weights []int, rc config.RouterConfig, exec Executor, state StateStore) (*Router, []*config.Deployment) {
	t.Helper()
	cfg := &config.Config{Router: rc}
	for _, w := range weights {
		cfg.ModelList = append(cfg.ModelList, config.Deployment{
			ModelName: group,
			Weight:    w,
			Params: config.DeploymentParams{
				Format: core.FormatAnthropic, APIBase: "https://up.example.com",
				Model: group, AuthMode: core.AuthModeAPIKey,
				AuthHeader: "x-api-key", APIKey: "k",
			},
		})
	}
	if err := config.Finalize(cfg); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if state == nil {
		state = NewMemState()
	}
	r, err := New(cfg, state, exec, discardLogger(), Options{Seed: 42})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, cfg.Groups()[group]
}

func upstreamErr(status int) error {
	return &core.UpstreamError{StatusCode: status, Deployment: "x", Header: http.Header{}}
}

// Weight must translate proportionally into traffic share.
func TestWeightedShuffleDistribution(t *testing.T) {
	exec := &fakeExec{replies: map[string]error{}}
	rc := config.RouterConfig{Strategy: config.StrategyWeightedShuffle}
	r, deps := buildRouter(t, "g", []int{9, 1}, rc, exec, nil)

	const trials = 4000
	counts := map[string]int{}
	for range trials {
		res, err := r.Route(context.Background(), "g", &provider.Request{}, Overrides{})
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		counts[res.Deployment.ID()]++
		res.Response.Body.Close()
	}

	heavy := float64(counts[deps[0].ID()]) / trials
	if heavy < 0.86 || heavy > 0.94 {
		t.Errorf("weight 9 of 10 took %.3f of traffic, want ~0.90", heavy)
	}
	if counts[deps[1].ID()] == 0 {
		t.Error("the low-weight deployment never received traffic")
	}
}

func TestZeroWeightsFallBackToUniform(t *testing.T) {
	exec := &fakeExec{replies: map[string]error{}}
	rc := config.RouterConfig{Strategy: config.StrategyWeightedShuffle}
	r, _ := buildRouter(t, "g", []int{0, 0}, rc, exec, nil)
	// Weight 0 is normalized to 1 by defaults, so this simply must not hang or
	// error; both deployments remain selectable.
	res, err := r.Route(context.Background(), "g", &provider.Request{}, Overrides{})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	res.Response.Body.Close()
}

// A retry must land on a different deployment, not the one that just failed.
func TestRetryExcludesFailedDeployment(t *testing.T) {
	rc := config.RouterConfig{
		Strategy: config.StrategyWeightedShuffle, NumRetries: 3,
		Cooldown: config.CooldownConfig{AllowedFails: intPtr(100), Period: time.Minute},
		Backoff:  config.BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond},
	}
	exec := &fakeExec{replies: map[string]error{}}
	r, deps := buildRouter(t, "g", []int{1, 1}, rc, exec, nil)

	// Fail the first deployment; the second succeeds.
	exec.replies[deps[0].ID()] = upstreamErr(http.StatusInternalServerError)

	res, err := r.Route(context.Background(), "g", &provider.Request{}, Overrides{})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	defer res.Response.Body.Close()

	if res.Deployment.ID() != deps[1].ID() {
		t.Errorf("served by %s, want the healthy deployment %s", res.Deployment.ID(), deps[1].ID())
	}
	calls := exec.callList()
	seen := map[string]int{}
	for _, c := range calls {
		seen[c]++
	}
	if seen[deps[0].ID()] > 1 {
		t.Errorf("the failed deployment was retried %d times; retries must exclude it", seen[deps[0].ID()])
	}
}

func TestRetriesExhaustedReturnsFirstError(t *testing.T) {
	rc := config.RouterConfig{
		Strategy: config.StrategyWeightedShuffle, NumRetries: 2,
		Cooldown: config.CooldownConfig{AllowedFails: intPtr(100), Period: time.Minute},
		Backoff:  config.BackoffConfig{Initial: time.Nanosecond, Max: time.Nanosecond},
	}
	exec := &fakeExec{replies: map[string]error{}}
	r, deps := buildRouter(t, "g", []int{1, 1}, rc, exec, nil)
	for _, d := range deps {
		exec.replies[d.ID()] = upstreamErr(http.StatusBadGateway)
	}

	_, err := r.Route(context.Background(), "g", &provider.Request{}, Overrides{})
	var ue *core.UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != http.StatusBadGateway {
		t.Fatalf("err = %v, want the upstream 502", err)
	}
}

// A 400 is the caller's fault, so it must not be retried.
func TestNonRetryableStatusStopsImmediately(t *testing.T) {
	rc := config.RouterConfig{
		Strategy: config.StrategyWeightedShuffle, NumRetries: 3,
		Cooldown: config.CooldownConfig{AllowedFails: intPtr(100), Period: time.Minute},
		Backoff:  config.BackoffConfig{Initial: time.Nanosecond, Max: time.Nanosecond},
	}
	exec := &fakeExec{replies: map[string]error{}}
	r, deps := buildRouter(t, "g", []int{1}, rc, exec, nil)
	exec.replies[deps[0].ID()] = upstreamErr(http.StatusBadRequest)

	if _, err := r.Route(context.Background(), "g", &provider.Request{}, Overrides{}); err == nil {
		t.Fatal("expected an error")
	}
	if n := len(exec.callList()); n != 1 {
		t.Errorf("made %d attempts, want 1: a 400 must not be retried", n)
	}
}

func TestLeastBusyCounterNeverGoesNegative(t *testing.T) {
	state := NewMemState()
	ctx := context.Background()
	// More completions than dispatches must not drive the counter below zero,
	// which would make a deployment look permanently idle.
	for range 3 {
		_ = state.EndRequest(ctx, "d1", time.Millisecond, true)
	}
	if n, _ := state.InFlight(ctx, "d1"); n != 0 {
		t.Fatalf("in-flight = %d, want 0", n)
	}
	_ = state.BeginRequest(ctx, "d1")
	if n, _ := state.InFlight(ctx, "d1"); n != 1 {
		t.Fatalf("in-flight = %d, want 1", n)
	}
}

func TestInFlightReleasedConcurrently(t *testing.T) {
	exec := &fakeExec{replies: map[string]error{}}
	state := NewMemState()
	rc := config.RouterConfig{Strategy: config.StrategyLeastBusy}
	r, deps := buildRouter(t, "g", []int{1, 1}, rc, exec, state)

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			res, err := r.Route(context.Background(), "g", &provider.Request{}, Overrides{})
			if err != nil {
				return
			}
			io.Copy(io.Discard, res.Response.Body)
			res.Response.Body.Close()
		})
	}
	wg.Wait()

	for _, d := range deps {
		if n, _ := state.InFlight(context.Background(), d.ID()); n != 0 {
			t.Errorf("deployment %s leaked %d in-flight slots", d.ID(), n)
		}
	}
}

// Closing the body twice must not double-release the in-flight slot.
func TestDoubleCloseReleasesOnce(t *testing.T) {
	exec := &fakeExec{replies: map[string]error{}}
	state := NewMemState()
	rc := config.RouterConfig{Strategy: config.StrategyWeightedShuffle}
	r, deps := buildRouter(t, "g", []int{1}, rc, exec, state)

	_ = state.BeginRequest(context.Background(), deps[0].ID()) // an unrelated in-flight request
	res, err := r.Route(context.Background(), "g", &provider.Request{}, Overrides{})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	res.Response.Body.Close()
	res.Response.Body.Close()

	if n, _ := state.InFlight(context.Background(), deps[0].ID()); n != 1 {
		t.Errorf("in-flight = %d, want 1: a second Close must not release again", n)
	}
}

func TestFallbackToAnotherGroup(t *testing.T) {
	cfg := &config.Config{
		Router: config.RouterConfig{
			Strategy: config.StrategyWeightedShuffle, NumRetries: 0,
			Cooldown:  config.CooldownConfig{AllowedFails: intPtr(100), Period: time.Minute},
			Backoff:   config.BackoffConfig{Initial: time.Nanosecond, Max: time.Nanosecond},
			Fallbacks: []config.FallbackRule{{From: "primary", To: []string{"backup"}}},
		},
	}
	for _, name := range []string{"primary", "backup"} {
		cfg.ModelList = append(cfg.ModelList, config.Deployment{
			ModelName: name, Weight: 1,
			Params: config.DeploymentParams{
				Format: core.FormatAnthropic, APIBase: "https://up.example.com",
				Model: name, AuthMode: core.AuthModeAPIKey, AuthHeader: "x-api-key", APIKey: "k",
			},
		})
	}
	if err := config.Finalize(cfg); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	exec := &fakeExec{replies: map[string]error{}}
	r, err := New(cfg, NewMemState(), exec, discardLogger(), Options{Seed: 7})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	exec.replies[cfg.Groups()["primary"][0].ID()] = upstreamErr(http.StatusServiceUnavailable)

	res, err := r.Route(context.Background(), "primary", &provider.Request{}, Overrides{})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	defer res.Response.Body.Close()

	if res.Deployment.ModelName != "backup" {
		t.Errorf("served by %q, want the backup group", res.Deployment.ModelName)
	}
	if res.AttemptedFallback != 1 {
		t.Errorf("AttemptedFallback = %d, want 1", res.AttemptedFallback)
	}

	// With fallbacks disabled the original failure must surface instead.
	exec.replies[cfg.Groups()["backup"][0].ID()] = upstreamErr(http.StatusTeapot)
	if _, err := r.Route(context.Background(), "primary", &provider.Request{}, Overrides{DisableFallbacks: true}); err == nil {
		t.Error("expected an error when fallbacks are disabled")
	}
}

func TestUnknownModelGroup(t *testing.T) {
	exec := &fakeExec{replies: map[string]error{}}
	r, _ := buildRouter(t, "g", []int{1}, config.RouterConfig{Strategy: config.StrategyWeightedShuffle}, exec, nil)
	if _, err := r.Route(context.Background(), "nope", &provider.Request{}, Overrides{}); !errors.Is(err, core.ErrModelNotFound) {
		t.Fatalf("err = %v, want ErrModelNotFound", err)
	}
}

// intPtr is a helper for the pointer-valued allowed_fails setting.
func intPtr(n int) *int { return &n }
