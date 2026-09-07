// Package metrics holds the gateway's counters, gauges and histograms, and
// publishes them in the Prometheus text exposition format and over OTLP.
//
// Neither publisher pulls in a client library. The metric set is fixed and
// known at compile time, so what a library would buy here — dynamic
// registration, custom collectors, a global default registry — this gateway
// does not need, and the project's dependencies are a YAML parser and a Redis
// client. Both formats are rendered from one snapshot (see family.go) so they
// cannot disagree about a number.
package metrics

import (
	"io"
	"maps"
	"slices"
	"sync"
	"time"
)

// Bucket sets.
//
// Each is chosen for the shape of the thing it measures rather than copied from
// the others, because a histogram whose buckets do not bracket the real
// distribution reports a p99 that is really "the top bucket" and moves only
// when the traffic changes shape entirely.
var (
	// latencyBuckets spans a gateway's whole range: token counting answers in
	// milliseconds, a long completion runs for minutes.
	latencyBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}

	// ttftBuckets are tighter and stop earlier. Time to first token is the
	// number a user actually feels, it is usually under a few seconds, and the
	// interesting movement is in the sub-second range that latencyBuckets
	// lumps into two buckets.
	ttftBuckets = []float64{0.05, 0.1, 0.2, 0.35, 0.5, 0.75, 1, 1.5, 2, 3, 5, 10, 30, 60}

	// tokenBuckets bracket a prompt from a one-line question to a filled
	// million-token context, which is the range a coding agent actually spans
	// within a single conversation.
	tokenBuckets = []float64{100, 500, 1000, 2500, 5000, 10000, 25000, 50000, 100000, 200000, 400000, 800000, 1500000}

	// byteBuckets cover a small JSON body up to the default 32 MiB max.
	byteBuckets = []float64{512, 2048, 8192, 32768, 131072, 524288, 2097152, 8388608, 33554432}

	// throughputBuckets are output tokens per second. Generation rates sit in
	// the tens for a large model and the low hundreds for a small one.
	throughputBuckets = []float64{5, 10, 20, 35, 50, 75, 100, 150, 250, 500, 1000}
)

// Metric names. Declared as constants so the recorder, the tests and the
// documentation check cannot drift apart on a typo.
const (
	MRequests        = "gateway_requests_total"
	MRequestDuration = "gateway_request_duration_seconds"
	MInFlight        = "gateway_in_flight"
	MUptime          = "gateway_uptime_seconds"
	MBuildInfo       = "gateway_build_info"
	MUpstreamStatus  = "gateway_upstream_responses_total"

	MTokens       = "gateway_tokens_total"
	MInputTokens  = "gateway_input_tokens_total"
	MOutputTokens = "gateway_output_tokens_total"
	MPromptSize   = "gateway_prompt_tokens"
	MOutputSize   = "gateway_completion_tokens"

	MCost              = "gateway_cost_total"
	MCacheDiscount     = "gateway_prompt_cache_discount_total"
	MCacheWritePremium = "gateway_prompt_cache_write_premium_total"

	MPromptCacheTokens   = "gateway_prompt_cache_tokens_total"
	MPromptCacheRequests = "gateway_prompt_cache_requests_total"
	MPromptAffinity      = "gateway_prompt_affinity_total"

	MTimeToFirstToken = "gateway_time_to_first_token_seconds"
	MThroughput       = "gateway_output_tokens_per_second"
	MStreams          = "gateway_streams_total"

	MRetries              = "gateway_retries_total"
	MFallbacks            = "gateway_fallbacks_total"
	MCooldowns            = "gateway_cooldowns_total"
	MDeploymentAvailable  = "gateway_deployment_available"
	MDeploymentThrottled  = "gateway_deployment_throttled_total"
	MRejections           = "gateway_rejections_total"
	MSharedStateDegraded  = "gateway_shared_state_degradations_total"
	MVirtualKeys          = "gateway_virtual_keys"
	MSSO                  = "gateway_sso_grants_total"
	MAuditFailures        = "gateway_audit_write_failures_total"
	MAuditSealed          = "gateway_audit_sealed"
	MSpendHistoryRows     = "gateway_spend_history_rows_total"
	MSpendHistoryDropped  = "gateway_spend_history_dropped_total"
	MSpendHistoryPending  = "gateway_spend_history_pending"
	MResponseCache        = "gateway_response_cache_requests_total"
	MResponseCacheEntries = "gateway_response_cache_entries"

	MRequestBytes  = "gateway_request_body_bytes"
	MResponseBytes = "gateway_response_body_bytes"
)

