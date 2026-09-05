package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/provider"
)

// Executor performs one upstream attempt. provider.Client satisfies it.
type Executor interface {
	Do(ctx context.Context, dep *config.Deployment, req *provider.Request) (*provider.Response, error)
}

// Router resolves a model name to a deployment and drives retries and fallbacks.
type Router struct {
	cfg      config.RouterConfig
	groups   map[string][]*config.Deployment
	strategy Strategy
	state    StateStore
	exec     Executor
	log      *slog.Logger

	// rnd is guarded because math/rand/v2.Rand is not safe for concurrent use.
	rndMu sync.Mutex
	rnd   *rand.Rand

	now func() time.Time
}

// Options configures a Router. Seed and Now exist so tests can make selection
// and timing deterministic.
type Options struct {
	Seed uint64
	Now  func() time.Time
}

// New builds a Router from validated configuration.
func New(cfg *config.Config, state StateStore, exec Executor, log *slog.Logger, opts Options) (*Router, error) {
	strategy, err := NewStrategy(cfg.Router)
	if err != nil {
		return nil, err
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	seed := opts.Seed
	if seed == 0 {
		seed = rand.Uint64()
	}
	return &Router{
		cfg:      cfg.Router,
		groups:   cfg.Groups(),
		strategy: strategy,
		state:    state,
		exec:     exec,
		log:      log,
		rnd:      rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		now:      opts.Now,
	}, nil
}

// Groups returns the configured model group names.
func (r *Router) Groups() map[string][]*config.Deployment { return r.groups }

// HasGroup reports whether a model group exists.
func (r *Router) HasGroup(name string) bool {
	_, ok := r.groups[name]
	return ok
}

// Result describes how a request was ultimately served.
type Result struct {
	Response          *provider.Response
	Deployment        *config.Deployment
	AttemptedRetries  int
	AttemptedFallback int
}

// Route serves a request, retrying within the requested model group and then
// falling back to other groups.
//
// Retries are exhausted within a group before any fallback is considered, and
// each fallback hop is given its own full retry budget. If everything fails the
// original error is returned rather than the last fallback's, because the first
// failure is what actually describes the problem.
func (r *Router) Route(ctx context.Context, model string, req *provider.Request, overrides Overrides) (*Result, error) {
	attempted := map[string]bool{model: true}
	result, firstErr := r.routeGroup(ctx, model, req, overrides, 0)
	if firstErr == nil {
		return result, nil
	}

	if overrides.DisableFallbacks {
		return nil, firstErr
	}

	chain := r.fallbacksFor(model, firstErr)
	hops := 0
	for _, next := range chain {
		if hops >= r.cfg.MaxFallbackHops {
			r.log.Warn("fallback chain truncated", "model", model, "max_hops", r.cfg.MaxFallbackHops)
			break
		}
		if attempted[next] {
			continue // loop protection
		}
		attempted[next] = true
		hops++

		r.log.Info("falling back", "from", model, "to", next, "hop", hops, "cause", firstErr)
		res, err := r.routeGroup(ctx, next, req, overrides, hops)
		if err == nil {
			res.AttemptedFallback = hops
			return res, nil
		}
	}
	return nil, firstErr
}

// routeGroup attempts one model group, retrying across its deployments.
func (r *Router) routeGroup(ctx context.Context, model string, req *provider.Request, overrides Overrides, hop int) (*Result, error) {
	deployments, ok := r.groups[model]
	if !ok {
		return nil, fmt.Errorf("model %q: %w", model, core.ErrModelNotFound)
	}

	maxRetries := r.cfg.NumRetries
	if overrides.NumRetries != nil {
		maxRetries = *overrides.NumRetries
	}

	// Deployments that have already failed this request are excluded from
	// re-selection, so a retry makes progress instead of possibly landing back
	// on the deployment that just failed.
	failed := make(map[string]bool)
	var firstErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		dep, err := r.pick(ctx, deployments, failed, overrides)
		if err != nil {
			if firstErr != nil {
				return nil, firstErr
			}
			if len(failed) == 0 && !overrides.AllowPassthrough && allPassthrough(deployments) {
				return nil, fmt.Errorf("model %q is served only by passthrough deployments: %w", model, core.ErrPassthroughNotAllowed)
			}
			return nil, fmt.Errorf("model %q: %w", model, err)
		}

		resp, attemptErr := r.attempt(ctx, dep, req, overrides)
		if attemptErr == nil {
			return &Result{Response: resp, Deployment: dep, AttemptedRetries: attempt, AttemptedFallback: hop}, nil
		}
		if firstErr == nil {
			firstErr = attemptErr
		}
		failed[dep.ID()] = true

		if !retryable(attemptErr) || attempt == maxRetries {
			return nil, firstErr
		}

		// Prefer an untried deployment, and go there straight away: with an
		// alternative available there is nothing to wait for. Only when every
		// deployment has failed is it worth pausing before trying again, and
		// the failure set is then cleared so the next attempt can proceed.
		if r.hasUntried(ctx, deployments, failed, overrides) {
			continue
		}
		if err := r.sleep(ctx, r.backoff(attempt, attemptErr)); err != nil {
			return nil, err
		}
		failed = make(map[string]bool)
	}
	return nil, firstErr
}

