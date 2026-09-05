// Package core holds the value types and sentinel errors shared by every
// gateway package. It is a leaf: it imports nothing from the rest of the
// module, so any package may depend on it without risking an import cycle.
package core

import (
	"errors"
	"fmt"
	"net/http"
)

// Sentinel errors for conditions the HTTP layer must translate into specific
// status codes. Wrap them with %w; match them with errors.Is.
var (
	// ErrKeyInvalid means no virtual key was presented, or the presented key
	// is not known to the gateway.
	ErrKeyInvalid = errors.New("virtual key invalid")
	// ErrKeyBlocked means the key exists but has been disabled or has expired.
	ErrKeyBlocked = errors.New("virtual key blocked or expired")
	// ErrModelNotAllowed means the key is valid but not permitted to call the
	// requested model group.
	ErrModelNotAllowed = errors.New("model not allowed for this key")
	// ErrModelNotFound means no model group with the requested name is
	// configured.
	ErrModelNotFound = errors.New("model not found")
	// ErrNoHealthyDeployment means every deployment in the group was filtered
	// out (cooled down, over its rate limit, or already failed this request).
	ErrNoHealthyDeployment = errors.New("no healthy deployment available")
	// ErrRateLimited means a per-key or per-deployment rate limit was hit.
	ErrRateLimited = errors.New("rate limit exceeded")
	// ErrPassthroughNotAllowed means the key may not use a passthrough
	// deployment, which would relay its own upstream credential.
	ErrPassthroughNotAllowed = errors.New("passthrough not allowed for this key")
	// ErrUpstreamHostNotAllowed means a passthrough deployment points at a host
	// outside the configured allowlist. Relaying a caller credential there
	// would leak it, so the request is refused.
	ErrUpstreamHostNotAllowed = errors.New("upstream host not in allowlist")
	// ErrBodyTooLarge means the request body exceeded the configured limit.
	ErrBodyTooLarge = errors.New("request body too large")
	// ErrBudgetExceeded means the key has spent its budget for the window.
	ErrBudgetExceeded = errors.New("budget exceeded")
)

// StatusFor maps a gateway error to the HTTP status the client should see.
// Errors that do not match a sentinel become 500.
func StatusFor(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, ErrKeyInvalid):
		return http.StatusUnauthorized
	case errors.Is(err, ErrKeyBlocked):
		return http.StatusForbidden
	case errors.Is(err, ErrModelNotAllowed), errors.Is(err, ErrPassthroughNotAllowed):
		return http.StatusForbidden
	case errors.Is(err, ErrUpstreamHostNotAllowed):
		return http.StatusForbidden
	case errors.Is(err, ErrModelNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrRateLimited):
		return http.StatusTooManyRequests
	case errors.Is(err, ErrBudgetExceeded):
		// 402 rather than 429: the caller is not going too fast, they are out of
		// money, and retrying later will not help until the window rolls over.
		return http.StatusPaymentRequired
	case errors.Is(err, ErrNoHealthyDeployment):
		return http.StatusServiceUnavailable
	case errors.Is(err, ErrBodyTooLarge):
		return http.StatusRequestEntityTooLarge
	default:
		return http.StatusInternalServerError
	}
}

// UpstreamError carries a non-2xx response from a provider. The gateway relays
// the status and body verbatim: Claude Code's capability-downgrade retry logic
// matches on the upstream's own error wording, so re-wrapping it in a gateway
// envelope would break that recovery path.
type UpstreamError struct {
	StatusCode int
	Body       []byte
	Header     http.Header
	Deployment string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream returned %d from deployment %s", e.StatusCode, e.Deployment)
}

// Retryable reports whether this status is worth another attempt. It mirrors
// the set LiteLLM retries on: 408, 409, 429 and all 5xx.
func (e *UpstreamError) Retryable() bool {
	switch e.StatusCode {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return true
	}
	return e.StatusCode >= 500
}
