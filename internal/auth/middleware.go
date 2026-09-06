package auth

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/limiter"
	"github.com/erickardus/ai-gateway/internal/spend"
)

// Context carries the authenticated caller through a request. It is returned
// from Authenticate and threaded explicitly rather than smuggled through the
// request context, so a handler cannot forget to authenticate and silently
// receive a zero value.
type Context struct {
	Key         *core.Key
	Credentials Credentials
	// Scope is the project, team or organisation the key belongs to, already
	// resolved, or nil for a key outside the hierarchy. Its ancestors reach
	// through Parent, so every check that walks the chain has it in hand
	// without a second lookup.
	Scope *core.Scope
}

// ScopeSubjects returns the ledger subjects this request's spend accumulates
// under, innermost first, or nil for a key outside any hierarchy.
func (c *Context) ScopeSubjects() []string {
	if c == nil || c.Scope == nil {
		return nil
	}
	chain := c.Scope.Chain()
	out := make([]string, 0, len(chain))
	for _, s := range chain {
		out = append(out, s.SpendSubject())
	}
	return out
}

// ScopeAliases returns display labels parallel to ScopeSubjects, falling back
// to the scope's id where none was configured.
func (c *Context) ScopeAliases() []string {
	if c == nil || c.Scope == nil {
		return nil
	}
	chain := c.Scope.Chain()
	out := make([]string, 0, len(chain))
	for _, s := range chain {
		if s.Alias != "" {
			out = append(out, s.Alias)
			continue
		}
		out = append(out, s.ID)
	}
	return out
}

// Authenticator resolves and authorizes virtual keys.
type Authenticator struct {
	store       KeyStore
	headerNames []string
	masterKey   string
	// limits counts each key's own rate allowance. It starts per-process and is
	// replaced by a shared implementation where the gateway runs as a fleet;
	// see UseKeyLimiter.
	limits KeyLimiter
	// scopes is the resolved entitlement hierarchy, keyed by fully qualified
	// id. Empty where no hierarchy is configured, which is what makes every
	// scope check below a no-op for a gateway that declares none.
	scopes map[string]*core.Scope
	// tokens verifies an identity provider's own token presented on a request.
	// Nil unless JWT authentication is configured; see UseTokenVerifier.
	tokens TokenVerifier
}

// NewAuthenticator builds an Authenticator and seeds the store with the keys
// declared in configuration. Config-declared plaintext keys are hashed here and
// not retained.
func NewAuthenticator(ctx context.Context, store KeyStore, cfg config.VirtualKeysConfig, scopes map[string]*core.Scope) (*Authenticator, error) {
	a := &Authenticator{
		store:       store,
		headerNames: cfg.HeaderNames,
		masterKey:   cfg.MasterKey,
		limits:      NewLocalLimiter(),
		scopes:      scopes,
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
			MaxBudget:        spec.MaxBudget,
			BudgetDuration:   spec.BudgetDuration,
			Scope:            spec.Scope,
		}
		if err := store.Put(ctx, k); err != nil {
			return nil, fmt.Errorf("seed virtual_keys.keys[%d]: %w", i, err)
		}
	}
	return a, nil
}

// HeaderNames returns the custom headers that may carry a virtual key.
func (a *Authenticator) HeaderNames() []string { return a.headerNames }

// UseKeyLimiter replaces the per-key rate limiter, so that several replicas
// enforce one allowance between them rather than one each.
//
// It is set after construction rather than taken as configuration because
// sharing is a deployment decision, not a property of the keys: the same key
// list runs as a single instance with no Redis and as a fleet with one, and
// only the process wiring the gateway together knows which. A nil limiter is
// ignored, so a caller that has no shared store need not branch.
func (a *Authenticator) UseKeyLimiter(l KeyLimiter) {
	if l != nil {
		a.limits = l
	}
}

// UseTokenVerifier enables authenticating a request with the identity
// provider's own token instead of a virtual key.
//
// Like UseKeyLimiter it is set after construction, because whether an identity
// provider is reachable is a deployment fact rather than a property of the key
// list. A nil verifier leaves the gateway accepting virtual keys only, which is
// what an unconfigured gateway does.
func (a *Authenticator) UseTokenVerifier(v TokenVerifier) {
	if v != nil {
		a.tokens = v
	}
}

