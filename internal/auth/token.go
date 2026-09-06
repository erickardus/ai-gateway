package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/erickardus/ai-gateway/internal/core"
)

// TokenVerifier verifies an identity provider's own token, presented on an
// inference request instead of a virtual key, and returns the entitlements it
// stands for.
//
// It is an interface here for the same reason KeyStore is one: the gateway's
// authentication path should not know how a token is verified, only that
// something can. The implementation lives in the sso package, which already
// holds the provider's key set and the role mapping.
//
// The returned Key is ephemeral — it is never stored, and exists only for the
// life of the request. What makes it accountable anyway is Subject: it carries
// the provider's stable identifier for the person, so the key's SpendSubject is
// the same one an SSO-issued key for that identity would use. A developer who
// switches between a gateway key and a raw provider token draws on one budget,
// not two.
type TokenVerifier interface {
	// VerifyToken checks a token and returns the key it authenticates as. The
	// error is what the caller sees, so it must not disclose why verification
	// failed beyond what an unauthenticated caller may already know.
	VerifyToken(ctx context.Context, raw string) (*core.Key, error)
}

// LooksLikeJWT reports whether a credential is shaped like a JSON Web Token,
// which is how the gateway decides to verify a credential rather than look it
// up.
//
// It is a shape test, not a validity test: everything that decides whether the
// token is *acceptable* happens in the verifier, against the provider's keys.
// This only routes the credential to the right check, and it is deliberately
// strict about what it claims, because a false positive here turns a mistyped
// virtual key into a confusing token error.
//
// A key this gateway generated cannot collide with a token: it is a fixed
// prefix followed by base64url, which has no ".". But a key an operator wrote
// by hand is unconstrained, and "team.gateway.2024" is three base64url runs
// separated by dots — so segment counting alone would route that key to the
// token verifier and 401 it the moment jwt_auth was switched on.
//
// The first segment is therefore decoded and required to be a JWS header: a
// JSON object naming an algorithm. That is what a token always is and what an
// operator's key is essentially never, and it costs one small base64 decode on
// credentials that have already been narrowed to exactly two dots.
func LooksLikeJWT(v string) bool {
	v = core.StripScheme(v)
	if strings.HasPrefix(v, core.PrefixVirtualKey) {
		return false
	}
	first := strings.IndexByte(v, '.')
	if first <= 0 {
		return false
	}
	second := strings.IndexByte(v[first+1:], '.')
	if second <= 0 {
		return false
	}
	second += first + 1
	if second == len(v)-1 {
		// A signature is never empty. An "alg: none" token — the canonical JWT
		// forgery — has exactly this shape, and the verifier refuses it on the
		// algorithm anyway; refusing it here as well means it is not even
		// reported as a token.
		return false
	}
	if !isBase64URL(v[:first]) || !isBase64URL(v[first+1:second]) || !isBase64URL(v[second+1:]) {
		return false
	}
	return isJWSHeader(v[:first])
}

// maxHeaderSegment bounds what is decoded to answer the question. A JWS header
// is a few dozen bytes; anything approaching this is not one, and refusing to
// decode it keeps an oversized credential from being work the gateway does.
const maxHeaderSegment = 1024

// isJWSHeader reports whether a token's first segment decodes to a JSON object
// declaring an algorithm.
//
// The algorithm is not checked against the allowlist here — that is the
// verifier's job, and doing it here would make an unsupported algorithm present
// as an unknown virtual key rather than as the token it is.
func isJWSHeader(segment string) bool {
	if len(segment) > maxHeaderSegment {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(segment, "="))
	if err != nil {
		return false
	}
	var h struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return false
	}
	return h.Alg != ""
}

// isBase64URL reports whether every byte is in the base64url alphabet, with or
// without padding.
func isBase64URL(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '=':
		default:
			return false
		}
	}
	return true
}
