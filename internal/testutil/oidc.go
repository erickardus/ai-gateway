package testutil

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// OIDC is a fake OpenID Connect provider for tests.
//
// It exists so the whole login can run inside go test: discovery, key set,
// authorization redirect and both token grants. The fields are knobs — set them
// between calls to steer what the next login returns, including the failures a
// real provider produces, which are the cases worth having.
type OIDC struct {
	Server   *httptest.Server
	ClientID string

	// Key signs the tokens. Exported so a test can sign one itself and check
	// that a token from the wrong key is refused.
	Key *rsa.PrivateKey
	// KeyID is published in the key set and stamped on issued tokens.
	KeyID string

	mu sync.Mutex
	// Subject, Email and Groups are what the next issued token asserts.
	Subject string
	Email   string
	Name    string
	Groups  []string
	// RefuseRefresh makes the refresh grant fail the way a provider does once
	// an account has been disabled.
	RefuseRefresh bool
	// RotateRefresh makes each refresh return a new refresh token, as some
	// providers do and others do not.
	RotateRefresh bool
	// TokenTTL is how long issued tokens claim to be valid.
	TokenTTL time.Duration

	nonces   map[string]string
	refreshN int
}

// NewOIDC starts a fake provider and stops it when the test ends.
func NewOIDC(t *testing.T, clientID string) *OIDC {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	o := &OIDC{
		ClientID: clientID,
		Key:      key,
		KeyID:    "test-key",
		Subject:  "user-1",
		Email:    "dev@example.com",
		Name:     "Dev Example",
		Groups:   []string{"platform-eng"},
		TokenTTL: time.Hour,
		nonces:   map[string]string{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", o.handleDiscovery)
	mux.HandleFunc("/jwks", o.handleJWKS)
	mux.HandleFunc("/authorize", o.handleAuthorize)
	mux.HandleFunc("/token", o.handleToken)
	o.Server = httptest.NewServer(mux)
	t.Cleanup(o.Server.Close)
	return o
}

// Issuer is the provider's base URL.
func (o *OIDC) Issuer() string { return o.Server.URL }

// Set applies changes to what the next token asserts, under the lock the
// handlers take.
func (o *OIDC) Set(fn func(*OIDC)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	fn(o)
}

func (o *OIDC) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                 o.Server.URL,
		"authorization_endpoint": o.Server.URL + "/authorize",
		"token_endpoint":         o.Server.URL + "/token",
		"jwks_uri":               o.Server.URL + "/jwks",
	})
}

func (o *OIDC) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	pub := o.Key.Public().(*rsa.PublicKey)
	writeJSON(w, map[string]any{"keys": []any{map[string]any{
		"kty": "RSA",
		"kid": o.KeyID,
		"alg": "RS256",
		"use": "sig",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

// handleAuthorize plays the part of the consent screen: it accepts and
// immediately redirects back with a code.
func (o *OIDC) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := "code-" + q.Get("state")

	o.mu.Lock()
	o.nonces[code] = q.Get("nonce")
	o.mu.Unlock()

	target, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	rq := target.Query()
	rq.Set("code", code)
	rq.Set("state", q.Get("state"))
	target.RawQuery = rq.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

func (o *OIDC) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	var nonce string
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		nonce = o.nonces[r.Form.Get("code")]
		delete(o.nonces, r.Form.Get("code"))
	case "refresh_token":
		if o.RefuseRefresh {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{
				"error":             "invalid_grant",
				"error_description": "the account is disabled",
			})
			return
		}
	default:
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "unsupported_grant_type"})
		return
	}

	refresh := "refresh-token"
	if o.RotateRefresh {
		o.refreshN++
		refresh = "refresh-token-" + string(rune('a'+o.refreshN%26))
	}
	writeJSON(w, map[string]any{
		"token_type":    "Bearer",
		"id_token":      o.signLocked(o.claimsLocked(nonce)),
		"refresh_token": refresh,
	})
}

// claimsLocked builds the claim set for an issued token.
func (o *OIDC) claimsLocked(nonce string) map[string]any {
	now := time.Now()
	c := map[string]any{
		"iss":    o.Server.URL,
		"sub":    o.Subject,
		"aud":    o.ClientID,
		"exp":    now.Add(o.TokenTTL).Unix(),
		"iat":    now.Unix(),
		"email":  o.Email,
		"name":   o.Name,
		"groups": o.Groups,
	}
	if nonce != "" {
		c["nonce"] = nonce
	}
	return c
}

// Claims returns the claim set the provider would issue now, for a test that
// wants to alter one field before signing it.
func (o *OIDC) Claims(nonce string) map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.claimsLocked(nonce)
}

// Sign returns a signed token for arbitrary claims, so a test can construct
// one a real provider never would.
func (o *OIDC) Sign(claims map[string]any) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.signLocked(claims)
}

// SignWith signs claims using another key, for the case that matters most: a
// token whose signature verifies against nothing the provider published.
func (o *OIDC) SignWith(key *rsa.PrivateKey, kid string, claims map[string]any) string {
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}
	return signRS256(key, header, claims)
}

func (o *OIDC) signLocked(claims map[string]any) string {
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": o.KeyID}
	return signRS256(o.Key, header, claims)
}

func signRS256(key *rsa.PrivateKey, header, claims map[string]any) string {
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// UnsignedToken builds a token declaring "alg":"none", the canonical JWT
// forgery.
func UnsignedToken(claims map[string]any) string {
	h, _ := json.Marshal(map[string]any{"alg": "none", "typ": "JWT"})
	c, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c) + "."
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// DiscardLogger returns a logger that writes nowhere, for components that log
// on paths a test drives deliberately.
func DiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
