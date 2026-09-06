package sso

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/testutil"
)

// jwtAuthConfig is testConfig with per-request token authentication turned on.
func jwtAuthConfig(issuer string, adjust func(*config.SSOConfig)) config.SSOConfig {
	cfg := testConfig(issuer)
	ttl := time.Minute
	cfg.JWTAuth = config.JWTAuthConfig{
		Enabled:   true,
		Audiences: []string{"gateway"},
		CacheTTL:  &ttl,
	}
	if adjust != nil {
		adjust(&cfg)
	}
	return cfg
}

func newJWTProvider(t *testing.T, adjust func(*config.SSOConfig)) (*Provider, *testutil.OIDC) {
	t.Helper()
	idp := testutil.NewOIDC(t, "gateway")
	return New(jwtAuthConfig(idp.Issuer(), adjust), testutil.DiscardLogger()), idp
}

// The provider's own token authenticates a request, and resolves to the same
// entitlements the login path would have issued a key for.
func TestVerifyTokenAuthenticatesAgainstTheRoleMapping(t *testing.T) {
	p, idp := newJWTProvider(t, nil)
	// No nonce: a token presented on a request was not minted for a login this
	// gateway started.
	raw := idp.Sign(idp.Claims(""))

	key, err := p.VerifyToken(context.Background(), raw)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if key.Subject != "user-1" {
		t.Errorf("Subject = %q, want user-1", key.Subject)
	}
	// platform-eng is the first matching role in testConfig.
	if len(key.Models) != 1 || key.Models[0] != "anthropic-claude" {
		t.Errorf("Models = %v, want the role's list", key.Models)
	}
	if !key.AllowPassthrough {
		t.Error("AllowPassthrough should come from the matched role")
	}
	if key.ExpiresAt == nil {
		t.Error("the token's expiry should bound the key it authenticates as")
	}
}

// A person is one budget however they authenticate. A key issued at login and a
// token verified per request both account to the identity, so switching between
// them cannot hand anyone a second allowance.
func TestTokenAndIssuedKeyShareASpendSubject(t *testing.T) {
	p, idp := newJWTProvider(t, nil)
	raw := idp.Sign(idp.Claims(""))

	fromToken, err := p.VerifyToken(context.Background(), raw)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	id, err := p.Verify(context.Background(), idp.Sign(idp.Claims("n")), "n")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	role, ok := p.Role(id)
	if !ok {
		t.Fatal("Role: identity matched none")
	}
	issued := KeyForRole(id, role)
	issued.Hash = "a-stored-hash"

	if fromToken.SpendSubject() != issued.SpendSubject() {
		t.Errorf("spend subjects differ: token %q, issued key %q",
			fromToken.SpendSubject(), issued.SpendSubject())
	}
}

// A token refreshed every hour must not hand its holder a fresh rate-limit
// window each time, so the rate-limit subject is the identity rather than the
// credential.
func TestTokenRateLimitSubjectIsStableAcrossTokens(t *testing.T) {
	p, idp := newJWTProvider(t, nil)
	ctx := context.Background()

	first, err := p.VerifyToken(ctx, idp.Sign(idp.Claims("")))
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	// A second, distinct token for the same person.
	claims := idp.Claims("")
	claims["iat"] = time.Now().Add(-time.Second).Unix()
	claims["jti"] = "another"
	second, err := p.VerifyToken(ctx, idp.Sign(claims))
	if err != nil {
		t.Fatalf("VerifyToken (second token): %v", err)
	}
	if first.Hash != second.Hash {
		t.Errorf("two tokens for one identity gave rate-limit subjects %q and %q", first.Hash, second.Hash)
	}
	if first.Hash != KeyPrefixJWT+"user-1" {
		t.Errorf("Hash = %q, want it derived from the subject", first.Hash)
	}
}

