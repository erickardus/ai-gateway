package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
			io.WriteString(w, u.streamBody(cached, askedForStreamUsage(body)))
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

// breakpoint is the ephemeral cache_control marker a caller places to say where
// its cacheable prefix ends.
var breakpoint = map[string]any{"type": "ephemeral"}

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

// askedForStreamUsage reports whether a request opted into usage on a streamed
// reply. It is the condition OpenAI puts on reporting any: without it the stream
// ends with no usage object anywhere, and a gateway has nothing to bill.
func askedForStreamUsage(body []byte) bool {
	var doc struct {
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return false
	}
	return doc.StreamOptions != nil && doc.StreamOptions.IncludeUsage
}

// streamBody is the response as an event sequence. Anthropic reports usage on
// message_start whether or not it was asked to; an OpenAI-compatible server
// reports it in a final chunk only when the request opted in, which is the
// difference that decides whether the deployment is billed at all.
func (u *cachingUpstream) streamBody(cached, withUsage bool) string {
	if u.format == core.FormatOpenAI {
		return u.openAIStreamBody(cached, withUsage)
	}
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

// openAIStreamBody is the chunk sequence a streamed Chat Completions reply
// carries: content chunks throughout, and the whole tally in one final chunk
// with an empty choices array — present only where the caller asked for it.
func (u *cachingUpstream) openAIStreamBody(cached, withUsage bool) string {
	events := []string{
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}]}`,
		"",
	}
	if withUsage {
		cachedTokens := 0
		if cached {
			cachedTokens = u.prefixTokens
		}
		events = append(events, fmt.Sprintf(
			`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[],"usage":`+
				`{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,`+
				`"prompt_tokens_details":{"cached_tokens":%d}}}`,
			u.prefixTokens+uncachedInput, outputTokens,
			u.prefixTokens+uncachedInput+outputTokens, cachedTokens), "")
	}
	return strings.Join(append(events, "data: [DONE]", ""), "\n")
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
	h.Write(withoutBreakpoints(doc.System))
	h.Write(withoutBreakpoints(doc.Tools))
	for i, m := range doc.Messages {
		if i >= 2 {
			break
		}
		h.Write(withoutBreakpoints(m))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// withoutBreakpoints renders a value with every cache_control member removed.
//
// A provider caches the tokenized prompt. A breakpoint says where a prefix ends
// rather than forming part of it, so a client walking its marker forward as a
// conversation grows is still sending the prefix the earlier turn warmed, and
// the provider reads it back. A fake that hashed the marker would bill a write
// on every turn however the gateway routed, which would make the tests below
// unable to tell good routing from bad.
//
// It decodes and re-encodes rather than splicing bytes, so this oracle shares no
// code with the stripping it is here to check.
func withoutBreakpoints(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return raw
	}
	out, err := json.Marshal(stripControl(value))
	if err != nil {
		return raw
	}
	return out
}

func stripControl(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, inner := range v {
			if key == "cache_control" {
				continue
			}
			out[key] = stripControl(inner)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, inner := range v {
			out[i] = stripControl(inner)
		}
		return out
	}
	return value
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
	// Two of the three turns read the cache, each saving the gap between input
	// and cache-read pricing on the whole prefix. The first turn wrote it, at a
	// premium over the input it replaced — the figure is net, so that comes back
	// off.
	want := 2*float64(prefix)*(price.InputPer1M-price.CacheReadPer1M)/1e6 +
		float64(prefix)*(price.InputPer1M-price.CacheWritePer1M)/1e6

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
		for _, turn := range turns {
			messages = append(messages, map[string]any{"role": "user", "content": turn})
		}
	} else {
		// Claude Code's own shape: a breakpoint on the last system block, one on
		// the last tool, and one on the last message. The third is the
		// interesting one — it walks forward with every turn while the prompt
		// behind it does not change, which is the traffic prefix affinity has to
		// survive.
		document["system"] = []any{map[string]any{"type": "text", "text": longSystem, "cache_control": breakpoint}}
		document["tools"] = []any{map[string]any{"name": "read_file", "cache_control": breakpoint}}
		for i, turn := range turns {
			block := map[string]any{"type": "text", "text": turn}
			if i == len(turns)-1 {
				block["cache_control"] = breakpoint
			}
			messages = append(messages, map[string]any{"role": "user", "content": []any{block}})
		}
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

// streamed marks a request body as a streaming request, which is how a chat UI
// sends every one of them.
func streamed(body string) string {
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		panic(err)
	}
	doc["stream"] = true
	out, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return string(out)
}

// runStreamedOpenAIConversation drives a growing conversation through the
// Chat Completions endpoint, the way a chat UI does.
func runStreamedOpenAIConversation(t *testing.T, h *harness, turns int) {
	t.Helper()
	messages := []string{"first turn", "answer"}
	for i := 0; i < turns; i++ {
		body := streamed(conversationBody("openai-gpt", messages...))
		rec := h.do(t, claudeCodeRequest("/v1/chat/completions", body))
		if rec.Code != http.StatusOK {
			t.Fatalf("turn %d: status = %d, body = %s", i, rec.Code, rec.Body.String())
		}
		messages = append(messages, fmt.Sprintf("turn %d", i))
	}
}

// TestAStreamedOpenAIConversationIsBilled is the same arithmetic as the
// non-streamed case, over the transport most OpenAI-compatible traffic actually
// uses.
//
// A streamed Chat Completions reply reports no usage unless the request asked
// for it, so the whole workload below is billed at zero by a gateway that does
// not ask — and zero is a plausible-looking number that no error, header or
// metric contradicts. The companion test says what that costs.
func TestAStreamedOpenAIConversationIsBilled(t *testing.T) {
	const (
		turns  = 4
		prefix = 25_000
	)
	upstream := newCachingUpstream(core.FormatOpenAI, prefix)
	upstream.streaming = true
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, format: core.FormatOpenAI,
		upstream: upstream.handler(),
	})

	runStreamedOpenAIConversation(t, h, turns)

	price := harnessOpts{format: core.FormatOpenAI}.defaultPricing()
	// The cold turn charges the whole prefix as input; each warm turn charges it
	// once, at the cache-read price.
	want := float64(prefix)*price.InputPer1M/1e6 +
		float64(prefix*(turns-1))*price.CacheReadPer1M/1e6 +
		float64(uncachedInput*turns)*price.InputPer1M/1e6 +
		float64(outputTokens*turns)*price.OutputPer1M/1e6
	assertLedgerCost(t, h, want)

	// The prompt cache is only visible on this traffic because the usage arrived
	// at all, so the savings figure rests on the same thing the cost does.
	totals := deploymentTotals(t, h)
	if totals.CacheReadTokens != prefix*(turns-1) {
		t.Errorf("cache read tokens = %d, want %d", totals.CacheReadTokens, prefix*(turns-1))
	}
}

// TestAnUnaskedStreamIsBilledAtNothing is the companion: the same workload with
// observability.stream_usage off, which is what every gateway that does not ask
// for usage reports for streamed OpenAI-compatible traffic.
//
// It exists so the test above is known to be measuring something. A cost model
// can only be verified against a configuration where it produces a different
// answer.
func TestAnUnaskedStreamIsBilledAtNothing(t *testing.T) {
	off := false
	upstream := newCachingUpstream(core.FormatOpenAI, 25_000)
	upstream.streaming = true
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, format: core.FormatOpenAI,
		streamUsage: &off, upstream: upstream.handler(),
	})

	runStreamedOpenAIConversation(t, h, 4)

	if got := ledgerCost(t, h); got != 0 {
		t.Fatalf("cost = %.6f, want 0: this test only means something while the traffic is unbilled", got)
	}
	totals := deploymentTotals(t, h)
	if totals.InputTokens != 0 || totals.CacheReadTokens != 0 {
		t.Errorf("tokens recorded = %+v, want none", totals)
	}
	// Four turns of real traffic, worth real money, recorded as nothing at all.
	t.Logf("four streamed turns over a 25k-token prefix billed $0.00")
}

// TestAWriteNobodyReadsIsReportedAsALoss covers the direction the savings figure
// could not previously express.
//
// Writing a cache costs more than the input it replaces. A request that
// establishes an entry and never reads one back is therefore dearer than the
// same request with no caching at all — which is exactly what every turn of a
// scattered conversation looks like. Reported as a floor of zero it was
// invisible; reported as the negative number it is, it is the one figure that
// says prompt caching is currently costing this operator money.
func TestAWriteNobodyReadsIsReportedAsALoss(t *testing.T) {
	const written = 40_000
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true,
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"msg_1","type":"message","usage":`+
				`{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":0,`+
				`"cache_creation_input_tokens":%d}}`, written)
		},
	})

	if rec := h.do(t, claudeCodeRequest("/v1/messages", promptBody("be helpful", "hi"))); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	price := harnessOpts{}.defaultPricing()
	want := float64(written) * (price.InputPer1M - price.CacheWritePer1M) / 1e6
	if want >= 0 {
		t.Fatal("this test needs a write priced above input to mean anything")
	}

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
}

