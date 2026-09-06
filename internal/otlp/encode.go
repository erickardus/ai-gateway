package otlp

import (
	"time"

	"github.com/erickardus/ai-gateway/internal/metrics"
)

// Resource attributes identify the thing being measured, as opposed to the
// measurement. They are sent once per export rather than repeated on every
// data point, which is the main structural difference from a Prometheus scrape
// — where the equivalent has to be attached by the scraper.
type Resource struct {
	Attributes []metrics.Label
}

// scopeName identifies the instrumentation that produced these metrics. OTLP
// requires it and consumers group by it, so it names this gateway rather than a
// library.
const scopeName = "github.com/erickardus/ai-gateway"

// aggregationCumulative is AggregationTemporality.AGGREGATION_TEMPORALITY_CUMULATIVE.
//
// Cumulative rather than delta because that is what the registry actually
// holds: counters run from process start and are never reset, so every export
// carries the same start timestamp and a running total. A delta exporter would
// have to remember what it last sent and subtract, which turns a dropped export
// into permanently lost counts rather than one that the next export makes good.
const aggregationCumulative = 2

// EncodeProtobuf renders a snapshot as an OTLP ExportMetricsServiceRequest.
//
// startedAt is the process start; it becomes the start timestamp of every
// cumulative series, which is what tells a consumer that a counter jumping from
// nothing is a new process rather than a rate spike.
func EncodeProtobuf(families []metrics.Family, res Resource, startedAt, now time.Time) []byte {
	start, ts := uint64(startedAt.UnixNano()), uint64(now.UnixNano())

	var e buf
	// ExportMetricsServiceRequest.resource_metrics = 1
	e.message(1, func(rm *buf) {
		// ResourceMetrics.resource = 1
		rm.message(1, func(r *buf) {
			// Resource.attributes = 1
			for _, a := range res.Attributes {
				r.message(1, func(kv *buf) { encodeKeyValue(kv, a) })
			}
		})
		// ResourceMetrics.scope_metrics = 2
		rm.message(2, func(sm *buf) {
			// ScopeMetrics.scope = 1
			sm.message(1, func(sc *buf) {
				sc.str(1, scopeName) // InstrumentationScope.name = 1
			})
			// ScopeMetrics.metrics = 2
			for _, f := range families {
				sm.message(2, func(m *buf) { encodeMetric(m, f, start, ts) })
			}
		})
	})
	return e.b
}

// encodeKeyValue writes common.v1.KeyValue{key = 1, value = 2} whose AnyValue
// carries string_value = 1. Every attribute this gateway emits is a string:
// numbers belong in the measurement, not in the dimensions.
func encodeKeyValue(e *buf, l metrics.Label) {
	e.str(1, l.Key)
	e.message(2, func(v *buf) { v.str(1, l.Value) })
}

// encodeMetric writes metrics.v1.Metric{name = 1, description = 2, unit = 3}
// followed by whichever data field its kind selects.
func encodeMetric(e *buf, f metrics.Family, start, ts uint64) {
	e.str(1, f.Name)
	e.str(2, f.Help)
	e.str(3, f.Unit)

	switch f.Kind {
	case metrics.KindGauge:
		// Metric.gauge = 5, Gauge.data_points = 1
		e.message(5, func(g *buf) {
			for _, p := range f.Points {
				g.message(1, func(dp *buf) { encodeNumberPoint(dp, p, start, ts, false) })
			}
		})
	case metrics.KindHistogram:
		// Metric.histogram = 9
		e.message(9, func(h *buf) {
			for _, p := range f.Points {
				h.message(1, func(dp *buf) { encodeHistogramPoint(dp, p, start, ts) })
			}
			h.varint(2, aggregationCumulative) // Histogram.aggregation_temporality = 2
		})
	default:
		// Metric.sum = 7
		e.message(7, func(s *buf) {
			for _, p := range f.Points {
				s.message(1, func(dp *buf) { encodeNumberPoint(dp, p, start, ts, true) })
			}
			s.varint(2, aggregationCumulative) // Sum.aggregation_temporality = 2
			s.boolean(3, true)                 // Sum.is_monotonic = 3
		})
	}
}

// encodeNumberPoint writes metrics.v1.NumberDataPoint.
//
// A gauge carries no start timestamp: it describes the instant it was read, not
// an interval, and a consumer that sees one on a gauge may reject the point.
func encodeNumberPoint(e *buf, p metrics.Point, start, ts uint64, cumulative bool) {
	if cumulative {
		e.fixed64(2, start) // start_time_unix_nano = 2
	}
	e.fixed64(3, ts)     // time_unix_nano = 3
	e.double(4, p.Value) // as_double = 4
	for _, l := range p.Labels {
		e.message(7, func(kv *buf) { encodeKeyValue(kv, l) }) // attributes = 7
	}
}

// encodeHistogramPoint writes metrics.v1.HistogramDataPoint.
//
// bucket_counts must hold exactly one more entry than explicit_bounds: the
// registry's counts cover each bound, and the extra entry is everything above
// the last one. They are also *cumulative* in the registry and *per-bucket* on
// the wire, so they are differenced here rather than sent as held — sending the
// running totals would report a wildly wrong distribution that still parses.
func encodeHistogramPoint(e *buf, p metrics.Point, start, ts uint64) {
	h := p.Hist
	if h == nil {
		return
	}
	e.fixed64(2, start)              // start_time_unix_nano = 2
	e.fixed64(3, ts)                 // time_unix_nano = 3
	e.fixed64(4, h.Count)            // count = 4
	e.double(5, h.Sum)               // sum = 5
	e.packedFixed64(6, perBucket(h)) // bucket_counts = 6
	e.packedDouble(7, h.Bounds)      // explicit_bounds = 7
	for _, l := range p.Labels {
		e.message(9, func(kv *buf) { encodeKeyValue(kv, l) }) // attributes = 9
	}
	if h.Count > 0 {
		e.double(11, h.Min) // min = 11
		e.double(12, h.Max) // max = 12
	}
}

// perBucket converts the registry's cumulative bucket counts into the
// per-bucket counts OTLP expects, with a final entry for the overflow above the
// last bound.
func perBucket(h *metrics.HistogramData) []uint64 {
	out := make([]uint64, len(h.Bounds)+1)
	var prev uint64
	for i := range h.Bounds {
		var c uint64
		if i < len(h.Counts) {
			c = h.Counts[i]
		}
		// Guard against a decreasing sequence rather than underflowing to a
		// vast number: the counts are cumulative by construction, but an
		// unsigned subtraction is an unforgiving place to be merely confident.
		if c >= prev {
			out[i] = c - prev
		}
		prev = c
	}
	if h.Count >= prev {
		out[len(h.Bounds)] = h.Count - prev
	}
	return out
}
