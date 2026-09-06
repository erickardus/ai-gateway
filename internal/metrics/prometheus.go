package metrics

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// writePrometheus renders a snapshot in the Prometheus text exposition format.
//
// The format carries no unit field — its convention is to put the unit in the
// metric name, which these names do — so Family.Unit is unused here and matters
// only to OTLP.
func writePrometheus(w io.Writer, families []Family) (int64, error) {
	var b strings.Builder
	for _, f := range families {
		if len(f.Points) == 0 {
			continue
		}
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", f.Name, f.Help, f.Name, typeName(f.Kind))
		for _, p := range f.Points {
			switch f.Kind {
			case KindHistogram:
				writeHistogramPoint(&b, f.Name, p)
			default:
				fmt.Fprintf(&b, "%s%s %s\n", f.Name, renderLabels(p.Labels), formatFloat(p.Value))
			}
		}
	}
	n, err := io.WriteString(w, b.String())
	return int64(n), err
}

func typeName(k Kind) string {
	switch k {
	case KindGauge:
		return "gauge"
	case KindHistogram:
		return "histogram"
	default:
		return "counter"
	}
}

func writeHistogramPoint(b *strings.Builder, name string, p Point) {
	h := p.Hist
	if h == nil {
		return
	}
	for i, bound := range h.Bounds {
		var count uint64
		if i < len(h.Counts) {
			count = h.Counts[i]
		}
		fmt.Fprintf(b, "%s_bucket%s %d\n", name, renderLabels(withLE(p.Labels, formatFloat(bound))), count)
	}
	fmt.Fprintf(b, "%s_bucket%s %d\n", name, renderLabels(withLE(p.Labels, "+Inf")), h.Count)
	fmt.Fprintf(b, "%s_sum%s %s\n", name, renderLabels(p.Labels), formatFloat(h.Sum))
	fmt.Fprintf(b, "%s_count%s %d\n", name, renderLabels(p.Labels), h.Count)
}

// withLE appends the le label a histogram bucket requires. It goes last rather
// than in sorted position because the exposition format reads by name, and
// keeping it at the end leaves the identifying labels of a series contiguous
// and legible in a raw scrape.
func withLE(labels []Label, le string) []Label {
	out := make([]Label, 0, len(labels)+1)
	out = append(out, labels...)
	return append(out, Label{Key: "le", Value: le})
}

func renderLabels(labels []Label) string {
	if len(labels) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, l := range labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.Key)
		b.WriteString(`="`)
		b.WriteString(escape(l.Value))
		b.WriteString(`"`)
	}
	b.WriteByte('}')
	return b.String()
}

// escape applies the exposition format's label-value escaping. Deployment IDs
// and model names come from configuration, but escaping them anyway keeps a
// stray quote or newline from producing a corrupt scrape.
func escape(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

// formatFloat renders a float in the shortest form that round-trips, which is
// what the exposition format expects.
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
