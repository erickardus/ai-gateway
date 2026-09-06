package server

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/erickardus/ai-gateway/internal/audit"
	"github.com/erickardus/ai-gateway/internal/auth"
	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/metrics"
	"github.com/erickardus/ai-gateway/internal/sso"
)

// UseSSO enables the SSO endpoints.
//
// Like UseKeyLimiter, it is wired after construction rather than taken as a
// constructor argument: whether pending logins are held in this process or
// shared through Redis is a deployment decision, and the process assembling the
// gateway is the only thing that knows which. A nil provider leaves the
// endpoints unregistered, so a gateway with no SSO serves no OIDC surface.
func (s *Server) UseSSO(p *sso.Provider, state sso.Store) {
	if p == nil || state == nil {
		return
	}
	s.sso, s.ssoState = p, state
}

// maxDeviceName bounds the device label a client may send. It ends up in a key
// alias, in logs and in spend reports, all of which are read by people.
const maxDeviceName = 64

// handleSSOLogin starts a login. The client opens this in a browser.
//
// The client's PKCE challenge is recorded here and checked at redemption, so
// the one-time code the callback hands back can only be spent by the process
// that started the login — not by anything else that can see a loopback URL.
func (s *Server) handleSSOLogin(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirectURI := q.Get("redirect_uri")
	challenge := q.Get("code_challenge")

	if err := sso.ValidateLoopback(redirectURI); err != nil {
		// Refused before anything else happens, and never echoed back to the
		// address that was rejected: sending a browser to an unvalidated
		// address is the failure this check exists to prevent.
		s.ssoRefuse(w, http.StatusBadRequest, err.Error())
		return
	}
	if q.Get("code_challenge_method") != "S256" {
		s.ssoRefuse(w, http.StatusBadRequest,
			"code_challenge_method must be S256; the plain method offers no protection over a loopback redirect")
		return
	}
	if len(challenge) < 43 {
		s.ssoRefuse(w, http.StatusBadRequest, "code_challenge is missing or too short")
		return
	}

	state, err := sso.NewToken()
	if err != nil {
		s.ssoFail(w, r, err)
		return
	}
	nonce, err := sso.NewToken()
	if err != nil {
		s.ssoFail(w, r, err)
		return
	}

	login := sso.Login{
		RedirectURI: redirectURI,
		Challenge:   challenge,
		Nonce:       nonce,
		Device:      sanitizeDevice(q.Get("device")),
	}
	if err := s.ssoState.PutLogin(r.Context(), state, login, sso.LoginTTL); err != nil {
		s.ssoFail(w, r, err)
		return
	}

	target, err := s.sso.AuthCodeURL(r.Context(), state, nonce)
	if err != nil {
		s.ssoFail(w, r, err)
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// handleSSOCallback receives the provider's redirect, mints the key, and sends
// the browser back to the waiting client with a one-time code.
func (s *Server) handleSSOCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	login, ok, err := s.ssoState.TakeLogin(r.Context(), q.Get("state"))
	if err != nil {
		s.ssoFail(w, r, err)
		return
	}
	if !ok {
		// Either the login expired, or this callback has already been used, or
		// it belongs to no login this gateway started. All three are the same
		// answer, and there is nowhere safe to redirect to: the address to
		// return to was part of the state that is missing.
		s.ssoRefuse(w, http.StatusBadRequest,
			"this sign-in link has expired or was already used. Run the login command again.")
		return
	}

	// From here the client's address is known and validated, so a failure can
	// be reported to the terminal the developer is actually looking at.
	if e := q.Get("error"); e != "" {
		s.ssoReturn(w, r, login.RedirectURI, "", providerError(e, q.Get("error_description")))
		return
	}
	code := q.Get("code")
	if code == "" {
		s.ssoReturn(w, r, login.RedirectURI, "", "the provider returned no authorization code")
		return
	}

	ticketCode, err := s.completeSSOLogin(r, login, code)
	if err != nil {
		s.log.Warn("sso login failed", "error", err, "request_id", RequestIDFrom(r.Context()))
		s.metrics.RecordSSO(metrics.SSOLogin, metrics.SSOFailure)
		s.ssoReturn(w, r, login.RedirectURI, "", err.Error())
		return
	}
	s.metrics.RecordSSO(metrics.SSOLogin, metrics.SSOSuccess)
	s.ssoReturn(w, r, login.RedirectURI, ticketCode, "")
}

// completeSSOLogin exchanges the provider's code, decides what the identity may
// do, issues the key, and leaves a ticket for the client to collect.
func (s *Server) completeSSOLogin(r *http.Request, login sso.Login, code string) (string, error) {
	ctx := r.Context()
	tokens, err := s.sso.Exchange(ctx, code)
	if err != nil {
		return "", err
	}
	id, err := s.sso.Verify(ctx, tokens.IDToken, login.Nonce)
	if err != nil {
		return "", err
	}
	role, ok := s.sso.Role(id)
	if !ok {
		// Recorded, unlike the two failures above it. This is the refusal an
		// auditor asks about — someone the identity provider vouched for was
		// denied a credential by this gateway's own rules — and it has an actor
		// to name. A failed exchange or an unverifiable token has neither: the
		// endpoint is unauthenticated by design, nothing was established about
		// the caller, and an audit write fsyncs, so recording them would let a
		// stranger write to the operator's disk at will.
		reason := fmt.Errorf("%s is not in any group this gateway grants access to", id.Display())
		if auditErr := s.recordAuditErr(r, s.ssoEvent(r, audit.ActionSSOGrant, id, audit.Event{
			Outcome: audit.OutcomeRefused,
			Reason:  reason.Error(),
		})); auditErr != nil {
			return "", auditErr
		}
		return "", reason
	}

	plaintext, key, err := s.issueSSOKey(r, audit.ActionSSOGrant, id, role, login.Device)
	if err != nil {
		return "", err
	}

	ticketCode, err := sso.NewToken()
	if err != nil {
		return "", err
	}
	ticket := sso.Ticket{
		Challenge:    login.Challenge,
		Key:          plaintext,
		KeyHash:      key.Hash,
		Subject:      id.Subject,
		Display:      id.Display(),
		Role:         role.Match,
		ExpiresAt:    *key.ExpiresAt,
		RefreshToken: tokens.RefreshToken,
	}
	if err := s.ssoState.PutTicket(ctx, ticketCode, ticket, sso.TicketTTL); err != nil {
		return "", err
	}
	return ticketCode, nil
}

// ssoExchangeRequest is the body of POST /sso/exchange.
type ssoExchangeRequest struct {
	Code         string `json:"code"`
	CodeVerifier string `json:"code_verifier"`
}

// handleSSOExchange redeems a one-time code for the key.
func (s *Server) handleSSOExchange(w http.ResponseWriter, r *http.Request) {
	var req ssoExchangeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "request body is not valid JSON")
		return
	}
	if req.Code == "" || req.CodeVerifier == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "code and code_verifier are required")
		return
	}

	// The ticket is consumed whether or not the verifier matches. A code that
	// survived a failed redemption could be guessed at repeatedly.
	ticket, ok, err := s.ssoState.TakeTicket(r.Context(), req.Code)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"this code has expired or was already redeemed")
		return
	}
	if !sso.VerifyChallenge(ticket.Challenge, req.CodeVerifier) {
		s.log.Warn("sso: code redeemed with a mismatched verifier", "subject", ticket.Subject)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "code_verifier does not match")
		return
	}

	s.writeSSOGrant(w, ssoGrant{
		Identity:     ticket.Display,
		Subject:      ticket.Subject,
		Role:         ticket.Role,
		Key:          ticket.Key,
		ExpiresAt:    ticket.ExpiresAt,
		RefreshToken: ticket.RefreshToken,
	})
}

