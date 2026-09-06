package provider

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erickardus/ai-gateway/internal/core"
)

// The payloads below are the shapes real providers send, kept verbatim rather
// than reduced to the fields under test. A usage parser that is right about a
// hand-trimmed object and wrong about the object a provider actually sends is
// wrong where it counts, and the difference between the two is exactly the kind
// of nesting — prompt_tokens_details, cache_creation — that carries the cache
// counters.

// TestUsageIsReadUnderTheFormatThatProducedIt is the money test for parsing.
//
// The two formats disagree about what an input count includes, and the whole
// cost model rests on resolving that disagreement the same way the provider's
// invoice does.
func TestUsageIsReadUnderTheFormatThatProducedIt(t *testing.T) {
	tests := []struct {
		name    string
		format  core.Format
		payload string
		want    core.Usage
	}{
		{
			name:   "anthropic non-streaming with both cache counters",
			format: core.FormatAnthropic,
			payload: `{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4-5",` +
				`"usage":{"input_tokens":137,"output_tokens":842,` +
				`"cache_creation_input_tokens":0,"cache_read_input_tokens":21504}}`,
			// input_tokens already excludes both cache counters, so nothing is
			// carved out of it.
			want: core.Usage{InputTokens: 137, OutputTokens: 842, CacheReadTokens: 21504},
		},
		{
			name:   "anthropic cold turn writing the cache",
			format: core.FormatAnthropic,
			payload: `{"usage":{"input_tokens":88,"output_tokens":516,` +
				`"cache_creation_input_tokens":21504,"cache_read_input_tokens":0}}`,
			want: core.Usage{InputTokens: 88, OutputTokens: 516, CacheWriteTokens: 21504},
		},
		{
			name:   "anthropic breakdown by cache lifetime",
			format: core.FormatAnthropic,
			payload: `{"usage":{"input_tokens":12,"output_tokens":30,"cache_read_input_tokens":100,` +
				`"cache_creation_input_tokens":4096,` +
				`"cache_creation":{"ephemeral_5m_input_tokens":1024,"ephemeral_1h_input_tokens":3072}}}`,
			want: core.Usage{
				InputTokens: 12, OutputTokens: 30, CacheReadTokens: 100,
				CacheWriteTokens: 4096, CacheWrite1hTokens: 3072,
			},
		},
		{
			name:   "anthropic breakdown larger than the flat total",
			format: core.FormatAnthropic,
			// Some responses report only the short tier in the flat field while
			// the breakdown carries both. Believing the smaller number would
			// under-bill every long write.
			payload: `{"usage":{"input_tokens":4,"output_tokens":6,"cache_creation_input_tokens":1024,` +
				`"cache_creation":{"ephemeral_5m_input_tokens":1024,"ephemeral_1h_input_tokens":2048}}}`,
			want: core.Usage{InputTokens: 4, OutputTokens: 6, CacheWriteTokens: 3072, CacheWrite1hTokens: 2048},
		},
		{
			name:   "openai chat completions with a cached prefix",
			format: core.FormatOpenAI,
			payload: `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-5",` +
				`"usage":{"prompt_tokens":2048,"completion_tokens":97,"total_tokens":2145,` +
				`"prompt_tokens_details":{"cached_tokens":1920,"audio_tokens":0},` +
				`"completion_tokens_details":{"reasoning_tokens":64}}}`,
			// prompt_tokens includes the cached tokens, so uncached input is
			// what is left after carving them out: 2048 - 1920.
			want: core.Usage{InputTokens: 128, OutputTokens: 97, CacheReadTokens: 1920},
		},
		{
			name:   "openai with no cached prefix",
			format: core.FormatOpenAI,
			payload: `{"usage":{"prompt_tokens":2048,"completion_tokens":97,` +
				`"prompt_tokens_details":{"cached_tokens":0}}}`,
			want: core.Usage{InputTokens: 2048, OutputTokens: 97},
		},
		{
			name:    "openai-compatible server omitting the details object",
			format:  core.FormatOpenAI,
			payload: `{"usage":{"prompt_tokens":700,"completion_tokens":12}}`,
			want:    core.Usage{InputTokens: 700, OutputTokens: 12},
		},
		{
			name:   "openai responses-style naming",
			format: core.FormatOpenAI,
			payload: `{"usage":{"input_tokens":5000,"output_tokens":250,` +
				`"input_tokens_details":{"cached_tokens":4096}}}`,
			// input_tokens under this naming is the whole input, unlike
			// Anthropic's field of the same name.
			want: core.Usage{InputTokens: 904, OutputTokens: 250, CacheReadTokens: 4096},
		},
		{
			name:   "deepseek-style hit and miss split",
			format: core.FormatOpenAI,
			payload: `{"usage":{"prompt_tokens":3000,"completion_tokens":40,` +
				`"prompt_cache_hit_tokens":2560,"prompt_cache_miss_tokens":440}}`,
			want: core.Usage{InputTokens: 440, OutputTokens: 40, CacheReadTokens: 2560},
		},
		{
			name:   "a cached count larger than the input it belongs to",
			format: core.FormatOpenAI,
			// Nonsense from upstream must not mint savings: the read is clamped
			// to the input it was part of rather than driving InputTokens
			// negative and inflating what caching appears to have saved.
			payload: `{"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":9000}}}`,
			want:    core.Usage{InputTokens: 0, OutputTokens: 5, CacheReadTokens: 100},
		},
		{
			name:    "negative counters are refused rather than credited",
			format:  core.FormatAnthropic,
			payload: `{"usage":{"input_tokens":-5,"output_tokens":10,"cache_read_input_tokens":-1}}`,
			want:    core.Usage{OutputTokens: 10},
		},
		{
			name:    "an envelope with no usage at all",
			format:  core.FormatAnthropic,
			payload: `{"id":"msg_2","type":"message","content":[]}`,
			want:    core.Usage{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := UsageFromBody([]byte(tt.payload), tt.format)
			if got != tt.want {
				t.Errorf("usage = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestOpenAICachedTokensAreNeverBilledTwice states the invariant directly, in
// the currency it matters in.
//
// Reading an OpenAI response with Anthropic's rule is not a rounding error: a
// conversation with a 100k cached prefix is charged for 100k tokens of fresh
// input on every turn, on top of the cache read it already paid for. At the
// prices below that is the difference between a bill and four times a bill.
func TestOpenAICachedTokensAreNeverBilledTwice(t *testing.T) {
	const payload = `{"usage":{"prompt_tokens":100000,"completion_tokens":500,` +
		`"prompt_tokens_details":{"cached_tokens":98000}}}`
	price := core.Pricing{InputPer1M: 1.25, OutputPer1M: 10, CacheReadPer1M: 0.125}

	usage, ok := UsageFromBody([]byte(payload), core.FormatOpenAI)
	if !ok {
		t.Fatal("usage was not parsed")
	}
	if sum := usage.InputTokens + usage.CacheReadTokens; sum != 100000 {
		t.Errorf("input + cache read = %d, want 100000: every prompt token must be counted exactly once", sum)
	}

	got := price.Cost(usage)
	want := 2000*1.25/1e6 + 98000*0.125/1e6 + 500*10/1e6
	if !closeEnough(got, want) {
		t.Errorf("cost = %.8f, want %.8f", got, want)
	}

	// What the same response would have cost read as an uncarved input count,
	// which is what a gateway that ignores the details object reports.
	naive := price.Cost(core.Usage{InputTokens: 100000, OutputTokens: 500, CacheReadTokens: 98000})
	if naive <= got {
		t.Fatal("the double-billing baseline should be more expensive; the test proves nothing otherwise")
	}
	if ratio := naive / got; ratio < 3 {
		t.Errorf("double billing inflates cost only %.1fx; expected the gap this test exists to prevent to be several fold", ratio)
	}
}

// TestAnthropicInputIsNotCarvedUp guards the mirror-image mistake. Applying the
// OpenAI rule to Anthropic would subtract cache reads from an input count that
// never included them, under-reporting input on every cached turn.
func TestAnthropicInputIsNotCarvedUp(t *testing.T) {
	const payload = `{"usage":{"input_tokens":137,"output_tokens":10,"cache_read_input_tokens":21504}}`
	usage, _ := UsageFromBody([]byte(payload), core.FormatAnthropic)
	if usage.InputTokens != 137 {
		t.Errorf("InputTokens = %d, want 137: Anthropic reports input excluding its cache counters", usage.InputTokens)
	}
	if usage.CacheReadTokens != 21504 {
		t.Errorf("CacheReadTokens = %d, want 21504", usage.CacheReadTokens)
	}
}

// TestStreamedUsageIsAssembledAcrossEvents covers the shape Claude Code
// actually receives. The cache counters arrive on message_start and the output
// count on message_delta, so a parser that keeps only the last event it saw
// loses whichever half it did not end on.
func TestStreamedUsageIsAssembledAcrossEvents(t *testing.T) {
	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":21,"output_tokens":1,` +
			`"cache_creation_input_tokens":0,"cache_read_input_tokens":18944}}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":402}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	usage, err := Relay(httptest.NewRecorder(), strings.NewReader(stream), core.FormatAnthropic)
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	want := core.Usage{InputTokens: 21, OutputTokens: 402, CacheReadTokens: 18944}
	if usage != want {
		t.Errorf("usage = %+v, want %+v", usage, want)
	}
}

// TestStreamedOpenAIUsageArrivesInTheFinalChunk covers the OpenAI streaming
// shape: every chunk carries a null usage until the last one, which carries the
// whole tally including the cached split.
func TestStreamedOpenAIUsageArrivesInTheFinalChunk(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}],"usage":null}`,
		"",
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[],` +
			`"usage":{"prompt_tokens":8192,"completion_tokens":120,"total_tokens":8312,` +
			`"prompt_tokens_details":{"cached_tokens":7168}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	usage, err := Relay(httptest.NewRecorder(), strings.NewReader(stream), core.FormatOpenAI)
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	want := core.Usage{InputTokens: 1024, OutputTokens: 120, CacheReadTokens: 7168}
	if usage != want {
		t.Errorf("usage = %+v, want %+v", usage, want)
	}
}

// TestStreamedTallyNeverRegresses proves the merge rule holds when a later
// event reports fewer tokens than an earlier one, which is what a provider
// interleaving per-chunk and cumulative counters looks like.
func TestStreamedTallyNeverRegresses(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"usage":{"input_tokens":50,"output_tokens":300,"cache_read_input_tokens":900,` +
			`"cache_creation_input_tokens":128,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":128}}}`,
		`data: {"usage":{"output_tokens":7}}`,
		"",
	}, "\n")

	usage, err := Relay(httptest.NewRecorder(), strings.NewReader(stream), core.FormatAnthropic)
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	want := core.Usage{
		InputTokens: 50, OutputTokens: 300, CacheReadTokens: 900,
		CacheWriteTokens: 128, CacheWrite1hTokens: 128,
	}
	if usage != want {
		t.Errorf("usage = %+v, want %+v", usage, want)
	}
}

// TestUsageSurvivesAChunkBoundaryMidField relays a response one byte at a time.
// A sniffer that parsed partial lines would read a truncated number and bill it.
func TestUsageSurvivesAChunkBoundaryMidField(t *testing.T) {
	const line = `data: {"usage":{"prompt_tokens":12345,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":12000}}}` + "\n"

	usage, err := Relay(httptest.NewRecorder(), oneByteAtATime(line), core.FormatOpenAI)
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	want := core.Usage{InputTokens: 345, OutputTokens: 10, CacheReadTokens: 12000}
	if usage != want {
		t.Errorf("usage = %+v, want %+v", usage, want)
	}
}

// oneByteAtATime is a reader that returns a single byte per Read, which is the
// worst chunking a relay can face.
func oneByteAtATime(s string) *byteReader { return &byteReader{data: []byte(s)} }

type byteReader struct {
	data []byte
	at   int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.at >= len(r.data) {
		return 0, io.EOF
	}
	p[0] = r.data[r.at]
	r.at++
	return 1, nil
}

// FuzzUsageAccounting asserts the invariants that keep a malformed or hostile
// upstream response from producing a bill nobody can explain: no panic, no
// negative counter, and — under the OpenAI rule — no token counted twice.
func FuzzUsageAccounting(f *testing.F) {
	seeds := []string{
		`{"usage":{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":4}}`,
		`{"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":9}}}`,
		`{"message":{"usage":{"input_tokens":5,"cache_creation":{"ephemeral_1h_input_tokens":7}}}}`,
		`{"usage":{"prompt_tokens":-1,"prompt_cache_hit_tokens":9223372036854775807}}`,
		`{"usage":null}`,
		`not json at all`,
	}
	for _, s := range seeds {
		f.Add(s, true)
	}

	f.Fuzz(func(t *testing.T, payload string, anthropic bool) {
		format := core.FormatOpenAI
		if anthropic {
			format = core.FormatAnthropic
		}
		usage, ok := UsageFromBody([]byte(payload), format)
		if !ok {
			if usage != (core.Usage{}) {
				t.Fatalf("usage reported despite no parse: %+v", usage)
			}
			return
		}
		if usage.InputTokens < 0 || usage.OutputTokens < 0 ||
			usage.CacheReadTokens < 0 || usage.CacheWriteTokens < 0 || usage.CacheWrite1hTokens < 0 {
			t.Fatalf("negative counter in %+v", usage)
		}
		if usage.CacheWrite1hTokens > usage.CacheWriteTokens {
			t.Fatalf("long-TTL writes exceed writes in %+v; the subset must stay inside its total", usage)
		}
		// A cost model must not go backwards on any input: a negative price for
		// a served request is a refund the provider never issued.
		price := core.Pricing{InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3, CacheWritePer1M: 3.75, CacheWrite1hPer1M: 6}
		if cost := price.Cost(usage); cost < 0 {
			t.Fatalf("negative cost %v from %+v", cost, usage)
		}
		// Savings are net, so a negative one is a real report rather than a
		// fault. What must hold on every input is the identity they are defined
		// by: what was charged plus what caching took off it is the bill the
		// same tokens would have run up as ordinary input. The bound keeps the
		// sum below from overflowing on counters no provider would report.
		const sane = 1 << 40
		if usage.InputTokens < sane && usage.CacheReadTokens < sane && usage.CacheWriteTokens < sane {
			uncached := core.Usage{
				InputTokens:  usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens,
				OutputTokens: usage.OutputTokens,
			}
			got, want := price.Cost(usage)+price.CacheSavings(usage), price.Cost(uncached)
			if diff := got - want; diff > 1e-6*(1+abs(want)) || diff < -1e-6*(1+abs(want)) {
				t.Fatalf("cost plus savings = %v, want the uncached bill %v, from %+v", got, want, usage)
			}
		}
	})
}

func closeEnough(a, b float64) bool {
	const epsilon = 1e-9
	d := a - b
	return d < epsilon && d > -epsilon
}

// TestOneFigureUnderTwoNamesIsNotAdded covers a server that spells the same
// counter both ways. Summing them would double the input on every request it
// serves, which is a bill that grows with no token count to explain it.
func TestOneFigureUnderTwoNamesIsNotAdded(t *testing.T) {
	const payload = `{"usage":{"prompt_tokens":1000,"input_tokens":1000,` +
		`"completion_tokens":50,"output_tokens":50,"prompt_tokens_details":{"cached_tokens":800}}}`

	got, ok := UsageFromBody([]byte(payload), core.FormatOpenAI)
	if !ok {
		t.Fatal("usage was not parsed")
	}
	want := core.Usage{InputTokens: 200, OutputTokens: 50, CacheReadTokens: 800}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// A reply produced by a server-side tool loop is several turns against the model
// reported as one response. The totals are already summed, but the split of its
// cache writes between the five-minute and one-hour tiers is reported only per
// iteration — so a request whose long writes all happened inside the loop looks
// like a request with no long writes, and is billed at a rate a third cheaper
// than the one the provider charged.
func TestALongWriteInsideAToolLoopIsPricedAtItsOwnTier(t *testing.T) {
	const payload = `{"usage":{"input_tokens":120,"output_tokens":400,` +
		`"cache_creation_input_tokens":30000,"cache_read_input_tokens":5000,` +
		`"iterations":[` +
		`{"cache_creation":{"ephemeral_5m_input_tokens":4000,"ephemeral_1h_input_tokens":6000}},` +
		`{"cache_creation":{"ephemeral_5m_input_tokens":2000,"ephemeral_1h_input_tokens":18000}}]}}`

	usage, ok := UsageFromBody([]byte(payload), core.FormatAnthropic)
	if !ok {
		t.Fatal("usage was not parsed")
	}
	// The totals come from the top level, which already aggregates the loop.
	// Summing the iterations into them as well would bill the reply twice.
	if usage.InputTokens != 120 || usage.CacheReadTokens != 5000 || usage.CacheWriteTokens != 30000 {
		t.Errorf("totals = %+v, want the top-level figures unchanged", usage)
	}
	if usage.CacheWrite1hTokens != 24000 {
		t.Errorf("one-hour writes = %d, want 24000 summed across the loop", usage.CacheWrite1hTokens)
	}

	price := core.Pricing{InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3, CacheWritePer1M: 3.75, CacheWrite1hPer1M: 6}
	want := 120*3/1e6 + 400*15/1e6 + 5000*0.3/1e6 + 6000*3.75/1e6 + 24000*6/1e6
	if got := price.Cost(usage); !closeEnough(got, want) {
		t.Errorf("cost = %.8f, want %.8f", got, want)
	}
}

// Whatever the per-iteration breakdowns do not account for is a write at the
// default lifetime. Attributing an unexplained remainder to the long tier would
// charge the premium for tokens the provider never said were written at it.
func TestAnUnexplainedWriteRemainderIsChargedAtTheShortTier(t *testing.T) {
	const payload = `{"usage":{"input_tokens":10,"output_tokens":20,` +
		`"cache_creation_input_tokens":9000,` +
		`"iterations":[{"cache_creation":{"ephemeral_5m_input_tokens":1000,"ephemeral_1h_input_tokens":2000}}]}}`

	usage, ok := UsageFromBody([]byte(payload), core.FormatAnthropic)
	if !ok {
		t.Fatal("usage was not parsed")
	}
	if usage.CacheWriteTokens != 9000 || usage.CacheWrite1hTokens != 2000 {
		t.Errorf("writes = %d of which %d long, want 9000 and 2000",
			usage.CacheWriteTokens, usage.CacheWrite1hTokens)
	}
}

// A response reporting its own breakdown is answering the question directly, so
// the iterations are not consulted at all.
func TestAReportedBreakdownWinsOverTheIterations(t *testing.T) {
	const payload = `{"usage":{"input_tokens":10,"output_tokens":20,` +
		`"cache_creation_input_tokens":5000,` +
		`"cache_creation":{"ephemeral_5m_input_tokens":5000,"ephemeral_1h_input_tokens":0},` +
		`"iterations":[{"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":5000}}]}}`

	usage, ok := UsageFromBody([]byte(payload), core.FormatAnthropic)
	if !ok {
		t.Fatal("usage was not parsed")
	}
	if usage.CacheWrite1hTokens != 0 {
		t.Errorf("one-hour writes = %d, want 0: the response reported its own split", usage.CacheWrite1hTokens)
	}
}
