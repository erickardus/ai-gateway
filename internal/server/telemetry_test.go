package server

import (
	"bytes"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/erickardus/ai-gateway/internal/metrics"
	"github.com/erickardus/ai-gateway/internal/otlp"
)

// scrape renders the harness's registry.
func scrape(t *testing.T, h *harness) string {
	t.Helper()
	var b strings.Builder
	if _, err := h.metrics.WriteTo(&b); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return b.String()
}

// series reads one value out of a scrape, matching the metric name and
// requiring every given label fragment to be present.
func series(t *testing.T, scrape, name string, labels ...string) (float64, bool) {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `(\{[^}]*\})? (\S+)$`)
	for _, m := range re.FindAllStringSubmatch(scrape, -1) {
		ok := true
		for _, l := range labels {
			if !strings.Contains(m[1], l) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			t.Fatalf("value %q for %s: %v", m[2], name, err)
		}
		return v, true
	}
	return 0, false
}

func mustSeries(t *testing.T, scrape, name string, labels ...string) float64 {
	t.Helper()
	v, ok := series(t, scrape, name, labels...)
	if !ok {
		t.Fatalf("metric %s%v not found in scrape:\n%s", name, labels, scrape)
	}
	return v
}

// A total token count cannot answer the question an operator actually has.
// Output tokens are what a model generates and what dominates a bill; input is
// what a prompt cache acts on. Summed together, neither is visible.
func TestTokensAreSplitByDirection(t *testing.T) {
	h := newHarness(t, harnessOpts{
		allowPassthrough: true,
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"usage":{"input_tokens":40,"output_tokens":60}}`)
		},
	})
	h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))

	out := scrape(t, h)
	if got := mustSeries(t, out, metrics.MInputTokens); got != 40 {
		t.Errorf("input tokens = %v, want 40", got)
	}
	if got := mustSeries(t, out, metrics.MOutputTokens); got != 60 {
		t.Errorf("output tokens = %v, want 60", got)
	}
	if got := mustSeries(t, out, metrics.MTokens); got != 100 {
		t.Errorf("total tokens = %v, want 100", got)
	}
}

// Net cache savings are signed: caching costs more than it saves whenever
// prefixes are written and never read back. A single counter cannot carry that,
// so the two halves are published apart and both only ever rise.
func TestCacheDiscountAndPremiumArePublishedApart(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "api_key",
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":1000000,"cache_creation_input_tokens":1000000}}`)
		},
	})
	h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))

	out := scrape(t, h)
	// The harness prices anthropic at input 3, read 0.3, write 3.75 per 1M.
	if got := mustSeries(t, out, metrics.MCacheDiscount); got != 2.7 {
		t.Errorf("discount = %v, want 2.7 (1M reads at input 3 less read 0.3)", got)
	}
	if got := mustSeries(t, out, metrics.MCacheWritePremium); got != 0.75 {
		t.Errorf("premium = %v, want 0.75 (1M writes at write 3.75 over input 3)", got)
	}
	// Neither may ever be negative, which is the property that makes them
	// counters at all.
	for _, name := range []string{metrics.MCacheDiscount, metrics.MCacheWritePremium} {
		if v := mustSeries(t, out, name); v < 0 {
			t.Errorf("%s = %v, want a non-negative counter", name, v)
		}
	}
}

// A stream that dies after its first chunk is a 200 with a truncated body: the
// request counter, the status class and the latency histogram all record a
// perfectly ordinary success. Only this counter says otherwise.
func TestStreamOutcomeIsRecordedSeparatelyFromRequestOutcome(t *testing.T) {
	h := newHarness(t, harnessOpts{
		allowPassthrough: true,
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			rc := http.NewResponseController(w)
			io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":0}}}\n\n")
			rc.Flush()
			io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":4,\"output_tokens\":11}}\n\n")
			rc.Flush()
		},
	})
	h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","stream":true,"messages":[]}`))

	out := scrape(t, h)
	if got := mustSeries(t, out, metrics.MStreams, `outcome="completed"`); got != 1 {
		t.Errorf("completed streams = %v, want 1", got)
	}
	// Time to first token must have been observed, and measured from the
	// request arriving rather than from the relay starting.
	if got := mustSeries(t, out, metrics.MTimeToFirstToken+"_count"); got != 1 {
		t.Errorf("time-to-first-token observations = %v, want 1", got)
	}
	if _, ok := series(t, out, metrics.MTimeToFirstToken+"_sum"); !ok {
		t.Error("time-to-first-token histogram has no sum")
	}
}

