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
	// Format is the wire format Body is written in, which is the serving
	// deployment's rather than the caller's whenever the request was translated
	// on the way out. Only the envelope around the message is rewritten for the
	// caller; the upstream's own wording is preserved, because that wording is
	// what a client matches on to decide whether to retry with a capability
	// disabled.
	Format Format
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

// ScopeBudgetError is a refusal by a shared budget rather than by the caller's
// own, carrying which level refused.
//
// It exists because the two are not the same fact and a caller acts on them
// differently. "This key has exhausted its budget" sends a developer to ask for
// their own cap to be raised; if what actually happened is that their team's
// pool ran out, that request is addressed to the wrong person and the answer
// will not help them. Naming the level is what makes the refusal actionable.
//
// It wraps ErrBudgetExceeded, so every existing check — the 402 status above
// included — keeps working without knowing this type exists. The figures stay
// out of it deliberately: what a team has spent belongs to whoever owns the
// team, and the caller learning which scope refused them learns the name of a
// group they are already in.
type ScopeBudgetError struct {
	Kind ScopeKind
	ID   string
}

func (e *ScopeBudgetError) Error() string {
	return string(e.Kind) + " " + e.ID + " has exhausted its shared budget"
}

// Unwrap makes errors.Is(err, ErrBudgetExceeded) true, so the status mapping
// and every other caller stay unchanged.
func (e *ScopeBudgetError) Unwrap() error { return ErrBudgetExceeded }

// ScopeModelError is a model refused by a scope rather than by the key's own
// allowlist, carrying which level withheld it.
//
// "This key is not permitted to call the requested model" is true either way,
// which is why this took longer to matter than the budget case — but it is not
// actionable. A developer whose key lists the model and is refused anyway has
// no way to discover that their team's allowlist is what stopped them, and the
// natural next step, asking for the key's allowlist to be widened, changes
// nothing. The scope id is the name of a group the caller is already in.
type ScopeModelError struct {
	Kind  ScopeKind
	ID    string
	Model string
}

func (e *ScopeModelError) Error() string {
	return string(e.Kind) + " " + e.ID + " does not allow " + e.Model
}

// Unwrap makes errors.Is(err, ErrModelNotAllowed) true, so the 403 mapping and
// every other caller stay unchanged.
func (e *ScopeModelError) Unwrap() error { return ErrModelNotAllowed }
