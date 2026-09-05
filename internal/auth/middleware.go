package auth

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/limiter"
)

// ctxKey is unexported so nothing outside this package can plant or overwrite an
// authentication result in a request context.
type ctxKey struct{}

// Context carries the authenticated caller through the request.
type Context struct {
	Key         *core.Key
	Credentials Credentials
}

// NewContext returns ctx carrying the authentication result.
func NewContext(ctx context.Context, ac *Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, ac)
}

// FromContext returns the authentication result placed by the middleware.
func FromContext(ctx context.Context) (*Context, bool) {
	ac, ok := ctx.Value(ctxKey{}).(*Context)
	return ac, ok
}

// Authenticator resolves and authorizes virtual keys.
type Authenticator struct {
	store       KeyStore
	headerNames []string
	masterKey   string
	limits      *limiter.Limiter
	now         func() time.Time
}

// NewAuthenticator builds an Authenticator and seeds the store with the keys
// declared in configuration. Config-declared plaintext keys are hashed here and
// not retained.
func NewAuthenticator(ctx context.Context, store KeyStore, cfg config.VirtualKeysConfig) (*Authenticator, error) {
	a := &Authenticator{
		store:       store,
		headerNames: cfg.HeaderNames,
		masterKey:   cfg.MasterKey,
		limits:      limiter.New(),
		now:         time.Now,
	}
	for i, spec := range cfg.Keys {
		k := &core.Key{
			Hash:             HashKey(spec.Key),
			Alias:            spec.Alias,
			Models:           spec.Models,
			RPMLimit:         spec.RPMLimit,
			TPMLimit:         spec.TPMLimit,
			AllowPassthrough: spec.AllowPassthrough,
			Blocked:          spec.Blocked,
			CreatedAt:        time.Now().UTC(),
			ExpiresAt:        spec.ExpiresAt,
		}
		if err := store.Put(ctx, k); err != nil {
			return nil, fmt.Errorf("seed virtual_keys.keys[%d]: %w", i, err)
		}
	}
	return a, nil
}

// HeaderNames returns the custom headers that may carry a virtual key.
func (a *Authenticator) HeaderNames() []string { return a.headerNames }

// Limiter exposes the per-key limiter so completed requests can report token
// usage back against the calling key.
func (a *Authenticator) Limiter() *limiter.Limiter { return a.limits }

// HasMasterKey reports whether key-management endpoints are enabled.
func (a *Authenticator) HasMasterKey() bool { return a.masterKey != "" }

// IsMasterKey reports whether the presented credential is the master key.
func (a *Authenticator) IsMasterKey(presented string) bool {
	return a.masterKey != "" && SecretsEqual(a.masterKey, presented)
}

// Authenticate resolves the virtual key on a request and applies the checks that
// do not depend on which model is being asked for. Model authorization happens
// later, once the body has been read.
func (a *Authenticator) Authenticate(ctx context.Context, h http.Header) (*Context, error) {
	creds, ok := ExtractGatewayKey(h, a.headerNames)
	if !ok {
		return nil, core.ErrKeyInvalid
	}

	// The master key authenticates as an unrestricted caller so that an
	// operator can exercise the gateway without minting a virtual key first.
	if a.IsMasterKey(creds.Key) {
		return &Context{
			Key:         &core.Key{Hash: "master", Alias: "master-key", AllowPassthrough: true},
			Credentials: creds,
		}, nil
	}

	key, err := a.store.Get(ctx, HashKey(creds.Key))
	if err != nil {
		return nil, core.ErrKeyInvalid
	}
	if err := key.Usable(a.now()); err != nil {
		return nil, err
	}
	if !a.limits.Reserve(key.Hash, key.RPMLimit, key.TPMLimit) {
		return nil, core.ErrRateLimited
	}
	return &Context{Key: key, Credentials: creds}, nil
}

// AuthorizeModel checks that the authenticated key may call the named model
// group.
func (a *Authenticator) AuthorizeModel(ac *Context, model string) error {
	if ac == nil || ac.Key == nil {
		return core.ErrKeyInvalid
	}
	if !ac.Key.AllowsModel(model) {
		return fmt.Errorf("key %q may not call %q: %w", ac.Key.Alias, model, core.ErrModelNotAllowed)
	}
	return nil
}
