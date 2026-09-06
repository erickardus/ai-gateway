package otlp

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/erickardus/ai-gateway/internal/metrics"
)

func sampleRegistry(t *testing.T) *metrics.Registry {
	t.Helper()
	r := metrics.New()
	r.Observe(metrics.Result{
		Model: "claude", Deployment: "claude#abc", Outcome: metrics.OutcomeSuccess,
		Latency:      1500 * time.Millisecond,
		Tokens:       250,
		InputTokens:  100,
		OutputTokens: 150,
		PromptTokens: 100,
		Cost:         0.0042,
	})
	return r
}

func sampleResource() Resource {
	return Resource{Attributes: []metrics.Label{
		{Key: "service.name", Value: "ai-gateway"},
		{Key: "service.version", Value: "test"},
	}}
}

// The whole envelope, decoded field by field. If any number in the encoder is
// wrong against opentelemetry-proto, the structure this walks does not exist.
func TestProtobufEnvelopeStructure(t *testing.T) {
	start := time.Unix(1700000000, 0)
	now := start.Add(90 * time.Second)
	body := EncodeProtobuf(sampleRegistry(t).Collect(), sampleResource(), start, now)

	top, err := parsePB(body)
	if err != nil {
		t.Fatalf("parse export: %v", err)
	}
	// ExportMetricsServiceRequest.resource_metrics = 1
	rm, err := pbSub(top, 1)
	if err != nil {
		t.Fatalf("resource_metrics: %v", err)
	}
	// ResourceMetrics.resource = 1 → Resource.attributes = 1
	resource, err := pbSub(rm, 1)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}
	attrs := pbAll(resource, 1)
	if len(attrs) != 2 {
		t.Fatalf("resource attributes = %d, want 2", len(attrs))
	}
	kv, err := parsePB(attrs[0].data)
	if err != nil {
		t.Fatalf("attribute: %v", err)
	}
	if got := pbString(kv, 1); got != "service.name" {
		t.Errorf("first attribute key = %q, want service.name", got)
	}
	anyValue, err := pbSub(kv, 2)
	if err != nil {
		t.Fatalf("attribute value: %v", err)
	}
	if got := pbString(anyValue, 1); got != "ai-gateway" {
		t.Errorf("first attribute value = %q, want ai-gateway", got)
	}

	// ResourceMetrics.scope_metrics = 2 → ScopeMetrics.scope = 1
	sm, err := pbSub(rm, 2)
	if err != nil {
		t.Fatalf("scope_metrics: %v", err)
	}
	scope, err := pbSub(sm, 1)
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	if got := pbString(scope, 1); got != scopeName {
		t.Errorf("scope name = %q, want %q", got, scopeName)
	}
	if len(pbAll(sm, 2)) == 0 {
		t.Fatal("no metrics in scope_metrics")
	}
}

