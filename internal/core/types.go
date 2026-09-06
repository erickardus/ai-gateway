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
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
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
}

// Zero reports whether no pricing was configured.
func (p Pricing) Zero() bool {
	return p.InputPer1M == 0 && p.OutputPer1M == 0 && p.CacheReadPer1M == 0 &&
		p.CacheWritePer1M == 0 && p.CacheWrite1hPer1M == 0
}

// write1hRate is what a one-hour cache write costs, falling back to the
// five-minute price where none was configured.
func (p Pricing) write1hRate() float64 {
	if p.CacheWrite1hPer1M == 0 {
		return p.CacheWritePer1M
	}
	return p.CacheWrite1hPer1M
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
	write1h := min(max(u.CacheWrite1hTokens, 0), u.CacheWriteTokens)
	write5m := u.CacheWriteTokens - write1h
	return float64(u.CacheReadTokens)*(p.InputPer1M-p.CacheReadPer1M)/perMillion +
		float64(write5m)*(p.InputPer1M-p.CacheWritePer1M)/perMillion +
		float64(write1h)*(p.InputPer1M-p.write1hRate())/perMillion
}

// Cost returns what a request cost, in the currency the pricing was written in.
//
// The two cache-write tiers are billed apart: CacheWrite1hTokens is a subset of
// CacheWriteTokens, so the short-TTL remainder is what is left after it, and a
// malformed breakdown claiming more long writes than writes is clamped rather
// than allowed to charge for tokens the provider never reported.
func (p Pricing) Cost(u Usage) float64 {
	const perMillion = 1_000_000.0
	write1h := min(max(u.CacheWrite1hTokens, 0), u.CacheWriteTokens)
	write5m := u.CacheWriteTokens - write1h
	return float64(u.InputTokens)*p.InputPer1M/perMillion +
		float64(u.OutputTokens)*p.OutputPer1M/perMillion +
		float64(u.CacheReadTokens)*p.CacheReadPer1M/perMillion +
		float64(write5m)*p.CacheWritePer1M/perMillion +
		float64(write1h)*p.write1hRate()/perMillion
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
