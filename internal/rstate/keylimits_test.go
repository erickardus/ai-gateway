package rstate

import (
	"context"
	"sync"
	"testing"
)

// The regression this whole type exists for: a virtual key's own rpm_limit was
// counted per process, so a fleet handed every key its limit once per replica
// while docs/observability.md promised the opposite.
func TestKeyRateLimitSharedAcrossInstances(t *testing.T) {
	prefix := uniquePrefix(t)
	a, b := newStore(t, prefix).KeyLimiter(), newStore(t, prefix).KeyLimiter()
	ctx := context.Background()

	const rpm = 10
	admitted := 0
	for i := range 40 {
		limiter := a
		if i%2 == 1 {
			limiter = b
		}
		ok, err := limiter.Reserve(ctx, "key-hash-1", rpm, 0)
		if err != nil {
			t.Fatalf("Reserve: %v", err)
		}
		if ok {
			admitted++
		}
	}
	if admitted != rpm {
		t.Errorf("admitted %d of 40 across two instances, want exactly %d: the key limit is not shared", admitted, rpm)
	}
}

// Tokens reported by a completed response on one instance must count against
// the key's allowance on every other.
func TestKeyTokenLimitSharedAcrossInstances(t *testing.T) {
	prefix := uniquePrefix(t)
	a, b := newStore(t, prefix).KeyLimiter(), newStore(t, prefix).KeyLimiter()
	ctx := context.Background()

	const tpm = 1000
	if ok, err := a.Reserve(ctx, "key-hash-2", 0, tpm); err != nil || !ok {
		t.Fatalf("first Reserve = %v, %v", ok, err)
	}
	a.AddTokens(ctx, "key-hash-2", tpm)

	ok, err := b.Reserve(ctx, "key-hash-2", 0, tpm)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if ok {
		t.Error("the other instance admitted a request after the key's whole token allowance was spent elsewhere")
	}
}

// A key hash and a deployment id must never draw on the same window, or a
// collision between the two namespaces would spend one's allowance on the
// other.
func TestKeyLimitsDoNotShareTheDeploymentNamespace(t *testing.T) {
	prefix := uniquePrefix(t)
	store := newStore(t, prefix)
	keys := store.KeyLimiter()
	ctx := context.Background()

	const subject = "same-name"
	for range 5 {
		if ok, _ := keys.Reserve(ctx, subject, 5, 0); !ok {
			t.Fatal("key limiter refused within its own allowance")
		}
	}
	if ok, _ := keys.Reserve(ctx, subject, 5, 0); ok {
		t.Fatal("key limiter admitted past its allowance")
	}

	// The deployment of the same name must still have its full window.
	admitted := 0
	for range 5 {
		if ok, _ := store.Reserve(ctx, subject, 5, 0); ok {
			admitted++
		}
	}
	if admitted != 5 {
		t.Errorf("deployment admitted %d of 5, want 5: the key counters ate its window", admitted)
	}
}

// Reserve must be atomic: two instances racing for the last slot cannot both
// win it.
func TestKeyReserveIsAtomicUnderConcurrency(t *testing.T) {
	prefix := uniquePrefix(t)
	limiters := []*KeyLimiter{newStore(t, prefix).KeyLimiter(), newStore(t, prefix).KeyLimiter()}
	ctx := context.Background()

	const rpm = 25
	var mu sync.Mutex
	admitted := 0
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if ok, _ := limiters[i%len(limiters)].Reserve(ctx, "key-hash-3", rpm, 0); ok {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if admitted != rpm {
		t.Errorf("admitted %d concurrent requests, want exactly %d", admitted, rpm)
	}
}

// A revoked key's counters go with it, on every instance rather than only the
// one that served the delete.
func TestForgetClearsSharedKeyCounters(t *testing.T) {
	prefix := uniquePrefix(t)
	a, b := newStore(t, prefix).KeyLimiter(), newStore(t, prefix).KeyLimiter()
	ctx := context.Background()

	for range 5 {
		a.Reserve(ctx, "key-hash-4", 5, 0)
	}
	if ok, _ := b.Reserve(ctx, "key-hash-4", 5, 0); ok {
		t.Fatal("limit was not reached")
	}

	a.Forget(ctx, "key-hash-4")

	if ok, err := b.Reserve(ctx, "key-hash-4", 5, 0); err != nil || !ok {
		t.Errorf("Reserve after Forget = %v, %v; the counter survived the revocation on another instance", ok, err)
	}
}

// An unlimited key must cost no round trip at all.
func TestKeyLimiterSkipsRedisWhenUnlimited(t *testing.T) {
	prefix := uniquePrefix(t)
	limiter := newStore(t, prefix).KeyLimiter()

	ok, err := limiter.Reserve(context.Background(), "key-hash-5", 0, 0)
	if err != nil || !ok {
		t.Fatalf("Reserve with no limits = %v, %v", ok, err)
	}
}
