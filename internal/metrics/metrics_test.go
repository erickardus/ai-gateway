package metrics

import (
	"strings"
	"testing"
	"time"
)

func render(r *Registry) string {
	var b strings.Builder
	if _, err := r.WriteTo(&b); err != nil {
		panic(err)
	}
	return b.String()
}

func TestExpositionFormat(t *testing.T) {
	r := New()
	r.Observe(Result{
		Model: "claude", Deployment: "claude#abc", Outcome: OutcomeSuccess,
		Latency: 1500 * time.Millisecond, Tokens: 250, Cost: 0.0042,
		Retries: 1, Fallbacks: 2,
	})
	out := render(r)

	for _, want := range []string{
		`# TYPE gateway_requests_total counter`,
		`gateway_requests_total{model="claude",deployment="claude#abc",outcome="success"} 1`,
		`gateway_tokens_total{model="claude",deployment="claude#abc"} 250`,
		`gateway_retries_total{model="claude",deployment="claude#abc"} 1`,
		`gateway_fallbacks_total{model="claude"} 2`,
		`# TYPE gateway_request_duration_seconds histogram`,
		`gateway_request_duration_seconds_count{model="claude",deployment="claude#abc"} 1`,
		`gateway_uptime_seconds`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

// Buckets must be cumulative and the +Inf bucket must equal the count, or
// Prometheus rejects the histogram.
func TestHistogramIsCumulative(t *testing.T) {
	r := New()
	for _, d := range []time.Duration{10 * time.Millisecond, 300 * time.Millisecond, 45 * time.Second} {
		r.Observe(Result{Model: "m", Deployment: "d", Outcome: OutcomeSuccess, Latency: d})
	}
	out := render(r)

	var prev uint64
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "gateway_request_duration_seconds_bucket") {
			continue
		}
		var count uint64
		if _, err := fmtSscan(line, &count); err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		if count < prev {
			t.Fatalf("buckets are not cumulative: %d after %d in %q", count, prev, line)
		}
		prev = count
	}
	if !strings.Contains(out, `le="+Inf"`) {
		t.Error("missing the +Inf bucket")
	}
	if !strings.Contains(out, `gateway_request_duration_seconds_count{model="m",deployment="d"} 3`) {
		t.Errorf("count should be 3:\n%s", out)
	}
	if prev != 3 {
		t.Errorf("final bucket = %d, want 3 to match the count", prev)
	}
}

// fmtSscan reads the trailing integer of an exposition line.
func fmtSscan(line string, out *uint64) (int, error) {
	fields := strings.Fields(line)
	var n uint64
	for _, c := range fields[len(fields)-1] {
		n = n*10 + uint64(c-'0')
	}
	*out = n
	return 1, nil
}

// Label values are escaped, so a stray quote cannot produce a corrupt scrape.
func TestLabelEscaping(t *testing.T) {
	r := New()
	r.Observe(Result{Model: `we"ird`, Deployment: "a\\b", Outcome: OutcomeSuccess, Latency: time.Second})
	out := render(r)
	if !strings.Contains(out, `model="we\"ird"`) {
		t.Errorf("quote was not escaped:\n%s", out)
	}
	if !strings.Contains(out, `deployment="a\\b"`) {
		t.Errorf("backslash was not escaped:\n%s", out)
	}
}

func TestInFlightGaugeNeverNegative(t *testing.T) {
	r := New()
	r.InFlightAdd("m", "d", 1)
	r.InFlightAdd("m", "d", -1)
	r.InFlightAdd("m", "d", -1) // unbalanced
	out := render(r)
	if !strings.Contains(out, `gateway_in_flight{model="m",deployment="d"} 0`) {
		t.Errorf("gauge should clamp at 0:\n%s", out)
	}
}

func TestRejectionsCarryReason(t *testing.T) {
	r := New()
	r.Observe(Result{Model: "m", Outcome: OutcomeRejected, RejectReason: "budget_exceeded"})
	out := render(r)
	if !strings.Contains(out, `gateway_rejections_total{model="m",outcome="budget_exceeded"} 1`) {
		t.Errorf("missing rejection reason:\n%s", out)
	}
}

// Output ordering must be stable, or every scrape diff looks like a change.
func TestOutputIsStable(t *testing.T) {
	r := New()
	for _, m := range []string{"zeta", "alpha", "mid"} {
		r.Observe(Result{Model: m, Deployment: "d", Outcome: OutcomeSuccess, Latency: time.Second})
	}
	// Uptime legitimately advances between scrapes; everything else must not.
	stable := func() string {
		var kept []string
		for _, line := range strings.Split(render(r), "\n") {
			if !strings.HasPrefix(line, "gateway_uptime_seconds") {
				kept = append(kept, line)
			}
		}
		return strings.Join(kept, "\n")
	}
	first := stable()
	for range 5 {
		if got := stable(); got != first {
			t.Fatalf("successive scrapes of unchanged state differ:\n--- first ---\n%s\n--- later ---\n%s", first, got)
		}
	}
	if strings.Index(first, `model="alpha"`) > strings.Index(first, `model="zeta"`) {
		t.Error("series are not sorted")
	}
}

// A nil registry must be safe: metrics are optional.
func TestNilRegistryIsSafe(t *testing.T) {
	var r *Registry
	r.Observe(Result{Model: "m"})
	r.InFlightAdd("m", "d", 1)
}

func TestEmptyRegistryOmitsSeries(t *testing.T) {
	out := render(New())
	if strings.Contains(out, "gateway_requests_total") {
		t.Error("a counter with no series should be omitted entirely")
	}
	if !strings.Contains(out, "gateway_uptime_seconds") {
		t.Error("uptime should always be present")
	}
}
