package auth

import (
	"context"

	"github.com/erickardus/ai-gateway/internal/limiter"
)

// KeyLimiter enforces a virtual key's own request- and token-per-minute
// allowance.
//
// It is an interface for exactly the reason router.StateStore is one: a limit
// counted inside one process is silently multiplied by the replica count, so a
// key with rpm_limit: 60 gets 180 across three instances. The deployment-side
// limits have had a shared implementation from the start; this is the same
// state, belonging to the caller rather than to the upstream, and it needs the
// same treatment.
//
// Reserve is the only method that can fail, and a shared implementation should
// fail rarely: losing the backing store degrades to counting locally rather
// than refusing traffic, on the same reasoning as the routing state store —
// enforcing a limit per instance is a smaller failure than not serving at all.
type KeyLimiter interface {
	// Reserve charges one request against the subject, reporting whether it
	// fit within the limits. A limit of zero means unlimited.
	Reserve(ctx context.Context, subject string, rpm, tpm int) (bool, error)
	// ReserveAll charges one request against every subject at once, returning
	// the index of the first that refused or -1 where all fit.
	//
	// It takes a set for two reasons, and the second is why it is not merely
	// Reserve in a loop. A request is charged against its key and every scope
	// above it, so a loop is a round trip per level on the hot path — and a
	// loop is not atomic, so a request refused by an outer scope keeps the
	// increment it already made to an inner one. One call is one round trip and
	// all-or-nothing.
	ReserveAll(ctx context.Context, claims []limiter.Claim) (int, error)
	// AddTokens records what a completed response consumed. It is called after
	// the response has been relayed, so it reports rather than admits.
	AddTokens(ctx context.Context, subject string, tokens int)
	// Forget drops a subject's counters. It is called when a key is revoked,
	// so a deleted key leaves nothing behind.
	Forget(ctx context.Context, subject string)
}

// LocalLimiter counts within one process. It is what a gateway running as a
// single instance uses, and what a shared limiter falls back to while its
// backing store is unreachable.
type LocalLimiter struct{ l *limiter.Limiter }

// NewLocalLimiter returns a per-process key limiter.
func NewLocalLimiter() *LocalLimiter { return &LocalLimiter{l: limiter.New()} }

// Reserve implements KeyLimiter. It never returns an error: the counters are
// in this process, so there is nothing that can be unreachable.
func (k *LocalLimiter) Reserve(_ context.Context, subject string, rpm, tpm int) (bool, error) {
	return k.l.Reserve(subject, rpm, tpm), nil
}

// ReserveAll implements KeyLimiter.
func (k *LocalLimiter) ReserveAll(_ context.Context, claims []limiter.Claim) (int, error) {
	return k.l.ReserveAll(claims), nil
}

// AddTokens implements KeyLimiter.
func (k *LocalLimiter) AddTokens(_ context.Context, subject string, tokens int) {
	k.l.AddTokens(subject, tokens)
}

// Forget implements KeyLimiter.
func (k *LocalLimiter) Forget(_ context.Context, subject string) { k.l.Forget(subject) }

// Snapshot reports the subject's current counts, for diagnostics and tests.
func (k *LocalLimiter) Snapshot(subject string) (requests, tokens int) {
	return k.l.Snapshot(subject)
}

// Len reports how many counters are held.
func (k *LocalLimiter) Len() int { return k.l.Len() }
