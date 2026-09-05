package router

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
)

// Overrides carries per-request routing directives. A nil field means "use the
// configured value".
type Overrides struct {
	NumRetries *int
	// Timeout bounds a non-streaming request end to end.
	Timeout *time.Duration
	// StreamTimeout bounds time to the first chunk of a streaming response. It
	// is deliberately a separate field: collapsing both headers into one would
	// let a stream setting silently cap non-streaming requests.
	StreamTimeout    *time.Duration
	DisableFallbacks bool
	// Tags label the request for attribution in logs. They are consumed here
	// and never forwarded upstream.
	Tags []string
	// AllowPassthrough reports whether the calling key may use a deployment
	// that relays its own credential upstream. Keys without it are routed only
	// to deployments holding a server-side credential.
	AllowPassthrough bool
	// PromptPrefix fingerprints the cacheable prefix of the request, so the
	// router can send it back to the deployment already holding that prefix in
	// its prompt cache. Empty disables the preference for this request.
	//
	// It is a hash, never prompt text: it travels into routing state and, with
	// Redis configured, out of the process.
	PromptPrefix string
}

// OverridesFromHeaders reads the per-request routing headers. Names match
// LiteLLM's so existing tooling works unchanged. A malformed value is ignored
// rather than failing the request.
func OverridesFromHeaders(h http.Header) Overrides {
	var o Overrides
	if v := h.Get("x-litellm-num-retries"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 10 {
			o.NumRetries = &n
		}
	}
	if d, ok := parseTimeoutHeader(h.Get("x-litellm-timeout")); ok {
		o.Timeout = &d
	}
	if d, ok := parseTimeoutHeader(h.Get("x-litellm-stream-timeout")); ok {
		o.StreamTimeout = &d
	}
	if v := strings.TrimSpace(h.Get("x-litellm-tags")); v != "" {
		for _, tag := range strings.Split(v, ",") {
			if tag = strings.TrimSpace(tag); tag != "" {
				o.Tags = append(o.Tags, tag)
			}
		}
	}
	return o
}

// maxOverrideTimeout caps a client-supplied deadline. Without a ceiling a large
// value overflows the float-to-Duration conversion into a negative number,
// which reads as "no deadline at all" and would let one header pin an upstream
// connection and an in-flight routing slot open indefinitely.
const maxOverrideTimeout = 30 * time.Minute

// parseTimeoutHeader reads a timeout expressed in seconds, ignoring values that
// are malformed, non-positive, or beyond the ceiling.
func parseTimeoutHeader(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	secs, err := strconv.ParseFloat(v, 64)
	if err != nil || secs <= 0 || math.IsNaN(secs) || math.IsInf(secs, 0) {
		return 0, false
	}
	if secs > maxOverrideTimeout.Seconds() {
		return maxOverrideTimeout, true
	}
	return time.Duration(secs * float64(time.Second)), true
}

// fallbacksFor returns the model groups to try after model has failed, choosing
// the list that matches the kind of failure.
func (r *Router) fallbacksFor(model string, err error) []string {
	if isContextWindowError(err) {
		if to := lookup(r.cfg.ContextWindowFallbacks, model); len(to) > 0 {
			return to
		}
	}
	if isContentPolicyError(err) {
		if to := lookup(r.cfg.ContentPolicyFallbacks, model); len(to) > 0 {
			return to
		}
	}
	return lookup(r.cfg.Fallbacks, model)
}

func lookup(rules []config.FallbackRule, model string) []string {
	for _, rule := range rules {
		if rule.From == model {
			return rule.To
		}
	}
	return nil
}

// Error-classification markers.
//
// Providers signal these conditions only in the error body, and they word them
// differently, so classification is necessarily heuristic. Two things reduce the
// guesswork: the provider's own structured error type is checked first, and both
// lists are package variables so a deployment against a provider with different
// wording can extend them without a code change.
var (
	// ContextWindowErrorTypes are structured error type values that mean the
	// prompt exceeded the model's context window.
	ContextWindowErrorTypes = []string{"context_length_exceeded", "prompt_too_long", "context_window_exceeded"}
	// ContextWindowErrorMarkers are substrings matched against the error body
	// when no structured type is recognized.
	ContextWindowErrorMarkers = []string{"context length", "context window", "prompt is too long", "prompt_too_long", "maximum context", "too many tokens"}

	// ContentPolicyErrorTypes are structured error type values meaning the
	// request was refused on content grounds.
	ContentPolicyErrorTypes = []string{"content_policy_violation", "content_filter", "invalid_prompt"}
	// ContentPolicyErrorMarkers are the substring fallback for the above.
	ContentPolicyErrorMarkers = []string{"content policy", "content_policy", "content filter", "safety", "blocked by"}
)

// errorEnvelope covers the shapes Anthropic and OpenAI use for their structured
// error type. Absent fields decode as empty.
type errorEnvelope struct {
	Error struct {
		Type string `json:"type"`
		Code string `json:"code"`
	} `json:"error"`
	Type string `json:"type"`
}

// classifyUpstream reports whether an upstream error matches a class, preferring
// the provider's structured error type over body text.
func classifyUpstream(err error, statuses []int, types, markers []string) bool {
	var ue *core.UpstreamError
	if !errors.As(err, &ue) {
		return false
	}
	if !slices.Contains(statuses, ue.StatusCode) {
		return false
	}

	var env errorEnvelope
	if json.Unmarshal(ue.Body, &env) == nil {
		for _, candidate := range []string{env.Error.Type, env.Error.Code, env.Type} {
			if candidate == "" {
				continue
			}
			if slices.ContainsFunc(types, func(t string) bool { return strings.EqualFold(t, candidate) }) {
				return true
			}
		}
	}

	body := strings.ToLower(string(ue.Body))
	return slices.ContainsFunc(markers, func(m string) bool { return strings.Contains(body, m) })
}

// isContextWindowError reports whether the upstream rejected the request for
// exceeding the model's context window.
func isContextWindowError(err error) bool {
	return classifyUpstream(err, []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge},
		ContextWindowErrorTypes, ContextWindowErrorMarkers)
}

// isContentPolicyError reports whether the upstream refused on content grounds.
func isContentPolicyError(err error) bool {
	return classifyUpstream(err, []int{http.StatusBadRequest, http.StatusForbidden},
		ContentPolicyErrorTypes, ContentPolicyErrorMarkers)
}
