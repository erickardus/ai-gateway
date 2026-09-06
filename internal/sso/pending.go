package sso

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/url"
	"sync"
	"time"
)

// LoginTTL bounds how long a login may sit half-finished. It has to cover a
// developer being bounced through a provider that asks for a second factor,
// and no longer.
const LoginTTL = 10 * time.Minute

// TicketTTL bounds the window between the browser returning a code and the CLI
// redeeming it. The CLI is waiting on a local listener when the code arrives,
// so this is generous already.
const TicketTTL = 2 * time.Minute

// maxPendingLogins bounds what an unauthenticated caller can make the gateway
// hold. /sso/login takes no credential — it cannot, since issuing one is the
// point — so without a cap, repeated calls would grow the map until the TTL
// caught up, at whatever rate the caller chose. Ten thousand is far above any
// real login rate and a couple of megabytes at worst.
const maxPendingLogins = 10_000

// Login is one authorization request in flight, remembered from the moment the
// gateway redirects a browser to the provider until that browser comes back.
type Login struct {
	// RedirectURI is the CLI's loopback listener, validated as loopback before
	// it was stored.
	RedirectURI string
	// Challenge is the CLI's PKCE challenge. The ticket the callback leaves can
	// only be redeemed by whoever holds the matching verifier, so another
	// process on the same machine cannot race the CLI for the key.
	Challenge string
	// Nonce ties the provider's token back to this request.
	Nonce string
	// Device names the machine being signed in, so one person's laptops get
	// separate keys.
	Device string
}

// Ticket is what a completed login leaves behind for the CLI to collect. It
// holds the one copy of the plaintext key that will ever exist.
type Ticket struct {
	Challenge    string
	Key          string
	KeyHash      string
	Subject      string
	Display      string
	Role         string
	ExpiresAt    time.Time
	RefreshToken string
}

// Store keeps the short-lived state of logins in flight.
//
// It is an interface for the same reason auth.KeyLimiter is one: the browser
// leaves the gateway and comes back, and behind a load balancer it may come
// back to a different instance. A process-local store is right for a single
// instance and wrong for a fleet, and only the process wiring the gateway
// together knows which it is.
type Store interface {
	PutLogin(ctx context.Context, state string, l Login, ttl time.Duration) error
	// TakeLogin returns a login and removes it. A state is good for one
	// callback: without that, a captured callback URL could be replayed.
	TakeLogin(ctx context.Context, state string) (Login, bool, error)
	PutTicket(ctx context.Context, code string, t Ticket, ttl time.Duration) error
	// TakeTicket returns a ticket and removes it, for the same reason.
	TakeTicket(ctx context.Context, code string) (Ticket, bool, error)
}

// MemStore keeps pending state in this process.
type MemStore struct {
	mu      sync.Mutex
	logins  map[string]entry[Login]
	tickets map[string]entry[Ticket]
	now     func() time.Time
}

type entry[T any] struct {
	value   T
	expires time.Time
}

// NewMemStore returns a process-local store.
func NewMemStore() *MemStore {
	return &MemStore{
		logins:  make(map[string]entry[Login]),
		tickets: make(map[string]entry[Ticket]),
		now:     time.Now,
	}
}

// PutLogin implements Store.
func (m *MemStore) PutLogin(_ context.Context, state string, l Login, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked()
	// Refusing the newest login is the wrong half to drop in the abstract, but
	// the right one here: evicting an older entry instead would let a caller
	// spend this budget to cancel other people's logins in flight.
	if len(m.logins) >= maxPendingLogins {
		return errorf("too many sign-ins in progress; try again in a moment")
	}
	m.logins[state] = entry[Login]{value: l, expires: m.now().Add(ttl)}
	return nil
}

// TakeLogin implements Store.
func (m *MemStore) TakeLogin(_ context.Context, state string) (Login, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.logins[state]
	delete(m.logins, state)
	if !ok || m.now().After(e.expires) {
		return Login{}, false, nil
	}
	return e.value, true, nil
}

// PutTicket implements Store.
func (m *MemStore) PutTicket(_ context.Context, code string, t Ticket, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked()
	m.tickets[code] = entry[Ticket]{value: t, expires: m.now().Add(ttl)}
	return nil
}

// TakeTicket implements Store.
func (m *MemStore) TakeTicket(_ context.Context, code string) (Ticket, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.tickets[code]
	delete(m.tickets, code)
	if !ok || m.now().After(e.expires) {
		return Ticket{}, false, nil
	}
	return e.value, true, nil
}

// sweepLocked drops expired entries. Logins are short-lived and few, so this
// runs on write rather than on a timer: a store nobody is writing to is not
// growing either. It holds one unredeemed key plaintext per abandoned login,
// which is exactly why it should not accumulate them.
func (m *MemStore) sweepLocked() {
	now := m.now()
	for k, e := range m.logins {
		if now.After(e.expires) {
			delete(m.logins, k)
		}
	}
	for k, e := range m.tickets {
		if now.After(e.expires) {
			delete(m.tickets, k)
		}
	}
}

// NewToken returns an unguessable opaque value, used for states, nonces and
// one-time codes.
func NewToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Challenge returns the S256 PKCE challenge for a verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// VerifyChallenge reports whether a verifier matches a challenge.
func VerifyChallenge(challenge, verifier string) bool {
	if challenge == "" || verifier == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(challenge), []byte(Challenge(verifier))) == 1
}

// ValidateLoopback checks a client's redirect URI.
//
// This is the security-critical check on the whole flow. The gateway sends a
// browser to whatever address this returns, carrying a one-time code that can
// be redeemed for a virtual key — so an address that is not this machine turns
// the gateway into an open redirect that hands the key to whoever asked. Only
// a loopback address is accepted, and it is checked by parsing the host as an
// IP rather than by comparing strings: "127.0.0.1.example.com" begins with a
// loopback address and belongs to somebody else.
func ValidateLoopback(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errorf("redirect_uri: not a valid URL")
	}
	if u.Scheme != "http" {
		return errorf("redirect_uri: scheme must be http, got %q; the target is a loopback listener on the developer's own machine", u.Scheme)
	}
	if u.User != nil {
		return errorf("redirect_uri: must not carry userinfo")
	}
	host := u.Hostname()
	if host == "localhost" {
		// Accepted, but note it resolves through the client's own resolver;
		// the numeric forms below are the ones the CLI actually sends.
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errorf("redirect_uri: host must be a loopback address, got %q", host)
	}
	return nil
}
