package core

import (
	"strings"
	"time"
)

// Format identifies the wire protocol a deployment speaks. The gateway does not
// translate between formats in v1: an ingress in one format routes only to
// deployments in that same format.
type Format string

const (
	// FormatAnthropic is the Anthropic Messages API (/v1/messages).
	FormatAnthropic Format = "anthropic"
	// FormatOpenAI is the OpenAI Chat Completions API (/v1/chat/completions).
	FormatOpenAI Format = "openai"
)

// AuthMode selects which credential the gateway presents upstream.
type AuthMode string

const (
	// AuthModeAPIKey replaces the caller's credential with the deployment's own
	// configured key. The provider credential never leaves the server.
	AuthModeAPIKey AuthMode = "api_key"
	// AuthModePassthrough relays the caller's credential upstream untouched.
	// This is what preserves a Claude.ai subscription login when Claude Code
	// points at the gateway: the OAuth token in Authorization reaches Anthropic
	// unchanged, while the gateway authenticates the caller from a separate
	// custom header.
	AuthModePassthrough AuthMode = "passthrough"
)

// Upstream credential prefixes. A value carrying one of these is an Anthropic
// credential bound for the provider, never a gateway virtual key. Discriminating
// on these prefixes is what lets subscription passthrough and gateway
// authentication share a single endpoint.
const (
	// PrefixAnthropicOAuth marks a Claude.ai subscription OAuth token.
	PrefixAnthropicOAuth = "sk-ant-oat"
	// PrefixAnthropicAPI marks an Anthropic Console API key.
	PrefixAnthropicAPI = "sk-ant-api"
	// PrefixVirtualKey marks a key this gateway issued.
	PrefixVirtualKey = "sk-vk-"
)

// IsUpstreamCredential reports whether a header value carries a provider
// credential rather than a gateway virtual key.
func IsUpstreamCredential(v string) bool {
	v = StripScheme(v)
	return strings.HasPrefix(v, PrefixAnthropicOAuth) || strings.HasPrefix(v, PrefixAnthropicAPI)
}

// StripScheme removes a leading authorization scheme from a header value and
// returns the bare token.
//
// It trims surrounding whitespace before matching and compares the scheme
// case-insensitively. That matters because IsUpstreamCredential is built on it:
// a value that evaded the scheme match through padding or unusual casing would
// be misread as a gateway key rather than a provider credential.
func StripScheme(v string) string {
	v = strings.TrimSpace(v)
	for _, scheme := range []string{"bearer", "basic"} {
		if len(v) > len(scheme) &&
			strings.EqualFold(v[:len(scheme)], scheme) &&
			isSpace(v[len(scheme)]) {
			return strings.TrimSpace(v[len(scheme):])
		}
	}
	return v
}

// isSpace reports whether c is a space or tab, the only whitespace permitted
// between an authorization scheme and its credential.
func isSpace(c byte) bool { return c == ' ' || c == '\t' }

