// Package router selects a deployment for each request and drives retries and
// fallbacks.
package router

import (
	"context"
	"sync"
	"time"

	"github.com/erickardus/ai-gateway/internal/limiter"
)

// maxLatencySamples bounds the rolling latency window per deployment, so memory
// stays flat regardless of traffic.
const maxLatencySamples = 10

// StateStore holds the runtime state routing decisions are made from. Every
// method is context-first and returns an error so that a Redis-backed
// implementation, shared by several gateway instances, can satisfy the same
// interface without changing any caller.
type StateStore interface {
	// BeginRequest records that a request has been dispatched to a deployment.
	BeginRequest(ctx context.Context, id string) error
	// EndRequest records completion. It must be called exactly once for each
	// BeginRequest, or the in-flight count will drift.
	EndRequest(ctx context.Context, id string, took time.Duration, ok bool) error
	// InFlight reports outstanding requests for a deployment.
	InFlight(ctx context.Context, id string) (int, error)
	// MeanLatency reports the mean of the recent latency samples. ok is false
	// when the deployment has not been observed yet.
	MeanLatency(ctx context.Context, id string) (mean time.Duration, ok bool, err error)
	// InCooldown reports whether a deployment is currently ejected.
	InCooldown(ctx context.Context, id string, now time.Time) (bool, error)
	// RecordFailure counts a failure and ejects the deployment once it has
	// failed more than allowedFails times within the period.
	RecordFailure(ctx context.Context, id string, now time.Time, allowedFails int, period time.Duration) error
	// Allow reports whether a deployment has capacity, without consuming any.
	// Filtering uses this so that budget is only spent on the deployment
	// actually dispatched to.
	Allow(ctx context.Context, id string, rpm, tpm int) (bool, error)
	// Reserve consumes one request's capacity, reporting whether it fit.
	Reserve(ctx context.Context, id string, rpm, tpm int) (bool, error)
	// AddTokens records token usage reported by a completed response.
	AddTokens(ctx context.Context, id string, tokens int) error
	// TokensUsed reports tokens consumed in the current window. It is the read
	// half of AddTokens, so usage-based routing can go through the interface
	// rather than reaching into a concrete implementation.
	TokensUsed(ctx context.Context, id string) (int, error)
	// Affinity reports which deployment last served a request carrying this
	// prompt-prefix fingerprint, so the next one can reuse the prompt cache it
	// warmed. A miss is not an error.
	Affinity(ctx context.Context, fingerprint string) (string, bool, error)
	// SetAffinity pins a fingerprint to a deployment for ttl. It is called only
	// after a successful response, so a pin always names a deployment that has
	// actually served this prefix.
	SetAffinity(ctx context.Context, fingerprint, id string, ttl time.Duration) error
}

// maxAffinityEntries bounds each generation of the affinity map, so a gateway
// seeing an unbounded stream of distinct prompts keeps flat memory. Two
// generations are held at once, so the real ceiling is twice this.
const maxAffinityEntries = 10_000

// deploymentState is the in-memory state of a single deployment.
type deploymentState struct {
	inFlight int
	latency  []time.Duration

	cooldownUntil time.Time
	failures      int
	failureWindow time.Time
}

// MemState is an in-process StateStore. It is correct for a single gateway
// instance; a shared implementation behind the same interface is what makes
// several instances agree.
type MemState struct {
	mu     sync.RWMutex
	states map[string]*deploymentState
	limits *limiter.Limiter

	// Prompt-prefix pins are held in two generations rather than one map with
	// per-entry eviction. When the live generation fills it becomes the older
	// one and a fresh map takes over, which bounds memory in O(1) without ever
	// walking the map to find something to evict — and a pin demoted to the
	// older generation is still served, so a busy conversation is not dropped
	// merely because unrelated traffic filled the map.
	affinityMu   sync.Mutex
	affinityCur  map[string]affinityPin
	affinityPrev map[string]affinityPin

	now func() time.Time
}

// affinityPin is one prompt prefix's deployment, with its expiry.
type affinityPin struct {
	deployment string
	expires    time.Time
}

// NewMemState returns an in-memory state store.
func NewMemState() *MemState {
	return &MemState{
		states:      make(map[string]*deploymentState),
		limits:      limiter.New(),
		affinityCur: make(map[string]affinityPin),
		now:         time.Now,
	}
}

// Affinity implements StateStore.
func (s *MemState) Affinity(_ context.Context, fingerprint string) (string, bool, error) {
	s.affinityMu.Lock()
	defer s.affinityMu.Unlock()

	pin, ok := s.affinityCur[fingerprint]
	if !ok {
		pin, ok = s.affinityPrev[fingerprint]
	}
	if !ok || !s.now().Before(pin.expires) {
		return "", false, nil
	}
	return pin.deployment, true, nil
}

