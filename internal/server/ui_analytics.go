package server

import (
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/erickardus/ai-gateway/internal/metrics"
	"github.com/erickardus/ai-gateway/internal/reqlog"
)

// Bounds on what the console may ask for.
//
// The window is capped rather than validated because the buffer is a fixed
// number of records rather than a span of time: asking for a week does not
// produce a week, it produces however much of the last week the ring still
// holds, drawn across enough buckets to make the emptiness look like data. The
// bucket count is bounded at both ends for the same reason — a dozen buckets is
// the fewest that shows a shape, and 240 is more points than a chart in a
// browser can distinguish.
const (
	analyticsDefaultWindow = time.Hour
	analyticsMaxWindow     = 24 * time.Hour
	analyticsDefaultBucket = 60
	analyticsMinBuckets    = 12
	analyticsMaxBuckets    = 240
)

// analyticsNote is what the numbers on this page do and do not mean.
//
// Both halves have caught people out. Latency percentiles that included refused
// requests reported a p50 of a millisecond on a gateway whose upstream was
// slow, because a key at its budget is refused instantly and in volume; and a
// fleet behind a load balancer shows each instance only the traffic it served,
// so two operators looking at the same page disagree about how many requests
// there were.
const analyticsNote = "Aggregated from the recent-request buffer, which is per-process and bounded: this is a sample of one instance's recent traffic, not fleet-wide history. Latency percentiles cover only requests that actually called an upstream, so neither a refusal decided before dispatch nor a response served from the cache pulls them down."

// analyticsBucket is one interval of the series.
//
// Latencies accumulate in an unexported slice so a bucket can compute its own
// percentiles once the whole window has been walked. It is not marshalled, and
// deliberately not exported: the raw per-request durations are reqlog's to
// serve, and a chart asking for a day of buckets should not receive every
// latency in the ring alongside them.
type analyticsBucket struct {
	Start            time.Time `json:"start"`
	Requests         int       `json:"requests"`
	Errors           int       `json:"errors"`
	Rejected         int       `json:"rejected"`
	CacheHits        int       `json:"cache_hits"`
	InputTokens      int       `json:"input_tokens"`
	OutputTokens     int       `json:"output_tokens"`
	CacheReadTokens  int       `json:"cache_read_tokens"`
	CacheWriteTokens int       `json:"cache_write_tokens"`
	Cost             float64   `json:"cost"`
	CacheSavings     float64   `json:"cache_savings"`
	LatencyP50MS     float64   `json:"latency_p50_ms"`
	LatencyP95MS     float64   `json:"latency_p95_ms"`

	latencies []float64
}

// analyticsTotals is the window's whole traffic, which the series cannot supply:
// a percentile of percentiles is not a percentile, and the counts an operator
// reads first — how much was retried, how much fell back — are only legible
// against the total rather than per bucket.
type analyticsTotals struct {
	Requests         int     `json:"requests"`
	Errors           int     `json:"errors"`
	Rejected         int     `json:"rejected"`
	CacheHits        int     `json:"cache_hits"`
	Streamed         int     `json:"streamed"`
	Retried          int     `json:"retried"`
	FellBack         int     `json:"fell_back"`
	BillableRequests int     `json:"billable_requests"`
	InputTokens      int     `json:"input_tokens"`
	OutputTokens     int     `json:"output_tokens"`
	CacheReadTokens  int     `json:"cache_read_tokens"`
	CacheWriteTokens int     `json:"cache_write_tokens"`
	Cost             float64 `json:"cost"`
	CacheSavings     float64 `json:"cache_savings"`
}

// analyticsLatency is the window's latency distribution.
//
// TTFT and throughput are reported beside the total latency rather than folded
// into it because they describe a streamed reply, where the number a caller
// experiences is when the first token arrived rather than when the last did.
type analyticsLatency struct {
	P50MS         float64 `json:"p50_ms"`
	P95MS         float64 `json:"p95_ms"`
	P99MS         float64 `json:"p99_ms"`
	MaxMS         float64 `json:"max_ms"`
	TTFTP50MS     float64 `json:"ttft_p50_ms"`
	TTFTP95MS     float64 `json:"ttft_p95_ms"`
	ThroughputP50 float64 `json:"throughput_p50_tps"`
}

