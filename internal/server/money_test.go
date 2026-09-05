package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/spend"
)

// These tests assert on money rather than on mechanism.
//
// Every other test in this package asks whether a request was routed, marked or
// counted correctly. These ask what the operator is charged for a workload, and
// compare it against the figure worked out by hand from the provider's published
// prices. That is the only assertion a prompt-caching bug cannot pass: a
// mispriced token, a double-counted prefix and a scattered conversation all show
// up here as a number that is too large, whatever the routing headers say.

// cachingUpstream is a fake provider that behaves like one holding a prompt
// cache: the first request carrying a given prefix is billed as a cache write,
// and every later request carrying the same prefix as a cache read.
//
// The point is that it is per-upstream. A conversation that moves between
// deployments finds a cold cache each time, which is precisely the money the
// gateway's affinity exists to save, and it is invisible unless the fake charges
// for it the way a real provider does.
type cachingUpstream struct {
	mu sync.Mutex
	// warm records the prefixes this upstream has already cached.
	warm map[string]bool
	// prefixTokens is how large the cacheable prefix is, in tokens.
	prefixTokens int
	// writes and reads count what this upstream billed, so a test can say how
	// many times a workload paid the premium.
	writes, reads int
	format        core.Format
	// streaming makes the upstream answer in SSE, which is how Claude Code
	// receives every response it ever gets.
	streaming bool
}

func newCachingUpstream(format core.Format, prefixTokens int) *cachingUpstream {
	return &cachingUpstream{warm: map[string]bool{}, prefixTokens: prefixTokens, format: format}
}

// cachingCluster is one independent provider per deployment. Sharing a single
// fake between deployments would make every upstream warm as soon as any one of
// them was, which is the opposite of the situation affinity exists for: it would
// let a scattered conversation look free.
type cachingCluster []*cachingUpstream

func newCachingCluster(t *testing.T, format core.Format, prefixTokens, n int) cachingCluster {
	t.Helper()
	cluster := make(cachingCluster, n)
	for i := range cluster {
		cluster[i] = newCachingUpstream(format, prefixTokens)
	}
	return cluster
}

func (c cachingCluster) handlers() []http.HandlerFunc {
	out := make([]http.HandlerFunc, len(c))
	for i, u := range c {
		out[i] = u.handler()
	}
	return out
}

// counts totals what the whole group billed. Writes are the figure that matters:
// each one is a prefix a conversation paid the premium to establish somewhere it
// had not been before.
func (c cachingCluster) counts() (writes, reads int) {
	for _, u := range c {
		w, r := u.counts()
		writes += w
		reads += r
	}
	return writes, reads
}

func (u *cachingUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cached := u.observe(body)

		w.Header().Set("Content-Type", "application/json")
		if u.streaming {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, u.streamBody(cached))
			return
		}
		io.WriteString(w, u.body(cached))
	}
}

// observe records the request's prefix and reports whether this upstream had
// already cached it.
func (u *cachingUpstream) observe(body []byte) bool {
	fingerprint := prefixOf(body)
	u.mu.Lock()
	defer u.mu.Unlock()
	cached := u.warm[fingerprint]
	u.warm[fingerprint] = true
	if cached {
		u.reads++
	} else {
		u.writes++
	}
	return cached
}

func (u *cachingUpstream) counts() (writes, reads int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.writes, u.reads
}

// uncachedInput and output are the small per-turn figures that sit alongside the
// cached prefix: the newest message and the reply.
const (
	uncachedInput = 40
	outputTokens  = 100
)

func (u *cachingUpstream) body(cached bool) string {
	if u.format == core.FormatOpenAI {
		// prompt_tokens is the whole input including whatever was cached, which
		// is the convention that makes double billing possible.
		cachedTokens := 0
		if cached {
			cachedTokens = u.prefixTokens
		}
		return fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion","usage":`+
			`{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,`+
			`"prompt_tokens_details":{"cached_tokens":%d}}}`,
			u.prefixTokens+uncachedInput, outputTokens, u.prefixTokens+uncachedInput+outputTokens, cachedTokens)
	}
	read, write := 0, u.prefixTokens
	if cached {
		read, write = u.prefixTokens, 0
	}
	return fmt.Sprintf(`{"id":"msg_1","type":"message","usage":`+
		`{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}`,
		uncachedInput, outputTokens, read, write)
}

