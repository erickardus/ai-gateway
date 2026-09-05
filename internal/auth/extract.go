package auth

import (
	"net/http"
	"strings"

	"github.com/erickardus/ai-gateway/internal/core"
)

// Credentials describes what a request presented.
type Credentials struct {
	// Key is the gateway virtual key, with any authorization scheme stripped.
	Key string
	// ViaHeader is the canonical name of the header the key arrived in. The
	// transport needs it: a credential that authenticated the caller here must
	// never also be forwarded upstream, or the gateway would leak its own
	// virtual key to the provider.
	ViaHeader string
}

// ExtractGatewayKey finds the virtual key on an inbound request.
//
// The ordering matters, and so does what it refuses to do. Claude Code
// authenticated with a claude.ai subscription puts an OAuth token in
// Authorization; that token belongs to Anthropic, not to this gateway. Reading
// it as a virtual key would both fail authentication and consume a credential
// that has to survive the hop. So Authorization and x-api-key are only ever
// considered once their value has been checked against the known upstream
// credential prefixes.
//
// Resolution order:
//  1. each configured custom header, in order (typically x-gateway-key, then
//     x-litellm-api-key for compatibility with LiteLLM-configured tooling)
//  2. Authorization, only if it does not carry an upstream credential
//  3. x-api-key, under the same condition
//
// A "Bearer ", "bearer " or "Basic " scheme prefix is stripped from any of them.
func ExtractGatewayKey(h http.Header, headerNames []string) (Credentials, bool) {
	for _, name := range headerNames {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			return Credentials{Key: core.StripScheme(v), ViaHeader: http.CanonicalHeaderKey(name)}, true
		}
	}

	for _, name := range []string{"Authorization", "X-Api-Key"} {
		v := strings.TrimSpace(h.Get(name))
		if v == "" {
			continue
		}
		if core.IsUpstreamCredential(v) {
			// A provider credential bound for the upstream, not a key for us.
			continue
		}
		return Credentials{Key: core.StripScheme(v), ViaHeader: http.CanonicalHeaderKey(name)}, true
	}

	return Credentials{}, false
}

// IsGatewayKeyHeader reports whether a header name is one the gateway consumes
// for its own authentication and must therefore strip before forwarding.
func IsGatewayKeyHeader(name string, headerNames []string) bool {
	for _, n := range headerNames {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}