// analyticsCount is one row of the outcome and reject-reason breakdowns.
type analyticsCount struct {
	Name  string `json:"-"`
	Count int    `json:"count"`
}

// analyticsOutcome and analyticsReason name the same pair differently, because
// "outcome" and "reason" are what the two lists are about and a shared field
// name would make the console's two tables read as one thing measured twice.
type analyticsOutcome struct {
	Outcome string `json:"outcome"`
	Count   int    `json:"count"`
}

type analyticsReason struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// analyticsDimension is one model group, deployment or key, with the figures
// that make a table of them sortable by the thing an operator is chasing.
type analyticsDimension struct {
	Name         string  `json:"name"`
	Requests     int     `json:"requests"`
	Errors       int     `json:"errors"`
	Cost         float64 `json:"cost"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	LatencyP50MS float64 `json:"latency_p50_ms"`
	LatencyP95MS float64 `json:"latency_p95_ms"`

	latencies []float64
}

// analyticsKeyLimit caps the key breakdown. A gateway can hold thousands of
// keys and a chart of thousands of series is not a chart; the ten busiest are
// what a page about the last hour is asking about.
const analyticsKeyLimit = 10

// handleUIAnalytics aggregates the recent-request buffer into a time series.
//
// It computes over the ring rather than over the spend history because the two
// answer different questions with different latencies: history is written
// asynchronously and rolled up by day, and this page is the one an operator
// opens while something is going wrong right now. Nothing is stored for it —
// the buffer is already being kept for the traffic view, and this is a second
// reading of the same records.
func (s *Server) handleUIAnalytics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	window := analyticsDefaultWindow
	if raw := q.Get("window"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error",
				"window: must be a duration such as 15m or 6h, got "+strconv.Quote(raw))
			return
		}
		if d <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request_error",
				"window: must be positive, got "+strconv.Quote(raw))
			return
		}
		window = min(d, analyticsMaxWindow)
	}
	count := analyticsDefaultBucket
	if raw := q.Get("buckets"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error",
				"buckets: must be a number, got "+strconv.Quote(raw))
			return
		}
		count = min(max(n, analyticsMinBuckets), analyticsMaxBuckets)
	}

	// The step is rounded up to a whole second and the window recomputed from
	// it, so that bucket_seconds is an integer the caller can label an axis with
	// rather than a fraction they have to render. The window that comes back is
	// therefore the one actually covered, which is why it is reported rather
	// than echoed: a request for 48h answers with 24h, and a chart drawn against
	// what it asked for would be a chart with the wrong axis.
	step := time.Duration(math.Ceil(float64(window)/float64(count)/float64(time.Second))) * time.Second
	step = max(step, time.Second)
	count = int(math.Ceil(float64(window) / float64(step)))

	// Truncated to a whole second so that from, to and every bucket boundary
	// render the same way. Without it the window's ends encode as RFC 3339 with
	// no fractional part while the bucket starts encode with one, and a chart
	// plotting the first point against the window's start finds two timestamps
	// that disagree by a fraction of a second for no reason a reader can see.
	to := time.Now().UTC().Truncate(time.Second)
	from := to.Add(-step * time.Duration(count))

	filter := reqlog.Filter{
		ModelGroup: q.Get("model_group"),
		Deployment: q.Get("deployment"),
	}
	// The whole ring is read rather than a page of it: every record inside the
	// window contributes, and a limit here would silently truncate the oldest
	// buckets into emptiness that looks like quiet traffic.
	records := s.traffic.Recent(s.traffic.Cap(), filter)

	series := make([]analyticsBucket, count)
	for i := range series {
		series[i] = analyticsBucket{Start: from.Add(step * time.Duration(i))}
	}

	var totals analyticsTotals
	var latencies, ttfts, throughputs []float64
	outcomes := make(map[string]int)
	reasons := make(map[string]int)
	groups := make(map[string]*analyticsDimension)
	deployments := make(map[string]*analyticsDimension)
	keys := make(map[string]*analyticsDimension)

	for i := range records {
		rec := &records[i]
		if rec.At.Before(from) {
			continue
		}
		idx := int(rec.At.Sub(from) / step)
		// A record stamped inside the sub-second that truncating the window's
		// end discarded — or added by another request while this walk was
		// running — belongs to the newest bucket rather than to one past the end
		// of the slice. Dropping it instead would lose the most recent traffic,
		// which is the traffic the page was opened to look at.
		idx = min(max(idx, 0), count-1)
		bucket := &series[idx]

		// A request that reached a deployment is the only one whose duration
		// describes anything an upstream did. A refusal is decided in
		// microseconds and arrives in volume — a key at its budget refuses every
		// request it makes — so counting those would report a p50 of nothing at
		// all on a gateway whose upstream is slow.
		//
		// A cache hit is excluded for the same reason and needs saying
		// separately, because it does not look like a refusal: it is recorded
		// against the sentinel deployment "cache", so a test for a deployment
		// alone admits it. It answered without calling anyone, in well under a
		// millisecond, and a warm cache would otherwise report a gateway as
		// having grown faster when all that changed is how often it was asked
		// the same question twice.
		served := rec.Deployment != "" && rec.Outcome != metrics.OutcomeCacheHit

		bucket.Requests++
		totals.Requests++
		bucket.InputTokens += rec.Usage.InputTokens
		bucket.OutputTokens += rec.Usage.OutputTokens
		bucket.CacheReadTokens += rec.Usage.CacheReadTokens
		bucket.CacheWriteTokens += rec.Usage.CacheWriteTokens
		bucket.Cost += rec.Cost
		bucket.CacheSavings += rec.CacheSavings
		totals.InputTokens += rec.Usage.InputTokens
		totals.OutputTokens += rec.Usage.OutputTokens
		totals.CacheReadTokens += rec.Usage.CacheReadTokens
		totals.CacheWriteTokens += rec.Usage.CacheWriteTokens
		totals.Cost += rec.Cost
		totals.CacheSavings += rec.CacheSavings

		if served {
			bucket.latencies = append(bucket.latencies, rec.LatencyMS)
			latencies = append(latencies, rec.LatencyMS)
		}
		if rec.TTFTMS > 0 {
			ttfts = append(ttfts, rec.TTFTMS)
		}
		if rec.ThroughputTPS > 0 {
			throughputs = append(throughputs, rec.ThroughputTPS)
		}

		failed := isUpstreamOrGatewayError(rec.Outcome)
		switch {
		case failed:
			bucket.Errors++
			totals.Errors++
		case rec.Outcome == metrics.OutcomeRejected:
			bucket.Rejected++
			totals.Rejected++
		case rec.Outcome == metrics.OutcomeCacheHit:
			bucket.CacheHits++
			totals.CacheHits++
		}
		if rec.Streaming {
			totals.Streamed++
		}
		if rec.Retries > 0 {
			totals.Retried++
		}
		if rec.Fallbacks > 0 {
			totals.FellBack++
		}
		if rec.Billable {
			totals.BillableRequests++
		}

		outcomes[rec.Outcome]++
		if rec.RejectReason != "" {
			reasons[rec.RejectReason]++
		}

		addDimension(groups, rec.ModelGroup, rec, failed, served)
		addDimension(deployments, rec.Deployment, rec, failed, served)
		// A key is named by its alias where it has one, and by the subject its
		// spend accumulates under where it does not — the same identity the
		// spend report is keyed by, so the two pages name the same caller.
		name := rec.KeyAlias
		if name == "" {
			name = rec.SpendSubject
		}
		addDimension(keys, name, rec, failed, served)
	}

	for i := range series {
		b := &series[i]
		slices.Sort(b.latencies)
		b.LatencyP50MS = percentile(b.latencies, 50)
		b.LatencyP95MS = percentile(b.latencies, 95)
	}
	slices.Sort(latencies)
	slices.Sort(ttfts)
	slices.Sort(throughputs)

	writeJSON(w, http.StatusOK, map[string]any{
		"window":         (step * time.Duration(count)).String(),
		"from":           from.Format(time.RFC3339),
		"to":             to.Format(time.RFC3339),
		"bucket_seconds": int(step.Seconds()),
		"series":         series,
		"totals":         totals,
		"latency": analyticsLatency{
			P50MS:         percentile(latencies, 50),
			P95MS:         percentile(latencies, 95),
			P99MS:         percentile(latencies, 99),
			MaxMS:         percentile(latencies, 100),
			TTFTP50MS:     percentile(ttfts, 50),
			TTFTP95MS:     percentile(ttfts, 95),
			ThroughputP50: percentile(throughputs, 50),
		},
		"outcomes":       outcomeRows(outcomes),
		"reject_reasons": reasonRows(reasons),
		"groups":         dimensionRows(groups, 0),
		"deployments":    dimensionRows(deployments, 0),
		"keys":           dimensionRows(keys, analyticsKeyLimit),
		"note":           analyticsNote,
	})
}

// isUpstreamOrGatewayError reports whether an outcome describes a failure the
// gateway or its upstream produced, as opposed to a request the gateway refused
// on purpose.
//
// The two are kept apart everywhere else in this codebase — see the audit
// package on refused against error — and they mean opposite things about the
// gateway's health: a page full of rejections is the gateway working, and a
// page full of errors is not.
func isUpstreamOrGatewayError(outcome string) bool {
	return outcome == metrics.OutcomeUpstream || outcome == metrics.OutcomeGateway
}

// addDimension folds one record into a per-name breakdown, ignoring a record
// with no name to fold into: a refusal has no deployment, and a bucket labelled
// with the empty string would sit in the table looking like a deployment
// somebody had forgotten to name.
func addDimension(into map[string]*analyticsDimension, name string, rec *reqlog.Record, failed, served bool) {
	if name == "" {
		return
	}
	row := into[name]
	if row == nil {
		row = &analyticsDimension{Name: name}
		into[name] = row
	}
	row.Requests++
	if failed {
		row.Errors++
	}
	row.Cost += rec.Cost
	row.InputTokens += rec.Usage.InputTokens
	row.OutputTokens += rec.Usage.OutputTokens
	if served {
		row.latencies = append(row.latencies, rec.LatencyMS)
	}
}

// dimensionRows renders a breakdown busiest first, keeping at most limit rows.
// A limit of zero keeps them all.
func dimensionRows(from map[string]*analyticsDimension, limit int) []analyticsDimension {
	out := make([]analyticsDimension, 0, len(from))
	for _, row := range from {
		slices.Sort(row.latencies)
		row.LatencyP50MS = percentile(row.latencies, 50)
		row.LatencyP95MS = percentile(row.latencies, 95)
		out = append(out, *row)
	}
	// Ties break on the name so that two equally busy deployments do not swap
	// places between one page refresh and the next, which reads as movement in
	// data that has not changed.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Name < out[j].Name
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func outcomeRows(from map[string]int) []analyticsOutcome {
	out := make([]analyticsOutcome, 0, len(from))
	for _, row := range countRows(from) {
		out = append(out, analyticsOutcome{Outcome: row.Name, Count: row.Count})
	}
	return out
}

func reasonRows(from map[string]int) []analyticsReason {
	out := make([]analyticsReason, 0, len(from))
	for _, row := range countRows(from) {
		out = append(out, analyticsReason{Reason: row.Name, Count: row.Count})
	}
	return out
}

// countRows orders a tally by count, breaking ties on the name for the same
// reason dimensionRows does.
func countRows(from map[string]int) []analyticsCount {
	out := make([]analyticsCount, 0, len(from))
	for name, count := range from {
		out = append(out, analyticsCount{Name: name, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// percentile is the nearest-rank percentile of an already sorted slice.
//
// Nearest-rank rather than an interpolating variant because every value here is
// a measurement that actually happened: p95 of a hundred requests is the 95th
// slowest of them, not a number between two of them that no request took. An
// empty set answers zero, which is the only honest figure for a window in which
// nothing was served — and is why the count beside it is what a reader should
// look at first.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	rank = min(max(rank, 1), len(sorted))
	return sorted[rank-1]
}