// streamBody is the same response as the event sequence Anthropic actually
// sends, with the cache counters on message_start and the output count on
// message_delta.
func (u *cachingUpstream) streamBody(cached bool) string {
	read, write := 0, u.prefixTokens
	if cached {
		read, write = u.prefixTokens, 0
	}
	return strings.Join([]string{
		"event: message_start",
		fmt.Sprintf(`data: {"type":"message_start","message":{"usage":{"input_tokens":%d,"output_tokens":1,`+
			`"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}}`, uncachedInput, read, write),
		"",
		"event: message_delta",
		fmt.Sprintf(`data: {"type":"message_delta","usage":{"output_tokens":%d}}`, outputTokens),
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")
}

// prefixOf hashes the part of a request a provider would cache: everything
// except the trailing message. It stands in for the provider's own tokenized
// prefix, and shares its property of being unchanged as a conversation grows.
func prefixOf(body []byte) string {
	var doc struct {
		System   json.RawMessage   `json:"system"`
		Tools    json.RawMessage   `json:"tools"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "unparsed"
	}
	h := sha256.New()
	h.Write(doc.System)
	h.Write(doc.Tools)
	for i, m := range doc.Messages {
		if i >= 2 {
			break
		}
		h.Write(m)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestAConversationPaysForItsPrefixOnce is the headline guarantee. A ten-turn
// conversation through a load-balanced group must pay the cache-write premium
// once, not once per turn, and the ledger must say so in dollars.
func TestAConversationPaysForItsPrefixOnce(t *testing.T) {
	const (
		turns  = 10
		prefix = 20_000
	)
	cluster := newCachingCluster(t, core.FormatAnthropic, prefix, 3)
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true,
		extraDeployments: 2, upstreams: cluster.handlers(),
	})

	runConversation(t, h, "/v1/messages", "anthropic-claude", turns)

	writes, reads := cluster.counts()
	if writes != 1 {
		t.Errorf("the conversation paid %d cache writes, want 1: every extra write is the premium paid for landing on a cold upstream", writes)
	}
	if reads != turns-1 {
		t.Errorf("cache reads = %d, want %d", reads, turns-1)
	}

	// Worked out from the harness's Anthropic prices: one write of the prefix,
	// nine reads of it, and ten turns of fresh input and output.
	price := harnessOpts{}.defaultPricing()
	want := float64(prefix)*price.CacheWritePer1M/1e6 +
		float64(prefix*(turns-1))*price.CacheReadPer1M/1e6 +
		float64(uncachedInput*turns)*price.InputPer1M/1e6 +
		float64(outputTokens*turns)*price.OutputPer1M/1e6
	assertLedgerCost(t, h, want)
}

// TestScatteringAConversationCostsRealMoney is the same workload with affinity
// switched off. It exists so the guarantee above is known to be doing something:
// a test that only ever runs the cheap configuration cannot tell a working
// optimization from an absent one.
func TestScatteringAConversationCostsRealMoney(t *testing.T) {
	const (
		turns  = 10
		prefix = 20_000
	)
	off := false
	cluster := newCachingCluster(t, core.FormatAnthropic, prefix, 3)
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true,
		extraDeployments: 2, upstreams: cluster.handlers(),
		promptCache: config.PromptCacheConfig{Affinity: &off},
	})

	runConversation(t, h, "/v1/messages", "anthropic-claude", turns)

	writes, _ := cluster.counts()
	if writes < 2 {
		t.Fatalf("unbalanced traffic wrote the cache %d times; with affinity off the conversation should have found a cold upstream at least once", writes)
	}

	scattered := ledgerCost(t, h)
	price := harnessOpts{}.defaultPricing()
	pinned := float64(prefix)*price.CacheWritePer1M/1e6 +
		float64(prefix*(turns-1))*price.CacheReadPer1M/1e6 +
		float64(uncachedInput*turns)*price.InputPer1M/1e6 +
		float64(outputTokens*turns)*price.OutputPer1M/1e6
	if scattered <= pinned {
		t.Errorf("scattering cost %.6f against a pinned %.6f; the pin must be the cheaper arrangement", scattered, pinned)
	}
	t.Logf("ten turns over three deployments: pinned $%.4f, scattered $%.4f (%.1fx)", pinned, scattered, scattered/pinned)
}

// TestOpenAICachedPrefixIsBilledOnce runs the same shape of workload through an
// OpenAI-compatible deployment, where the provider reports its cached tokens
// inside the input count rather than beside it.
//
// Without the format-aware reading of that field, every cached token here is
// charged twice — once as input, once as a cache read — and the gateway reports
// savings it did not make.
func TestOpenAICachedPrefixIsBilledOnce(t *testing.T) {
	const (
		turns  = 6
		prefix = 30_000
	)
	cluster := newCachingCluster(t, core.FormatOpenAI, prefix, 3)
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, format: core.FormatOpenAI,
		extraDeployments: 2, upstreams: cluster.handlers(),
	})

	runConversation(t, h, "/v1/chat/completions", "openai-gpt", turns)

	writes, reads := cluster.counts()
	if writes != 1 || reads != turns-1 {
		t.Errorf("upstream saw %d cold prefixes and %d warm ones, want 1 and %d", writes, reads, turns-1)
	}

	price := harnessOpts{format: core.FormatOpenAI}.defaultPricing()
	// The cold turn charges the whole prefix as input; each warm turn charges
	// it once, at the cache-read price.
	want := float64(prefix)*price.InputPer1M/1e6 +
		float64(prefix*(turns-1))*price.CacheReadPer1M/1e6 +
		float64(uncachedInput*turns)*price.InputPer1M/1e6 +
		float64(outputTokens*turns)*price.OutputPer1M/1e6
	assertLedgerCost(t, h, want)

	// And the tokens themselves are counted once each, which is the invariant
	// the cost above rests on.
	totals := deploymentTotals(t, h)
	wantPrompt := prefix*turns + uncachedInput*turns
	if got := totals.InputTokens + totals.CacheReadTokens; got != wantPrompt {
		t.Errorf("prompt tokens recorded = %d, want %d: a cached token counted in both buckets is a token billed twice",
			got, wantPrompt)
	}
}

// TestStreamedCacheCountersReachTheLedger covers the response shape Claude Code
// actually receives. The counters arrive on message_start, mid-stream, and are
// the only place the cache is ever mentioned.
func TestStreamedCacheCountersReachTheLedger(t *testing.T) {
	const prefix = 12_000
	upstream := newCachingUpstream(core.FormatAnthropic, prefix)
	upstream.streaming = true
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, upstream: upstream.handler(),
	})

	body := promptBody("be helpful", "first turn", "answer")
	for i := 0; i < 2; i++ {
		rec := h.do(t, claudeCodeRequest("/v1/messages", body))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	}

	totals := deploymentTotals(t, h)
	if totals.CacheWriteTokens != prefix {
		t.Errorf("cache write tokens = %d, want %d from the message_start event", totals.CacheWriteTokens, prefix)
	}
	if totals.CacheReadTokens != prefix {
		t.Errorf("cache read tokens = %d, want %d", totals.CacheReadTokens, prefix)
	}
	if totals.OutputTokens != 2*outputTokens {
		t.Errorf("output tokens = %d, want %d from the message_delta event", totals.OutputTokens, 2*outputTokens)
	}
}

// TestSavingsAreWhatWasNotChargedRatherThanAGuess checks the figure /spend
// reports beside cost. It is the number an operator uses to justify the feature,
// so it has to be the arithmetic difference against ordinary input pricing on
// the deployment that actually served the request.
func TestSavingsAreWhatWasNotChargedRatherThanAGuess(t *testing.T) {
	const prefix = 50_000
	upstream := newCachingUpstream(core.FormatAnthropic, prefix)
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, upstream: upstream.handler(),
	})

	body := promptBody("be helpful", "first turn", "answer")
	for i := 0; i < 3; i++ {
		h.do(t, claudeCodeRequest("/v1/messages", body))
	}

	price := harnessOpts{}.defaultPricing()
	// Two of the three turns read the cache; each saved the gap between input
	// and cache-read pricing on the whole prefix.
	want := 2 * float64(prefix) * (price.InputPer1M - price.CacheReadPer1M) / 1e6

	rows, err := h.ledger.Deployments(t.Context())
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("deployment rows = %d, want 1", len(rows))
	}
	if !nearly(rows[0].CacheSavings, want) {
		t.Errorf("cache savings = %.6f, want %.6f", rows[0].CacheSavings, want)
	}
	// Savings sit beside cost, never inside it: cost is what was charged.
	if rows[0].Cost >= rows[0].CacheSavings+rows[0].Cost+1 {
		t.Error("cost and savings must be separate figures")
	}
}

// TestOneHourWritesArePricedAtTheirOwnRate covers the tier that costs twice base
// input. A gateway that folds it into the five-minute price under-reports the
// bill by more than a third of every long write.
func TestOneHourWritesArePricedAtTheirOwnRate(t *testing.T) {
	const long = 40_000
	priced := core.Pricing{
		InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3,
		CacheWritePer1M: 3.75, CacheWrite1hPer1M: 6,
	}
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, pricing: &priced,
		upstream: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"usage":{"input_tokens":10,"output_tokens":20,`+
				`"cache_creation_input_tokens":%d,`+
				`"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":%d}}}`, long, long)
		},
	})

	h.do(t, claudeCodeRequest("/v1/messages", promptBody("be helpful", "hi")))

	want := float64(long)*priced.CacheWrite1hPer1M/1e6 +
		10*priced.InputPer1M/1e6 + 20*priced.OutputPer1M/1e6
	assertLedgerCost(t, h, want)

	atFiveMinuteRate := float64(long)*priced.CacheWritePer1M/1e6 +
		10*priced.InputPer1M/1e6 + 20*priced.OutputPer1M/1e6
	if nearly(want, atFiveMinuteRate) {
		t.Fatal("the two tiers are priced the same in this test, so it proves nothing")
	}
}