// spendMasterKey reaches the master-key-only reporting endpoints.
const spendMasterKey = "sk-master-MONEY"

// TestPromptCacheCostConsumesABudget covers the half of a budget that is easy to
// forget it has.
//
// A conversation through this gateway can spend most of its money on cache reads
// and writes rather than on ordinary input: that is the traffic the whole prompt
// cache exists for. A budget check that priced only input and output would let a
// key run indefinitely on the tokens it actually costs the most for.
func TestPromptCacheCostConsumesABudget(t *testing.T) {
	price := harnessOpts{}.defaultPricing()
	const read, write = 400_000, 200_000
	perRequest := float64(read)*price.CacheReadPer1M/1e6 + float64(write)*price.CacheWritePer1M/1e6

	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true,
		// Exhausted by one such request, whose entire cost is cache tokens: a
		// budget check pricing only input and output would see nothing spent
		// and admit the next one.
		maxBudget: perRequest * 0.9,
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"msg_1","type":"message","usage":`+
				`{"input_tokens":0,"output_tokens":0,`+
				`"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}`, read, write)
		},
	})

	body := promptBody("be helpful", "hi")
	if rec := h.do(t, claudeCodeRequest("/v1/messages", body)); rec.Code != http.StatusOK {
		t.Fatalf("first request = %d, want 200", rec.Code)
	}
	rec := h.do(t, claudeCodeRequest("/v1/messages", body))
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("second request = %d, want 402: a budget spent entirely on cache tokens was not spent at all", rec.Code)
	}

	if got := ledgerCost(t, h); !nearly(got, perRequest) {
		t.Errorf("billed $%.6f for one request, want $%.6f", got, perRequest)
	}
}

