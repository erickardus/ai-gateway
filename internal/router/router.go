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
	prompt   config.PromptCacheConfig
	groups   map[string][]*config.Deployment
	strategy Strategy
	state    StateStore
	exec     Executor
	log      *slog.Logger

	// rnd is guarded because math/rand/v2.Rand is not safe for concurrent use.
	// The lock is taken only around a draw, never across a strategy's state
	// lookups, which a distributed StateStore would perform over the network.
	rndMu sync.Mutex
	rnd   *rand.Rand

	// ejected reports a deployment leaving the rotation. Nil where the caller
	// asked for no notification.
	ejected func(model, deployment string)

	now func() time.Time
}

// Options configures a Router. Seed makes selection deterministic in tests;
// timing determinism comes from testing/synctest, so there is no clock seam.
type Options struct {
	Seed uint64
	// Ejected is called when a failure puts a deployment into cooldown.
	//
	// The router publishes this rather than the server because an ejection is
	// not a property of any one request: the request that triggered it usually
	// succeeds elsewhere on a retry, so nothing the caller sees says a
	// deployment just left the rotation. Optional; nil disables it.
	Ejected func(model, deployment string)
}

// New builds a Router from validated configuration.
func New(cfg *config.Config, state StateStore, exec Executor, log *slog.Logger, opts Options) (*Router, error) {
	strategy, err := NewStrategy(cfg.Router)
	if err != nil {
		return nil, err
	}
	seed := opts.Seed
	if seed == 0 {
		seed = rand.Uint64()
	}
	return &Router{
		cfg:      cfg.Router,
		prompt:   cfg.PromptCache,
		groups:   cfg.Groups(),
		strategy: strategy,
		state:    state,
		exec:     exec,
		log:      log,
		rnd:      rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		ejected:  opts.Ejected,
		now:      time.Now,
	}, nil
}

// rand returns a generator safe to use for one selection. Callers hold no lock;
// the returned value is a snapshot seeded from the shared stream.
func (r *Router) rand() *rand.Rand {
	r.rndMu.Lock()
	defer r.rndMu.Unlock()
	return rand.New(rand.NewPCG(r.rnd.Uint64(), r.rnd.Uint64()))
}

// Strategy returns the name of the active routing strategy.
func (r *Router) Strategy() string { return r.strategy.Name() }

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
	// PromptAffinity reports what the prompt-prefix pin did for this request.
	// It is what makes a cache-affinity regression visible before it shows up
	// as a bill.
	PromptAffinity string

	// promptKey and promptTTL carry what RecordPrefix needs to pin this
	// request's prefix once the response has been read. They are unexported
	// because the caller's only business with them is handing the Result back.
	promptKey string
	promptTTL time.Duration
}

