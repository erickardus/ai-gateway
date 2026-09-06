// Package ui serves the gateway's admin interface: a single-page application
// built from web/, embedded into the binary, and the browser session that
// authenticates it.
//
// The session is deliberately its own credential plane, separate from both of
// the two the gateway already juggles. A virtual key says who is calling the
// gateway; an upstream credential says how the gateway calls a provider; a UI
// session says who is looking at the operator console. Keeping the third apart
// from the first is not decoration — a cookie that could authenticate
// POST /v1/messages would be a session riding along on every inference request
// a browser could be tricked into making, which is the credential collision
// this gateway exists to avoid, wearing a different hat.
//
// Two things enforce that separation. The cookie is scoped to Path=/ui, so the
// browser itself never sends it anywhere else; and auth.ExtractGatewayKey reads
// headers only, so nothing on the inference path can read a cookie even if one
// arrived.
package ui

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CookieName is the session cookie. The gw_ prefix keeps it distinct from
// anything else served on the same host, since a gateway is often reverse
// proxied alongside other applications.
const CookieName = "gw_ui_session"

// CookiePath scopes the cookie to the UI and its API.
//
// It is the load-bearing half of the separation described in the package
// comment: with this path the browser will not attach the session to
// /v1/messages, /key/* or anything else the gateway serves, so the UI's
// credential cannot reach the inference plane even by accident.
const CookiePath = "/ui"

// CSRFHeader must be present on every state-changing UI request.
//
// It is a header a form post or a cross-site <img> cannot set, so requiring it
// means a request that changes something had to come from JavaScript running on
// the gateway's own origin. SameSite=Strict on the cookie is the first defence
// and this is the second, because SameSite is a browser policy and the header
// is a property of the request itself.
const CSRFHeader = "x-gateway-ui"

// sessionInfo is the HKDF context string. It is versioned so that changing the
// token's shape invalidates every session signed under the old one rather than
// letting two formats be verified by one key.
const sessionInfo = "ai-gateway ui session v1"

// Sessions issues and verifies browser sessions.
//
// Sessions are stateless: the token carries its own expiry and is signed, so
// nothing is stored server-side and nothing has to be replicated. That matters
// for the arrangement deploy/docker-compose.yml runs — several gateways behind
// one load balancer — where a session held in one process's memory would sign
// the operator out on every other request. Deriving the signing key from the
// master key instead means every instance verifies every session without
// sharing any state at all.
//
// The cost of statelessness is that a token cannot be revoked before it
// expires. That is why the TTL is short and why rotating the master key
// invalidates every session at once: the derived signing key changes with it.
type Sessions struct {
	secret []byte
	ttl    time.Duration
	// now is injectable so expiry can be tested without sleeping.
	now func() time.Time
}

// NewSessions derives a signing key from the master key.
//
// An empty master key falls back to a random per-process secret. That keeps the
// type usable in tests and leaves no path where an unset credential silently
// produces a predictable signing key — but it also means sessions do not
// survive a restart or span instances, which is why config.validateUI refuses
// to enable the UI without a master key in the first place.
func NewSessions(masterKey string, ttl time.Duration) (*Sessions, error) {
	secret := make([]byte, 32)
	if masterKey == "" {
		if _, err := rand.Read(secret); err != nil {
			return nil, fmt.Errorf("generate ui session secret: %w", err)
		}
	} else {
		derived, err := hkdf.Key(sha256.New, []byte(masterKey), nil, sessionInfo, 32)
		if err != nil {
			return nil, fmt.Errorf("derive ui session secret: %w", err)
		}
		secret = derived
	}
	return &Sessions{secret: secret, ttl: ttl, now: time.Now}, nil
}

// TTL is how long a session issued now will live.
func (s *Sessions) TTL() time.Duration { return s.ttl }

// Issue returns a signed token for subject and the instant it stops being
// valid. The subject is a label for the audit trail — "master" today, an
// identity provider's subject once the UI accepts an SSO login — and is signed
// along with the expiry so neither can be edited by whoever holds the cookie.
func (s *Sessions) Issue(subject string) (token string, expires time.Time) {
	expires = s.now().Add(s.ttl).UTC()
	payload := subject + "|" + strconv.FormatInt(expires.Unix(), 10)
	return encode(payload) + "." + encode(string(s.sign(payload))), expires
}

// Validate reports the subject a token was issued to, or false if the token is
// unsigned, altered or expired.
//
// The signature is checked before the expiry is read, because an expiry read
// from an unverified token is a number the client chose.
func (s *Sessions) Validate(token string) (subject string, ok bool) {
	rawPayload, rawMAC, found := strings.Cut(token, ".")
	if !found {
		return "", false
	}
	payload, err := decode(rawPayload)
	if err != nil {
		return "", false
	}
	mac, err := decode(rawMAC)
	if err != nil {
		return "", false
	}
	if !hmac.Equal([]byte(mac), s.sign(payload)) {
		return "", false
	}
	subject, rawExpiry, found := strings.Cut(payload, "|")
	if !found {
		return "", false
	}
	unix, err := strconv.ParseInt(rawExpiry, 10, 64)
	if err != nil {
		return "", false
	}
	if !s.now().Before(time.Unix(unix, 0)) {
		return "", false
	}
	return subject, true
}

func (s *Sessions) sign(payload string) []byte {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

func encode(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func decode(s string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	return string(b), err
}