// TestAFailedAttemptIsNotBilled covers what a retry does to the ledger.
//
// A request that failed on one deployment and succeeded on another is one
// answer, and the operator pays for one. Charging the failed attempt would bill
// for a response nobody received; charging the wrong deployment would send an
// operator looking for capacity in the wrong place.
func TestAFailedAttemptIsNotBilled(t *testing.T) {
	// The first request to reach any upstream fails, so a retry is certain
	// whichever deployment the strategy picked first.
	var served atomic.Int64
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		if served.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"overloaded"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","usage":`+
			`{"input_tokens":1000,"output_tokens":500,"cache_read_input_tokens":20000,"cache_creation_input_tokens":2000}}`)
	}
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, extraDeployments: 1,
		upstreams: []http.HandlerFunc{upstream, upstream},
	})

	rec := h.do(t, claudeCodeRequest("/v1/messages", promptBody("be helpful", "hi")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := served.Load(); got != 2 {
		t.Fatalf("upstreams saw %d requests, want 2: the retry this test needs did not happen", got)
	}

	price := harnessOpts{}.defaultPricing()
	want := 1000*price.InputPer1M/1e6 + 500*price.OutputPer1M/1e6 +
		20000*price.CacheReadPer1M/1e6 + 2000*price.CacheWritePer1M/1e6
	assertLedgerCost(t, h, want)

	// And it is charged to the deployment that answered, which is the one whose
	// prompt cache is now warm.
	rows, err := h.ledger.Deployments(t.Context())
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	served2 := rec.Header().Get("x-gateway-deployment")
	if len(rows) != 1 {
		t.Fatalf("%d deployments hold ledger rows for one answer: %+v", len(rows), rows)
	}
	if rows[0].Subject != served2 {
		t.Errorf("the bill landed on %s, but %s served the request", rows[0].Subject, served2)
	}
}

// TestSpendTotalsCarryBothSignsOfSavings covers the report an operator reads.
//
// A fleet has turns that read caches and turns that only write them, and the
// total has to be their arithmetic sum. Summing magnitudes, or floored at zero,
// would report a fleet that is losing money on caching as one that is breaking
// even.
func TestSpendTotalsCarryBothSignsOfSavings(t *testing.T) {
	var served atomic.Int64
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, masterKey: spendMasterKey,
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if served.Add(1) == 1 {
				// A cold turn: the prefix is written and nothing is read.
				io.WriteString(w, `{"usage":{"input_tokens":10,"output_tokens":20,`+
					`"cache_read_input_tokens":0,"cache_creation_input_tokens":100000}}`)
				return
			}
			// A warm turn: the same prefix read back.
			io.WriteString(w, `{"usage":{"input_tokens":10,"output_tokens":20,`+
				`"cache_read_input_tokens":100000,"cache_creation_input_tokens":0}}`)
		},
	})

	body := promptBody("be helpful", "hi")
	for range 2 {
		if rec := h.do(t, claudeCodeRequest("/v1/messages", body)); rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
	}

	price := harnessOpts{}.defaultPricing()
	cold := 100_000 * (price.InputPer1M - price.CacheWritePer1M) / 1e6
	warm := 100_000 * (price.InputPer1M - price.CacheReadPer1M) / 1e6
	if cold >= 0 || warm <= 0 {
		t.Fatal("this test needs one turn of each sign to mean anything")
	}

	req := httptest.NewRequest(http.MethodGet, "/spend/deployments", nil)
	req.Header.Set("x-gateway-key", spendMasterKey)
	var report struct {
		TotalCacheSavings float64 `json:"total_cache_savings"`
	}
	if err := json.Unmarshal(h.do(t, req).Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !nearly(report.TotalCacheSavings, cold+warm) {
		t.Errorf("total_cache_savings = %.6f, want %.6f (a %.6f loss and a %.6f saving)",
			report.TotalCacheSavings, cold+warm, cold, warm)
	}
}

