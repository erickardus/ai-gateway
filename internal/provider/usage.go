package provider

import (
	"encoding/json"

	"github.com/erickardus/ai-gateway/internal/core"
)

// Usage parsing is where a prompt cache turns into money, so it is kept in one
// place with one rule: what reaches core.Usage.InputTokens is input the provider
// charged at the full input rate, and nothing else.
//
// The two wire formats report that differently, and the difference is not a
// detail:
//
//   - Anthropic reports input_tokens *excluding* both cache counters, alongside
//     cache_read_input_tokens and cache_creation_input_tokens.
//   - An OpenAI-compatible response reports prompt_tokens *including* its cached
//     tokens, with prompt_tokens_details.cached_tokens saying how many of them
//     were cached.
//
// Read an OpenAI response with Anthropic's rule and every cached token is
// charged twice — once at the input rate inside prompt_tokens, once at the
// cache-read rate — and the savings the gateway reports are savings nobody made.
// Ignore the field entirely, which is what a gateway does by default, and an
// OpenAI-compatible deployment simply has no prompt-cache accounting at all: its
// cheapest tokens are billed as its most expensive, and no metric says so.
//
// So the format decides the arithmetic. It is the format of the deployment the
// request was routed to, which the gateway always knows, rather than something
// guessed from the payload's shape.

// usageEnvelope covers both formats' containers; absent fields decode as zero.
type usageEnvelope struct {
	Usage *usageFields `json:"usage"`
	// Anthropic reports usage on message_start nested under "message".
	Message *struct {
		Usage *usageFields `json:"usage"`
	} `json:"message"`
}