// ssoRenewRequest is the body of POST /sso/renew.
type ssoRenewRequest struct {
	RefreshToken string `json:"refresh_token"`
	Device       string `json:"device"`
}

// handleSSORenew reissues a key for an identity that is still valid.
//
// The refresh token is spent against the provider rather than merely checked
// here, which is the point: it is what makes a disabled account stop renewing
// without an operator having to revoke anything at the gateway as well.
func (s *Server) handleSSORenew(w http.ResponseWriter, r *http.Request) {
	var req ssoRenewRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "request body is not valid JSON")
		return
	}
	if req.RefreshToken == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "refresh_token is required")
		return
	}

	tokens, err := s.sso.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		s.log.Info("sso renewal refused by the provider", "error", err)
		s.metrics.RecordSSO(metrics.SSORenewal, metrics.SSOFailure)
		writeError(w, http.StatusUnauthorized, "authentication_error",
			"the identity provider refused this renewal; sign in again")
		return
	}
	id, err := s.sso.Verify(r.Context(), tokens.IDToken, "")
	if err != nil {
		s.metrics.RecordSSO(metrics.SSORenewal, metrics.SSOFailure)
		writeError(w, http.StatusUnauthorized, "authentication_error", err.Error())
		return
	}
	role, ok := s.sso.Role(id)
	if !ok {
		// Entitlements are re-derived on every renewal, so someone moved out of
		// a group loses access at their next renewal rather than at their next
		// key expiry. That is the moment worth recording: an identity the
		// provider still vouches for, refused here.
		s.metrics.RecordSSO(metrics.SSORenewal, metrics.SSOFailure)
		message := fmt.Sprintf("%s is no longer in any group this gateway grants access to", id.Display())
		if auditErr := s.recordAuditErr(r, s.ssoEvent(r, audit.ActionSSORenew, id, audit.Event{
			Outcome: audit.OutcomeRefused,
			Reason:  message,
		})); auditErr != nil {
			s.fail(w, r, auditErr)
			return
		}
		writeError(w, http.StatusForbidden, "permission_error", message)
		return
	}

	plaintext, key, err := s.issueSSOKey(r, audit.ActionSSORenew, id, role, sanitizeDevice(req.Device))
	if err != nil {
		s.metrics.RecordSSO(metrics.SSORenewal, metrics.SSOFailure)
		s.fail(w, r, err)
		return
	}
	// A provider that rotates refresh tokens returns a new one; one that does
	// not returns nothing, and the client keeps what it has.
	refresh := tokens.RefreshToken
	if refresh == "" {
		refresh = req.RefreshToken
	}
	s.metrics.RecordSSO(metrics.SSORenewal, metrics.SSOSuccess)
	s.writeSSOGrant(w, ssoGrant{
		Identity:     id.Display(),
		Subject:      id.Subject,
		Role:         role.Match,
		Key:          plaintext,
		ExpiresAt:    *key.ExpiresAt,
		RefreshToken: refresh,
	})
}