// RequestOutcome describes how a request ended, for the outcome label.
const (
	OutcomeSuccess  = "success"
	OutcomeUpstream = "upstream_error"
	OutcomeGateway  = "gateway_error"
	OutcomeRejected = "rejected"
	// OutcomeCacheHit marks a request served from cache, which called no
	// upstream and cost nothing.
	OutcomeCacheHit = "cache_hit"
	// OutcomeClientGone marks a request the caller abandoned before it was
	// answered. It is kept out of OutcomeGateway because the error rate is read
	// as "how often is the gateway failing", and a client that hung up did not
	// fail: a CLI exiting after its last answer cancels whatever it still had in
	// flight, which is normal behaviour and would otherwise report as a fault
	// with no cause to find.
	OutcomeClientGone = "client_disconnected"
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

	// OutcomeCompleted and OutcomeInterrupted describe how a stream ended. They
	// are their own counter rather than an outcome on the request counter
	// because a stream that dies after its first chunk is a 200 with a truncated
	// body: the request looks successful everywhere else.
	OutcomeCompleted   = "completed"
	OutcomeInterrupted = "interrupted"
)

// family is one metric and every series recorded against it.
type family struct {
	name, help, unit string
	kind             Kind
	bounds           []float64
	points           map[string]*point
}

type point struct {
	labels []Label
	value  float64
	hist   *histogram
}

// histogram is a cumulative histogram over a family's bounds.
type histogram struct {
	counts   []uint64
	sum      float64
	total    uint64
	min, max float64
}

func (h *histogram) observe(bounds []float64, v float64) {
	if h.counts == nil {
		h.counts = make([]uint64, len(bounds))
		h.min, h.max = v, v
	}
	for i, bound := range bounds {
		if v <= bound {
			h.counts[i]++
		}
	}
	h.sum += v
	h.total++
	h.min = min(h.min, v)
	h.max = max(h.max, v)
}

// GaugeFunc supplies a gauge's series at collection time rather than
// accumulating them as events arrive.
//
// Some of what an operator most wants on a dashboard is state someone else
// already owns: which deployments are cooling down is the router's, how many
// entries the response cache holds is the cache's, how many virtual keys exist
// is the key store's. Copying each into a counter here would mean a second
// source of truth that drifts whenever an update path is missed. A callback
// reads the real one at scrape time instead.
type GaugeFunc func() []Point

type gaugeSource struct {
	name, help, unit string
	fn               GaugeFunc
}

// Registry holds the gateway's metrics.
type Registry struct {
	mu       sync.Mutex
	families []*family
	byName   map[string]*family

	gaugeMu sync.RWMutex
	gauges  []gaugeSource

	startedAt time.Time
	now       func() time.Time
}