// TestAResponseCacheHitReportsNoPromptCacheSavings keeps the two caches apart in
// the accounting. A request the gateway answered itself called no provider, so
// counting the stored response's cache tokens again would report savings twice
// for one prompt.
func TestAResponseCacheHitReportsNoPromptCacheSavings(t *testing.T) {
	upstream := newCachingUpstream(core.FormatAnthropic, 9_000)
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, cache: true,
		upstream: upstream.handler(),
	})

	body := promptBody("be helpful", "identical request")
	first := h.do(t, claudeCodeRequest("/v1/messages", body))
	if got := first.Header().Get("x-gateway-cache"); got != "miss" {
		t.Fatalf("first request cache = %q, want miss", got)
	}
	afterFirst := ledgerCost(t, h)

	second := h.do(t, claudeCodeRequest("/v1/messages", body))
	if got := second.Header().Get("x-gateway-cache"); got != "hit" {
		t.Fatalf("second request cache = %q, want hit", got)
	}
	if got := ledgerCost(t, h); !nearly(got, afterFirst) {
		t.Errorf("cost moved from %.6f to %.6f on a request that called no provider", afterFirst, got)
	}
}

// runConversation drives a growing conversation through the gateway, which is
// what a client does between turns: the prefix stays byte-identical and the
// newest message is appended.
func runConversation(t *testing.T, h *harness, path, model string, turns int) {
	t.Helper()
	messages := []string{"first turn", "answer"}
	for i := 0; i < turns; i++ {
		body := conversationBody(model, messages...)
		rec := h.do(t, claudeCodeRequest(path, body))
		if rec.Code != http.StatusOK {
			t.Fatalf("turn %d: status = %d, body = %s", i, rec.Code, rec.Body.String())
		}
		messages = append(messages, fmt.Sprintf("turn %d", i))
	}
}

