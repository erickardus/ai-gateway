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
}

// New returns a Limiter using the wall clock.
func New() *Limiter {
	return &Limiter{counters: make(map[string]*counter), now: time.Now}
}

// NewWithClock returns a Limiter driven by the supplied clock, for tests.
func NewWithClock(now func() time.Time) *Limiter {
	return &Limiter{counters: make(map[string]*counter), now: now}
}

// currentLocked returns the subject's counter, rolling the window if it has
// expired. Callers must hold the mutex.
func (l *Limiter) currentLocked(subject string) *counter {
	now := l.now()
	c, ok := l.counters[subject]
	if !ok {
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
