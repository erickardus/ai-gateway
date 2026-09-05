package router

import (
	"errors"
	"math"
	"net/http"
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
	// AllowPassthrough reports whether the calling key may use a deployment
	// that relays its own credential upstream. Keys without it are routed only
	// to deployments holding a server-side credential.
	AllowPassthrough bool
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

// isContextWindowError reports whether the upstream rejected the request for
// exceeding the model's context window. Providers signal this only in the error
// body, so the text is matched rather than a status code.
func isContextWindowError(err error) bool {
	var ue *core.UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != http.StatusBadRequest {
		return false
	}
	body := strings.ToLower(string(ue.Body))
	for _, marker := range []string{"context length", "context window", "prompt is too long", "prompt_too_long", "maximum context", "too many tokens"} {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}

// isContentPolicyError reports whether the upstream refused on content policy
// grounds.
func isContentPolicyError(err error) bool {
	var ue *core.UpstreamError
	if !errors.As(err, &ue) {
		return false
	}
	if ue.StatusCode != http.StatusBadRequest && ue.StatusCode != http.StatusForbidden {
		return false
	}
	body := strings.ToLower(string(ue.Body))
	for _, marker := range []string{"content policy", "content_policy", "content filter", "safety", "blocked by"} {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}