// declare is the metric catalogue. Everything the gateway publishes is listed
// here once, with the help text an operator reads on a dashboard and the UCUM
// unit an OTLP consumer needs to label an axis.
var declare = []family{
	{name: MRequests, kind: KindCounter, unit: "{request}",
		help: "Requests handled, by model, deployment and outcome."},
	{name: MRequestDuration, kind: KindHistogram, unit: "s", bounds: latencyBuckets,
		help: "Wall time from the gateway receiving a request to finishing the response, including every retry and fallback it made."},
	{name: MInFlight, kind: KindGauge, unit: "{request}",
		help: "Requests currently outstanding against an upstream."},
	{name: MUpstreamStatus, kind: KindCounter, unit: "{response}",
		help: "Upstream HTTP responses by status class, counting every attempt rather than only the one that was returned to the caller."},

	{name: MTokens, kind: KindCounter, unit: "{token}",
		help: "Every token an upstream reported, input and output together."},
	{name: MInputTokens, kind: KindCounter, unit: "{token}",
		help: "Input tokens charged at the ordinary input rate, excluding anything read from or written to the provider's prompt cache."},
	{name: MOutputTokens, kind: KindCounter, unit: "{token}",
		help: "Output tokens generated."},
	{name: MPromptSize, kind: KindHistogram, unit: "{token}", bounds: tokenBuckets,
		help: "Prompt size per request, cached and uncached input together. The distribution, not the total: a mean prompt size hides the long-context requests that dominate a bill."},
	{name: MOutputSize, kind: KindHistogram, unit: "{token}", bounds: tokenBuckets,
		help: "Completion size per request."},

	{name: MCost, kind: KindCounter, unit: "USD",
		help: "Cost charged to the operator. Excludes passthrough traffic, which bills the caller's own subscription."},
	{name: MCacheDiscount, kind: KindCounter, unit: "USD",
		help: "What reading the provider's prompt cache took off the bill, against the same tokens charged as ordinary input."},
	{name: MCacheWritePremium, kind: KindCounter, unit: "USD",
		help: "What writing the provider's prompt cache added to the bill, over the same tokens charged as ordinary input. Subtract this from the discount for net savings; a negative result is caching costing more than it saves."},

	{name: MPromptCacheTokens, kind: KindCounter, unit: "{token}",
		help: "Provider prompt-cache tokens, by outcome. A read is billed at a fraction of input; a write at a premium, and write_1h is the long-TTL subset of write rather than an addition to it."},
	{name: MPromptCacheRequests, kind: KindCounter, unit: "{request}",
		help: "Dispatched requests that did or did not read from the provider's prompt cache."},
	{name: MPromptAffinity, kind: KindCounter, unit: "{request}",
		help: "Requests by what the prompt-prefix pin did: honoured, passed over, or newly established."},

	{name: MTimeToFirstToken, kind: KindHistogram, unit: "s", bounds: ttftBuckets,
		help: "Time from the gateway receiving a streaming request to relaying its first content chunk. The latency a user actually feels, which total duration does not describe."},
	{name: MThroughput, kind: KindHistogram, unit: "{token}/s", bounds: throughputBuckets,
		help: "Output tokens per second over a streamed response, measured after the first chunk so it describes generation rather than queueing."},
	{name: MStreams, kind: KindCounter, unit: "{stream}",
		help: "Streamed responses by how they ended. An interrupted stream is a 200 with a truncated body, so it is invisible in the request counter."},

	{name: MRetries, kind: KindCounter, unit: "{attempt}",
		help: "Retry attempts made, labelled by the deployment the retry landed on."},
	{name: MFallbacks, kind: KindCounter, unit: "{hop}",
		help: "Fallback hops taken out of a model group."},
	{name: MCooldowns, kind: KindCounter, unit: "{ejection}",
		help: "Deployment ejections. Connection errors and 4xx responses do not count toward ejection; see docs/routing.md."},
	{name: MDeploymentThrottled, kind: KindCounter, unit: "{request}",
		help: "Requests that found a deployment at its own rpm or tpm limit and were routed elsewhere. Distinct from a caller's key hitting its limit, which is a rejection."},
	{name: MRejections, kind: KindCounter, unit: "{request}",
		help: "Requests refused before dispatch, by reason. These reached no upstream and cost nothing."},
	{name: MSharedStateDegraded, kind: KindCounter, unit: "{event}",
		help: "Times shared state became unreachable and this instance fell back to per-process limits, which silently multiplies every limit by the replica count."},
	{name: MSSO, kind: KindCounter, unit: "{grant}",
		help: "Virtual keys issued through an SSO login, by kind and outcome. A run of renewal failures is what an identity provider outage looks like before anyone's key has expired."},
	{name: MAuditFailures, kind: KindCounter, unit: "{record}",
		help: "Administrative actions refused because the audit log would not take the record. Any value above zero means the gateway is currently unable to administer keys, which nothing else reports."},
	{name: MResponseCache, kind: KindCounter, unit: "{request}",
		help: "Response-cache lookups by outcome. A hit calls no upstream and costs nothing."},

	{name: MRequestBytes, kind: KindHistogram, unit: "By", bounds: byteBuckets,
		help: "Request body size received from the caller."},
	{name: MResponseBytes, kind: KindHistogram, unit: "By", bounds: byteBuckets,
		help: "Response body size relayed to the caller."},
}

