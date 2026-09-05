// Package provider builds and executes upstream requests and relays responses.
//
// Its guiding constraint comes from Anthropic's gateway compatibility rules: a
// gateway must forward anthropic-* headers and request bodies unchanged, treat
// them as open lists rather than allowlists, stream without buffering, and relay
// upstream errors verbatim. Code here therefore avoids interpreting the payload
// wherever it can.
package provider

import (
	"net/http"
	"strings"

	"github.com/erickardus/ai-gateway/internal/auth"
	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
)

// hopByHop are connection-scoped headers that must never be forwarded.
var hopByHop = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"proxy-connection":    true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

// gatewayOnly are headers the gateway consumes and must not pass on. The control
// headers come from core so that the package reading them and this package
// stripping them cannot drift apart.
var gatewayOnly = func() map[string]bool {
	m := map[string]bool{"host": true, "content-length": true}
	for _, h := range core.ControlHeaders {
		m[strings.ToLower(h)] = true
	}
	return m
}()

// credentialHeaders are the headers that may carry a caller credential.
var credentialHeaders = map[string]bool{
	"authorization": true,
	"x-api-key":     true,
}

// BuildUpstreamHeaders produces the header set for the upstream request.
//
// In passthrough mode the caller's own credential is relayed untouched, which is
// what keeps a Claude.ai subscription login working through the gateway. The one
// header that is never relayed is the one that authenticated the caller here:
// forwarding that would hand the gateway's own virtual key to the provider.
//
// anthropic-* headers are forwarded as an open list rather than an allowlist.
// New Claude Code releases introduce new anthropic-beta capability values, and a
// gateway pinned to the values it knows today silently breaks the next one. In
// particular a subscription login carries an OAuth capability in anthropic-beta
// that the upstream requires; stripping it fails the request with 401.
func BuildUpstreamHeaders(in http.Header, params config.DeploymentParams, creds auth.Credentials, keyHeaderNames []string) http.Header {
	out := make(http.Header, len(in)+2)

	for name, values := range in {
		lower := strings.ToLower(name)

		if hopByHop[lower] || gatewayOnly[lower] {
			continue
		}
		// Never forward the header the gateway authenticated with, nor any
		// header configured to carry a virtual key.
		if auth.IsGatewayKeyHeader(name, keyHeaderNames) {
			continue
		}
		if credentialHeaders[lower] {
			// Handled below, per auth mode.
			continue
		}
		for _, v := range values {
			out.Add(name, v)
		}
	}

	switch params.AuthMode {
	case core.AuthModePassthrough:
		// Relay the caller's provider credential. Two conditions must both
		// hold: the header must not be the one that authenticated them here,
		// and the value must actually look like a provider credential. Testing
		// only the header name would forward the gateway's own virtual key
		// upstream whenever a caller sets it in Authorization as well.
		for _, name := range []string{"Authorization", "X-Api-Key"} {
			if strings.EqualFold(name, creds.ViaHeader) {
				continue
			}
			v := in.Get(name)
			if v == "" || !core.IsUpstreamCredential(v) {
				continue
			}
			out.Set(name, v)
		}
	case core.AuthModeAPIKey:
		// The caller's credentials were already dropped above; present the
		// deployment's own.
		value := params.APIKey
		if params.AuthScheme != "" {
			value = params.AuthScheme + " " + value
		}
		if params.AuthHeader != "" {
			out.Set(params.AuthHeader, value)
		}
	}

	return out
}

// SanitizeResponseHeaders copies an upstream response's headers to the client,
// dropping only those that describe the upstream connection rather than the
// payload.
func SanitizeResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		if hopByHop[strings.ToLower(name)] {
			continue
		}
		for _, v := range values {
			dst.Add(name, v)
		}
	}
}