// SetAffinity implements StateStore.
func (s *MemState) SetAffinity(_ context.Context, fingerprint, id string, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	s.affinityMu.Lock()
	defer s.affinityMu.Unlock()

	if len(s.affinityCur) >= maxAffinityEntries {
		s.affinityPrev = s.affinityCur
		s.affinityCur = make(map[string]affinityPin, maxAffinityEntries/4)
	}
	s.affinityCur[fingerprint] = affinityPin{deployment: id, expires: s.now().Add(ttl)}
	return nil
}

// Prepare pre-creates state for known deployment IDs so that the read paths —
// cooldown and in-flight checks, which run once per candidate on every request —
// never have to insert and can therefore take a shared read lock.
func (s *MemState) Prepare(ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if _, ok := s.states[id]; !ok {
			s.states[id] = &deploymentState{}
		}
	}
}

// readLocked returns a deployment's state for reading, or nil when it has none
// yet. Callers must hold at least the read lock.
func (s *MemState) readLocked(id string) *deploymentState { return s.states[id] }

// getLocked returns a deployment's state, creating it if absent. Callers must
// hold the write lock.
func (s *MemState) getLocked(id string) *deploymentState {
	st, ok := s.states[id]
	if !ok {
		st = &deploymentState{}
		s.states[id] = st
	}
	return st
}

// BeginRequest implements StateStore.
func (s *MemState) BeginRequest(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getLocked(id).inFlight++
	return nil
}

// EndRequest implements StateStore. The in-flight count is clamped at zero so
// that an unbalanced call can never drive it negative and make a broken
// deployment look permanently idle to the least-busy strategy.
func (s *MemState) EndRequest(_ context.Context, id string, took time.Duration, ok bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.getLocked(id)
	if st.inFlight > 0 {
		st.inFlight--
	}
	if ok {
		st.latency = append(st.latency, took)
		if len(st.latency) > maxLatencySamples {
			st.latency = st.latency[len(st.latency)-maxLatencySamples:]
		}
	}
	return nil
}

// InFlight implements StateStore.
func (s *MemState) InFlight(_ context.Context, id string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := s.readLocked(id)
	if st == nil {
		return 0, nil
	}
	return st.inFlight, nil
}

// MeanLatency implements StateStore.
func (s *MemState) MeanLatency(_ context.Context, id string) (time.Duration, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := s.readLocked(id)
	if st == nil || len(st.latency) == 0 {
		return 0, false, nil
	}
	var total time.Duration
	for _, d := range st.latency {
		total += d
	}
	return total / time.Duration(len(st.latency)), true, nil
}

// InCooldown implements StateStore.
func (s *MemState) InCooldown(_ context.Context, id string, now time.Time) (bool, error) {
	// The common case is "not cooling down", which needs only a read lock.
	s.mu.RLock()
	st := s.readLocked(id)
	if st == nil || st.cooldownUntil.IsZero() {
		s.mu.RUnlock()
		return false, nil
	}
	cooling := now.Before(st.cooldownUntil)
	s.mu.RUnlock()
	if cooling {
		return true, nil
	}

	// Expired: take the write lock to clear it.
	s.mu.Lock()
	defer s.mu.Unlock()
	st = s.getLocked(id)
	if st.cooldownUntil.IsZero() || now.Before(st.cooldownUntil) {
		return !st.cooldownUntil.IsZero(), nil
	}
	// Expired: recovery is by elapsed time, and the failure tally resets with it
	// so a recovered deployment starts from a clean slate.
	st.cooldownUntil = time.Time{}
	st.failures = 0
	return false, nil
}

// RecordFailure implements StateStore. A configured allowedFails always means
// what it says: exceeding it ejects the deployment for the cooldown period.
func (s *MemState) RecordFailure(_ context.Context, id string, now time.Time, allowedFails int, period time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.getLocked(id)

	if st.failureWindow.IsZero() || now.Sub(st.failureWindow) >= period {
		st.failureWindow = now
		st.failures = 0
	}
	st.failures++
	if st.failures > allowedFails {
		st.cooldownUntil = now.Add(period)
	}
	return nil
}

// Allow implements StateStore.
func (s *MemState) Allow(_ context.Context, id string, rpm, tpm int) (bool, error) {
	return s.limits.Allow(id, rpm, tpm), nil
}

// Reserve implements StateStore.
func (s *MemState) Reserve(_ context.Context, id string, rpm, tpm int) (bool, error) {
	return s.limits.Reserve(id, rpm, tpm), nil
}

// TokensUsed implements StateStore.
func (s *MemState) TokensUsed(_ context.Context, id string) (int, error) {
	_, tokens := s.limits.Snapshot(id)
	return tokens, nil
}

// AddTokens implements StateStore.
func (s *MemState) AddTokens(_ context.Context, id string, tokens int) error {
	s.limits.AddTokens(id, tokens)
	return nil
}