// TestAnUnpricedLongWriteIsReported covers the gap between what a deployment is
// billed and what its cost model can express.
//
// A one-hour cache write costs twice base input where the five-minute tier costs
// 1.25x. A deployment configured without cache_write_1h_per_1m falls back to the
// shorter price, which is right until a caller opts into the longer TTL and then
// understates every long write it makes by more than a third. Nothing else would
// say so: the request succeeds, the tokens are counted, and only the total is
// wrong — so the log line naming the deployment and the missing key is the whole
// mechanism.
func TestAnUnpricedLongWriteIsReported(t *testing.T) {
	const long = 50_000
	// No cache_write_1h_per_1m, which validation permits: it is correct until a
	// caller asks for the longer lifetime.
	priced := core.Pricing{InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3, CacheWritePer1M: 3.75}
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, pricing: &priced,
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"usage":{"input_tokens":10,"output_tokens":20,`+
				`"cache_creation_input_tokens":%d,`+
				`"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":%d}}}`, long, long)
		},
	})

	body := promptBody("be helpful", "hi")
	for range 3 {
		if rec := h.do(t, claudeCodeRequest("/v1/messages", body)); rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
	}

	logs := h.logBuf.String()
	if !strings.Contains(logs, "one-hour cache writes at a deployment priced only for five-minute ones") {
		t.Errorf("nothing in the log says the bill is understated:\n%s", logs)
	}
	// It is a fact about the deployment's configuration, so it is said once
	// however much traffic is mispriced.
	if got := strings.Count(logs, "cache_write_1h_per_1m"); got != 1 {
		t.Errorf("warned %d times, want 1", got)
	}
	if !strings.Contains(logs, h.deploymentID) {
		t.Errorf("the warning does not name the deployment to fix:\n%s", logs)
	}

	// Meanwhile the traffic is billed at the price that exists, which is the
	// understatement the warning is about.
	want := 3 * (float64(long)*priced.CacheWritePer1M/1e6 + 10*priced.InputPer1M/1e6 + 20*priced.OutputPer1M/1e6)
	assertLedgerCost(t, h, want)
}

// A deployment that does price the long tier says nothing, or the warning would
// be noise on a correctly configured fleet.
func TestACorrectlyPricedLongWriteIsSilent(t *testing.T) {
	priced := core.Pricing{
		InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3,
		CacheWritePer1M: 3.75, CacheWrite1hPer1M: 6,
	}
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, pricing: &priced,
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"usage":{"input_tokens":10,"output_tokens":20,`+
				`"cache_creation_input_tokens":50000,`+
				`"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":50000}}}`)
		},
	})

	h.do(t, claudeCodeRequest("/v1/messages", promptBody("be helpful", "hi")))

	if logs := h.logBuf.String(); strings.Contains(logs, "cache_write_1h_per_1m") {
		t.Errorf("a correctly priced deployment was warned about:\n%s", logs)
	}
}