func TestVerifyTokenRejects(t *testing.T) {
	t.Run("a token for another audience", func(t *testing.T) {
		p, idp := newJWTProvider(t, func(c *config.SSOConfig) {
			c.JWTAuth.Audiences = []string{"api://something-else"}
		})
		if _, err := p.VerifyToken(context.Background(), idp.Sign(idp.Claims(""))); !errors.Is(err, core.ErrKeyInvalid) {
			t.Fatalf("got %v, want ErrKeyInvalid", err)
		}
	})

	t.Run("an expired token", func(t *testing.T) {
		p, idp := newJWTProvider(t, nil)
		claims := idp.Claims("")
		claims["exp"] = time.Now().Add(-time.Hour).Unix()
		if _, err := p.VerifyToken(context.Background(), idp.Sign(claims)); !errors.Is(err, core.ErrKeyInvalid) {
			t.Fatalf("got %v, want ErrKeyInvalid", err)
		}
	})

	t.Run("an identity matching no role", func(t *testing.T) {
		p, idp := newJWTProvider(t, func(c *config.SSOConfig) {
			// Drop the catch-all, so an unrecognised group matches nothing.
			c.Roles = []config.SSORole{{Match: "platform-eng", Models: []string{"anthropic-claude"}}}
		})
		idp.Set(func(o *testutil.OIDC) { o.Groups = []string{"contractors"} })
		if _, err := p.VerifyToken(context.Background(), idp.Sign(idp.Claims(""))); !errors.Is(err, core.ErrKeyInvalid) {
			t.Fatalf("got %v, want ErrKeyInvalid", err)
		}
	})

	t.Run("a token signed by another key", func(t *testing.T) {
		p, idp := newJWTProvider(t, nil)
		other := testutil.NewOIDC(t, "gateway")
		raw := idp.SignWith(other.Key, idp.KeyID, idp.Claims(""))
		if _, err := p.VerifyToken(context.Background(), raw); !errors.Is(err, core.ErrKeyInvalid) {
			t.Fatalf("got %v, want ErrKeyInvalid", err)
		}
	})

	t.Run("any token while the feature is off", func(t *testing.T) {
		idp := testutil.NewOIDC(t, "gateway")
		p := New(testConfig(idp.Issuer()), testutil.DiscardLogger())
		if _, err := p.VerifyToken(context.Background(), idp.Sign(idp.Claims(""))); !errors.Is(err, core.ErrKeyInvalid) {
			t.Fatalf("an unconfigured gateway must accept no tokens, got %v", err)
		}
	})
}

// The reason a token failed is not disclosed. A caller learning which check
// rejected it learns what to change about a forged one.
func TestVerifyTokenDoesNotDiscloseWhyItFailed(t *testing.T) {
	p, idp := newJWTProvider(t, nil)
	claims := idp.Claims("")
	claims["exp"] = time.Now().Add(-time.Hour).Unix()

	_, err := p.VerifyToken(context.Background(), idp.Sign(claims))
	if err == nil {
		t.Fatal("want an error")
	}
	if msg := err.Error(); msg != core.ErrKeyInvalid.Error() {
		t.Errorf("error should be the bare sentinel, got %q", msg)
	}
}

// Caching verification is an optimization, never an extension: an entry is
// dropped at the token's own expiry when that comes first.
func TestVerifiedTokenCacheNeverOutlivesTheToken(t *testing.T) {
	p, idp := newJWTProvider(t, func(c *config.SSOConfig) {
		// A cache TTL far longer than the token's remaining life.
		long := 24 * time.Hour
		c.JWTAuth.CacheTTL = &long
	})
	now := time.Now()
	p.now = func() time.Time { return now }

	claims := idp.Claims("")
	claims["exp"] = now.Add(30 * time.Second).Unix()
	raw := idp.Sign(claims)

	if _, err := p.VerifyToken(context.Background(), raw); err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	digest := digestOf(raw)
	if _, ok := p.tokens.get(digest, now); !ok {
		t.Fatal("the verified token should be cached")
	}
	// Past the point verify would still accept the token — its expiry plus the
	// clock-skew allowance — but well inside the configured 24h TTL. The bound
	// is the token's, not the TTL's.
	beyond := now.Add(30*time.Second + clockSkew + time.Second)
	if _, ok := p.tokens.get(digest, beyond); ok {
		t.Error("a cache entry must not outlive the token it was derived from")
	}
	// And not a moment longer than the TTL either, for a token that outlives it.
	long := idp.Claims("")
	long["exp"] = now.Add(72 * time.Hour).Unix()
	longRaw := idp.Sign(long)
	if _, err := p.VerifyToken(context.Background(), longRaw); err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if _, ok := p.tokens.get(digestOf(longRaw), now.Add(25*time.Hour)); ok {
		t.Error("a cache entry must not outlive the configured TTL either")
	}
}

