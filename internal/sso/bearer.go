package sso

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
)

// tokenCacheMax bounds the verified-token cache.
//
// The cache holds one entry per distinct live token, which is one per caller
// per token lifetime — a few thousand covers a large fleet of services, and
// exceeding it means something is minting tokens per request rather than
// reusing them. Losing an entry costs a signature verification, never a wrong
// answer, so the eviction below can afford to be blunt.
const tokenCacheMax = 4096

// KeyPrefixJWT marks the ephemeral key a verified token authenticates as. It is
// the rate-limit subject for that caller, and it is deliberately not a hash: no
// such key is ever stored, so there is nothing to look up and nothing whose
// disclosure would matter. What it must be is stable for one identity, so a
// caller that refreshes its token every hour keeps one rate-limit window rather
// than being handed a fresh allowance with every new token.
const KeyPrefixJWT = "jwt:"

// tokenEntry is one verified token's result and the moment it stops being
// reusable.
type tokenEntry struct {
	key     *core.Key
	expires time.Time
}

// tokenCache memoizes verification by token, so a caller sending many requests
// pays one signature check rather than one per request.
//
// It is keyed on a digest of the token rather than the token: the map is in
// memory that a heap dump or a debugger reaches, and a bearer token there is a
// usable credential where a digest of one is not.
type tokenCache struct {
	mu      sync.Mutex
	entries map[string]tokenEntry
}

func newTokenCache() *tokenCache {
	return &tokenCache{entries: make(map[string]tokenEntry)}
}

func (c *tokenCache) get(digest string, now time.Time) (*core.Key, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[digest]
	if !ok || !now.Before(e.expires) {
		return nil, false
	}
	// A copy, because the caller receives it as the request's own key and the
	// request path is entitled to treat that as private.
	clone := *e.key
	return &clone, true
}

func (c *tokenCache) put(digest string, key *core.Key, expires time.Time, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= tokenCacheMax {
		for k, e := range c.entries {
			if !now.Before(e.expires) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= tokenCacheMax {
			// Everything held is still live, so there is nothing to expire and
			// no ordering to evict by. Dropping the lot costs one verification
			// per active caller and cannot be wrong; keeping a least-recently-
			// used list to avoid it would be bookkeeping on the request path
			// for a case that means the deployment is already misconfigured.
			clear(c.entries)
		}
	}
	clone := *key
	c.entries[digest] = tokenEntry{key: &clone, expires: expires}
}

// VerifyToken implements auth.TokenVerifier: it checks an identity provider's
// own token and returns the entitlements it stands for.
//
// Nothing is stored. The key exists for the life of the request, and everything
// that makes it accountable — the spend subject, the rate-limit subject, the
// scope it belongs to — is derived from the identity rather than from a
// credential the gateway issued. That is the whole difference between this and
// the login path: there is no credential of the gateway's to revoke, because
// the gateway never made one, and access ends when the provider stops signing
// tokens for that person.
func (p *Provider) VerifyToken(ctx context.Context, raw string) (*core.Key, error) {
	if !p.cfg.JWTAuth.Enabled {
		return nil, core.ErrKeyInvalid
	}
	digest := digestOf(raw)
	now := p.now()

	if key, ok := p.tokens.get(digest, now); ok {
		return key, nil
	}

	// No nonce: there was no authorization request to tie this to. The token
	// stands on its signature, its issuer, its audience and its expiry, which
	// is what a bearer token is.
	id, err := p.verify(ctx, raw, "", p.cfg.JWTAuth.Audiences)
	if err != nil {
		// The reason is logged rather than returned. A caller learning which
		// check failed learns what to change about a forged token, and a caller
		// presenting a genuine one is not helped by the distinction.
		p.log.Debug("jwt auth: token rejected", "error", err)
		return nil, core.ErrKeyInvalid
	}

	role, ok := p.Role(id)
	if !ok {
		// Debug rather than Warn, unlike the login path's identical condition.
		// That one fires once per login; this one fires once per *request*, so
		// anyone holding a valid token from the issuer — or one misconfigured
		// service in a retry loop — could drive warning lines at whatever rate
		// they liked. The refusal still reaches the caller as a 401 and the
		// access log still records it; what drops to debug is the detail of
		// which claims failed to match.
		p.log.Debug("jwt auth: identity matches no configured role",
			"identity", id.Display(), "subject", id.Subject, "roles", id.Roles)
		return nil, core.ErrKeyInvalid
	}

	key := KeyForRole(id, role)
	key.Hash = KeyPrefixJWT + id.Subject
	key.CreatedAt = now.UTC()

	// The token's own expiry becomes the key's, so a cached entry that somehow
	// outlived its bound is still refused by the ordinary usability check
	// rather than relying on the cache alone to be right.
	//
	// Carrying the same clock-skew allowance verify granted is what keeps that
	// a backstop rather than a second, stricter deadline. Without it the two
	// disagree for the last minute of every token: verify accepts a token whose
	// exp has just passed, because the gateway's clock may be ahead of the
	// provider's, and an expiry check with no allowance then refuses the key it
	// just produced — as a 403, which tells a client its access was withdrawn
	// rather than that its token needs refreshing.
	usableUntil := tokenDeadline(id.Expiry)
	if usableUntil != nil {
		key.ExpiresAt = usableUntil
	}

	// The entry lives for the configured TTL or until the token stops being
	// accepted, whichever comes first. Never past the token: caching is the
	// gateway making verification cheaper, not the gateway extending a
	// credential.
	until := now.Add(p.cfg.JWTAuth.TTL())
	if usableUntil != nil && usableUntil.Before(until) {
		until = *usableUntil
	}
	p.tokens.put(digest, key, until, now)

	return key, nil
}

// tokenDeadline is the moment a token stops being accepted: its own expiry plus
// the allowance verify already applies to it. Nil where the token named no
// expiry, which verify refuses anyway.
func tokenDeadline(exp time.Time) *time.Time {
	if exp.IsZero() {
		return nil
	}
	deadline := exp.Add(clockSkew)
	return &deadline
}

// digestOf is the cache key for a token: a digest rather than the token itself,
// so the map holds nothing usable as a credential.
func digestOf(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// KeyForRole builds the entitlements a role grants an identity.
//
// It is shared by the two ways an identity becomes a caller — a key issued at
// login, and a token verified on a request — so that the same person under the
// same role gets the same models, the same limits and the same pool either way.
// Anything that differs between them is set by the caller: a stored key gets a
// hash and a device, a verified token gets neither.
func KeyForRole(id *Identity, role config.SSORole) *core.Key {
	return &core.Key{
		Alias:            id.Display(),
		Models:           role.Models,
		RPMLimit:         role.RPMLimit,
		TPMLimit:         role.TPMLimit,
		AllowPassthrough: role.AllowPassthrough,
		MaxBudget:        role.MaxBudget,
		BudgetDuration:   role.BudgetDuration,
		Subject:          id.Subject,
		Scope:            role.Scope,
	}
}