// TestALongContextConversationIsBilledAtTheTierItRanIn covers the premium a
// provider charges above a prompt size, end to end and in dollars.
//
// It is the traffic prompt caching exists for: a long conversation, most of it
// served from the cache. Priced at the small-request rates the bill is roughly
// half of what the provider charged, the savings figure is understated by the
// same proportion, and every one of those numbers looks entirely ordinary.
func TestALongContextConversationIsBilledAtTheTierItRanIn(t *testing.T) {
	const (
		cached = 190_000
		fresh  = 20_000
		out    = 1_000
	)
	priced := core.Pricing{
		InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3, CacheWritePer1M: 3.75,
		LongContext: &core.LongContextPricing{
			AbovePromptTokens: 200_000,
			InputPer1M:        6, OutputPer1M: 22.5, CacheReadPer1M: 0.6, CacheWritePer1M: 7.5,
		},
	}
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, pricing: &priced,
		upstream: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"usage":{"input_tokens":%d,"output_tokens":%d,`+
				`"cache_read_input_tokens":%d,"cache_creation_input_tokens":0}}`, fresh, out, cached)
		},
	})

	h.do(t, claudeCodeRequest("/v1/messages", promptBody("be helpful", "hi")))

	want := fresh*6/1e6 + cached*0.6/1e6 + out*22.5/1e6
	assertLedgerCost(t, h, want)

	atTheSmallRates := fresh*3/1e6 + cached*0.3/1e6 + out*15/1e6
	if nearly(want, atTheSmallRates) {
		t.Fatal("the two tiers are priced the same in this test, so it proves nothing")
	}

	// Savings are what the cache took off this bill, so they are measured
	// against the input rate this request would actually have paid.
	rows, err := h.ledger.Deployments(t.Context())
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("deployment rows = %d, want 1", len(rows))
	}
	wantSavings := cached * (6 - 0.6) / 1e6
	if !nearly(rows[0].CacheSavings, wantSavings) {
		t.Errorf("cache_savings = %.6f, want %.6f", rows[0].CacheSavings, wantSavings)
	}
}

// The same understatement can hide one tier down. A deployment that prices
// one-hour writes at the small size and a long-context block that does not
// prices a large request's long writes at the long-context five-minute rate,
// with the base one-hour price sitting there looking correct — so the warning
// asks the question the pricing asks, which is what the tier this request was
// billed at named, and points at the block to edit.
func TestAnUnpricedLongWriteInTheLongContextTierIsReported(t *testing.T) {
	const long = 250_000
	priced := core.Pricing{
		InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3,
		CacheWritePer1M: 3.75, CacheWrite1hPer1M: 6,
		LongContext: &core.LongContextPricing{
			AbovePromptTokens: 200_000,
			InputPer1M:        6, OutputPer1M: 22.5, CacheReadPer1M: 0.6, CacheWritePer1M: 7.5,
		},
	}
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, pricing: &priced,
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"usage":{"input_tokens":10,"output_tokens":20,`+
				`"cache_creation_input_tokens":%d,`+
				`"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":%d}}}`, long, long)
		},
	})

	if rec := h.do(t, claudeCodeRequest("/v1/messages", promptBody("be helpful", "hi"))); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	if logs := h.logBuf.String(); !strings.Contains(logs, "cost.long_context.cache_write_1h_per_1m") {
		t.Errorf("the warning does not name the block to edit:\n%s", logs)
	}

	// And the bill is the long-context five-minute rate, which is what the
	// fallback resolves to rather than the base one-hour price.
	want := float64(long)*7.5/1e6 + 10*6/1e6 + 20*22.5/1e6
	assertLedgerCost(t, h, want)
}
