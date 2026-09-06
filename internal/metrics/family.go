package metrics

import (
	"slices"
	"strings"
)

// This file defines what a metric *is*, independently of how it is published.
//
// There are two publishers — the Prometheus text exposition format and the OTLP
// exporter — and they must never disagree about a number. Rather than let each
// walk the registry's internal maps, both render the same snapshot: Collect
// produces a []Family, and neither publisher can see anything else. A metric
// added to the registry therefore appears on both without being wired twice,
// which is the same reason spend and metrics share one instrumentation point.

// Kind is a metric's shape. It maps onto both publishers: a Counter is a
// Prometheus counter and an OTLP monotonic cumulative Sum, a Gauge is a gauge
// and an OTLP Gauge, a Histogram is a histogram in both.
type Kind int

const (
	KindCounter Kind = iota
	KindGauge
	KindHistogram
)

// Label is one dimension of a series. Kept as an explicit key/value pair rather
// than a struct of named fields because OTLP carries attributes as a list and
// Prometheus as a brace expression; a fixed struct would force every publisher
// to know which fields exist.
type Label struct {
	Key   string
	Value string
}

// Point is one series within a family.
//
// Value carries counters and gauges. Hist carries histograms, and is nil for
// every other kind. Integer counters are kept as float64 deliberately: every
// count this gateway records fits exactly in a float64's 53-bit mantissa many
// times over, and one representation keeps both publishers simple.
type Point struct {
	Labels []Label
	Value  float64
	Hist   *HistogramData
}

// HistogramData is a cumulative histogram: Counts[i] holds observations at or
// below Bounds[i], and Count holds every observation including those above the
// last bound.
type HistogramData struct {
	Bounds []float64
	Counts []uint64
	Sum    float64
	Count  uint64
	Min    float64
	Max    float64
}

// Family is one metric with all of its series.
//
// Unit is UCUM, as OTLP requires: "s" for seconds, "By" for bytes, "{token}"
// for a dimensionless count of tokens, "USD" for money. Prometheus ignores it —
// its convention is to put the unit in the metric name, which these names
// already do — but OTLP consumers use it to label axes, so a metric without one
// renders as a bare number in every dashboard that reads it.
type Family struct {
	Name   string
	Help   string
	Unit   string
	Kind   Kind
	Points []Point
}

// labelOrder ranks the dimensions every series shares, so a rendered label set
// reads from the general to the specific: which model group, then which
// deployment within it, then what happened.
//
// A plain alphabetical sort would be equally stable and put deployment before
// model, which is the wrong way round for a human reading a raw scrape. Any key
// not listed sorts after these, alphabetically among itself.
var labelOrder = map[string]int{
	"model":      0,
	"deployment": 1,
	"outcome":    2,
	"status":     3,
}

func labelRank(key string) int {
	if r, ok := labelOrder[key]; ok {
		return r
	}
	return len(labelOrder)
}

func compareLabelKeys(a, b string) int {
	if ra, rb := labelRank(a), labelRank(b); ra != rb {
		return ra - rb
	}
	return strings.Compare(a, b)
}

// labelsOf builds an ordered label list, dropping empty values.
//
// The ordering is what makes a scrape diffable: map iteration order would
// otherwise reorder attributes between collections and make every scrape look
// changed. Dropping empties is what keeps a series that has no deployment — a
// request rejected before dispatch reached none — from carrying deployment="".
func labelsOf(pairs ...string) []Label {
	out := make([]Label, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i] == "" || pairs[i+1] == "" {
			continue
		}
		out = append(out, Label{Key: pairs[i], Value: pairs[i+1]})
	}
	slices.SortFunc(out, func(a, b Label) int { return compareLabelKeys(a.Key, b.Key) })
	return out
}

// compareLabels orders two label sets, so a family's points are emitted in a
// stable order regardless of the order they were first observed in.
func compareLabels(a, b []Label) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := compareLabelKeys(a[i].Key, b[i].Key); c != 0 {
			return c
		}
		if c := strings.Compare(a[i].Value, b[i].Value); c != 0 {
			return c
		}
	}
	return len(a) - len(b)
}
