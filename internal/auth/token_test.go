package auth

import (
	"context"
	"errors"
	"testing"

	"time"

	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
)

// The two credential shapes must never be confused: a virtual key routed to the
// token verifier fails with a token error, and a token routed to the key store
// fails as an unknown key.
func TestLooksLikeJWT(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"a three-segment token", "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1LTEifQ.c2ln", true},
		{"a token behind a bearer scheme", "Bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1In0.c2ln", true},
		{"a virtual key", "sk-vk-8Zq3_aBcD-eFgHiJkLmNoPqRsTuVwXyZ0123456789", false},
		{"an anthropic api key", "sk-ant-api03-abcdef", false},
		{"a subscription oauth token", "sk-ant-oat01-abcdef", false},
		{"an empty value", "", false},
		{"two segments", "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1In0", false},
		{"four segments", "a.b.c.d", false},
		{"an empty signature, as an alg-none token has", "eyJhbGciOiJub25lIn0.eyJzdWIiOiJ1In0.", false},
		{"an empty header", ".eyJzdWIiOiJ1In0.c2ln", false},
		{"a value with characters base64url has no room for", "eyJh+GciOiJSUzI1NiJ9.eyJzdWIiOiJ1In0.c2ln", false},
		// An operator's hand-written key is unconstrained, and these are three
		// base64url runs separated by dots. Segment counting alone would route
		// them to the token verifier the moment jwt_auth was switched on, and
		// 401 keys that had been working.
		{"a hand-written key that looks structurally like a token", "team.gateway.2024", false},
		{"a dotted key with base64url segments", "acme.platform.abcdef", false},
		// A first segment that decodes to JSON but declares no algorithm is not
		// a JWS header.
		{"three segments whose header names no algorithm", "eyJ0eXAiOiJKV1QifQ.eyJzdWIiOiJ1In0.c2ln", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LooksLikeJWT(c.in); got != c.want {
				t.Errorf("LooksLikeJWT(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// stubVerifier stands in for the identity provider, so the routing decision in
// Authenticate can be tested without one.
type stubVerifier struct {
	key    *core.Key
	err    error
	called int
}

func (s *stubVerifier) VerifyToken(context.Context, string) (*core.Key, error) {
	s.called++
	if s.err != nil {
		return nil, s.err
	}
	return s.key, nil
}

func TestAuthenticateRoutesCredentialsByShape(t *testing.T) {
	const storedKey = "sk-vk-stored"
	const token = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1LTEifQ.c2ln"

	newAuth := func(t *testing.T, v TokenVerifier) *Authenticator {
		t.Helper()
		a, err := NewAuthenticator(context.Background(), NewMemStore(), config.VirtualKeysConfig{
			HeaderNames: []string{"x-gateway-key"},
			Keys:        []config.KeySpec{{Key: storedKey, Alias: "stored"}},
		}, nil)
		if err != nil {
			t.Fatalf("NewAuthenticator: %v", err)
		}
		a.UseTokenVerifier(v)
		return a
	}

	t.Run("a token reaches the verifier", func(t *testing.T) {
		v := &stubVerifier{key: &core.Key{Hash: "jwt:user-1", Alias: "dev@example.com", Subject: "user-1"}}
		ac, err := newAuth(t, v).Authenticate(context.Background(), header(token))
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if v.called != 1 {
			t.Errorf("verifier called %d times, want 1", v.called)
		}
		if ac.Key.Subject != "user-1" {
			t.Errorf("Subject = %q, want the verified identity", ac.Key.Subject)
		}
	})

	t.Run("a virtual key never reaches the verifier", func(t *testing.T) {
		v := &stubVerifier{err: errors.New("should not be called")}
		ac, err := newAuth(t, v).Authenticate(context.Background(), header(storedKey))
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if v.called != 0 {
			t.Errorf("verifier called %d times for a virtual key, want 0", v.called)
		}
		if ac.Key.Alias != "stored" {
			t.Errorf("Alias = %q, want the stored key", ac.Key.Alias)
		}
	})

	t.Run("a token is refused where no verifier is configured", func(t *testing.T) {
		a, err := NewAuthenticator(context.Background(), NewMemStore(), config.VirtualKeysConfig{
			HeaderNames: []string{"x-gateway-key"},
		}, nil)
		if err != nil {
			t.Fatalf("NewAuthenticator: %v", err)
		}
		if _, err := a.Authenticate(context.Background(), header(token)); !errors.Is(err, core.ErrKeyInvalid) {
			t.Fatalf("got %v, want ErrKeyInvalid", err)
		}
	})
}

// A token-authenticated caller lands in whatever scope its role names, so the
// two features compose: an identity verified per request draws on its team's
// pool exactly as an issued key would.
func TestTokenAuthenticatedCallerJoinsItsScope(t *testing.T) {
	scopes := testHierarchy(t, func(_, team, _ *core.Scope) { team.MaxBudget = 10 })
	a, err := NewAuthenticator(context.Background(), NewMemStore(),
		config.VirtualKeysConfig{HeaderNames: []string{"x-gateway-key"}}, scopes)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	a.UseTokenVerifier(&stubVerifier{key: &core.Key{
		Hash: "jwt:user-1", Subject: "user-1", Scope: "acme/platform",
	}})

	ac, err := a.Authenticate(context.Background(), header("eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1In0.c2ln"))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if ac.Scope == nil || ac.Scope.ID != "acme/platform" {
		t.Fatalf("Scope = %v, want the team named by the role", ac.Scope)
	}
	if got := ac.ScopeSubjects(); len(got) != 2 || got[0] != "team:acme/platform" {
		t.Errorf("ScopeSubjects() = %v, want the team then its organisation", got)
	}
}

// A verified token's expiry is enforced at authentication, not only by the
// verifier's cache. The cache is bounded by the token's expiry, so this branch
// should be unreachable through the real verifier — it is defence in depth
// against that bound ever being wrong, and it is tested because an unreachable
// guard nobody exercises is a guard nobody knows is broken.
func TestExpiredVerifiedTokenIsRefusedAtAuthentication(t *testing.T) {
	expired := time.Now().Add(-time.Minute)
	a, err := NewAuthenticator(context.Background(), NewMemStore(),
		config.VirtualKeysConfig{HeaderNames: []string{"x-gateway-key"}}, nil)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	// A verifier that hands back a key whose token has already expired, as a
	// stale cache entry would.
	a.UseTokenVerifier(&stubVerifier{key: &core.Key{
		Hash: "jwt:user-1", Subject: "user-1", ExpiresAt: &expired,
	}})

	_, err = a.Authenticate(context.Background(), header("eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1In0.c2ln"))
	if !errors.Is(err, core.ErrKeyBlocked) {
		t.Fatalf("got %v, want ErrKeyBlocked", err)
	}
}