// issueSSOKey mints the virtual key for an identity and retires the one it
// replaces on the same machine.
//
// Retiring the previous key is what keeps a repeated login from accumulating
// live credentials for one person on one laptop. It is scoped to the device so
// that signing in on a second machine does not sign the first one out.
func (s *Server) issueSSOKey(r *http.Request, action string, id *sso.Identity, role config.SSORole, device string) (string, *core.Key, error) {
	ctx := r.Context()
	plaintext, hash, err := auth.Generate()
	if err != nil {
		return "", nil, err
	}
	now := time.Now().UTC()
	expires := now.Add(s.sso.Config().KeyDuration)
	// The entitlements come from the shared helper rather than being spelled
	// out here, so an identity authenticating with the provider's own token
	// gets exactly what its issued key would have granted. Two copies of this
	// mapping would drift, and the drift would be a person holding two
	// credentials that permit different things.
	key := sso.KeyForRole(id, role)
	key.Hash = hash
	key.Alias = ssoAlias(id, device)
	key.CreatedAt = now
	key.ExpiresAt = &expires
	key.Device = device

	// Write-ahead, as everywhere else: the grant is recorded before the key
	// that carries it exists, so a log that cannot take the record hands the
	// developer a failed sign-in rather than an unrecorded credential.
	ev := s.ssoEvent(r, action, id, audit.Event{
		Target: hash,
		Detail: detail("role", role.Match, "device", device),
	})
	if auditErr := s.recordAuditErr(r, ev); auditErr != nil {
		return "", nil, auditErr
	}
	if err := s.store.Put(ctx, key); err != nil {
		s.recordAuditFailure(r, ev, err)
		return "", nil, err
	}
	s.retireSSOKeys(r, id, device, hash)

	s.log.Info("sso key issued",
		"identity", id.Display(), "subject", id.Subject, "device", device,
		"role", role.Match, "hash", hash, "expires_at", expires.Format(time.RFC3339))
	return plaintext, key, nil
}

// retireSSOKeys deletes the keys this identity previously held on this machine.
//
// Their spend is deliberately left in the ledger. It accumulates under the
// person rather than the credential — see core.Key.SpendSubject — so dropping
// it here would hand every developer a way to clear their own budget by
// signing in again.
func (s *Server) retireSSOKeys(r *http.Request, id *sso.Identity, device, keep string) {
	ctx := r.Context()
	keys, err := s.store.List(ctx)
	if err != nil {
		s.log.Warn("sso: could not list keys to retire the previous one", "error", err, "subject", id.Subject)
		return
	}
	for _, k := range keys {
		if k.Subject != id.Subject || k.Device != device || k.Hash == keep {
			continue
		}
		// A retirement is a key.delete like any other, and is recorded as one.
		// Without this a reader sees credentials granted and never sees them
		// destroyed: the chain would name a key that is missing from
		// /key/list with nothing saying where it went, which is exactly the
		// question an auditor reconciling the two would ask.
		//
		// Write-ahead, so a record that cannot be written leaves the superseded
		// key in place rather than destroying one unrecorded. That is the same
		// choice the loop already makes when the delete itself fails, and it
		// errs the same way: a second live key for one person and device, which
		// the next sign-in retires.
		ev := s.ssoEvent(r, audit.ActionKeyDelete, id, audit.Event{
			TargetKind: audit.TargetKey,
			Target:     k.Hash,
			Detail:     detail("device", device, "reason", "superseded"),
		})
		if auditErr := s.recordAuditErr(r, ev); auditErr != nil {
			s.log.Warn("sso: could not record retiring a superseded key, so it was left in place",
				"error", auditErr, "hash", k.Hash)
			continue
		}
		if err := s.store.Delete(ctx, k.Hash); err != nil {
			s.recordAuditFailure(r, ev, err)
			s.log.Warn("sso: could not retire a superseded key", "error", err, "hash", k.Hash)
			continue
		}
		s.auth.ForgetKey(ctx, k.Hash)
		s.log.Info("sso key retired", "hash", k.Hash, "subject", id.Subject, "device", device)
	}
}