// A non-streamed request has no first token to time, and recording one would
// put the whole request duration into a histogram meant for perceived latency.
func TestNonStreamingRequestRecordsNoStreamMetrics(t *testing.T) {
	h := newHarness(t, harnessOpts{allowPassthrough: true})
	h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))

	out := scrape(t, h)
	if _, ok := series(t, out, metrics.MStreams, `outcome="completed"`); ok {
		t.Error("a non-streamed request was counted as a stream")
	}
	if _, ok := series(t, out, metrics.MTimeToFirstToken+"_count"); ok {
		t.Error("a non-streamed request recorded a time to first token")
	}
}

func TestUpstreamStatusClassIsRecorded(t *testing.T) {
	h := newHarness(t, harnessOpts{allowPassthrough: true})
	h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))

	out := scrape(t, h)
	if got := mustSeries(t, out, metrics.MUpstreamStatus, `status="2xx"`); got != 1 {
		t.Errorf("2xx responses = %v, want 1", got)
	}
}

func TestBodySizesAreRecorded(t *testing.T) {
	h := newHarness(t, harnessOpts{allowPassthrough: true})
	body := `{"model":"anthropic-claude","messages":[]}`
	h.do(t, claudeCodeRequest("/v1/messages", body))

	out := scrape(t, h)
	if got := mustSeries(t, out, metrics.MRequestBytes+"_sum"); got != float64(len(body)) {
		t.Errorf("request bytes = %v, want %d", got, len(body))
	}
	if got := mustSeries(t, out, metrics.MResponseBytes+"_count"); got != 1 {
		t.Errorf("response byte observations = %v, want 1", got)
	}
}

// A rejected request reached no upstream. Recording a status class or a stream
// outcome for it would put a dimension on the metric that describes nothing.
func TestRejectedRequestRecordsNoUpstreamSignals(t *testing.T) {
	h := newHarness(t, harnessOpts{allowPassthrough: true})
	h.do(t, claudeCodeRequest("/v1/messages", `{"model":"no-such-model","messages":[]}`))

	out := scrape(t, h)
	if got := mustSeries(t, out, metrics.MRejections, `outcome="model_unknown"`); got != 1 {
		t.Errorf("rejections = %v, want 1", got)
	}
	if _, ok := series(t, out, metrics.MUpstreamStatus); ok {
		t.Error("a rejected request recorded an upstream status class")
	}
}

