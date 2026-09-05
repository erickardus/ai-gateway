// Package metrics exposes gateway counters in the Prometheus text exposition
// format.
//
// The format is written by hand rather than pulling in a client library: the
// metric set is small and fixed, and the project's one dependency is a YAML
// parser. What the library would buy here — dynamic registration, custom
// collectors — this gateway does not need.
package metrics

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// latencyBuckets are cumulative upper bounds in seconds. They are spread wide
// because a gateway sees both token-counting calls answering in milliseconds and
// long completions running for minutes.
var latencyBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}

// labels identifies one time series.
type labels struct {
	model      string
	deployment string
	outcome    string
}

func (l labels) render() string {
	if l.model == "" && l.deployment == "" && l.outcome == "" {
		return ""
	}
	var parts []string
	if l.model != "" {
		parts = append(parts, `model="`+escape(l.model)+`"`)
	}
	if l.deployment != "" {
		parts = append(parts, `deployment="`+escape(l.deployment)+`"`)
	}
	if l.outcome != "" {
		parts = append(parts, `outcome="`+escape(l.outcome)+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// escape applies the exposition format's label-value escaping. Deployment IDs
// and model names come from configuration, but escaping them anyway keeps a
// stray quote or newline from producing a corrupt scrape.
func escape(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

// histogram is a cumulative latency histogram.
type histogram struct {
	counts []uint64
	sum    float64
	total  uint64
}

func (h *histogram) observe(seconds float64) {
	if h.counts == nil {
		h.counts = make([]uint64, len(latencyBuckets))
	}
	for i, bound := range latencyBuckets {
		if seconds <= bound {
			h.counts[i]++
		}
	}
	h.sum += seconds
	h.total++
}

// Registry holds the gateway's metrics.
type Registry struct {
	mu sync.Mutex

	requests   map[labels]uint64
	tokens     map[labels]uint64
	cost       map[labels]float64
	retries    map[labels]uint64
	fallbacks  map[labels]uint64
	cooldowns  map[labels]uint64
	rejections map[labels]uint64
	latency    map[labels]*histogram
	inFlight   map[labels]int64
	// promptTokens splits the provider's prompt-cache counters by outcome, and
	// promptRequests counts how many requests read from that cache at all. The
	// two answer different questions: whether caching is saving money, and
	// whether it is working. Kept apart from tokens, which is the total.
	promptTokens   map[labels]uint64
	promptRequests map[labels]uint64
	// affinity counts what the prompt-prefix pin achieved, which is the early
	// warning that load balancing has started shredding prompt caches.
	affinity map[labels]uint64

	startedAt time.Time
	now       func() time.Time
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{
		requests:   map[labels]uint64{},
		tokens:     map[labels]uint64{},
		cost:       map[labels]float64{},
		retries:    map[labels]uint64{},
		fallbacks:  map[labels]uint64{},
		cooldowns:  map[labels]uint64{},
		rejections: map[labels]uint64{},
		latency:    map[labels]*histogram{},
		inFlight:   map[labels]int64{},

		promptTokens:   map[labels]uint64{},
		promptRequests: map[labels]uint64{},
		affinity:       map[labels]uint64{},
		startedAt:      time.Now(),
		now:            time.Now,
	}
}

// RequestOutcome describes how a request ended, for the outcome label.
const (
	OutcomeSuccess  = "success"
	OutcomeUpstream = "upstream_error"
	OutcomeGateway  = "gateway_error"
	OutcomeRejected = "rejected"
	// OutcomeCacheHit marks a request served from cache, which called no
	// upstream and cost nothing.
	OutcomeCacheHit = "cache_hit"
)

// Prompt-cache outcome labels. "read" and "write" name the provider's own two
// counters; "hit" and "miss" say whether a request read from that cache at all.
//
// The affinity counter uses the same "hit" and "miss" and adds "new" for a
// prefix that had no pin yet. Keeping that third value out of "miss" is what
// lets the miss count mean "routing passed over a warm upstream" rather than
// also counting every conversation's opening turn.
const (
	OutcomeCacheRead  = "read"
	OutcomeCacheWrite = "write"
	// OutcomeCacheWrite1h is the part of the write total written with a
	// one-hour TTL. It is reported separately because it is priced separately:
	// twice base input against the five-minute tier's 1.25x, so a shift in the
	// mix moves the bill without moving the token count.
	OutcomeCacheWrite1h = "write_1h"
	OutcomeHit          = "hit"
	OutcomeMiss         = "miss"
	OutcomeNew          = "new"
)

// Result is everything the gateway records about one completed request.
type Result struct {
	Model        string
	Deployment   string
	Outcome      string
	Latency      time.Duration
	Tokens       int
	Cost         float64
	Retries      int
	Fallbacks    int
	CooledDown   bool
	RejectReason string
	// CacheReadTokens and CacheWriteTokens are the provider's prompt-cache
	// counters, separate from Tokens, which is every token the upstream
	// reported.
	CacheReadTokens  int
	CacheWriteTokens int
	// CacheWrite1hTokens is the long-TTL subset of CacheWriteTokens, exported
	// as its own series rather than added to the total.
	CacheWrite1hTokens int
	// PromptAffinity is "hit" or "miss" when a prompt-prefix pin was consulted,
	// and empty when none was.
	PromptAffinity string
}

// Observe records a completed request.
func (r *Registry) Observe(res Result) {
	if r == nil {
		return
	}
	l := labels{model: res.Model, deployment: res.Deployment, outcome: res.Outcome}
	unlabelled := labels{model: res.Model, deployment: res.Deployment}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.requests[l]++
	if res.Tokens > 0 {
		r.tokens[unlabelled] += uint64(res.Tokens)
	}
	if res.Cost > 0 {
		r.cost[unlabelled] += res.Cost
	}
	if res.CacheReadTokens > 0 {
		r.promptTokens[labels{model: res.Model, deployment: res.Deployment, outcome: OutcomeCacheRead}] += uint64(res.CacheReadTokens)
	}
	if res.CacheWriteTokens > 0 {
		r.promptTokens[labels{model: res.Model, deployment: res.Deployment, outcome: OutcomeCacheWrite}] += uint64(res.CacheWriteTokens)
	}
	if res.CacheWrite1hTokens > 0 {
		r.promptTokens[labels{model: res.Model, deployment: res.Deployment, outcome: OutcomeCacheWrite1h}] += uint64(res.CacheWrite1hTokens)
	}
	// Counted only for requests that actually reached an upstream: a rejected
	// or cache-served request never gave the provider a prompt to cache, and
	// counting it as a miss would understate the hit rate.
	if res.Outcome == OutcomeSuccess {
		hit := OutcomeMiss
		if res.CacheReadTokens > 0 {
			hit = OutcomeHit
		}
		r.promptRequests[labels{model: res.Model, deployment: res.Deployment, outcome: hit}]++
	}
	if res.PromptAffinity != "" {
		r.affinity[labels{model: res.Model, deployment: res.Deployment, outcome: res.PromptAffinity}]++
	}
	if res.Retries > 0 {
		r.retries[unlabelled] += uint64(res.Retries)
	}
	if res.Fallbacks > 0 {
		r.fallbacks[labels{model: res.Model}] += uint64(res.Fallbacks)
	}
	if res.CooledDown {
		r.cooldowns[unlabelled]++
	}
	if res.RejectReason != "" {
		r.rejections[labels{model: res.Model, outcome: res.RejectReason}]++
	}
	if res.Latency > 0 {
		h, ok := r.latency[unlabelled]
		if !ok {
			h = &histogram{}
			r.latency[unlabelled] = h
		}
		h.observe(res.Latency.Seconds())
	}
}

// InFlightAdd adjusts the in-flight gauge for a deployment.
func (r *Registry) InFlightAdd(model, deployment string, delta int64) {
	if r == nil {
		return
	}
	l := labels{model: model, deployment: deployment}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inFlight[l] += delta
	if r.inFlight[l] < 0 {
		r.inFlight[l] = 0
	}
}

// WriteTo renders the registry in the Prometheus text exposition format.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	r.mu.Lock()
	snapshot := r.snapshotLocked()
	uptime := r.now().Sub(r.startedAt).Seconds()
	r.mu.Unlock()

	var b strings.Builder

	writeCounter(&b, "gateway_requests_total", "Requests handled, by model, deployment and outcome.", snapshot.requests)
	writeCounter(&b, "gateway_tokens_total", "Tokens reported by upstreams.", snapshot.tokens)
	writeCounter(&b, "gateway_retries_total", "Retry attempts made.", snapshot.retries)
	writeCounter(&b, "gateway_fallbacks_total", "Fallback hops taken.", snapshot.fallbacks)
	writeCounter(&b, "gateway_cooldowns_total", "Deployment ejections.", snapshot.cooldowns)
	writeCounter(&b, "gateway_rejections_total", "Requests rejected before dispatch, by reason.", snapshot.rejections)
	writeCounter(&b, "gateway_prompt_cache_tokens_total", "Provider prompt-cache tokens, by outcome. A read is billed at a fraction of input; a write at a premium, and write_1h is the long-TTL subset of write rather than an addition to it.", snapshot.promptTokens)
	writeCounter(&b, "gateway_prompt_cache_requests_total", "Dispatched requests that did or did not read from the provider's prompt cache.", snapshot.promptRequests)
	writeCounter(&b, "gateway_prompt_affinity_total", "Requests by what the prompt-prefix pin did: honoured, passed over, or newly established.", snapshot.affinity)
	writeFloatCounter(&b, "gateway_cost_total", "Cost charged to the operator. Excludes passthrough traffic, which bills the caller's own subscription.", snapshot.cost)
	writeGauge(&b, "gateway_in_flight", "Requests currently outstanding.", snapshot.inFlight)
	writeHistogram(&b, "gateway_request_duration_seconds", "Request latency.", snapshot.latency)

	fmt.Fprintf(&b, "# HELP gateway_uptime_seconds Seconds since the gateway started.\n")
	fmt.Fprintf(&b, "# TYPE gateway_uptime_seconds gauge\n")
	fmt.Fprintf(&b, "gateway_uptime_seconds %s\n", formatFloat(uptime))

	n, err := io.WriteString(w, b.String())
	return int64(n), err
}

type snap struct {
	requests, tokens, retries, fallbacks, cooldowns, rejections map[labels]uint64
	promptTokens, promptRequests, affinity                      map[labels]uint64
	cost                                                        map[labels]float64
	inFlight                                                    map[labels]int64
	latency                                                     map[labels]histogram
}

func (r *Registry) snapshotLocked() snap {
	s := snap{
		requests:   maps.Clone(r.requests),
		tokens:     maps.Clone(r.tokens),
		retries:    maps.Clone(r.retries),
		fallbacks:  maps.Clone(r.fallbacks),
		cooldowns:  maps.Clone(r.cooldowns),
		rejections: maps.Clone(r.rejections),
		cost:       maps.Clone(r.cost),

		promptTokens:   maps.Clone(r.promptTokens),
		promptRequests: maps.Clone(r.promptRequests),
		affinity:       maps.Clone(r.affinity),
		inFlight:       maps.Clone(r.inFlight),
		latency:        make(map[labels]histogram, len(r.latency)),
	}
	for k, h := range r.latency {
		s.latency[k] = histogram{counts: slices.Clone(h.counts), sum: h.sum, total: h.total}
	}
	return s
}

// sortedLabels returns a series' labels in a stable order, so a scrape diff
// reflects real change rather than map iteration order.
func sortedLabels[V any](m map[labels]V) []labels {
	out := slices.Collect(maps.Keys(m))
	sort.Slice(out, func(i, j int) bool {
		if out[i].model != out[j].model {
			return out[i].model < out[j].model
		}
		if out[i].deployment != out[j].deployment {
			return out[i].deployment < out[j].deployment
		}
		return out[i].outcome < out[j].outcome
	})
	return out
}

func writeHeader(b *strings.Builder, name, help, kind string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

func writeCounter(b *strings.Builder, name, help string, m map[labels]uint64) {
	if len(m) == 0 {
		return
	}
	writeHeader(b, name, help, "counter")
	for _, l := range sortedLabels(m) {
		fmt.Fprintf(b, "%s%s %d\n", name, l.render(), m[l])
	}
}

func writeFloatCounter(b *strings.Builder, name, help string, m map[labels]float64) {
	if len(m) == 0 {
		return
	}
	writeHeader(b, name, help, "counter")
	for _, l := range sortedLabels(m) {
		fmt.Fprintf(b, "%s%s %s\n", name, l.render(), formatFloat(m[l]))
	}
}

func writeGauge(b *strings.Builder, name, help string, m map[labels]int64) {
	if len(m) == 0 {
		return
	}
	writeHeader(b, name, help, "gauge")
	for _, l := range sortedLabels(m) {
		fmt.Fprintf(b, "%s%s %d\n", name, l.render(), m[l])
	}
}

func writeHistogram(b *strings.Builder, name, help string, m map[labels]histogram) {
	if len(m) == 0 {
		return
	}
	writeHeader(b, name, help, "histogram")
	for _, l := range sortedLabels(m) {
		h := m[l]
		base := l.render()
		for i, bound := range latencyBuckets {
			var count uint64
			if i < len(h.counts) {
				count = h.counts[i]
			}
			fmt.Fprintf(b, "%s_bucket%s %d\n", name, withLE(base, formatFloat(bound)), count)
		}
		fmt.Fprintf(b, "%s_bucket%s %d\n", name, withLE(base, "+Inf"), h.total)
		fmt.Fprintf(b, "%s_sum%s %s\n", name, base, formatFloat(h.sum))
		fmt.Fprintf(b, "%s_count%s %d\n", name, base, h.total)
	}
}

// withLE inserts the le label a histogram bucket requires.
func withLE(base, le string) string {
	if base == "" {
		return `{le="` + le + `"}`
	}
	return base[:len(base)-1] + `,le="` + le + `"}`
}

// formatFloat renders a float in the shortest form that round-trips, which is
// what the exposition format expects.
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
