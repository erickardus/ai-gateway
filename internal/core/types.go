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
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Total returns the combined token count.
func (u Usage) Total() int { return u.InputTokens + u.OutputTokens }

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
