package limiter

import (
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Allow must not consume capacity; Reserve must.
func TestAllowDoesNotConsume(t *testing.T) {
	l := New()
	const rpm = 2

	for range 10 {
		if !l.Allow("k", rpm, 0) {
			t.Fatal("Allow consumed capacity; it must only report it")
		}
	}
	if req, _ := l.Snapshot("k"); req != 0 {
		t.Fatalf("Allow recorded %d requests, want 0", req)
	}

	if !l.Reserve("k", rpm, 0) || !l.Reserve("k", rpm, 0) {
		t.Fatal("Reserve rejected a request under the limit")
	}
	if l.Reserve("k", rpm, 0) {
		t.Fatal("Reserve admitted a request beyond the limit")
	}
	if l.Allow("k", rpm, 0) {
		t.Fatal("Allow reported capacity that Reserve had exhausted")
	}
}

func TestZeroLimitMeansUnlimited(t *testing.T) {
	l := New()
	for range 1000 {
		if !l.Reserve("k", 0, 0) {
			t.Fatal("a zero limit must mean unlimited")
		}
	}
}

// Concurrent callers must not both take the final slot.
func TestReserveIsAtomicUnderConcurrency(t *testing.T) {
	l := New()
	const (
		limit   = 50
		callers = 500
	)

	var admitted int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			if l.Reserve("shared", limit, 0) {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	if admitted != limit {
		t.Errorf("admitted %d requests, want exactly %d", admitted, limit)
	}
}

func TestSubjectsAreIndependent(t *testing.T) {
	l := New()
	if !l.Reserve("a", 1, 0) {
		t.Fatal("first subject rejected")
	}
	if l.Reserve("a", 1, 0) {
		t.Fatal("first subject exceeded its limit")
	}
	if !l.Reserve("b", 1, 0) {
		t.Fatal("a second subject was affected by the first's usage")
	}
}

func TestWindowRollover(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := New()
		if !l.Reserve("k", 1, 0) {
			t.Fatal("first request rejected")
		}
		if l.Reserve("k", 1, 0) {
			t.Fatal("second request admitted within the window")
		}

		time.Sleep(Window - time.Millisecond)
		if l.Reserve("k", 1, 0) {
			t.Fatal("window rolled over early")
		}

		time.Sleep(2 * time.Millisecond)
		if !l.Reserve("k", 1, 0) {
			t.Fatal("window did not roll over")
		}
		if req, tok := l.Snapshot("k"); req != 1 || tok != 0 {
			t.Errorf("after rollover: requests=%d tokens=%d, want 1 and 0", req, tok)
		}
	})
}

func TestAddTokensIgnoresNonPositive(t *testing.T) {
	l := New()
	l.AddTokens("k", 0)
	l.AddTokens("k", -5)
	if _, tok := l.Snapshot("k"); tok != 0 {
		t.Errorf("tokens = %d, want 0", tok)
	}
	l.AddTokens("k", 7)
	if _, tok := l.Snapshot("k"); tok != 7 {
		t.Errorf("tokens = %d, want 7", tok)
	}
}

// The counter map must not grow without bound: a gateway minting short-lived
// keys would otherwise retain an entry for every key it ever admitted.
func TestStaleCountersAreSwept(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := New()
		for i := range sweepEvery {
			l.Reserve(fmt.Sprintf("ephemeral-%d", i), 10, 0)
		}
		if l.Len() < sweepEvery {
			t.Fatalf("held %d counters, expected about %d before any sweep", l.Len(), sweepEvery)
		}

		// Let every counter go stale, then create enough new subjects to
		// trigger a sweep.
		time.Sleep(staleAfter + time.Minute)
		for i := range sweepEvery {
			l.Reserve(fmt.Sprintf("fresh-%d", i), 10, 0)
		}
		if l.Len() > sweepEvery+1 {
			t.Errorf("held %d counters after a sweep, want about %d: stale entries were retained", l.Len(), sweepEvery)
		}
	})
}

// A revoked key's counter is released immediately.
func TestForgetReleasesCounter(t *testing.T) {
	l := New()
	l.Reserve("doomed", 1, 0)
	if l.Len() != 1 {
		t.Fatalf("Len = %d, want 1", l.Len())
	}
	l.Forget("doomed")
	if l.Len() != 0 {
		t.Errorf("Len = %d, want 0 after Forget", l.Len())
	}
}
