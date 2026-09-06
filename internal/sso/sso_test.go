package sso

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"strings"
	"testing"
	"time"

	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/testutil"
)

func testConfig(issuer string) config.SSOConfig {
	return config.SSOConfig{
		Issuer:      issuer,
		ClientID:    "gateway",
		Scopes:      []string{"openid", "email", "groups", "offline_access"},
		RedirectURL: "https://gw.example.com/sso/callback",
		KeyDuration: 720 * time.Hour,
		RenewWithin: 168 * time.Hour,
		RoleClaim:   "groups",
		Roles: []config.SSORole{
			{Match: "platform-eng", Models: []string{"anthropic-claude"}, AllowPassthrough: true},
			{Match: "*", Models: []string{"anthropic-claude"}},
		},
		BaseURL: "https://gw.example.com",
		Model:   "anthropic-claude",
	}
}

func newProvider(t *testing.T) (*Provider, *testutil.OIDC) {
	t.Helper()
	idp := testutil.NewOIDC(t, "gateway")
	return New(testConfig(idp.Issuer()), testutil.DiscardLogger()), idp
}

func TestVerifyAcceptsAProviderToken(t *testing.T) {
	p, idp := newProvider(t)
	raw := idp.Sign(idp.Claims("nonce-1"))

	id, err := p.Verify(context.Background(), raw, "nonce-1")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.Subject != "user-1" {
		t.Errorf("Subject = %q, want user-1", id.Subject)
	}
	if id.Email != "dev@example.com" {
		t.Errorf("Email = %q, want dev@example.com", id.Email)
	}
	if len(id.Roles) != 1 || id.Roles[0] != "platform-eng" {
		t.Errorf("Roles = %v, want [platform-eng]", id.Roles)
	}
}