// conversationBody renders a request in the shape its format actually uses.
//
// The two put their stable prefix in different places — Anthropic in a top-level
// system block and tool table, OpenAI in a leading system message — and the
// fingerprint has to find it in both, so the test bodies are not a single shape
// with the model name swapped.
func conversationBody(model string, turns ...string) string {
	messages := make([]any, 0, len(turns)+1)
	document := map[string]any{"model": model}

	if strings.HasPrefix(model, "openai") {
		messages = append(messages, map[string]any{"role": "system", "content": longSystem})
		document["tools"] = []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "read_file"},
		}}
	} else {
		document["system"] = []any{map[string]any{"type": "text", "text": longSystem}}
		document["tools"] = []any{map[string]any{"name": "read_file"}}
	}

	for _, turn := range turns {
		messages = append(messages, map[string]any{"role": "user", "content": turn})
	}
	document["messages"] = messages

	body, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}
	return string(body)
}

func ledgerCost(t *testing.T, h *harness) float64 {
	t.Helper()
	rows, err := h.ledger.Deployments(t.Context())
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	total := 0.0
	for _, row := range rows {
		total += row.Cost
	}
	return total
}

func assertLedgerCost(t *testing.T, h *harness, want float64) {
	t.Helper()
	if got := ledgerCost(t, h); !nearly(got, want) {
		t.Errorf("billed $%.6f, want $%.6f (a gap of $%.6f on this workload alone)", got, want, got-want)
	}
}

// deploymentTotals sums every deployment's usage, since a balanced group spreads
// one workload across several rows.
func deploymentTotals(t *testing.T, h *harness) spend.Totals {
	t.Helper()
	rows, err := h.ledger.Deployments(t.Context())
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	var total spend.Totals
	for _, row := range rows {
		total.InputTokens += row.InputTokens
		total.OutputTokens += row.OutputTokens
		total.CacheReadTokens += row.CacheReadTokens
		total.CacheWriteTokens += row.CacheWriteTokens
	}
	return total
}

// nearly compares money. Costs are sums of products of floats, so an exact
// comparison would fail on association alone; a hundredth of a cent per million
// is far below anything an invoice records.
func nearly(a, b float64) bool {
	const epsilon = 1e-9
	d := a - b
	return d < epsilon && d > -epsilon
}