// Key is a virtual key: a credential this gateway issued to a caller. The
// plaintext key is never stored; Hash is the SHA-256 hex digest used for lookup.
type Key struct {
	Hash             string     `json:"hash"`
	Alias            string     `json:"alias,omitempty"`
	Models           []string   `json:"models,omitempty"`
	RPMLimit         int        `json:"rpm_limit,omitempty"`
	TPMLimit         int        `json:"tpm_limit,omitempty"`
	AllowPassthrough bool       `json:"allow_passthrough,omitempty"`
	Blocked          bool       `json:"blocked,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	// MaxBudget caps billable spend within BudgetDuration. Zero is unlimited.
	MaxBudget float64 `json:"max_budget,omitempty"`
	// BudgetDuration is the window MaxBudget applies over; zero means the key's
	// whole lifetime.
	BudgetDuration time.Duration `json:"budget_duration,omitempty"`
}

// Expired reports whether the key's expiry has passed as of now.
func (k *Key) Expired(now time.Time) bool {
	return k.ExpiresAt != nil && now.After(*k.ExpiresAt)
}

// Usable reports whether the key may be used at all, independent of which model
// it is asking for.
func (k *Key) Usable(now time.Time) error {
	if k.Blocked {
		return ErrKeyBlocked
	}
	if k.Expired(now) {
		return ErrKeyBlocked
	}
	return nil
}

// AllowsModel reports whether the key may call the named model group. An empty
// Models list means "any model". Entries support a trailing "*" wildcard, so
// "claude-*" matches "claude-sonnet-4-5".
func (k *Key) AllowsModel(model string) bool {
	if len(k.Models) == 0 {
		return true
	}
	for _, pattern := range k.Models {
		if matchPattern(pattern, model) {
			return true
		}
	}
	return false
}

// matchPattern matches an exact name or a trailing-"*" prefix pattern. A bare
// "*" matches everything.
func matchPattern(pattern, s string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(s, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == s
}

// Usage records token consumption reported by an upstream response. Fields are
// zero when the upstream did not report them.
//
// The cache fields matter for cost rather than volume: a cache read is priced at
// a fraction of ordinary input and a cache write at a premium, and Claude Code
// leans on prompt caching heavily, so a cost computed from input and output
// alone would be wrong by a wide margin on exactly the traffic this gateway is
// built for.
type Usage struct {
	// InputTokens is input the provider charged at the ordinary input rate:
	// tokens that were neither read from nor written to its prompt cache.
	//
	// The two wire formats disagree about this, which is the single most
	// expensive thing to get wrong here. Anthropic reports an input_tokens that
	// already excludes both cache counters, while an OpenAI-compatible response
	// reports a prompt_tokens that *includes* its cached tokens. Normalizing to
	// "uncached input" at the point of parsing means a cached token is priced
	// once, by whichever field the provider reported it in, rather than twice.
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	// CacheWrite1hTokens is the part of CacheWriteTokens written with a
	// one-hour TTL rather than the default five minutes. It is a subset, not an
	// addend: Anthropic reports the two as a breakdown of the same total, and
	// prices the long one at twice base input against the short one's 1.25x.
	//
	// A gateway that flattened them would under-report the cost of exactly the
	// traffic that opts into the longer cache, which is the traffic large
	// enough to have bothered.
	CacheWrite1hTokens int
}

// Total returns every token the upstream reported, for rate limiting.
//
// CacheWrite1hTokens is deliberately absent: it is a subset of
// CacheWriteTokens, and adding it would count those tokens twice against a
// caller's TPM limit.
func (u Usage) Total() int {
	return u.PromptTokens() + u.OutputTokens
}

// PromptTokens is every token the provider read as prompt, whatever it charged
// for them: ordinary input, plus the tokens it served from its cache, plus the
// tokens it wrote into it.
//
// It is the figure a provider measures its own long-context threshold against,
// and the one a caller means by "how big is this conversation now".
func (u Usage) PromptTokens() int {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// PromptCacheUsed reports whether the provider's prompt cache took any part in
// this request — either it served tokens from an entry, or it wrote one.
//
// It is what decides whether pinning this request's prefix could pay: a prefix
// no upstream cached has no warm copy anywhere, so sending the next request
// carrying it back to the same deployment concentrates load and buys nothing.
func (u Usage) PromptCacheUsed() bool {
	return u.CacheReadTokens > 0 || u.CacheWriteTokens > 0
}

// Empty reports whether the upstream reported no usage at all.
func (u Usage) Empty() bool { return u.Total() == 0 }

// Pricing is a deployment's per-token cost, expressed per million tokens because
// that is how providers publish it and because per-token figures are small
// enough to lose precision when written by hand.
type Pricing struct {
	InputPer1M      float64 `yaml:"input_per_1m"`
	OutputPer1M     float64 `yaml:"output_per_1m"`
	CacheReadPer1M  float64 `yaml:"cache_read_per_1m"`
	CacheWritePer1M float64 `yaml:"cache_write_per_1m"`
	// CacheWrite1hPer1M prices a write to the one-hour cache, which Anthropic
	// charges at twice base input where the default five-minute write costs
	// 1.25x. Left unset it falls back to CacheWritePer1M, which is correct for
	// a deployment whose callers never ask for the longer TTL and merely
	// optimistic for one whose callers do — so the first response that reports
	// a long write at a deployment without this price is logged, rather than
	// left to be discovered on an invoice.
	CacheWrite1hPer1M float64 `yaml:"cache_write_1h_per_1m"`
	// CacheWritesFree declares that this upstream charges nothing to write its
	// prompt cache, so an absent write price means zero rather than "unset".
	//
	// It exists because "speaks the Anthropic wire format" is not the same fact
	// as "is Anthropic, and prices like it". Anthropic charges a premium to
	// write, so a write price is required on an `anthropic` deployment and
	// required above input — but an Anthropic-compatible endpoint in front of
	// another model need not charge for writes at all, and Moonshot's does not.
	// Without this flag such a deployment cannot be priced: every price an
	// operator could write there would invent a premium the invoice never
	// shows, so the deployment is left unpriced and reports real billable
	// traffic at zero.
	//
	// It also sharpens an `openai` deployment, where the write price is
	// optional and an unset one falls back to the input rate — a deliberate
	// approximation for Qwen and MiniMax, which do charge, but an overstatement
	// for the larger part of that ecosystem which does not. Declaring writes
	// free replaces the approximation with the actual figure.
	//
	// Requires InputPer1M, and contradicts a non-zero write price at the same
	// tier; both are refused at load.
	CacheWritesFree bool `yaml:"cache_writes_free"`
	// LongContext prices a request whose prompt crosses a threshold the
	// provider charges a higher rate above. Anthropic's long-context tier is
	// the one in use today: a request whose prompt exceeds 200k tokens is
	// charged more for every class of token it uses, cache reads and writes
	// included.
	//
	// It is optional because most deployments never reach the threshold, and
	// nil where a provider has no such tier. Where a provider does have one and
	// this is left unset, the largest requests — the ones a prompt cache exists
	// for — are billed at the small-request rate, which understates the bill by
	// the whole of the premium.
	LongContext *LongContextPricing `yaml:"long_context"`
}

// LongContextPricing is what a deployment charges for a request whose prompt is
// large enough to cross into the provider's higher tier.
//
// The threshold is written out rather than assumed, because it is a fact about
// one provider's price list rather than about prompt caching: hardcoding 200k
// would silently misprice any deployment whose provider draws the line
// somewhere else, or moves it.
type LongContextPricing struct {
	// AbovePromptTokens is the prompt size, in tokens, above which these rates
	// apply. The comparison is against every prompt token the provider
	// reported — ordinary input, cache reads and cache writes together — since
	// that is the figure a provider measures its own threshold against.
	AbovePromptTokens int     `yaml:"above_prompt_tokens"`
	InputPer1M        float64 `yaml:"input_per_1m"`
	OutputPer1M       float64 `yaml:"output_per_1m"`
	CacheReadPer1M    float64 `yaml:"cache_read_per_1m"`
	CacheWritePer1M   float64 `yaml:"cache_write_per_1m"`
	CacheWrite1hPer1M float64 `yaml:"cache_write_1h_per_1m"`
}

// Zero reports whether no pricing was configured.
func (p Pricing) Zero() bool {
	return p.InputPer1M == 0 && p.OutputPer1M == 0 && p.CacheReadPer1M == 0 &&
		p.CacheWritePer1M == 0 && p.CacheWrite1hPer1M == 0 && p.LongContext == nil &&
		!p.CacheWritesFree
}

// rates is the price of each class of token for one request, once the tier the
// request falls into has been decided.
type rates struct {
	input, output, cacheRead, cacheWrite, cacheWrite1h float64
}

// rates resolves what this request is charged at.
//
// A tier is a property of the request rather than of the deployment: the same
// upstream charges one set of rates for a small prompt and another for a large
// one, so the threshold is compared against what the provider actually reported
// for this request rather than against anything configured.
//
// Every rate falls back to its base counterpart when the tier leaves it unset,
// so a partially written tier overcharges nothing it does not name. Validation
// refuses that at load; the fallback is what keeps a config loaded by an older
// binary from mispricing rather than crashing.
func (p Pricing) rates(u Usage) rates {
	// An unset cache-write price falls back to the input rate, not to nothing.
	// A free cache write does not exist: an omitted price means the provider
	// names no separate one, and the tokens are then charged as ordinary input.
	//
	// It matters because the price is optional on an `openai` deployment, where
	// most providers cache automatically and write for free — but Alibaba's Qwen
	// and MiniMax do charge, and report what they wrote nested inside
	// prompt_tokens_details. Those tokens are counted as writes wherever a
	// provider reports them, so without this fallback an operator who left the
	// optional price unset would see them billed at zero, which is further from
	// the invoice than the input rate they were charged at before the counter
	// was read at all.
	//
	// It never fires on an `anthropic` deployment: validation requires a write
	// price there, and requires it above input.
	//
	// Unless the deployment declares its writes free, which is the one case
	// where an absent price is an actual zero rather than an unstated one.
	writeRate := orRate(p.CacheWritePer1M, p.InputPer1M)
	if p.CacheWritesFree {
		writeRate = 0
	}
	base := rates{
		input:        p.InputPer1M,
		output:       p.OutputPer1M,
		cacheRead:    p.CacheReadPer1M,
		cacheWrite:   writeRate,
		cacheWrite1h: orRate(p.CacheWrite1hPer1M, writeRate),
	}
	long := p.TierFor(u)
	if long == nil {
		return base
	}
	return rates{
		input:      orRate(long.InputPer1M, base.input),
		output:     orRate(long.OutputPer1M, base.output),
		cacheRead:  orRate(long.CacheReadPer1M, base.cacheRead),
		cacheWrite: orRate(long.CacheWritePer1M, base.cacheWrite),
		cacheWrite1h: orRate(
			orRate(long.CacheWrite1hPer1M, long.CacheWritePer1M),
			base.cacheWrite1h),
	}
}

// TierFor returns the higher tier this request is charged at, or nil where it
// falls under the threshold or the deployment names no tier.
//
// It is exported because pricing a request is not the only thing that has to
// know which rates applied to it: a warning about a rate that fell back has to
// name the block the operator would edit.
func (p Pricing) TierFor(u Usage) *LongContextPricing {
	long := p.LongContext
	if long == nil || long.AbovePromptTokens <= 0 || u.PromptTokens() <= long.AbovePromptTokens {
		return nil
	}
	return long
}

// orRate returns a price, or the one it falls back to where none was
// configured. It is what makes an unset one-hour write cost a five-minute
// write, and an unset long-context rate cost its base rate.
func orRate(rate, fallback float64) float64 {
	if rate == 0 {
		return fallback
	}
	return rate
}

// CacheSavings returns what the prompt cache did to this request's bill: what
// its cached tokens would have cost as ordinary input, less what they actually
// cost.
//
// It is a **net** figure, so the write premium is inside it. Establishing an
// entry costs more than the input it replaces — 1.25x at the five-minute tier,
// twice at the one-hour one — so a request that writes a cache and never reads
// it back is dearer than one that did not cache at all. This reports that as a
// negative number rather than as nothing, and the sign is the point: a
// conversation scattered across deployments pays writes nobody reads, and there
// is no other figure in the gateway where that failure shows up as money. A
// counter of it, summed over a window, is the whole feature's report card.
//
// The identity it is defined by, which the accounting fuzz test holds it to:
//
//	Cost(u) + CacheSavings(u) == Cost(everything billed as ordinary input)
//
// It is reported rather than inferred because the alternative — reading a cache
// hit rate off a dashboard and multiplying — needs the price of the deployment
// that actually served each request, which a dashboard does not have. Zero when
// the deployment is unpriced, since there is then no arithmetic to do.
func (p Pricing) CacheSavings(u Usage) float64 {
	const perMillion = 1_000_000.0
	r := p.rates(u)
	write1h := min(max(u.CacheWrite1hTokens, 0), u.CacheWriteTokens)
	write5m := u.CacheWriteTokens - write1h
	return float64(u.CacheReadTokens)*(r.input-r.cacheRead)/perMillion +
		float64(write5m)*(r.input-r.cacheWrite)/perMillion +
		float64(write1h)*(r.input-r.cacheWrite1h)/perMillion
}

// CacheBreakdown splits CacheSavings into its two monotonic halves: what
// reading the cache took off the bill, and what writing it added.
//
// The signed total is the right shape for a report a person reads — it answers
// "is caching paying for itself" in one number. It is the wrong shape for a
// metric, because a counter that can decrease is not a counter: every rate()
// over it breaks the moment a deployment writes more cache than it reads back,
// which is precisely the condition worth alerting on. Published apart, both
// halves only ever rise and their difference is still one subtraction away.
func (p Pricing) CacheBreakdown(u Usage) (discount, premium float64) {
	const perMillion = 1_000_000.0
	r := p.rates(u)
	write1h := min(max(u.CacheWrite1hTokens, 0), u.CacheWriteTokens)
	write5m := u.CacheWriteTokens - write1h

	discount = float64(u.CacheReadTokens) * (r.input - r.cacheRead) / perMillion
	premium = float64(write5m)*(r.cacheWrite-r.input)/perMillion +
		float64(write1h)*(r.cacheWrite1h-r.input)/perMillion
	// A write priced at or below input is a discount rather than a premium; it
	// is not one this gateway has seen a provider charge, but the arithmetic
	// must not report a negative premium either way.
	return max(discount, 0), max(premium, 0)
}

// Cost returns what a request cost, in the currency the pricing was written in.
//
// The two cache-write tiers are billed apart: CacheWrite1hTokens is a subset of
// CacheWriteTokens, so the short-TTL remainder is what is left after it, and a
// malformed breakdown claiming more long writes than writes is clamped rather
// than allowed to charge for tokens the provider never reported.
func (p Pricing) Cost(u Usage) float64 {
	const perMillion = 1_000_000.0
	r := p.rates(u)
	write1h := min(max(u.CacheWrite1hTokens, 0), u.CacheWriteTokens)
	write5m := u.CacheWriteTokens - write1h
	return float64(u.InputTokens)*r.input/perMillion +
		float64(u.OutputTokens)*r.output/perMillion +
		float64(u.CacheReadTokens)*r.cacheRead/perMillion +
		float64(write5m)*r.cacheWrite/perMillion +
		float64(write1h)*r.cacheWrite1h/perMillion
}

// ControlHeaders are the headers the gateway interprets for its own routing and
// then consumes. They are listed here rather than in each package that touches
// them: the reader (router) and the stripper (provider) must agree, and when
// they drift the gateway forwards its own internal directives to the provider.
var ControlHeaders = []string{
	"x-litellm-num-retries",
	"x-litellm-timeout",
	"x-litellm-stream-timeout",
	"x-litellm-tags",
}

// DefaultKeyHeaderNames are the headers that may carry a virtual key, in
// precedence order. x-litellm-api-key is accepted so tooling already pointed at
// a LiteLLM proxy works unchanged.
var DefaultKeyHeaderNames = []string{"x-gateway-key", "x-litellm-api-key"}
