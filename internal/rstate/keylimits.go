package rstate

import (
	"context"

	"github.com/erickardus/ai-gateway/internal/limiter"
)

// Namespaces for the per-key counters.
//
// They are deliberately distinct from the deployment counters' "rpm" and "tpm"
// rather than the same keyspace addressed by a different id. The two count
// different things — a deployment's capacity at its provider, and a virtual
// key's allowance from the operator — and sharing one namespace would mean a
// key whose hash collided with a deployment id drew down that deployment's
// window. Separating them costs nothing and removes the question.
const (
	keyRPMNamespace = "key.rpm"
	keyTPMNamespace = "key.tpm"
)

// KeyLimiter enforces per-virtual-key rate limits across every instance sharing
// this Redis, satisfying auth.KeyLimiter.
//
// It reuses the same Lua scripts as the deployment limits, for the same reason
// those are scripts: INCR followed by EXPIRE is two round trips, and an
// instance that dies between them leaves a counter with no TTL — here, a key
// permanently at its limit, recoverable only by hand.
//
// Like every other read of shared state, it degrades to counting in-process
// when Redis is unreachable rather than refusing the request. That is a limit
// enforced per replica for the duration of the outage, which is the state a
// gateway without Redis runs in permanently.
type KeyLimiter struct {
	store *Store
	local *limiter.Limiter
}

// KeyLimiter returns a shared per-key rate limiter backed by this store.
func (s *Store) KeyLimiter() *KeyLimiter {
	return &KeyLimiter{store: s, local: limiter.New()}
}

// Reserve implements auth.KeyLimiter.
func (k *KeyLimiter) Reserve(ctx context.Context, subject string, rpm, tpm int) (bool, error) {
	if rpm <= 0 && tpm <= 0 {
		return true, nil
	}
	s := k.store
	rctx, cancel := s.ctx(ctx)
	defer cancel()

	res, err := s.scripts.reserve.Run(rctx, s.client,
		[]string{s.windowKey(keyRPMNamespace, subject), s.windowKey(keyTPMNamespace, subject)},
		rpm, tpm, windowSeconds).Int()
	if s.degrade(err, "key_reserve") {
		return k.local.Reserve(subject, rpm, tpm), nil
	}
	s.recovered()
	return res == 1, nil
}

// ReserveAll implements auth.KeyLimiter.
//
// One script over the whole chain: one round trip whatever the hierarchy's
// depth, and atomic, so a request refused by a team does not leave an increment
// on the key's own window.
//
// Degrading falls back to the local limiter's ReserveAll rather than to
// admitting the request, which keeps the all-or-nothing property while Redis is
// unreachable — per instance, like every other limit in that state.
func (k *KeyLimiter) ReserveAll(ctx context.Context, claims []limiter.Claim) (int, error) {
	if len(claims) == 0 {
		return -1, nil
	}
	// A chain that declares no limits anywhere has nothing to reserve, and
	// running a script to discover that would be a round trip per request for
	// the common case of a gateway with no rate limits configured.
	limited := false
	for _, c := range claims {
		if c.RPM > 0 || c.TPM > 0 {
			limited = true
			break
		}
	}
	if !limited {
		return -1, nil
	}

	s := k.store
	rctx, cancel := s.ctx(ctx)
	defer cancel()

	keys := make([]string, 0, len(claims)*2)
	args := make([]any, 0, len(claims)*2+1)
	for _, c := range claims {
		keys = append(keys,
			s.windowKey(keyRPMNamespace, c.Subject),
			s.windowKey(keyTPMNamespace, c.Subject))
		args = append(args, c.RPM, c.TPM)
	}
	args = append(args, windowSeconds)

	res, err := s.scripts.reserveAll.Run(rctx, s.client, keys, args...).Int()
	if s.degrade(err, "key_reserve_all") {
		return k.local.ReserveAll(claims), nil
	}
	s.recovered()
	// The script answers with a 1-based index, or 0 when everything fit.
	return res - 1, nil
}

// AddTokens implements auth.KeyLimiter.
func (k *KeyLimiter) AddTokens(ctx context.Context, subject string, tokens int) {
	if tokens <= 0 {
		return
	}
	s := k.store
	rctx, cancel := s.ctx(ctx)
	defer cancel()

	err := s.scripts.addTokens.Run(rctx, s.client,
		[]string{s.windowKey(keyTPMNamespace, subject)}, tokens, windowSeconds).Err()
	if s.degrade(err, "key_add_tokens") {
		k.local.AddTokens(subject, tokens)
		return
	}
	s.recovered()
}

// Snapshot reports the subject's current shared counts, for diagnostics and
// tests. It mirrors the local limiter's, so a test can ask the same question of
// either implementation.
//
// It reads rather than reserves, so it is deliberately not part of
// auth.KeyLimiter: the request path never needs a count it is not also
// consuming, and an interface method for one would invite a check-then-act
// pair where the script exists to avoid exactly that.
func (k *KeyLimiter) Snapshot(ctx context.Context, subject string) (requests, tokens int) {
	s := k.store
	rctx, cancel := s.ctx(ctx)
	defer cancel()

	vals, err := s.client.MGet(rctx,
		s.windowKey(keyRPMNamespace, subject),
		s.windowKey(keyTPMNamespace, subject)).Result()
	if err != nil || len(vals) < 2 {
		return 0, 0
	}
	return atoiValue(vals[0]), atoiValue(vals[1])
}

// atoiValue reads a counter Redis returned as a string, or nil for absent.
func atoiValue(v any) int {
	s, ok := v.(string)
	if !ok {
		return 0
	}
	return parseInt(s)
}

// Forget implements auth.KeyLimiter.
//
// Only the current window is deleted. Every other bucket already carries a TTL
// and expires on its own, so hunting for them would be a scan of the keyspace
// to save a minute of counters belonging to a key that can no longer be used.
func (k *KeyLimiter) Forget(ctx context.Context, subject string) {
	k.local.Forget(subject)

	s := k.store
	rctx, cancel := s.ctx(ctx)
	defer cancel()
	err := s.client.Del(rctx,
		s.windowKey(keyRPMNamespace, subject),
		s.windowKey(keyTPMNamespace, subject)).Err()
	if err != nil {
		// A revoked key cannot authenticate again, so a counter left behind is
		// a minute of dead data rather than a limit anyone can hit.
		s.log.Warn("clear rate-limit counters for revoked key", "error", err)
	}
}
