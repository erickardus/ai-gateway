// Package sso turns an OpenID Connect login into a gateway virtual key.
//
// The gateway is the only OIDC client in the arrangement. A developer's CLI
// holds no provider configuration and never speaks to the provider: it runs
// PKCE against the gateway, and the gateway runs the authorization-code flow
// against the identity provider. That keeps provider registration to a single
// redirect URI, and keeps the decision about what an identity is allowed to do
// on the server, where it can be audited.
//
// Only the pieces of OIDC this flow needs are implemented, and they are
// implemented against the standard library rather than a JOSE dependency: the
// gateway verifies ID tokens signed with RSA or ECDSA against the provider's
// published JWKS, and refuses everything else. Symmetric algorithms are refused
// outright — they would verify a token against the client secret, which turns
// anyone holding that secret into the identity provider — and so is "none".
package sso

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/erickardus/ai-gateway/internal/config"
)

// maxResponseBytes bounds what is read from the provider. Discovery documents
// and key sets are a few kilobytes; anything approaching this is a broken or
// hostile endpoint, and reading it unbounded would be the gateway's problem
// rather than the provider's.
const maxResponseBytes = 1 << 20

// httpTimeout bounds one call to the provider. It sits on the login path, where
// a developer is watching, so it is short enough to fail rather than hang.
const httpTimeout = 15 * time.Second

// Identity is who the provider says is logging in, reduced to what the gateway
// acts on. Nothing here grants anything: the role values are matched against
// configuration, which is where entitlements live.
type Identity struct {
	// Subject is the provider's stable identifier. It is what a key is bound
	// to, rather than the email, which people change.
	Subject string
	// Email and Name are for display and for a key's alias, so an operator
	// reading /key/list sees a person rather than a UUID.
	Email string
	Name  string
	// Roles holds the values of the configured role claim.
	Roles []string
	// Expiry is when the token asserting this identity stops being valid. It
	// bounds how long a per-request verification may be cached: a cache entry
	// outliving the token it was derived from would be the gateway extending a
	// credential the provider issued for less.
	Expiry time.Time
}

// Display renders the identity for a log line or a terminal, preferring the
// most human of whatever the provider supplied.
func (i Identity) Display() string {
	switch {
	case i.Email != "":
		return i.Email
	case i.Name != "":
		return i.Name
	default:
		return i.Subject
	}
}

// Tokens is what a token endpoint returned. Only the two fields the gateway
// acts on are kept: the ID token it verifies, and the refresh token it hands
// the client so a later renewal can re-check the identity.
//
// The access token is deliberately discarded. The gateway calls no provider API
// on the developer's behalf, so keeping one would be holding a credential with
// no use for it.
type Tokens struct {
	IDToken      string
	RefreshToken string
}

// Provider talks to one identity provider.
type Provider struct {
	cfg config.SSOConfig
	hc  *http.Client
	log *slog.Logger

	// now is overridden in tests so token lifetimes can be exercised without
	// sleeping.
	now func() time.Time

	mu     sync.Mutex
	disc   *discovery
	discAt time.Time
	keys   map[string]any
	keysAt time.Time

	// tokens memoizes per-request token verification. It has its own lock: a
	// cache hit must not queue behind a key-set refresh, which is the one thing
	// under mu that can take a network round trip.
	tokens *tokenCache
}

// New builds a Provider. The caller is expected to have validated cfg.
func New(cfg config.SSOConfig, log *slog.Logger) *Provider {
	return &Provider{
		cfg:    cfg,
		hc:     &http.Client{Timeout: httpTimeout},
		log:    log,
		now:    time.Now,
		tokens: newTokenCache(),
	}
}

// Config returns the provider's configuration, for handlers that need the key
// duration or the advertised client settings.
func (p *Provider) Config() config.SSOConfig { return p.cfg }

// errorf builds an error that is safe to hand a caller. Provider responses can
// carry a description of what went wrong, which is useful in a log and worth
// relaying to a developer staring at a browser tab, but it originates outside
// the gateway and is never interpolated into anything but an error string.
func errorf(format string, args ...any) error { return fmt.Errorf(format, args...) }