// RecordKeyTokens charges a completed response's tokens against the calling
// key's token-per-minute allowance.
//
// It is separate from Admit because tokens are only known once the response has
// been read, where requests are known before it is sent. Both land on the same
// counter.
func (a *Authenticator) RecordKeyTokens(ctx context.Context, keyHash string, tokens int) {
	if keyHash == "" || tokens <= 0 {
		return
	}
	a.limits.AddTokens(ctx, keyHash, tokens)
}

// RecordTokens charges a response's tokens against the calling key's allowance
// and against every shared allowance above it.
//
// It takes the authenticated context rather than a hash because a pooled tpm is
// only a pool if every member's tokens land on it; charging the key alone would
// leave a scope's tpm_limit reserving requests it never counted the tokens for.
func (a *Authenticator) RecordTokens(ctx context.Context, ac *Context, tokens int) {
	if ac == nil || ac.Key == nil || tokens <= 0 {
		return
	}
	a.RecordKeyTokens(ctx, ac.Key.Hash, tokens)
	for _, s := range ac.Scope.Chain() {
		if s.TPMLimit <= 0 {
			continue
		}
		a.limits.AddTokens(ctx, s.SpendSubject(), tokens)
	}
}

// ForgetKey drops a revoked key's rate-limit counters.
func (a *Authenticator) ForgetKey(ctx context.Context, keyHash string) {
	if keyHash == "" {
		return
	}
	a.limits.Forget(ctx, keyHash)
}

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

	// An identity provider's own token, verified on this request rather than
	// exchanged for a key at login. It is tried before the key store because a
	// token is never in it: nothing was issued, so a store lookup would be a
	// wasted miss on every such request.
	var key *core.Key
	if a.tokens != nil && LooksLikeJWT(creds.Key) {
		var err error
		if key, err = a.tokens.VerifyToken(ctx, creds.Key); err != nil {
			return nil, err
		}
	} else {
		var err error
		if key, err = a.store.Get(ctx, HashKey(creds.Key)); err != nil {
			return nil, core.ErrKeyInvalid
		}
	}

	// Run on both paths. A stored key has an expiry the operator set; a verified
	// token has the token's own, so this is the check that stops a cached
	// verification being honoured past the credential it was derived from — and
	// it holds even if the cache's own bound were ever wrong.
	if err := key.Usable(time.Now()); err != nil {
		return nil, err
	}
	return a.contextFor(key, creds)
}

// contextFor resolves a key's scope and refuses anything the hierarchy blocks.
//
// A key naming a scope that is not configured is refused rather than treated as
// unscoped. The key store outlives the configuration that produced it — a
// persisted file, or a fleet mid-rollout — so a scope that has been renamed or
// removed leaves keys pointing at nothing. Treating those as unscoped would
// silently drop the pooled cap they were issued under, which is the one failure
// mode a budget must not have.
func (a *Authenticator) contextFor(key *core.Key, creds Credentials) (*Context, error) {
	ac := &Context{Key: key, Credentials: creds}
	if key.Scope == "" {
		return ac, nil
	}
	scope, ok := a.scopes[key.Scope]
	if !ok {
		return nil, fmt.Errorf("key %q names scope %q, which is not configured: %w",
			key.Alias, key.Scope, core.ErrKeyInvalid)
	}
	if err := scope.Usable(); err != nil {
		return nil, err
	}
	ac.Scope = scope
	return ac, nil
}

// CheckBudget reports whether the key has spend remaining for its window.
//
// It is a pre-call check, so a key that has already overspent is refused before
// the gateway incurs any further cost. Only billable traffic counts: requests
// served by a passthrough deployment bill the caller's own subscription and so
// never consume a budget the operator set.
func (a *Authenticator) CheckBudget(ctx context.Context, ac *Context, ledger spend.Store) error {
	if ac == nil || ac.Key == nil || ledger == nil {
		return nil
	}
	// The key's own cap and every pool above it are read in one call.
	//
	// Asking separately would be a round trip per level on the hot path of
	// every request — up to four under a full organisation/team/project
	// hierarchy — for a set of subjects known before the request starts. Only
	// the caps that exist are asked about, so a gateway with no hierarchy asks
	// exactly what it always did.
	subjects, caps := budgetQuestions(ac)
	if len(subjects) == 0 {
		return nil
	}
	spends, err := ledger.Spends(ctx, subjects)
	if err != nil {
		return fmt.Errorf("read spend: %w", err)
	}

	// Innermost first, so the refusal names the level closest to the caller —
	// the one whose owner can actually do something about it. budgetQuestions
	// builds the slice in that order.
	for i, c := range caps {
		if i >= len(spends) || spends[i] < c.max {
			continue
		}
		if c.scope == nil {
			return fmt.Errorf("key %q has spent %.4f of its %.4f budget: %w",
				ac.Key.Alias, spends[i], c.max, core.ErrBudgetExceeded)
		}
		// The figures go to the log through the wrapped message; the typed
		// error is what reaches the caller, and it names the level without them.
		return fmt.Errorf("%s %q has spent %.4f of its %.4f shared budget: %w",
			c.scope.Kind, c.scope.ID, spends[i], c.max,
			&core.ScopeBudgetError{Kind: c.scope.Kind, ID: c.scope.ID})
	}
	return nil
}