// usageFields is every token counter either format is known to report.
//
// The token totals are pointers because their absence is meaningful: it is what
// distinguishes an event that reported no input from one that reported none,
// and an OpenAI "input_tokens" (which includes cached tokens) from Anthropic's
// (which does not).
type usageFields struct {
	// Anthropic Messages API.
	InputTokens              *int `json:"input_tokens"`
	OutputTokens             *int `json:"output_tokens"`
	CacheReadInputTokens     int  `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int  `json:"cache_creation_input_tokens"`
	// CacheCreation breaks the write total down by TTL. It appears on responses
	// from callers using extended cache lifetimes, and the two tiers are priced
	// differently enough that flattening them misprices the request.
	CacheCreation *cacheCreation `json:"cache_creation"`
	// Iterations appears on a response produced by a server-side tool loop,
	// where one reply is several turns against the model. The totals above are
	// already the sum of them, but the per-tier breakdown is reported only per
	// iteration — so a request whose long writes happened inside the loop looks
	// like a request with no long writes at all, and is billed at the
	// five-minute rate.
	Iterations []struct {
		CacheCreation *cacheCreation `json:"cache_creation"`
	} `json:"iterations"`

	// OpenAI Chat Completions.
	PromptTokens        *int `json:"prompt_tokens"`
	CompletionTokens    *int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	// InputTokensDetails is the same counter under the Responses API's naming,
	// which several OpenAI-compatible servers have adopted.
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	// DeepSeek and the servers that copied it report the hit/miss split at the
	// top level instead. Miss is not read: it is prompt_tokens minus hit, which
	// is what the subtraction below already computes.
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
}

// normalize turns one reported usage object into the gateway's own accounting,
// under the semantics of the format that produced it.
//
// It never returns a negative count and never lets cached tokens exceed the
// input they were part of: a provider reporting more cached tokens than prompt
// tokens would otherwise mint cache savings out of a typo upstream.
func (f *usageFields) normalize(format core.Format) core.Usage {
	var u core.Usage

	// The two formats spell the output count differently, and a server that
	// reports both is reporting one figure twice: whichever is present is the
	// answer, never their sum.
	u.OutputTokens = nonNegative(firstPresent(f.OutputTokens, f.CompletionTokens))

	if format == core.FormatOpenAI {
		// prompt_tokens is the whole input, cached part included, so the cached
		// count is carved out of it rather than added to it. input_tokens is the
		// same figure under the Responses API's naming — again one number under
		// two names, not two numbers.
		total := nonNegative(firstPresent(f.PromptTokens, f.InputTokens))
		cached := nonNegative(max(
			openAICached(f.PromptTokensDetails),
			openAICached(f.InputTokensDetails),
			f.PromptCacheHitTokens,
		))
		if cached > total {
			cached = total
		}
		u.CacheReadTokens = cached
		u.InputTokens = total - cached
		// No mainstream OpenAI-compatible provider charges for a cache write —
		// caching is automatic and the write is free — so there is nothing to
		// count here. A provider that starts reporting one in Anthropic's field
		// names is still read, since doing so cannot overstate the bill.
		u.CacheWriteTokens = nonNegative(f.CacheCreationInputTokens)
		return u
	}

	// Anthropic: input_tokens already excludes both cache counters, so the
	// three are simply added up at their own prices.
	u.InputTokens = nonNegative(firstPresent(f.InputTokens))
	u.CacheReadTokens = nonNegative(f.CacheReadInputTokens)
	u.CacheWriteTokens = nonNegative(f.CacheCreationInputTokens)
	if breakdown := f.writeBreakdown(); breakdown != nil {
		short, long := nonNegative(breakdown.Ephemeral5m), nonNegative(breakdown.Ephemeral1h)
		// The breakdown is authoritative where it is present and larger: some
		// responses carry the breakdown while cache_creation_input_tokens
		// reports only the five-minute tier, and undercounting a write is a
		// bill the gateway would never see coming.
		u.CacheWriteTokens = max(u.CacheWriteTokens, short+long)
		u.CacheWrite1hTokens = min(long, u.CacheWriteTokens)
	}
	return u
}

// cacheCreation is the split of a write total across the two cache lifetimes.
type cacheCreation struct {
	Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
	Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
}

// writeBreakdown returns how this response's cache writes divide between the two
// lifetimes, summing the per-iteration breakdowns of a server-tool loop where
// the response reports no breakdown of its own.
//
// Only the split is taken from the iterations, never the totals: the totals are
// already aggregated, and adding them again would bill a multi-turn reply
// several times over. The two tiers differ by more than a third in price, so a
// split that goes missing on exactly the requests large enough to run a tool
// loop is worth reassembling.
func (f *usageFields) writeBreakdown() *cacheCreation {
	if f.CacheCreation != nil {
		return f.CacheCreation
	}
	var sum cacheCreation
	found := false
	for _, it := range f.Iterations {
		if it.CacheCreation == nil {
			continue
		}
		found = true
		sum.Ephemeral5m += nonNegative(it.CacheCreation.Ephemeral5m)
		sum.Ephemeral1h += nonNegative(it.CacheCreation.Ephemeral1h)
	}
	if !found {
		return nil
	}
	// Whatever the iterations did not account for is a write at the default
	// lifetime: the total is authoritative, and attributing an unexplained
	// remainder to the long tier would charge the premium for it.
	if rest := nonNegative(f.CacheCreationInputTokens) - sum.Ephemeral5m - sum.Ephemeral1h; rest > 0 {
		sum.Ephemeral5m += rest
	}
	return &sum
}

// openAICached reads a cached-token count out of either details object.
func openAICached(details *struct {
	CachedTokens int `json:"cached_tokens"`
}) int {
	if details == nil {
		return 0
	}
	return details.CachedTokens
}

// firstPresent returns the first counter a response actually reported, so two
// spellings of one figure are never added together.
func firstPresent(values ...*int) int {
	for _, v := range values {
		if v != nil {
			return *v
		}
	}
	return 0
}

func nonNegative(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// merge folds one event's usage into the running tally.
//
// A streamed reply reports its counters across several events — Anthropic sends
// the input and cache counts on message_start and the output count on
// message_delta — so the tally keeps the largest figure seen for each field
// rather than the last. That is correct for both formats: neither ever revises a
// counter downwards within one response.
//
// The one-hour subset is kept consistent with the total it belongs to, so a
// later event that raises only the total cannot leave the subset describing more
// long writes than there were writes.
func mergeUsage(into *core.Usage, next core.Usage) {
	into.InputTokens = max(into.InputTokens, next.InputTokens)
	into.OutputTokens = max(into.OutputTokens, next.OutputTokens)
	into.CacheReadTokens = max(into.CacheReadTokens, next.CacheReadTokens)
	into.CacheWriteTokens = max(into.CacheWriteTokens, next.CacheWriteTokens)
	into.CacheWrite1hTokens = min(max(into.CacheWrite1hTokens, next.CacheWrite1hTokens), into.CacheWriteTokens)
}

// UsageFromBody reads the token usage out of a complete, non-streamed response
// body. It is the same parse the relay performs on the fly, exposed for callers
// that already hold the whole body.
func UsageFromBody(body []byte, format core.Format) (core.Usage, bool) {
	var env usageEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return core.Usage{}, false
	}
	fields := env.Usage
	if fields == nil && env.Message != nil {
		fields = env.Message.Usage
	}
	if fields == nil {
		return core.Usage{}, false
	}
	return fields.normalize(format), true
}