// pick chooses a deployment and consumes its rate-limit budget. Capacity is
// only consumed for the deployment actually dispatched to, so filtering never
// spends budget on candidates that go unused.
func (r *Router) pick(ctx context.Context, deployments []*config.Deployment, failed map[string]bool, overrides Overrides) (*config.Deployment, error) {
	candidates, err := r.candidates(ctx, deployments, failed, overrides)
	if err != nil {
		return nil, err
	}

	for len(candidates) > 0 {
		r.rndMu.Lock()
		dep, err := r.strategy.Pick(ctx, candidates, r.state, r.rnd)
		r.rndMu.Unlock()
		if err != nil {
			return nil, err
		}

		if dep.RPM <= 0 && dep.TPM <= 0 {
			return dep, nil
		}
		reserved, err := r.state.Reserve(ctx, dep.ID(), dep.RPM, dep.TPM)
		if err != nil {
			return nil, fmt.Errorf("reserve capacity: %w", err)
		}
		if reserved {
			return dep, nil
		}
		// Lost a race for the last slot; drop this one and choose again.
		candidates = without(candidates, dep)
	}
	return nil, core.ErrNoHealthyDeployment
}

// hasUntried reports whether any deployment outside the failed set could still
// serve this request. It does not consume capacity.
func (r *Router) hasUntried(ctx context.Context, deployments []*config.Deployment, failed map[string]bool, overrides Overrides) bool {
	candidates, err := r.candidates(ctx, deployments, failed, overrides)
	return err == nil && len(candidates) > 0
}

// without returns candidates with dep removed.
func without(candidates []*config.Deployment, dep *config.Deployment) []*config.Deployment {
	out := candidates[:0]
	for _, c := range candidates {
		if c.ID() != dep.ID() {
			out = append(out, c)
		}
	}
	return out
}

// attempt dispatches one request to one deployment and records the outcome.
func (r *Router) attempt(ctx context.Context, dep *config.Deployment, req *provider.Request, overrides Overrides) (*provider.Response, error) {
	timeout := r.cfg.Timeout
	if req.Stream && r.cfg.StreamTimeout > 0 {
		// For a streaming request the deadline bounds time to the first chunk,
		// not the whole response, which may legitimately run for minutes.
		timeout = r.cfg.StreamTimeout
	}
	if overrides.Timeout != nil {
		timeout = *overrides.Timeout
	}

	attemptCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		attemptCtx, cancel = context.WithTimeout(ctx, timeout)
	}

	if err := r.state.BeginRequest(ctx, dep.ID()); err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, fmt.Errorf("begin request: %w", err)
	}
	start := r.now()

	resp, err := r.exec.Do(attemptCtx, dep, req)
	if err != nil {
		// The attempt is over, so release both the in-flight slot and the
		// deadline before returning.
		_ = r.state.EndRequest(ctx, dep.ID(), r.now().Sub(start), false)
		if cancel != nil {
			cancel()
		}
		if coolable(err) {
			if cerr := r.state.RecordFailure(ctx, dep.ID(), r.now(), r.cfg.Cooldown.Fails(), r.cfg.Cooldown.Period); cerr != nil {
				r.log.Warn("record failure", "deployment", dep.ID(), "error", cerr)
			}
		}
		return nil, err
	}

	// Success: the body has not been read yet, so the in-flight slot and the
	// deadline stay held until the caller finishes relaying it.
	resp.Body = &trackedBody{
		readCloser: resp.Body,
		onClose: func() {
			_ = r.state.EndRequest(ctx, dep.ID(), r.now().Sub(start), true)
			if cancel != nil {
				cancel()
			}
		},
	}
	return resp, nil
}