// budgetCap pairs a subject's cap with the scope that set it, or nil for the
// caller's own key.
type budgetCap struct {
	max   float64
	scope *core.Scope
}

// budgetQuestions lists the budgets this request must satisfy, innermost first:
// the key's own, then each scope above it that declares one.
//
// A scope's budget is not a second allowance for this key. Every key beneath
// the scope has been writing to the same subject, so what comes back is what
// the whole team has spent — the difference between a role and a pool, and the
// only reason the chain is walked at all.
func budgetQuestions(ac *Context) ([]spend.Subject, []budgetCap) {
	var subjects []spend.Subject
	var caps []budgetCap
	if ac.Key.MaxBudget > 0 {
		subjects = append(subjects, spend.Subject{
			Kind: spend.KindKey, ID: ac.Key.SpendSubject(), Window: ac.Key.BudgetDuration,
		})
		caps = append(caps, budgetCap{max: ac.Key.MaxBudget})
	}
	for _, s := range ac.Scope.Chain() {
		if s.MaxBudget <= 0 {
			continue
		}
		subjects = append(subjects, spend.Subject{
			Kind: spend.KindScope, ID: s.SpendSubject(), Window: s.BudgetDuration,
		})
		caps = append(caps, budgetCap{max: s.MaxBudget, scope: s})
	}
	return subjects, caps
}

// Admit charges one request against the key's rate limits. It is deliberately
// separate from Authenticate: charging at authentication time would bill a key
// for requests the gateway then rejects for an unknown or forbidden model, and
// would let model discovery consume inference budget.
func (a *Authenticator) Admit(ctx context.Context, ac *Context) error {
	if ac == nil || ac.Key == nil {
		return core.ErrKeyInvalid
	}
	// A scope's rpm and tpm are shared the same way its budget is: one window
	// per scope, drawn down by every key beneath it. The key's own allowance and
	// every scope's are reserved in one all-or-nothing operation.
	//
	// That it is one operation is what makes it correct, not merely fast.
	// Reserving each in turn would let a request refused by an outer scope keep
	// the increment it had already made to an inner one, so a key's own window
	// would run ahead of the requests it actually served — and under Redis it
	// would be a round trip per level besides.
	chain := ac.Scope.Chain()
	claims := make([]limiter.Claim, 0, len(chain)+1)
	claims = append(claims, limiter.Claim{
		Subject: ac.Key.Hash, RPM: ac.Key.RPMLimit, TPM: ac.Key.TPMLimit,
	})
	for _, s := range chain {
		claims = append(claims, limiter.Claim{
			Subject: s.SpendSubject(), RPM: s.RPMLimit, TPM: s.TPMLimit,
		})
	}

	refused, err := a.limits.ReserveAll(ctx, claims)
	if err != nil {
		return fmt.Errorf("reserve capacity for key %q: %w", ac.Key.Alias, err)
	}
	switch {
	case refused < 0:
		return nil
	case refused == 0:
		return core.ErrRateLimited
	default:
		// claims[0] is the key, so the scopes are offset by one.
		s := chain[refused-1]
		return fmt.Errorf("%s %q is at its shared rate limit: %w", s.Kind, s.ID, core.ErrRateLimited)
	}
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
	// The chain is an intersection, so a key's own allowlist grants nothing its
	// scope withholds. The error names the level that refused rather than the
	// key, because that is where the allowlist an operator would edit lives.
	if denier := ac.Scope.Denier(model); denier != nil {
		return fmt.Errorf("%s %q does not allow %q: %w", denier.Kind, denier.ID, model,
			&core.ScopeModelError{Kind: denier.Kind, ID: denier.ID, Model: model})
	}
	return nil
}