// Prompt size is a distribution, not a total. A mean hides exactly the
// long-context requests that dominate a bill, which is what the histogram
// exists to surface.
func TestPromptSizeIsAHistogram(t *testing.T) {
	h := newHarness(t, harnessOpts{
		allowPassthrough: true,
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"usage":{"input_tokens":1000,"output_tokens":10,"cache_read_input_tokens":9000}}`)
		},
	})
	h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))

	out := scrape(t, h)
	// Prompt size counts cached input too: it is what the context window is
	// measured against, not what was charged at the input rate.
	if got := mustSeries(t, out, metrics.MPromptSize+"_sum"); got != 10000 {
		t.Errorf("prompt size = %v, want 10000 (1000 fresh + 9000 cached)", got)
	}
}

// The response cache is counted at the lookup, not inferred from the request
// outcome: a miss has no distinguishing outcome of its own, so the hit rate
// would have no denominator.
func TestResponseCacheLookupsAreCounted(t *testing.T) {
	h := newHarness(t, harnessOpts{allowPassthrough: true, cache: true})
	body := `{"model":"anthropic-claude","messages":[{"role":"user","content":"same"}]}`
	h.do(t, claudeCodeRequest("/v1/messages", body))
	h.do(t, claudeCodeRequest("/v1/messages", body))

	out := scrape(t, h)
	if got := mustSeries(t, out, metrics.MResponseCache, `outcome="miss"`); got != 1 {
		t.Errorf("cache misses = %v, want 1", got)
	}
	if got := mustSeries(t, out, metrics.MResponseCache, `outcome="hit"`); got != 1 {
		t.Errorf("cache hits = %v, want 1", got)
	}
}

// Every metric the registry publishes must carry a unit, because OTLP consumers
// label axes from it and a metric without one renders as a bare number.
func TestEveryPublishedMetricCarriesAUnit(t *testing.T) {
	r := metrics.New()
	r.Observe(metrics.Result{
		Model: "m", Deployment: "d", Outcome: metrics.OutcomeSuccess,
		Latency: 1, Tokens: 10, InputTokens: 4, OutputTokens: 6, PromptTokens: 4,
		Cost: 0.1, CacheDiscount: 0.2, CacheWritePremium: 0.05,
		CacheReadTokens: 1, CacheWriteTokens: 1, CacheWrite1hTokens: 1,
		PromptAffinity: metrics.OutcomeHit, Retries: 1, Fallbacks: 1, CooledDown: true,
		RejectReason: "rate_limited", UpstreamStatusClass: "2xx",
		RequestBytes: 10, ResponseBytes: 20,
		Streaming: true, StreamCompleted: true, TimeToFirstToken: 1, Throughput: 20,
	})
	r.RecordResponseCache("m", metrics.OutcomeHit)

	for _, f := range r.Collect() {
		if f.Unit == "" {
			t.Errorf("metric %q has no unit", f.Name)
		}
		if f.Help == "" {
			t.Errorf("metric %q has no help text", f.Name)
		}
	}
}

// A passthrough deployment bills the caller's own subscription, so it must
// record usage without ever contributing to a cost or savings counter.
func TestPassthroughRecordsUsageButNoMoney(t *testing.T) {
	h := newHarness(t, harnessOpts{
		allowPassthrough: true,
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":500}}`)
		},
	})
	h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))

	out := scrape(t, h)
	// 530, not 30: the total counts cached input too, since those tokens were
	// really processed. What passthrough must not produce is money.
	if got := mustSeries(t, out, metrics.MTokens); got != 530 {
		t.Errorf("tokens = %v, want 530 (10 input + 20 output + 500 cache reads)", got)
	}
	for _, name := range []string{metrics.MCost, metrics.MCacheDiscount, metrics.MCacheWritePremium} {
		if v, ok := series(t, out, name); ok && v != 0 {
			t.Errorf("%s = %v on passthrough traffic, want no charge at all", name, v)
		}
	}
}

// The OTLP export carries the same snapshot as a scrape, so it must be held to
// the same rule: no credential may reach a collector either. It is the easier
// one to get wrong, because the payload is opaque bytes that nobody reads.
func TestOTLPExportLeaksNoCredential(t *testing.T) {
	h := newHarness(t, harnessOpts{allowPassthrough: true, masterKey: "sk-master-SECRET"})
	h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))

	start := time.Now()
	body := otlp.EncodeProtobuf(h.metrics.Collect(),
		otlp.Resource{Attributes: []metrics.Label{{Key: "service.name", Value: "ai-gateway"}}},
		start, start)
	js, err := otlp.EncodeJSON(h.metrics.Collect(),
		otlp.Resource{Attributes: []metrics.Label{{Key: "service.name", Value: "ai-gateway"}}},
		start, start)
	if err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}

	for _, secret := range []string{testVirtualKey, "sk-master-SECRET", "sk-ant-oat01", "sk-ant-api03-SERVERSIDE"} {
		if bytes.Contains(body, []byte(secret)) {
			t.Errorf("a credential leaked into the OTLP protobuf export: %q", secret)
		}
		if bytes.Contains(js, []byte(secret)) {
			t.Errorf("a credential leaked into the OTLP JSON export: %q", secret)
		}
	}
}

// Both publishers must agree, because they render one snapshot. A metric that
// reached /metrics and not the collector would be a silent gap in exactly the
// setup where the collector is the only consumer.
func TestScrapeAndExportCarryTheSameMetrics(t *testing.T) {
	h := newHarness(t, harnessOpts{
		allowPassthrough: true,
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":5}}`)
		},
	})
	h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))
	h.do(t, claudeCodeRequest("/v1/messages", `{"model":"nope","messages":[]}`))

	out := scrape(t, h)
	start := time.Now()
	export := otlp.EncodeProtobuf(h.metrics.Collect(), otlp.Resource{}, start, start)

	for _, f := range h.metrics.Collect() {
		if !strings.Contains(out, f.Name) {
			t.Errorf("metric %q is in the snapshot but not in the scrape", f.Name)
		}
		if !bytes.Contains(export, []byte(f.Name)) {
			t.Errorf("metric %q is in the snapshot but not in the OTLP export", f.Name)
		}
	}
}
