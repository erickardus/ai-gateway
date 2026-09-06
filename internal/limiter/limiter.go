// Package limiter provides request- and token-per-minute counters.
//
// Windows are monotonic: each counter tracks the start of its own window and
// rolls over once a minute has elapsed. This deliberately avoids keying buckets
// on a wall-clock "HH-MM" string, which wraps every 24 hours and skews whenever
// the clock is adjusted.
package limiter

import (
	"sync"
	"time"
)

// Window is the counting period.
const Window = time.Minute

// counter is one deployment's or key's usage within the current window.
type counter struct {
	windowStart time.Time
	requests    int
	tokens      int
}

// Limiter tracks requests and tokens per subject within a rolling window.
// The zero value is not usable; call New.
type Limiter struct {
	mu       sync.Mutex
	counters map[string]*counter
	now      func() time.Time
	// creations counts counters created, to pace the stale sweep.
	creations int
}

// New returns a Limiter using the wall clock.
func New() *Limiter {
	return &Limiter{counters: make(map[string]*counter), now: time.Now}
}

// sweepEvery is how many counter creations trigger a sweep of stale entries.
// Sweeping on a counter is cheaper than running a background goroutine and keeps
// the limiter free of lifecycle management.
const sweepEvery = 512

// staleAfter is how long a counter may sit untouched before it is dropped. A
// counter older than this contributes nothing: its window has long rolled over.
const staleAfter = 4 * Window

// sweepLocked drops counters whose windows are long expired. Callers must hold
// the mutex.
//
// Without this the map only grows: a gateway minting short-lived keys — one per
// CI job, say — would retain a counter for every key it ever admitted, including
// keys since revoked.
func (l *Limiter) sweepLocked(now time.Time) {
	for subject, c := range l.counters {
		if now.Sub(c.windowStart) > staleAfter {
			delete(l.counters, subject)
		}
	}
}

// currentLocked returns the subject's counter, rolling the window if it has
// expired. Callers must hold the mutex.
func (l *Limiter) currentLocked(subject string) *counter {
	now := l.now()
	c, ok := l.counters[subject]
	if !ok {
		l.creations++
		if l.creations%sweepEvery == 0 {
			l.sweepLocked(now)
		}
		c = &counter{windowStart: now}
		l.counters[subject] = c
		return c
	}
	if now.Sub(c.windowStart) >= Window {
		c.windowStart = now
		c.requests = 0
		c.tokens = 0
	}
	return c
}

// Allow reports whether one more request fits within the given limits, without
// recording it. A limit of zero means unlimited.
func (l *Limiter) Allow(subject string, rpm, tpm int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.currentLocked(subject)
	if rpm > 0 && c.requests >= rpm {
		return false
	}
	if tpm > 0 && c.tokens >= tpm {
		return false
	}
	return true
}

// Reserve records one request against the subject if it fits within the limits,
// reporting whether it was admitted. Checking and recording happen under a
// single lock, so concurrent callers cannot both pass the same final slot.
func (l *Limiter) Reserve(subject string, rpm, tpm int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.currentLocked(subject)
	if rpm > 0 && c.requests >= rpm {
		return false
	}
	if tpm > 0 && c.tokens >= tpm {
		return false
	}
	c.requests++
	return true
}

// Claim is one subject's allowance within a set reserved together.
type Claim struct {
	Subject  string
	RPM, TPM int
}

// ReserveAll records one request against every subject if it fits within all of
// their limits, reporting the index of the first that refused or -1 on success.
//
// All or nothing, under one lock. Reserving each subject separately would let a
// request refused by an outer scope keep the increment it had already made to
// an inner one, so a key's own window would run ahead of the requests it
// actually served.
func (l *Limiter) ReserveAll(claims []Claim) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	counters := make([]*counter, len(claims))
	for i, c := range claims {
		cur := l.currentLocked(c.Subject)
		if c.RPM > 0 && cur.requests >= c.RPM {
			return i
		}
		if c.TPM > 0 && cur.tokens >= c.TPM {
			return i
		}
		counters[i] = cur
	}
	for _, cur := range counters {
		cur.requests++
	}
	return -1
}

// AddTokens records token usage reported after a response completes.
func (l *Limiter) AddTokens(subject string, tokens int) {
	if tokens <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.currentLocked(subject).tokens += tokens
}

// Snapshot returns the subject's current request and token counts.
func (l *Limiter) Snapshot(subject string) (requests, tokens int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.currentLocked(subject)
	return c.requests, c.tokens
}

// Forget removes a subject's counter. It is called when a virtual key is
// revoked, so a deleted key leaves nothing behind.
func (l *Limiter) Forget(subject string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.counters, subject)
}

// Len reports how many counters are held, for tests and diagnostics.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.counters)
}