// sleep waits for d, or returns early if the context is cancelled.
func (r *Router) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// candidates filters a group down to the deployments eligible right now.
func (r *Router) candidates(ctx context.Context, deployments []*config.Deployment, failed map[string]bool, overrides Overrides) ([]*config.Deployment, error) {
	now := r.now()
	out := make([]*config.Deployment, 0, len(deployments))
	for _, d := range deployments {
		if failed[d.ID()] {
			continue
		}
		// A passthrough deployment forwards the caller's own credential, so it
		// is only offered to keys explicitly permitted to use one.
		if d.Params.AuthMode == core.AuthModePassthrough && !overrides.AllowPassthrough {
			continue
		}
		cooling, err := r.state.InCooldown(ctx, d.ID(), now)
		if err != nil {
			return nil, fmt.Errorf("check cooldown: %w", err)
		}
		if cooling {
			continue
		}
		if d.RPM > 0 || d.TPM > 0 {
			ok, err := r.state.Allow(ctx, d.ID(), d.RPM, d.TPM)
			if err != nil {
				return nil, fmt.Errorf("check capacity: %w", err)
			}
			if !ok {
				continue
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// RecordUsage attributes token usage to the deployment that served a request.
func (r *Router) RecordUsage(ctx context.Context, dep *config.Deployment, usage core.Usage) {
	if dep == nil || usage.Total() == 0 {
		return
	}
	if err := r.state.AddTokens(ctx, dep.ID(), usage.Total()); err != nil {
		r.log.Warn("record usage", "deployment", dep.ID(), "error", err)
	}
}

// trackedBody releases routing state once the response body is closed, which is
// the point at which the request is genuinely finished.
type trackedBody struct {
	readCloser
	onClose func()
	once    sync.Once
}

type readCloser = interface {
	Read([]byte) (int, error)
	Close() error
}

// Close implements io.Closer.
func (b *trackedBody) Close() error {
	err := b.readCloser.Close()
	b.once.Do(b.onClose)
	return err
}

// retryable reports whether another attempt is worthwhile.
func retryable(err error) bool {
	var ue *core.UpstreamError
	if errors.As(err, &ue) {
		return ue.Retryable()
	}
	// A transport-level failure (connection refused, reset, timeout) is worth
	// trying against a different deployment.
	return !errors.Is(err, core.ErrUpstreamHostNotAllowed) &&
		!errors.Is(err, core.ErrModelNotFound) &&
		!errors.Is(err, context.Canceled)
}

// coolable reports whether a failure should count towards ejecting a deployment.
//
// Connection-level errors are excluded: they usually say more about the network
// than the deployment. Among 4xx only the statuses that indicate the deployment
// itself is unusable count; a plain 400 is the caller's fault and would
// otherwise let one malformed request eject a healthy upstream.
func coolable(err error) bool {
	var ue *core.UpstreamError
	if !errors.As(err, &ue) {
		return false
	}
	switch ue.StatusCode {
	case http.StatusTooManyRequests, http.StatusUnauthorized,
		http.StatusRequestTimeout, http.StatusNotFound:
		return true
	}
	return ue.StatusCode >= 500
}

// backoff returns the delay before the next attempt: exponential from the
// configured initial delay, capped, with jitter. An upstream Retry-After takes
// precedence when it names a sane delay.
func (r *Router) backoff(attempt int, err error) time.Duration {
	if d, ok := retryAfter(err); ok {
		return d
	}
	d := r.cfg.Backoff.Initial << attempt
	if d > r.cfg.Backoff.Max || d <= 0 {
		d = r.cfg.Backoff.Max
	}
	r.rndMu.Lock()
	jitter := r.rnd.Float64()
	r.rndMu.Unlock()
	return d + time.Duration(float64(d)*r.cfg.Backoff.Jitter*jitter)
}

// retryAfter reads an upstream Retry-After header, honouring only plausible
// values so a hostile or broken upstream cannot stall the gateway.
func retryAfter(err error) (time.Duration, bool) {
	var ue *core.UpstreamError
	if !errors.As(err, &ue) || ue.Header == nil {
		return 0, false
	}
	raw := ue.Header.Get("Retry-After")
	if raw == "" {
		return 0, false
	}
	secs, convErr := strconv.Atoi(raw)
	if convErr != nil || secs <= 0 || secs > 60 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

// DeploymentStatus reports whether a deployment is currently ejected and how
// many requests are outstanding against it, for health reporting.
func (r *Router) DeploymentStatus(ctx context.Context, id string) (cooling bool, inFlight int) {
	cooling, err := r.state.InCooldown(ctx, id, r.now())
	if err != nil {
		r.log.Warn("read cooldown for health", "deployment", id, "error", err)
	}
	inFlight, err = r.state.InFlight(ctx, id)
	if err != nil {
		r.log.Warn("read in-flight for health", "deployment", id, "error", err)
	}
	return cooling, inFlight
}

// allPassthrough reports whether every deployment in a group relays the caller's
// own credential.
func allPassthrough(deployments []*config.Deployment) bool {
	for _, d := range deployments {
		if d.Params.AuthMode != core.AuthModePassthrough {
			return false
		}
	}
	return len(deployments) > 0
}