// New returns an empty Registry.
func New() *Registry {
	r := &Registry{
		byName:    make(map[string]*family, len(declare)),
		startedAt: time.Now(),
		now:       time.Now,
	}
	for i := range declare {
		f := declare[i]
		f.points = map[string]*point{}
		r.families = append(r.families, &f)
		r.byName[f.name] = &f
	}
	return r
}

// RegisterGauge adds a gauge whose series are read at collection time. See
// GaugeFunc for why some state is published this way rather than accumulated.
func (r *Registry) RegisterGauge(name, help, unit string, fn GaugeFunc) {
	if r == nil || fn == nil {
		return
	}
	r.gaugeMu.Lock()
	defer r.gaugeMu.Unlock()
	r.gauges = append(r.gauges, gaugeSource{name: name, help: help, unit: unit, fn: fn})
}

// key encodes a label set for map lookup. The labels are already sorted, so the
// encoding is stable for a given set regardless of the order they were passed.
func key(labels []Label) string {
	if len(labels) == 0 {
		return ""
	}
	n := 0
	for _, l := range labels {
		n += len(l.Key) + len(l.Value) + 2
	}
	b := make([]byte, 0, n)
	for _, l := range labels {
		b = append(b, l.Key...)
		b = append(b, 0)
		b = append(b, l.Value...)
		b = append(b, 0)
	}
	return string(b)
}

// pointLocked finds or creates a series. The caller holds r.mu.
func (r *Registry) pointLocked(name string, labels []Label) (*family, *point) {
	f := r.byName[name]
	if f == nil {
		return nil, nil
	}
	k := key(labels)
	p := f.points[k]
	if p == nil {
		p = &point{labels: labels}
		f.points[k] = p
	}
	return f, p
}

// Add increments a counter or moves a gauge.
func (r *Registry) Add(name string, delta float64, labels ...string) {
	if r == nil || delta == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, p := r.pointLocked(name, labelsOf(labels...)); p != nil {
		p.value += delta
	}
}

// Observe records one sample into a histogram.
func (r *Registry) ObserveValue(name string, v float64, labels ...string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, p := r.pointLocked(name, labelsOf(labels...))
	if p == nil {
		return
	}
	if p.hist == nil {
		p.hist = &histogram{}
	}
	p.hist.observe(f.bounds, v)
}

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

	// InputTokens and OutputTokens split Tokens by direction. They are the two
	// halves an operator reasons about separately: output is what a model
	// generates and what dominates a bill, input is what a prompt cache acts on.
	InputTokens  int
	OutputTokens int
	// PromptTokens is every input token including cached ones, which is the
	// figure a context window is measured against.
	PromptTokens int

	// CacheDiscount and CacheWritePremium are the two monotonic halves of
	// cache savings. They are published separately because their difference is
	// signed — caching can cost more than it saves — and a counter that can
	// fall is not a counter.
	CacheDiscount     float64
	CacheWritePremium float64

	// Streaming marks a streamed response, and StreamCompleted whether it ran
	// to the end. TimeToFirstToken and Throughput are recorded only for one
	// that produced at least one chunk.
	Streaming        bool
	StreamCompleted  bool
	TimeToFirstToken time.Duration
	Throughput       float64

	// UpstreamStatusClass is the status class of the response the caller got,
	// as "2xx", "4xx", "5xx". Empty where no upstream answered.
	UpstreamStatusClass string
	// RequestBytes and ResponseBytes are body sizes, excluding headers.
	RequestBytes  int64
	ResponseBytes int64
	// Throttled counts deployments passed over for being at their own rate
	// limit, which is a capacity signal rather than a caller's fault.
	Throttled int
}

