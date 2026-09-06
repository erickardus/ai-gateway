// Package reqlog keeps the last few thousand completed requests in memory so
// an operator can see what the gateway actually did, rather than inferring it
// from counters.
//
// It exists because metrics and the spend ledger both aggregate, and the
// questions an operator asks about a gateway are usually about one request:
// why did this call fall back, did that conversation hit the prompt cache it
// warmed, which deployment served the request that took nine seconds. A counter
// can say a fallback happened; only a record says which key, to which group,
// away from which deployment.
//
// What it deliberately does not hold is any part of a request or response body.
// The gateway relays those bytes without materializing them, and a debugging
// convenience is not a good enough reason to start keeping other people's
// prompts in a buffer an admin page reads.
//
// The buffer is per process. With several instances behind a load balancer each
// holds only what it served, which is why records carry no instance identity and
// the UI says so rather than implying it is showing the whole fleet.
package reqlog

import (
	"sync"
	"time"

	"github.com/erickardus/ai-gateway/internal/core"
)

// Record is one completed request, as the gateway saw it.
//
// Durations are milliseconds and floats rather than time.Duration because this
// type is encoded straight to the UI, and a Duration marshals as a nanosecond
// integer that every consumer then has to know to divide.
type Record struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`

	KeyAlias string `json:"key_alias,omitempty"`
	// SpendSubject is the identity this request's spend accumulated under: the
	// key's hash for an ordinary key, and the person for one issued by an SSO
	// login. It is deliberately not called a hash — for an SSO key it is not
	// one, and a field named for the wrong thing gets labelled for the wrong
	// thing wherever it is displayed.
	SpendSubject string `json:"spend_subject,omitempty"`
	// Scope is the innermost project, team or organisation the key belongs to,
	// which is the level an operator scanning this list is usually filtering by.
	Scope string `json:"scope,omitempty"`

	ModelGroup string `json:"model_group,omitempty"`
	Deployment string `json:"deployment,omitempty"`
	Format     string `json:"format,omitempty"`
	Streaming  bool   `json:"streaming"`

	Outcome      string `json:"outcome"`
	RejectReason string `json:"reject_reason,omitempty"`
	StatusClass  string `json:"status_class,omitempty"`

	// Retries and Fallbacks are what the router had to do to get an answer, and
	// PromptAffinity whether the prefix pin was honoured. Together they are the
	// routing decision, which is otherwise invisible once the response is sent.
	Retries        int    `json:"retries"`
	Fallbacks      int    `json:"fallbacks"`
	PromptAffinity string `json:"prompt_affinity,omitempty"`

	Usage Usage `json:"usage"`
	// Cost is zero on a passthrough deployment, where the caller's own
	// subscription was billed. Billable is what tells that apart from a
	// deployment nobody priced.
	Cost         float64 `json:"cost"`
	Billable     bool    `json:"billable"`
	CacheSavings float64 `json:"cache_savings"`

	LatencyMS       float64 `json:"latency_ms"`
	TTFTMS          float64 `json:"ttft_ms,omitempty"`
	StreamCompleted bool    `json:"stream_completed,omitempty"`
	ThroughputTPS   float64 `json:"throughput_tps,omitempty"`

	RequestBytes  int64 `json:"request_bytes,omitempty"`
	ResponseBytes int64 `json:"response_bytes,omitempty"`
}

// Usage restates core.Usage with JSON names.
//
// core.Usage is an internal accounting type with no JSON tags, so encoding it
// directly would publish Go field names — CacheReadTokens — as this package's
// API. Restating it here keeps the wire shape a deliberate choice rather than a
// side effect of a struct definition three packages away.
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// UsageOf converts the gateway's internal counters.
func UsageOf(u core.Usage) Usage {
	return Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
	}
}

// Filter narrows a read. An empty field matches everything, so the zero Filter
// is "all records".
type Filter struct {
	SpendSubject string
	ModelGroup   string
	Deployment   string
	Outcome      string
	// Errors restricts the result to requests that did not succeed, which is
	// the filter an operator reaches for first and the one that cannot be
	// expressed by naming a single outcome.
	Errors bool
}

func (f Filter) matches(r *Record) bool {
	switch {
	case f.SpendSubject != "" && f.SpendSubject != r.SpendSubject:
		return false
	case f.ModelGroup != "" && f.ModelGroup != r.ModelGroup:
		return false
	case f.Deployment != "" && f.Deployment != r.Deployment:
		return false
	case f.Outcome != "" && f.Outcome != r.Outcome:
		return false
	case f.Errors && r.Outcome == OutcomeSuccess:
		return false
	}
	return true
}

// OutcomeSuccess is the outcome recorded for a request the caller received an
// answer to. It is declared here rather than imported so that reqlog does not
// depend on the server package that writes to it.
const OutcomeSuccess = "success"

// Ring is a fixed-size buffer of the most recent records.
//
// Fixed size is the point: a gateway under load produces records faster than
// anyone reads them, and an unbounded log inside a long-lived proxy is a memory
// leak with a nice name. The oldest record is overwritten without ceremony.
type Ring struct {
	mu   sync.RWMutex
	buf  []Record
	next int
	// full says whether the buffer has wrapped, which is what distinguishes a
	// half-filled ring from a full one whose newest record sits at index zero.
	full bool
}

// NewRing returns a buffer holding at most capacity records. A capacity below
// one disables recording, which is what an operator asking for zero history
// means.
func NewRing(capacity int) *Ring {
	if capacity < 1 {
		return &Ring{}
	}
	return &Ring{buf: make([]Record, capacity)}
}

// Enabled reports whether the ring stores anything.
func (r *Ring) Enabled() bool { return r != nil && len(r.buf) > 0 }

// Add records one completed request, evicting the oldest if the buffer is full.
func (r *Ring) Add(rec Record) {
	if !r.Enabled() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = rec
	r.next++
	if r.next == len(r.buf) {
		r.next = 0
		r.full = true
	}
}

// Recent returns up to limit matching records, newest first.
//
// It walks backwards from the write cursor and stops as soon as it has enough,
// so a filtered read of a large ring costs only what it returns unless the
// filter is selective enough to exhaust the buffer.
func (r *Ring) Recent(limit int, f Filter) []Record {
	if !r.Enabled() || limit < 1 {
		return []Record{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	stored := r.next
	if r.full {
		stored = len(r.buf)
	}
	out := make([]Record, 0, min(limit, stored))
	for i := 1; i <= stored && len(out) < limit; i++ {
		idx := (r.next - i + len(r.buf)) % len(r.buf)
		if rec := &r.buf[idx]; f.matches(rec) {
			out = append(out, *rec)
		}
	}
	return out
}

// Len reports how many records the ring currently holds.
func (r *Ring) Len() int {
	if !r.Enabled() {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.full {
		return len(r.buf)
	}
	return r.next
}

// Cap reports the ring's capacity, which the UI shows so that "200 requests" is
// legible as either the whole history or the visible end of a longer one.
func (r *Ring) Cap() int {
	if r == nil {
		return 0
	}
	return len(r.buf)
}