// The rejections matter more than the acceptance: each of these is a token that
// verifies as a JWT and must not be accepted as this gateway's caller.
func TestVerifyRejects(t *testing.T) {
	p, idp := newProvider(t)

	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	tamper := func(fn func(map[string]any)) string {
		c := idp.Claims("nonce-1")
		fn(c)
		return idp.Sign(c)
	}

	tests := []struct {
		name  string
		token string
		nonce string
		want  string
	}{
		{
			name:  "issued by another provider",
			token: tamper(func(c map[string]any) { c["iss"] = "https://evil.example.com" }),
			nonce: "nonce-1",
			want:  "issued by",
		},
		{
			name:  "issued for another client of the same provider",
			token: tamper(func(c map[string]any) { c["aud"] = "some-other-app" }),
			nonce: "nonce-1",
			want:  "not issued for this client",
		},
		{
			name:  "expired",
			token: tamper(func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }),
			nonce: "nonce-1",
			want:  "expired",
		},
		{
			name:  "no expiry at all",
			token: tamper(func(c map[string]any) { delete(c, "exp") }),
			nonce: "nonce-1",
			want:  "no expiry",
		},
		{
			name:  "not yet valid",
			token: tamper(func(c map[string]any) { c["nbf"] = time.Now().Add(time.Hour).Unix() }),
			nonce: "nonce-1",
			want:  "not valid until",
		},
		{
			name:  "no subject",
			token: tamper(func(c map[string]any) { delete(c, "sub") }),
			nonce: "nonce-1",
			want:  "no subject",
		},
		{
			name:  "replayed from another login",
			token: idp.Sign(idp.Claims("nonce-from-elsewhere")),
			nonce: "nonce-1",
			want:  "nonce does not match",
		},
		{
			name:  "signed by a key the provider never published",
			token: idp.SignWith(other, "test-key", idp.Claims("nonce-1")),
			nonce: "nonce-1",
			want:  "verification error",
		},
		{
			name:  "unsigned, declaring alg none",
			token: testutil.UnsignedToken(idp.Claims("nonce-1")),
			nonce: "nonce-1",
			want:  "not accepted",
		},
		{
			name:  "not a JWT at all",
			token: "nonsense",
			nonce: "nonce-1",
			want:  "three dot-separated segments",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.Verify(context.Background(), tc.token, tc.nonce)
			if err == nil {
				t.Fatalf("Verify accepted a token it must refuse")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// An unknown key id is how key rotation looks, so it must be followed, and a
// token still has to verify against whatever the refetch returned.
func TestVerifyFollowsKeyRotation(t *testing.T) {
	p, idp := newProvider(t)
	if _, err := p.Verify(context.Background(), idp.Sign(idp.Claims("n")), "n"); err != nil {
		t.Fatalf("first verify: %v", err)
	}

	rotated, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp.Set(func(o *testutil.OIDC) {
		o.Key = rotated
		o.KeyID = "test-key-2"
	})

	// A refetch is rate-limited, so a rotation is followed after that floor
	// rather than instantly. Past it, the new key must be picked up without
	// anyone restarting the gateway.
	if _, err := p.Verify(context.Background(), idp.Sign(idp.Claims("n2")), "n2"); err == nil {
		t.Error("an unknown key id triggered an immediate refetch, defeating the rate limit")
	}
	p.now = func() time.Time { return time.Now().Add(2 * jwksMinRefetch) }
	if _, err := p.Verify(context.Background(), idp.Sign(idp.Claims("n3")), "n3"); err != nil {
		t.Fatalf("after rotation: %v", err)
	}
}

func TestVerifyRejectsADocumentDeclaringAnotherIssuer(t *testing.T) {
	idp := testutil.NewOIDC(t, "gateway")
	cfg := testConfig(idp.Issuer() + "/tenant-b")
	p := New(cfg, testutil.DiscardLogger())

	_, err := p.Verify(context.Background(), idp.Sign(idp.Claims("n")), "n")
	if err == nil {
		t.Fatal("accepted a discovery document declaring a different issuer")
	}
}

func TestExchangeAndRefresh(t *testing.T) {
	p, idp := newProvider(t)
	ctx := context.Background()

	// The fake mints a code per state; the authorize hop is exercised in the
	// server test, so here the grant is called directly.
	idp.Set(func(o *testutil.OIDC) { o.RotateRefresh = true })
	tokens, err := p.Refresh(ctx, "refresh-token")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tokens.IDToken == "" {
		t.Error("Refresh returned no ID token")
	}
	if tokens.RefreshToken == "refresh-token" {
		t.Error("a rotating provider's new refresh token was not returned")
	}

	idp.Set(func(o *testutil.OIDC) { o.RefuseRefresh = true })
	if _, err := p.Refresh(ctx, "refresh-token"); err == nil {
		t.Fatal("Refresh succeeded after the provider disabled the account")
	} else if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error = %q, want it to relay the provider's own refusal", err)
	}
}

func TestRoleMatching(t *testing.T) {
	idp := testutil.NewOIDC(t, "gateway")
	cfg := testConfig(idp.Issuer())
	p := New(cfg, testutil.DiscardLogger())

	tests := []struct {
		name   string
		roles  []string
		want   string
		wantOK bool
	}{
		{name: "exact match", roles: []string{"platform-eng"}, want: "platform-eng", wantOK: true},
		{name: "first match wins", roles: []string{"platform-eng", "everyone"}, want: "platform-eng", wantOK: true},
		{name: "falls through to the catch-all", roles: []string{"design"}, want: "*", wantOK: true},
		{name: "no claim at all still reaches the catch-all", roles: nil, want: "*", wantOK: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := p.Role(&Identity{Subject: "u", Roles: tc.roles})
			if ok != tc.wantOK {
				t.Fatalf("matched = %v, want %v", ok, tc.wantOK)
			}
			if got.Match != tc.want {
				t.Errorf("role = %q, want %q", got.Match, tc.want)
			}
		})
	}
}

// Without a catch-all, an identity in no configured group is refused rather
// than quietly given the narrowest role. A provider covering the whole company
// authenticates everyone in it.
func TestRoleRefusesAnUnmatchedIdentity(t *testing.T) {
	idp := testutil.NewOIDC(t, "gateway")
	cfg := testConfig(idp.Issuer())
	cfg.Roles = []config.SSORole{{Match: "platform-eng"}}
	p := New(cfg, testutil.DiscardLogger())

	if _, ok := p.Role(&Identity{Subject: "u", Roles: []string{"contractors"}}); ok {
		t.Fatal("an identity in no configured group was granted a role")
	}
}

// The gateway redirects a browser to this address carrying a code that can be
// redeemed for a key, so anything that is not this machine must be refused.
func TestValidateLoopback(t *testing.T) {
	valid := []string{
		"http://127.0.0.1:1234/callback",
		"http://[::1]:1234/callback",
		"http://localhost:9999/cb",
	}
	for _, v := range valid {
		if err := ValidateLoopback(v); err != nil {
			t.Errorf("ValidateLoopback(%q) = %v, want accepted", v, err)
		}
	}

	invalid := []string{
		"http://evil.example.com/cb",
		"http://127.0.0.1.evil.example.com/cb",
		"http://10.0.0.5:1234/cb",
		"http://169.254.169.254/latest/meta-data",
		"https://127.0.0.1:1234/cb",
		"//evil.example.com/cb",
		"http://user:pass@127.0.0.1:1234/cb",
		"not a url at all",
		"",
	}
	for _, v := range invalid {
		if err := ValidateLoopback(v); err == nil {
			t.Errorf("ValidateLoopback(%q) accepted an address that is not this machine", v)
		}
	}
}

func TestPKCE(t *testing.T) {
	verifier, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	challenge := Challenge(verifier)

	if !VerifyChallenge(challenge, verifier) {
		t.Error("the verifier that produced the challenge did not match it")
	}
	other, _ := NewToken()
	if VerifyChallenge(challenge, other) {
		t.Error("a different verifier matched the challenge")
	}
	if VerifyChallenge("", verifier) || VerifyChallenge(challenge, "") {
		t.Error("an empty challenge or verifier matched")
	}
}

func TestMemStoreEntriesAreSingleUse(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()

	if err := s.PutLogin(ctx, "state", Login{Nonce: "n"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.TakeLogin(ctx, "state")
	if err != nil || !ok {
		t.Fatalf("TakeLogin = %v, %v", ok, err)
	}
	if got.Nonce != "n" {
		t.Errorf("Nonce = %q, want n", got.Nonce)
	}
	// A callback that could be replayed would be a second key for whoever
	// captured the URL.
	if _, ok, _ := s.TakeLogin(ctx, "state"); ok {
		t.Error("a login was redeemable twice")
	}

	if err := s.PutTicket(ctx, "code", Ticket{Key: "sk-vk-x"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.TakeTicket(ctx, "code"); !ok {
		t.Fatal("ticket not found")
	}
	if _, ok, _ := s.TakeTicket(ctx, "code"); ok {
		t.Error("a ticket was redeemable twice")
	}
}

func TestMemStoreExpires(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	if err := s.PutLogin(ctx, "state", Login{}, -time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.TakeLogin(ctx, "state"); ok {
		t.Error("an expired login was accepted")
	}
}

// /sso/login takes no credential, so the state it leaves behind is the one
// thing an unauthenticated caller can make the gateway accumulate.
func TestMemStoreBoundsPendingLogins(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()

	for i := range maxPendingLogins {
		if err := s.PutLogin(ctx, string(rune(i))+"-state", Login{}, time.Minute); err != nil {
			t.Fatalf("PutLogin %d: %v", i, err)
		}
	}
	if err := s.PutLogin(ctx, "one-too-many", Login{}, time.Minute); err == nil {
		t.Fatal("pending logins grow without bound")
	}

	// An expired entry frees the slot, so a burst does not wedge logins for a
	// whole TTL once it passes.
	s.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if err := s.PutLogin(ctx, "after-the-burst", Login{}, time.Minute); err != nil {
		t.Fatalf("a login after the burst expired was still refused: %v", err)
	}
}
