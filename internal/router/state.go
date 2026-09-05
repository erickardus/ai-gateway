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
}

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
	mu     sync.Mutex
	states map[string]*deploymentState
	limits *limiter.Limiter
}

// NewMemState returns an empty in-memory state store.
func NewMemState() *MemState {
	return &MemState{states: make(map[string]*deploymentState), limits: limiter.New()}
}

// NewMemStateWithClock returns a state store whose rate limiter uses the given
// clock, for tests.
func NewMemStateWithClock(now func() time.Time) *MemState {
	return &MemState{states: make(map[string]*deploymentState), limits: limiter.NewWithClock(now)}
}

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
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(id).inFlight, nil
}

// MeanLatency implements StateStore.
func (s *MemState) MeanLatency(_ context.Context, id string) (time.Duration, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.getLocked(id)
	if len(st.latency) == 0 {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.getLocked(id)
	if st.cooldownUntil.IsZero() {
		return false, nil
	}
	if now.Before(st.cooldownUntil) {
		return true, nil
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

// AddTokens implements StateStore.
func (s *MemState) AddTokens(_ context.Context, id string, tokens int) error {
	s.limits.AddTokens(id, tokens)
	return nil
}
