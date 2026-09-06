package otlp

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/erickardus/ai-gateway/internal/metrics"
)

// OTLP/JSON.
//
// The spec defines it alongside protobuf and the OpenTelemetry Collector's HTTP
// receiver accepts it, but vendor endpoints vary — so protobuf is the default
// and this is the option. It earns its place at debugging time: an export that
// a collector rejects is a payload you can read, which a protobuf body is not.
//
// The encoding is protobuf's canonical JSON mapping. Two rules of it matter
// here: field names are lowerCamelCase, and 64-bit integers are strings,
// because JSON numbers are float64 and would silently round a nanosecond
// timestamp. Getting the second wrong produces a payload that parses and
// carries the wrong time.

type jsonExport struct {
	ResourceMetrics []jsonResourceMetrics `json:"resourceMetrics"`
}

type jsonResourceMetrics struct {
	Resource     jsonResource       `json:"resource"`
	ScopeMetrics []jsonScopeMetrics `json:"scopeMetrics"`
}

type jsonResource struct {
	Attributes []jsonKeyValue `json:"attributes,omitempty"`
}

type jsonScopeMetrics struct {
	Scope   jsonScope    `json:"scope"`
	Metrics []jsonMetric `json:"metrics"`
}

type jsonScope struct {
	Name string `json:"name"`
}

type jsonKeyValue struct {
	Key   string       `json:"key"`
	Value jsonAnyValue `json:"value"`
}

type jsonAnyValue struct {
	StringValue string `json:"stringValue"`
}

type jsonMetric struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Unit        string         `json:"unit,omitempty"`
	Sum         *jsonSum       `json:"sum,omitempty"`
	Gauge       *jsonGauge     `json:"gauge,omitempty"`
	Histogram   *jsonHistogram `json:"histogram,omitempty"`
}

type jsonSum struct {
	DataPoints             []jsonNumberPoint `json:"dataPoints"`
	AggregationTemporality int               `json:"aggregationTemporality"`
	IsMonotonic            bool              `json:"isMonotonic"`
}

type jsonGauge struct {
	DataPoints []jsonNumberPoint `json:"dataPoints"`
}

type jsonHistogram struct {
	DataPoints             []jsonHistogramPoint `json:"dataPoints"`
	AggregationTemporality int                  `json:"aggregationTemporality"`
}

type jsonNumberPoint struct {
	Attributes        []jsonKeyValue `json:"attributes,omitempty"`
	StartTimeUnixNano string         `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string         `json:"timeUnixNano"`
	AsDouble          float64        `json:"asDouble"`
}

type jsonHistogramPoint struct {
	Attributes        []jsonKeyValue `json:"attributes,omitempty"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	TimeUnixNano      string         `json:"timeUnixNano"`
	Count             string         `json:"count"`
	Sum               float64        `json:"sum"`
	BucketCounts      []string       `json:"bucketCounts"`
	ExplicitBounds    []float64      `json:"explicitBounds"`
	Min               *float64       `json:"min,omitempty"`
	Max               *float64       `json:"max,omitempty"`
}

// EncodeJSON renders a snapshot as an OTLP/JSON ExportMetricsServiceRequest.
func EncodeJSON(families []metrics.Family, res Resource, startedAt, now time.Time) ([]byte, error) {
	start := strconv.FormatInt(startedAt.UnixNano(), 10)
	ts := strconv.FormatInt(now.UnixNano(), 10)

	out := jsonExport{ResourceMetrics: []jsonResourceMetrics{{
		Resource:     jsonResource{Attributes: jsonAttrs(res.Attributes)},
		ScopeMetrics: []jsonScopeMetrics{{Scope: jsonScope{Name: scopeName}}},
	}}}
	sm := &out.ResourceMetrics[0].ScopeMetrics[0]

	for _, f := range families {
		m := jsonMetric{Name: f.Name, Description: f.Help, Unit: f.Unit}
		switch f.Kind {
		case metrics.KindGauge:
			g := &jsonGauge{}
			for _, p := range f.Points {
				g.DataPoints = append(g.DataPoints, jsonNumberPoint{
					Attributes: jsonAttrs(p.Labels), TimeUnixNano: ts, AsDouble: p.Value,
				})
			}
			m.Gauge = g
		case metrics.KindHistogram:
			h := &jsonHistogram{AggregationTemporality: aggregationCumulative}
			for _, p := range f.Points {
				if p.Hist == nil {
					continue
				}
				counts := perBucket(p.Hist)
				buckets := make([]string, len(counts))
				for i, c := range counts {
					buckets[i] = strconv.FormatUint(c, 10)
				}
				dp := jsonHistogramPoint{
					Attributes:        jsonAttrs(p.Labels),
					StartTimeUnixNano: start,
					TimeUnixNano:      ts,
					Count:             strconv.FormatUint(p.Hist.Count, 10),
					Sum:               p.Hist.Sum,
					BucketCounts:      buckets,
					ExplicitBounds:    p.Hist.Bounds,
				}
				if p.Hist.Count > 0 {
					lo, hi := p.Hist.Min, p.Hist.Max
					dp.Min, dp.Max = &lo, &hi
				}
				h.DataPoints = append(h.DataPoints, dp)
			}
			m.Histogram = h
		default:
			s := &jsonSum{AggregationTemporality: aggregationCumulative, IsMonotonic: true}
			for _, p := range f.Points {
				s.DataPoints = append(s.DataPoints, jsonNumberPoint{
					Attributes:        jsonAttrs(p.Labels),
					StartTimeUnixNano: start,
					TimeUnixNano:      ts,
					AsDouble:          p.Value,
				})
			}
			m.Sum = s
		}
		sm.Metrics = append(sm.Metrics, m)
	}
	return json.Marshal(out)
}

func jsonAttrs(labels []metrics.Label) []jsonKeyValue {
	if len(labels) == 0 {
		return nil
	}
	out := make([]jsonKeyValue, 0, len(labels))
	for _, l := range labels {
		out = append(out, jsonKeyValue{Key: l.Key, Value: jsonAnyValue{StringValue: l.Value}})
	}
	return out
}