// findMetric walks the envelope and returns one named metric's fields.
func findMetric(t *testing.T, body []byte, name string) []pbField {
	t.Helper()
	top, err := parsePB(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rm, err := pbSub(top, 1)
	if err != nil {
		t.Fatalf("resource_metrics: %v", err)
	}
	sm, err := pbSub(rm, 2)
	if err != nil {
		t.Fatalf("scope_metrics: %v", err)
	}
	for _, m := range pbAll(sm, 2) {
		fields, err := parsePB(m.data)
		if err != nil {
			t.Fatalf("metric: %v", err)
		}
		if pbString(fields, 1) == name {
			return fields
		}
	}
	t.Fatalf("metric %q not found in export", name)
	return nil
}

// A counter must arrive as a monotonic cumulative Sum carrying the process
// start time. Sent as a gauge, or as a delta, every rate() over it is wrong.
func TestCounterIsAMonotonicCumulativeSum(t *testing.T) {
	start := time.Unix(1700000000, 0)
	now := start.Add(90 * time.Second)
	body := EncodeProtobuf(sampleRegistry(t).Collect(), sampleResource(), start, now)

	m := findMetric(t, body, metrics.MCost)
	if got := pbString(m, 3); got != "USD" {
		t.Errorf("unit = %q, want USD", got)
	}
	if _, ok := pbOne(m, 5); ok {
		t.Error("a counter was encoded as a Gauge (field 5)")
	}
	sum, err := pbSub(m, 7) // Metric.sum = 7
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if f, ok := pbOne(sum, 2); !ok || f.num64 != aggregationCumulative {
		t.Errorf("aggregation_temporality = %v, want %d (cumulative)", f.num64, aggregationCumulative)
	}
	if f, ok := pbOne(sum, 3); !ok || f.num64 != 1 {
		t.Error("is_monotonic was not set on a counter")
	}

	dps := pbAll(sum, 1)
	if len(dps) != 1 {
		t.Fatalf("data points = %d, want 1", len(dps))
	}
	dp, err := parsePB(dps[0].data)
	if err != nil {
		t.Fatalf("data point: %v", err)
	}
	if f, ok := pbOne(dp, 2); !ok || f.num64 != uint64(start.UnixNano()) {
		t.Errorf("start_time_unix_nano = %v, want %v", f.num64, start.UnixNano())
	}
	if f, ok := pbOne(dp, 3); !ok || f.num64 != uint64(now.UnixNano()) {
		t.Errorf("time_unix_nano = %v, want %v", f.num64, now.UnixNano())
	}
	if v, ok := pbDouble(dp, 4); !ok || math.Abs(v-0.0042) > 1e-12 {
		t.Errorf("as_double = %v, want 0.0042", v)
	}
	if got := len(pbAll(dp, 7)); got != 2 {
		t.Errorf("attributes = %d, want 2 (model, deployment)", got)
	}
}

// A gauge must not carry a start timestamp: it describes an instant, and OTLP
// consumers may reject a gauge point that claims an interval.
func TestGaugeCarriesNoStartTimestamp(t *testing.T) {
	start := time.Unix(1700000000, 0)
	body := EncodeProtobuf(sampleRegistry(t).Collect(), sampleResource(), start, start.Add(time.Minute))

	m := findMetric(t, body, metrics.MUptime)
	gauge, err := pbSub(m, 5) // Metric.gauge = 5
	if err != nil {
		t.Fatalf("gauge: %v", err)
	}
	dps := pbAll(gauge, 1)
	if len(dps) != 1 {
		t.Fatalf("data points = %d, want 1", len(dps))
	}
	dp, err := parsePB(dps[0].data)
	if err != nil {
		t.Fatalf("data point: %v", err)
	}
	if _, ok := pbOne(dp, 2); ok {
		t.Error("gauge data point carried start_time_unix_nano")
	}
	if _, ok := pbOne(dp, 3); !ok {
		t.Error("gauge data point carried no time_unix_nano")
	}
}

// The registry holds cumulative bucket counts and OTLP wants per-bucket ones,
// with one extra entry for everything above the last bound. Sending the
// cumulative counts as-is produces a payload that parses cleanly and describes
// a completely different distribution.
func TestHistogramBucketsAreDeltaAndCountOneMoreThanBounds(t *testing.T) {
	r := metrics.New()
	for _, d := range []time.Duration{
		10 * time.Millisecond,  // ≤ 0.05
		200 * time.Millisecond, // ≤ 0.25
		3 * time.Second,        // ≤ 5
		20 * time.Minute,       // above the last bound
	} {
		r.Observe(metrics.Result{
			Model: "m", Deployment: "d", Outcome: metrics.OutcomeSuccess, Latency: d,
		})
	}

	start := time.Unix(1700000000, 0)
	body := EncodeProtobuf(r.Collect(), sampleResource(), start, start.Add(time.Minute))
	m := findMetric(t, body, metrics.MRequestDuration)

	if got := pbString(m, 3); got != "s" {
		t.Errorf("unit = %q, want s", got)
	}
	hist, err := pbSub(m, 9) // Metric.histogram = 9
	if err != nil {
		t.Fatalf("histogram: %v", err)
	}
	if f, ok := pbOne(hist, 2); !ok || f.num64 != aggregationCumulative {
		t.Errorf("aggregation_temporality = %v, want cumulative", f.num64)
	}
	dps := pbAll(hist, 1)
	if len(dps) != 1 {
		t.Fatalf("data points = %d, want 1", len(dps))
	}
	dp, err := parsePB(dps[0].data)
	if err != nil {
		t.Fatalf("data point: %v", err)
	}

	countField, ok := pbOne(dp, 4)
	if !ok || countField.num64 != 4 {
		t.Errorf("count = %v, want 4", countField.num64)
	}
	bounds, ok := pbOne(dp, 7)
	if !ok {
		t.Fatal("explicit_bounds absent")
	}
	buckets, ok := pbOne(dp, 6)
	if !ok {
		t.Fatal("bucket_counts absent")
	}
	nBounds, nBuckets := len(unpackDouble(bounds.data)), len(unpackFixed64(buckets.data))
	if nBuckets != nBounds+1 {
		t.Fatalf("bucket_counts = %d, explicit_bounds = %d; OTLP requires exactly one more bucket than bounds", nBuckets, nBounds)
	}

	counts := unpackFixed64(buckets.data)
	var total uint64
	for _, c := range counts {
		total += c
	}
	if total != 4 {
		t.Errorf("bucket counts sum to %d, want 4; they are per-bucket, not cumulative", total)
	}
	if counts[0] != 1 {
		t.Errorf("first bucket = %d, want 1", counts[0])
	}
	if counts[len(counts)-1] != 1 {
		t.Errorf("overflow bucket = %d, want 1 (the 20-minute observation)", counts[len(counts)-1])
	}
	if v, ok := pbDouble(dp, 11); !ok || math.Abs(v-0.01) > 1e-9 {
		t.Errorf("min = %v, want 0.01", v)
	}
	if v, ok := pbDouble(dp, 12); !ok || math.Abs(v-1200) > 1e-9 {
		t.Errorf("max = %v, want 1200", v)
	}
}

// Protobuf's canonical JSON mapping sends 64-bit integers as strings. A
// timestamp emitted as a JSON number is silently rounded by every float64
// parser that reads it, producing a payload that validates and carries the
// wrong time.
func TestJSONEncodesSixtyFourBitFieldsAsStrings(t *testing.T) {
	start := time.Unix(1700000000, 123456789)
	now := start.Add(90 * time.Second)
	body, err := EncodeJSON(sampleRegistry(t).Collect(), sampleResource(), start, now)
	if err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}

	var out struct {
		ResourceMetrics []struct {
			Resource struct {
				Attributes []struct {
					Key   string `json:"key"`
					Value struct {
						StringValue string `json:"stringValue"`
					} `json:"value"`
				} `json:"attributes"`
			} `json:"resource"`
			ScopeMetrics []struct {
				Scope   struct{ Name string } `json:"scope"`
				Metrics []struct {
					Name string `json:"name"`
					Unit string `json:"unit"`
					Sum  *struct {
						DataPoints []struct {
							StartTimeUnixNano string  `json:"startTimeUnixNano"`
							TimeUnixNano      string  `json:"timeUnixNano"`
							AsDouble          float64 `json:"asDouble"`
						} `json:"dataPoints"`
						AggregationTemporality int  `json:"aggregationTemporality"`
						IsMonotonic            bool `json:"isMonotonic"`
					} `json:"sum"`
					Histogram *struct {
						DataPoints []struct {
							Count        string   `json:"count"`
							BucketCounts []string `json:"bucketCounts"`
						} `json:"dataPoints"`
					} `json:"histogram"`
				} `json:"metrics"`
			} `json:"scopeMetrics"`
		} `json:"resourceMetrics"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.ResourceMetrics) != 1 || len(out.ResourceMetrics[0].ScopeMetrics) != 1 {
		t.Fatalf("unexpected envelope shape")
	}
	rm := out.ResourceMetrics[0]
	if rm.Resource.Attributes[0].Value.StringValue != "ai-gateway" {
		t.Errorf("resource attribute = %+v", rm.Resource.Attributes[0])
	}
	if rm.ScopeMetrics[0].Scope.Name != scopeName {
		t.Errorf("scope = %q, want %q", rm.ScopeMetrics[0].Scope.Name, scopeName)
	}

	var sawSum, sawHist bool
	for _, m := range rm.ScopeMetrics[0].Metrics {
		if m.Name == metrics.MCost && m.Sum != nil {
			sawSum = true
			dp := m.Sum.DataPoints[0]
			if dp.StartTimeUnixNano != "1700000000123456789" {
				t.Errorf("startTimeUnixNano = %q, want the exact nanosecond value as a string", dp.StartTimeUnixNano)
			}
			if dp.TimeUnixNano == "" {
				t.Error("timeUnixNano absent")
			}
			if m.Sum.AggregationTemporality != aggregationCumulative || !m.Sum.IsMonotonic {
				t.Errorf("sum = %+v, want cumulative and monotonic", m.Sum)
			}
			if m.Unit != "USD" {
				t.Errorf("unit = %q, want USD", m.Unit)
			}
		}
		if m.Name == metrics.MRequestDuration && m.Histogram != nil {
			sawHist = true
			dp := m.Histogram.DataPoints[0]
			if dp.Count != "1" {
				t.Errorf("count = %q, want the string \"1\"", dp.Count)
			}
			if len(dp.BucketCounts) == 0 {
				t.Error("bucketCounts absent")
			}
		}
	}
	if !sawSum || !sawHist {
		t.Errorf("expected both a sum and a histogram in the export (sum=%v hist=%v)", sawSum, sawHist)
	}
}

// Both encodings render the same snapshot, so they must agree about it.
func TestBothEncodingsCarryTheSameMetrics(t *testing.T) {
	families := sampleRegistry(t).Collect()
	start := time.Unix(1700000000, 0)
	now := start.Add(time.Minute)

	pb := EncodeProtobuf(families, sampleResource(), start, now)
	js, err := EncodeJSON(families, sampleResource(), start, now)
	if err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}

	var out struct {
		ResourceMetrics []struct {
			ScopeMetrics []struct {
				Metrics []struct {
					Name string `json:"name"`
				} `json:"metrics"`
			} `json:"scopeMetrics"`
		} `json:"resourceMetrics"`
	}
	if err := json.Unmarshal(js, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, m := range out.ResourceMetrics[0].ScopeMetrics[0].Metrics {
		findMetric(t, pb, m.Name) // fails the test if absent from the protobuf
	}
	if len(out.ResourceMetrics[0].ScopeMetrics[0].Metrics) != len(families) {
		t.Errorf("json carried %d metrics, snapshot had %d",
			len(out.ResourceMetrics[0].ScopeMetrics[0].Metrics), len(families))
	}
}