// Observe records a completed request.
//
// This is the whole of the request-scoped instrumentation: the server calls it
// once, from the same place it records spend, so the two cannot disagree about
// what a request did.
func (r *Registry) Observe(res Result) {
	if r == nil {
		return
	}
	model, dep := res.Model, res.Deployment

	r.Add(MRequests, 1, "model", model, "deployment", dep, "outcome", res.Outcome)
	r.Add(MTokens, float64(res.Tokens), "model", model, "deployment", dep)
	r.Add(MInputTokens, float64(res.InputTokens), "model", model, "deployment", dep)
	r.Add(MOutputTokens, float64(res.OutputTokens), "model", model, "deployment", dep)
	r.Add(MCost, res.Cost, "model", model, "deployment", dep)
	r.Add(MCacheDiscount, res.CacheDiscount, "model", model, "deployment", dep)
	r.Add(MCacheWritePremium, res.CacheWritePremium, "model", model, "deployment", dep)

	r.Add(MPromptCacheTokens, float64(res.CacheReadTokens), "model", model, "deployment", dep, "outcome", OutcomeCacheRead)
	r.Add(MPromptCacheTokens, float64(res.CacheWriteTokens), "model", model, "deployment", dep, "outcome", OutcomeCacheWrite)
	r.Add(MPromptCacheTokens, float64(res.CacheWrite1hTokens), "model", model, "deployment", dep, "outcome", OutcomeCacheWrite1h)

	// Counted only for requests that actually reached an upstream: a rejected
	// or cache-served request never gave the provider a prompt to cache, and
	// counting it as a miss would understate the hit rate.
	if res.Outcome == OutcomeSuccess {
		hit := OutcomeMiss
		if res.CacheReadTokens > 0 {
			hit = OutcomeHit
		}
		r.Add(MPromptCacheRequests, 1, "model", model, "deployment", dep, "outcome", hit)
	}
	if res.PromptAffinity != "" {
		r.Add(MPromptAffinity, 1, "model", model, "deployment", dep, "outcome", res.PromptAffinity)
	}

	r.Add(MRetries, float64(res.Retries), "model", model, "deployment", dep)
	r.Add(MFallbacks, float64(res.Fallbacks), "model", model)
	r.Add(MDeploymentThrottled, float64(res.Throttled), "model", model)
	if res.CooledDown {
		r.Add(MCooldowns, 1, "model", model, "deployment", dep)
	}
	if res.RejectReason != "" {
		r.Add(MRejections, 1, "model", model, "outcome", res.RejectReason)
	}
	if res.UpstreamStatusClass != "" {
		r.Add(MUpstreamStatus, 1, "model", model, "deployment", dep, "status", res.UpstreamStatusClass)
	}

	if res.Latency > 0 {
		r.ObserveValue(MRequestDuration, res.Latency.Seconds(), "model", model, "deployment", dep)
	}
	if res.PromptTokens > 0 {
		r.ObserveValue(MPromptSize, float64(res.PromptTokens), "model", model, "deployment", dep)
	}
	if res.OutputTokens > 0 {
		r.ObserveValue(MOutputSize, float64(res.OutputTokens), "model", model, "deployment", dep)
	}
	if res.RequestBytes > 0 {
		r.ObserveValue(MRequestBytes, float64(res.RequestBytes), "model", model)
	}
	if res.ResponseBytes > 0 {
		r.ObserveValue(MResponseBytes, float64(res.ResponseBytes), "model", model)
	}

	if res.Streaming && res.Deployment != "" {
		ended := OutcomeInterrupted
		if res.StreamCompleted {
			ended = OutcomeCompleted
		}
		r.Add(MStreams, 1, "model", model, "deployment", dep, "outcome", ended)
		if res.TimeToFirstToken > 0 {
			r.ObserveValue(MTimeToFirstToken, res.TimeToFirstToken.Seconds(), "model", model, "deployment", dep)
		}
		if res.Throughput > 0 {
			r.ObserveValue(MThroughput, res.Throughput, "model", model, "deployment", dep)
		}
	}
}