// A cache hit answers without re-verifying, which is the whole reason it
// exists: a caller sending many requests pays one signature check.
func TestVerifiedTokenIsCached(t *testing.T) {
	p, idp := newJWTProvider(t, nil)
	raw := idp.Sign(idp.Claims(""))
	ctx := context.Background()

	if _, err := p.VerifyToken(ctx, raw); err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	// Break the provider: a second verification would have to fetch keys, and
	// the cached answer must not need them.
	idp.Server.Close()
	if _, err := p.VerifyToken(ctx, raw); err != nil {
		t.Fatalf("a cached token should not need the provider: %v", err)
	}
}

// The cache is bounded, so a caller minting a token per request cannot grow it
// without limit.
func TestVerifiedTokenCacheIsBounded(t *testing.T) {
	c := newTokenCache()
	now := time.Now()
	for i := range tokenCacheMax + 50 {
		c.put(string(rune(i))+"-digest", &core.Key{Subject: "s"}, now.Add(time.Hour), now)
	}
	if len(c.entries) > tokenCacheMax {
		t.Errorf("cache holds %d entries, want at most %d", len(c.entries), tokenCacheMax)
	}
}

// The authorized-party check must not switch itself off because the operator
// configured more than one acceptable audience.
//
// A gateway serving both logins and services lists an ID-token audience and an
// access-token one. A token another client of the same provider obtained, which
// happens to name this gateway among its audiences, is exactly what azp exists
// to catch — and it must still be caught in that configuration.
func TestAzpIsCheckedWhenTheClientIDIsWhatMatched(t *testing.T) {
	p, idp := newJWTProvider(t, func(c *config.SSOConfig) {
		c.JWTAuth.Audiences = []string{"gateway", "api://ai-gateway"}
	})
	ctx := context.Background()

	// A token naming this gateway plus an unrelated party, requested by some
	// other client. Only "gateway" matches what we accept.
	claims := idp.Claims("")
	claims["aud"] = []string{"gateway", "someone-else"}
	claims["azp"] = "an-entirely-different-client"
	if _, err := p.VerifyToken(ctx, idp.Sign(claims)); !errors.Is(err, core.ErrKeyInvalid) {
		t.Fatalf("a token another client obtained naming this gateway was accepted: %v", err)
	}

	// An access token minted for the API is unaffected: its audience is the
	// API, not the gateway's client id, so azp names the calling client
	// legitimately.
	claims = idp.Claims("")
	claims["aud"] = []string{"api://ai-gateway", "some-other-api"}
	claims["azp"] = "a-service-client"
	if _, err := p.VerifyToken(ctx, idp.Sign(claims)); err != nil {
		t.Fatalf("an access token for the configured API should be accepted: %v", err)
	}
}

// verify tolerates a clock a minute out of step with the provider's. The
// expiry the ephemeral key carries must tolerate the same, or the two disagree
// for the last minute of every token and the gateway refuses a credential it
// has just accepted.
func TestTokenExpiryCarriesTheClockSkewAllowance(t *testing.T) {
	p, idp := newJWTProvider(t, nil)
	now := time.Now()
	p.now = func() time.Time { return now }

	// A token whose exp has just passed, which is what a gateway clock running
	// slightly ahead of the provider's looks like.
	claims := idp.Claims("")
	claims["exp"] = now.Add(-30 * time.Second).Unix()

	key, err := p.VerifyToken(context.Background(), idp.Sign(claims))
	if err != nil {
		t.Fatalf("verify accepts a token inside the skew allowance: %v", err)
	}
	if key.ExpiresAt == nil {
		t.Fatal("the key should carry an expiry")
	}
	if err := key.Usable(now); err != nil {
		t.Errorf("the key verify just produced must be usable: %v", err)
	}

	// Past the allowance, both refuse it.
	claims = idp.Claims("")
	claims["exp"] = now.Add(-2 * time.Minute).Unix()
	if _, err := p.VerifyToken(context.Background(), idp.Sign(claims)); !errors.Is(err, core.ErrKeyInvalid) {
		t.Errorf("a token well past its expiry: got %v, want ErrKeyInvalid", err)
	}
}