// ssoGrant is what both /sso/exchange and /sso/renew return.
type ssoGrant struct {
	Identity     string
	Subject      string
	Role         string
	Key          string
	ExpiresAt    time.Time
	RefreshToken string
}

// writeSSOGrant renders a grant, including the client configuration derived
// from it.
func (s *Server) writeSSOGrant(w http.ResponseWriter, g ssoGrant) {
	cfg := s.sso.Config()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"identity":      g.Identity,
		"subject":       g.Subject,
		"role":          g.Role,
		"expires_at":    g.ExpiresAt.UTC().Format(time.RFC3339),
		"renew_within":  cfg.RenewWithin.String(),
		"refresh_token": g.RefreshToken,
		"env":           s.ssoEnv(g.Key),
		"note":          "This key is returned once. It belongs in ANTHROPIC_CUSTOM_HEADERS and nowhere else.",
	})
}

// ssoEnv is the Claude Code configuration a client should write.
//
// The gateway builds it rather than the client, so a developer needs nothing
// but the gateway's address and a change of model name reaches everyone at
// their next renewal.
//
// The key goes in a custom header and never in ANTHROPIC_API_KEY or
// ANTHROPIC_AUTH_TOKEN. Those two, and an apiKeyHelper, are the three ways
// Claude Code can be given a gateway credential, and all three displace a
// claude.ai subscription login — which would bill the developer per token for
// traffic their subscription already covers. Preserving that login is the whole
// reason this gateway exists, so this function is the one place the choice is
// made, and ssoEnvNeverDisplacesSubscription in the tests holds it to it.
func (s *Server) ssoEnv(key string) map[string]string {
	cfg := s.sso.Config()
	header := "x-gateway-key"
	if names := s.authHeaderNames(); len(names) > 0 {
		header = names[0]
	}
	env := map[string]string{
		"ANTHROPIC_CUSTOM_HEADERS": header + ": " + key,
	}
	if cfg.BaseURL != "" {
		env["ANTHROPIC_BASE_URL"] = cfg.BaseURL
	}
	if cfg.Model != "" {
		env["ANTHROPIC_MODEL"] = cfg.Model
	}
	return env
}

// ssoReturn sends the browser back to the waiting client. The address has
// already been validated as loopback.
func (s *Server) ssoReturn(w http.ResponseWriter, r *http.Request, redirectURI, code, failure string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		s.ssoRefuse(w, http.StatusBadRequest, "the client's address is not a valid URL")
		return
	}
	q := u.Query()
	if failure != "" {
		q.Set("error", failure)
	} else {
		q.Set("code", code)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// ssoRefuse renders an error in the browser, for the cases where there is no
// client address to return to.
func (s *Server) ssoRefuse(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Sign-in failed</title>`+
		`<body style="font:16px system-ui;margin:4rem auto;max-width:34rem">`+
		`<h1 style="font-size:1.25rem">Sign-in failed</h1><p>%s</p></body>`,
		html.EscapeString(message))
}

// ssoFail reports a gateway-side failure during a browser flow.
func (s *Server) ssoFail(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("sso", "error", err, "request_id", RequestIDFrom(r.Context()))
	s.metrics.RecordSSO(metrics.SSOLogin, metrics.SSOFailure)
	s.ssoRefuse(w, http.StatusBadGateway,
		"the gateway could not reach the identity provider. Try again, and tell your gateway operator if it persists.")
}

// providerError renders the provider's own refusal.
func providerError(code, description string) string {
	if description != "" {
		return "the identity provider refused the sign-in: " + code + ": " + description
	}
	return "the identity provider refused the sign-in: " + code
}

// ssoAlias names a key for the people who read key listings and spend reports.
func ssoAlias(id *sso.Identity, device string) string {
	if device == "" {
		return id.Display()
	}
	return id.Display() + " (" + device + ")"
}

// sanitizeDevice reduces a client-supplied machine name to something safe to
// store, log and display. It is decoration, not identity, so anything
// unsuitable is dropped rather than refused.
func sanitizeDevice(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= maxDeviceName {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.' || r == ' ':
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// decodeJSON reads a bounded JSON body.
func decodeJSON(w http.ResponseWriter, r *http.Request, into any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(into)
}