// InFlightAdd adjusts the in-flight gauge for a deployment.
func (r *Registry) InFlightAdd(model, deployment string, delta int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, p := r.pointLocked(MInFlight, labelsOf("model", model, "deployment", deployment))
	if p == nil {
		return
	}
	p.value += float64(delta)
	if p.value < 0 {
		p.value = 0
	}
}

// RecordResponseCache counts a response-cache lookup.
func (r *Registry) RecordResponseCache(model, outcome string) {
	r.Add(MResponseCache, 1, "model", model, "outcome", outcome)
}

// SSO grant kinds and outcomes, for the kind and outcome labels.
const (
	SSOLogin   = "login"
	SSORenewal = "renewal"

	SSOSuccess = "success"
	SSOFailure = "failure"
)

// RecordSSO counts an attempt to issue a key from an SSO identity.
func (r *Registry) RecordSSO(kind, outcome string) {
	r.Add(MSSO, 1, "kind", kind, "outcome", outcome)
}

// RecordSharedStateDegradation counts a fall back to per-instance state.
func (r *Registry) RecordSharedStateDegradation() {
	r.Add(MSharedStateDegraded, 1)
}

// Collect renders every family as a snapshot. Both publishers read this and
// nothing else.
func (r *Registry) Collect() []Family {
	if r == nil {
		return nil
	}
	now := r.now()

	r.mu.Lock()
	out := make([]Family, 0, len(r.families)+4)
	for _, f := range r.families {
		if len(f.points) == 0 {
			continue
		}
		fam := Family{Name: f.name, Help: f.help, Unit: f.unit, Kind: f.kind}
		for _, k := range slices.Sorted(maps.Keys(f.points)) {
			p := f.points[k]
			pt := Point{Labels: p.labels, Value: p.value}
			if p.hist != nil {
				pt.Hist = &HistogramData{
					Bounds: f.bounds,
					Counts: slices.Clone(p.hist.counts),
					Sum:    p.hist.sum,
					Count:  p.hist.total,
					Min:    p.hist.min,
					Max:    p.hist.max,
				}
			}
			fam.Points = append(fam.Points, pt)
		}
		slices.SortFunc(fam.Points, func(a, b Point) int { return compareLabels(a.Labels, b.Labels) })
		out = append(out, fam)
	}
	uptime := now.Sub(r.startedAt).Seconds()
	r.mu.Unlock()

	// Live gauges are read outside r.mu: a provider reads state owned by the
	// router, the cache or the key store, and holding the metrics lock across
	// someone else's lock is how a deadlock is built.
	r.gaugeMu.RLock()
	sources := slices.Clone(r.gauges)
	r.gaugeMu.RUnlock()
	for _, g := range sources {
		pts := g.fn()
		if len(pts) == 0 {
			continue
		}
		slices.SortFunc(pts, func(a, b Point) int { return compareLabels(a.Labels, b.Labels) })
		out = append(out, Family{Name: g.name, Help: g.help, Unit: g.unit, Kind: KindGauge, Points: pts})
	}

	out = append(out, Family{
		Name: MUptime, Kind: KindGauge, Unit: "s",
		Help:   "Seconds since the gateway started.",
		Points: []Point{{Value: uptime}},
	})
	return out
}

// WriteTo renders the registry in the Prometheus text exposition format.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	return writePrometheus(w, r.Collect())
}

// StartedAt is when the process began serving, which OTLP needs as the start
// timestamp of every cumulative series.
func (r *Registry) StartedAt() time.Time {
	if r == nil {
		return time.Time{}
	}
	return r.startedAt
}
