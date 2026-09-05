package router

import (
	"errors"
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
	NumRetries       *int
	Timeout          *time.Duration
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
	for _, name := range []string{"x-litellm-timeout", "x-litellm-stream-timeout"} {
		if v := h.Get(name); v != "" {
			if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
				d := time.Duration(secs * float64(time.Second))
				o.Timeout = &d
			}
		}
	}
	return o
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