// Prompt-affinity outcomes reported on a Result.
//
// AffinityNew and AffinityMiss are deliberately distinct. Both mean the request
// did not land on a pinned deployment, but only a miss is a problem: a new
// prefix has no pin to honour yet, and folding the two together would put every
// opening turn into the miss count. That is the figure an operator alerts on,
// and it would then never approach zero however well affinity was working.
const (
	// AffinityOff means no pin was in play: affinity is disabled, the request
	// has no cacheable prefix, or the group holds a single deployment.
	AffinityOff = ""
	// AffinityNew means this prefix had no pin to honour. Whether it has one
	// afterwards is decided by what the upstream reported: a prefix the
	// provider did not cache is not pinned, since there is no warm copy for a
	// later request to be sent back to.
	AffinityNew = "new"
	// AffinityHit means the pinned deployment served the request, so the
	// upstream's prompt cache was there to be read.
	AffinityHit = "hit"
	// AffinityMiss means a pin existed and something else served the request
	// anyway — the pinned deployment was cooling down, at its limit, carrying
	// too much of the group's load, or had already failed this request.
	//
	// It describes where the request landed, not what the router intended: a
	// pin passed over for load that the strategy then chose anyway still
	// reports a hit, because the request did reach the warm upstream, which is
	// what the metric is for.
	AffinityMiss = "miss"
)

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

	maxRetries := r.cfg.Retries()
	if overrides.NumRetries != nil {
		maxRetries = *overrides.NumRetries
	}

	// A pin is only worth consulting where there is a choice to bias. In a
	// single-deployment group every request already lands on the same upstream,
	// so a lookup and a write would buy nothing.
	affinityKey := ""
	if r.prompt.AffinityEnabled() && overrides.PromptPrefix != "" && len(deployments) > 1 {
		affinityKey = model + "\x00" + overrides.PromptPrefix
	}
	// A pin should outlive nothing and outlast nothing: it is meant to expire
	// with the cache entry it points at, which is longer than the default where
	// the caller asked for the extended lifetime.
	pinTTL := r.prompt.AffinityTTL
	if overrides.PromptPinTTL > pinTTL {
		pinTTL = overrides.PromptPinTTL
	}
	pinned := ""
	if affinityKey != "" {
		id, ok, err := r.state.Affinity(ctx, affinityKey)
		if err != nil {
			// A pin is an optimization. Losing it costs a cache write, not a
			// request, so the request goes on without one.
			r.log.Warn("read prompt affinity", "model", model, "error", err)
		} else if ok {
			pinned = id
		}
	}

	// Deployments that have already failed this request are excluded from
	// re-selection, so a retry makes progress instead of possibly landing back
	// on the deployment that just failed.
	failed := make(map[string]bool)
	var firstErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		dep, err := r.pick(ctx, deployments, failed, overrides, req.Format, pinned)
		if err != nil {
			if firstErr != nil {
				return nil, firstErr
			}
			return nil, fmt.Errorf("model %q: %w", model, err)
		}

		resp, attemptErr := r.attempt(ctx, dep, req, overrides)
		if attemptErr == nil {
			affinity := AffinityOff
			if affinityKey != "" {
				switch {
				case pinned == "":
					affinity = AffinityNew
				case dep.ID() == pinned:
					affinity = AffinityHit
				default:
					affinity = AffinityMiss
				}
			}
			return &Result{
				Response:          resp,
				Deployment:        dep,
				AttemptedRetries:  attempt,
				AttemptedFallback: hop,
				PromptAffinity:    affinity,
				promptKey:         affinityKey,
				promptTTL:         pinTTL,
			}, nil
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
		if r.hasUntried(ctx, deployments, failed, overrides, req.Format) {
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
func (r *Router) pick(ctx context.Context, deployments []*config.Deployment, failed map[string]bool, overrides Overrides, format core.Format, pinned string) (*config.Deployment, error) {
	candidates, why, err := r.candidates(ctx, deployments, failed, overrides, format)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, why.err()
	}

	// A prompt-prefix pin biases the choice; it never constrains it. The pinned
	// deployment has already been through the same health, format, permission
	// and capacity filters as every other candidate, and if it did not survive
	// them — or is carrying too much of the group's load, or loses the race for
	// its last slot — selection carries on exactly as it would with no pin at
	// all. Nothing is reserved on the way past, because a failed reservation
	// consumes nothing.
	if pinned != "" {
		for _, c := range candidates {
			if c.ID() != pinned {
				continue
			}
			overloaded, err := r.pinYieldsToLoad(ctx, candidates, c)
			if err != nil {
				return nil, err
			}
			if overloaded {
				break
			}
			reserved, err := r.reserve(ctx, c)
			if err != nil {
				return nil, err
			}
			if reserved {
				return c, nil
			}
			// It filled between the capacity check and now. Drop it so the
			// strategy does not spend a second round trip discovering the same
			// thing.
			candidates = without(candidates, c)
			break
		}
	}
	if len(candidates) == 0 {
		return nil, core.ErrNoHealthyDeployment
	}

	for len(candidates) > 0 {
		dep, err := r.strategy.Pick(ctx, candidates, r.state, r.rand())
		if err != nil {
			return nil, err
		}

		reserved, err := r.reserve(ctx, dep)
		if err != nil {
			return nil, err
		}
		if reserved {
			return dep, nil
		}
		// Lost a race for the last slot; drop this one and choose again.
		candidates = without(candidates, dep)
	}
	return nil, core.ErrNoHealthyDeployment
}

// pinYieldsToLoad reports whether the pinned deployment is carrying so much more
// work than the best alternative that honouring the pin would cost more in
// contention than it saves in cache reads.
//
// Load rather than traffic share is the test, because concentration is only a
// problem when there is something to contend for. A fingerprint covers a
// request's prefix, not a conversation, so traffic that shares a system prompt,
// its tools and its opening turns shares one pin — and where that is one busy
// conversation among idle peers, sending it all to one deployment is the feature
// working. Where it is the whole workload, the lead builds immediately and each
// request that yields brings it back down, so the group settles with most
// requests still reaching a warm cache.
//
// In-flight is per-instance even when the rest of routing state is shared, so
// this costs no round trip: it reads the load this instance is itself carrying,
// which is also the load it is in a position to redistribute.
func (r *Router) pinYieldsToLoad(ctx context.Context, candidates []*config.Deployment, pinned *config.Deployment) (bool, error) {
	if len(candidates) < 2 {
		return false, nil // nowhere else to send it
	}
	lead := r.prompt.MaxInFlightLead()

	pinnedLoad, err := r.state.InFlight(ctx, pinned.ID())
	if err != nil {
		// Without the signal, keep the pin. Losing cache hits is the more
		// expensive way to be wrong.
		r.log.Warn("read in-flight for prompt affinity", "deployment", pinned.ID(), "error", err)
		return false, nil
	}
	if pinnedLoad <= lead {
		// Cannot be more than lead ahead of anything, so there is nothing to
		// check and no reason to read every other candidate.
		return false, nil
	}

	for _, c := range candidates {
		if c.ID() == pinned.ID() {
			continue
		}
		load, err := r.state.InFlight(ctx, c.ID())
		if err != nil {
			r.log.Warn("read in-flight for prompt affinity", "deployment", c.ID(), "error", err)
			continue
		}
		if pinnedLoad-load > lead {
			return true, nil
		}
	}
	return false, nil
}

// reserve consumes one request's capacity on a deployment, reporting whether it
// fit. A deployment with no limits always fits and costs no round trip.
func (r *Router) reserve(ctx context.Context, dep *config.Deployment) (bool, error) {
	if dep.RPM <= 0 && dep.TPM <= 0 {
		return true, nil
	}
	reserved, err := r.state.Reserve(ctx, dep.ID(), dep.RPM, dep.TPM)
	if err != nil {
		return false, fmt.Errorf("reserve capacity: %w", err)
	}
	return reserved, nil
}

// hasUntried reports whether any deployment outside the failed set could still
// serve this request. It does not consume capacity.
func (r *Router) hasUntried(ctx context.Context, deployments []*config.Deployment, failed map[string]bool, overrides Overrides, format core.Format) bool {
	candidates, _, err := r.candidates(ctx, deployments, failed, overrides, format)
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
	// A streaming request gets a deadline on reaching the response headers
	// only. Attaching it to the request context would bound the whole response,
	// because net/http ties the body's lifetime to that context — so a long
	// completion would be severed mid-stream after the status line had already
	// been sent. A non-streaming request has no such distinction, so its
	// deadline covers the entire exchange.
	deadline := r.cfg.Timeout
	if req.Stream {
		deadline = r.cfg.StreamTimeout
		if overrides.StreamTimeout != nil {
			deadline = *overrides.StreamTimeout
		}
	} else if overrides.Timeout != nil {
		deadline = *overrides.Timeout
	}

	// The attempt context lives until the body is closed; a timer enforces the
	// deadline against it. For a streaming response the timer is stopped once
	// headers arrive so the body may run as long as it needs; for a
	// non-streaming one it keeps running and so bounds the whole exchange.
	attemptCtx, cancel := context.WithCancel(ctx)
	var timer *time.Timer
	if deadline > 0 {
		timer = time.AfterFunc(deadline, cancel)
	}
	stopTimer := func() {
		if timer != nil {
			timer.Stop()
		}
	}

	if err := r.state.BeginRequest(ctx, dep.ID()); err != nil {
		stopTimer()
		cancel()
		return nil, fmt.Errorf("begin request: %w", err)
	}
	start := r.now()

	resp, err := r.exec.Do(attemptCtx, dep, req)
	if err != nil {
		stopTimer()
		// The attempt is over, so release both the in-flight slot and the
		// context before returning.
		_ = r.state.EndRequest(ctx, dep.ID(), r.now().Sub(start), false)
		cancel()
		if coolable(err, dep.Params.AuthMode) {
			if cerr := r.state.RecordFailure(ctx, dep.ID(), r.now(), r.cfg.Cooldown.Fails(), r.cfg.Cooldown.Period); cerr != nil {
				r.log.Warn("record failure", "deployment", dep.ID(), "error", cerr)
			} else {
				r.noteEjection(ctx, dep)
			}
		}
		return nil, err
	}

	// Success: the body has not been read yet, so the in-flight slot stays held
	// until the caller finishes relaying it. Relaying uses a context detached
	// from the client's, so an aborted request still records its completion —
	// otherwise the in-flight count would drift upward and starve the
	// deployment under least-busy routing.
	if req.Stream {
		// The stream may now run for as long as the completion takes.
		stopTimer()
	}
	release := context.WithoutCancel(ctx)
	resp.Body = &trackedBody{
		readCloser: resp.Body,
		onClose: func() {
			stopTimer()
			_ = r.state.EndRequest(release, dep.ID(), r.now().Sub(start), true)
			cancel()
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
// rejection counts why deployments were filtered out, so the caller can report
// the real cause instead of reconstructing it afterwards. Guessing produced the
// wrong error in a mixed group: a key without passthrough permission, arriving
// while the other deployment was cooling down, was told there was no healthy
// deployment rather than that its key lacked a permission.
type rejection struct {
	passthrough int
	format      int
	cooldown    int
	capacity    int
	total       int
}

// err returns the most informative error for a fully rejected candidate set.
func (r rejection) err() error {
	switch {
	case r.total == 0:
		return core.ErrNoHealthyDeployment
	case r.passthrough == r.total:
		return fmt.Errorf("every deployment requires passthrough permission: %w", core.ErrPassthroughNotAllowed)
	case r.format == r.total:
		return fmt.Errorf("no deployment speaks the requested wire format: %w", core.ErrNoHealthyDeployment)
	case r.capacity == r.total:
		return fmt.Errorf("every deployment is at its rate limit: %w", core.ErrRateLimited)
	default:
		return core.ErrNoHealthyDeployment
	}
}

func (r *Router) candidates(ctx context.Context, deployments []*config.Deployment, failed map[string]bool, overrides Overrides, format core.Format) ([]*config.Deployment, rejection, error) {
	now := r.now()
	var why rejection
	out := make([]*config.Deployment, 0, len(deployments))
	for _, d := range deployments {
		if failed[d.ID()] {
			continue
		}
		why.total++
		// A passthrough deployment forwards the caller's own credential, so it
		// is only offered to keys explicitly permitted to use one.
		if d.Params.AuthMode == core.AuthModePassthrough && !overrides.AllowPassthrough {
			why.passthrough++
			continue
		}
		// The gateway does not translate between wire formats, so an ingress
		// may only reach deployments speaking its own. Without this a fallback
		// could relay an Anthropic body — and, on a passthrough deployment, the
		// caller's Anthropic credential — to a different vendor's host.
		if format != "" && d.Params.Format != format {
			why.format++
			continue
		}
		cooling, err := r.state.InCooldown(ctx, d.ID(), now)
		if err != nil {
			return nil, why, fmt.Errorf("check cooldown: %w", err)
		}
		if cooling {
			why.cooldown++
			continue
		}
		if d.RPM > 0 || d.TPM > 0 {
			ok, err := r.state.Allow(ctx, d.ID(), d.RPM, d.TPM)
			if err != nil {
				return nil, why, fmt.Errorf("check capacity: %w", err)
			}
			if !ok {
				why.capacity++
				continue
			}
		}
		out = append(out, d)
	}
	return out, why, nil
}

// RecordPrefix pins this request's cacheable prefix to the deployment that
// served it, once the response has been read and it is known whether there is
// anything to pin to.
//
// It is called after the relay rather than at the moment the upstream answers,
// because the answer is where the evidence is. A pin exists to send the next
// request carrying this prefix back to the upstream holding a warm copy of it,
// and a request the provider did not cache leaves no warm copy anywhere:
// pinning it would concentrate that prefix's traffic on one deployment in
// exchange for nothing. Prompts below the provider's minimum cacheable size do
// that on every request, as does an Anthropic caller sending no breakpoints
// with injection off.
//
// It is written on every success rather than only on a new pin: refreshing the
// TTL keeps an active conversation pinned for as long as it runs and lets it
// lapse once it stops, which is the lifetime the upstream gives the cache entry
// itself.
//
// The cost of deferring is a window, between dispatch and the end of the
// response, in which a concurrent request carrying the same prefix sees no pin.
// A conversation cannot race itself — its next turn waits on this reply — so
// that window belongs to distinct callers sharing a prefix, which is the
// concentration case affinity deliberately bounds anyway.
func (r *Router) RecordPrefix(ctx context.Context, res *Result, usage core.Usage) {
	if res == nil || res.promptKey == "" || res.Deployment == nil || res.promptTTL <= 0 {
		return
	}
	if !prefixWorthPinning(res.Deployment.Params.Format, usage) {
		return
	}
	if err := r.state.SetAffinity(ctx, res.promptKey, res.Deployment.ID(), res.promptTTL); err != nil {
		r.log.Warn("record prompt affinity", "deployment", res.Deployment.ID(), "error", err)
	}
}

// prefixWorthPinning reports whether this response is evidence that the prefix
// it carried is worth sending back to the same upstream.
//
// Anthropic says outright whether its cache took part, reporting the tokens it
// read from an entry and the tokens it wrote into one, so silence there means
// there is nothing on that deployment to return to.
//
// An OpenAI-compatible provider says no such thing. Its caching is automatic
// and its writes are free, so the first request of a conversation reports no
// cached tokens whether or not the prefix was stored — and requiring evidence
// would mean never establishing a pin, which is the feature switched off. The
// pin is free there for the same reason the write is.
//
// A response reporting no usage at all is pinned too. It is the answer a
// streamed reply gives when nothing asked for usage, and losing cache hits is
// the more expensive way to be wrong.
func prefixWorthPinning(format core.Format, usage core.Usage) bool {
	if format != core.FormatAnthropic || usage.Empty() {
		return true
	}
	return usage.PromptCacheUsed()
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
func coolable(err error, mode core.AuthMode) bool {
	var ue *core.UpstreamError
	if !errors.As(err, &ue) {
		return false
	}
	if mode == core.AuthModePassthrough {
		// On a passthrough deployment the credential is the caller's own, so an
		// authentication or quota rejection describes that caller, not the
		// deployment. Counting it would let one developer's expired
		// subscription token eject the shared upstream for everyone else.
		switch ue.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
			return false
		}
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
	return d + time.Duration(float64(d)*r.cfg.Backoff.JitterFactor()*jitter)
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

// noteEjection reports a deployment that this failure just put into cooldown.
//
// It asks the state store rather than being told, because whether a failure
// ejects is the store's decision: it depends on how many failures preceded it
// inside the window, and with Redis that tally is shared across every replica.
// The extra round trip is on the failure path only, and a deployment that was
// still a candidate a moment ago was by definition not cooling down before —
// so a "yes" here means this attempt is what ejected it.
func (r *Router) noteEjection(ctx context.Context, dep *config.Deployment) {
	if r.ejected == nil {
		return
	}
	cooling, err := r.state.InCooldown(ctx, dep.ID(), r.now())
	if err != nil || !cooling {
		return
	}
	r.log.Warn("deployment ejected",
		"deployment", dep.ID(), "model", dep.ModelName, "period", r.cfg.Cooldown.Period)
	r.ejected(dep.ModelName, dep.ID())
}
