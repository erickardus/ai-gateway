package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/erickardus/ai-gateway/internal/audit"
	"github.com/erickardus/ai-gateway/internal/auth"
	"github.com/erickardus/ai-gateway/internal/core"
)

// core.Key is already the JSON-tagged wire type and carries no plaintext — the
// key itself exists only in the response that issues it — so it is encoded
// directly. A parallel view struct would have to be updated in lockstep, and a
// field added to core.Key but forgotten here would silently vanish from the API.

// requireMaster gates the management endpoints on the master key. It reports
// whether the request may proceed.
//
// A refusal here is deliberately not audited, where a refused console sign-in
// is. The console's sign-in is a person at a form; this is an unauthenticated
// endpoint anyone who can open a socket may call as fast as they like, and an
// audit write fsyncs. Recording every attempt would hand a stranger a way to
// fill the operator's disk with evidence of nothing. The access log already
// carries the 401 with its source and request ID, which is the same fact
// without the amplification.
func (s *Server) requireMaster(w http.ResponseWriter, r *http.Request) bool {
	if !s.auth.HasMasterKey() {
		writeError(w, http.StatusNotFound, "not_found_error",
			"key management is disabled because no master key is configured")
		return false
	}
	creds, ok := auth.ExtractGatewayKey(r.Header, s.authHeaderNames())
	if !ok || !s.auth.IsMasterKey(creds.Key) {
		writeError(w, http.StatusUnauthorized, "authentication_error", "master key required")
		return false
	}
	return true
}

// generateRequest is the body of POST /key/generate.
type generateRequest struct {
	Alias            string   `json:"alias"`
	Models           []string `json:"models"`
	RPMLimit         int      `json:"rpm_limit"`
	TPMLimit         int      `json:"tpm_limit"`
	AllowPassthrough bool     `json:"allow_passthrough"`
	// Duration is a Go duration string such as "720h". Empty means no expiry.
	Duration string `json:"duration"`
	// MaxBudget caps billable spend over BudgetDuration. Zero is unlimited.
	//
	// A key minted here can carry a budget for the same reason a
	// config-declared one can: the ledger enforces the cap by key hash and
	// knows nothing about where the key was declared. Omitting these fields
	// would mean every key issued at runtime — which the quickstart's own
	// example does — silently had no cap at all.
	MaxBudget float64 `json:"max_budget"`
	// BudgetDuration is the window MaxBudget applies over, as a Go duration
	// string. Empty means the key's whole lifetime.
	BudgetDuration string `json:"budget_duration"`
	// Scope places the key in a project, team or organisation declared under
	// `rbac`, so its spend draws down that pool and its limits bind.
	Scope string `json:"scope"`
}

// handleKeyGenerate mints a new virtual key. The plaintext is returned exactly
// once here and never stored.
func (s *Server) handleKeyGenerate(w http.ResponseWriter, r *http.Request) {
	if !s.requireMaster(w, r) {
		return
	}
	s.keyGenerate(w, r)
}

// keyGenerate is handleKeyGenerate without the gate.
//
// The management handlers are split this way because the admin UI reaches the
// same operations through a browser session rather than a master-key header.
// Two callers, one authorization question each, and one implementation of the
// work — the alternative is a second set of handlers that drifts.
func (s *Server) keyGenerate(w http.ResponseWriter, r *http.Request) {
	var req generateRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "request body is not valid JSON")
			return
		}
	}

	var expiresAt *time.Time
	if req.Duration != "" {
		d, err := time.ParseDuration(req.Duration)
		if err != nil || d <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request_error",
				"duration must be a positive Go duration string, for example \"720h\"")
			return
		}
		t := time.Now().UTC().Add(d)
		expiresAt = &t
	}

	if req.MaxBudget < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"max_budget must not be negative")
		return
	}
	var budgetDuration time.Duration
	if req.BudgetDuration != "" {
		d, err := time.ParseDuration(req.BudgetDuration)
		if err != nil || d <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request_error",
				"budget_duration must be a positive Go duration string, for example \"720h\"")
			return
		}
		budgetDuration = d
	}
	// A window with nothing to cap is refused rather than ignored: it is far
	// likelier to be a budget the caller believes they set than a deliberate
	// no-op, and a key handed back with a window and no cap spends without
	// limit while looking constrained.
	if budgetDuration > 0 && req.MaxBudget == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"budget_duration requires max_budget; a window with no cap limits nothing")
		return
	}

	// An unknown scope is refused here rather than at first use. The endpoint
	// hands back a key that looks correct, so a mistyped team would otherwise
	// present as the key being rejected later, by which point whoever typed it
	// has moved on.
	if req.Scope != "" && s.cfg.Scope(req.Scope) == nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"scope "+strconv.Quote(req.Scope)+" is not declared under rbac")
		return
	}

	plaintext, hash, err := auth.Generate()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	key := &core.Key{
		Hash: hash, Alias: req.Alias, Models: req.Models,
		RPMLimit: req.RPMLimit, TPMLimit: req.TPMLimit,
		AllowPassthrough: req.AllowPassthrough,
		CreatedAt:        time.Now().UTC(), ExpiresAt: expiresAt,
		MaxBudget: req.MaxBudget, BudgetDuration: budgetDuration,
		Scope: req.Scope,
	}
	// Written before the key is stored rather than after, which is what makes a
	// failed audit write able to refuse the action: once the key is in the
	// store, refusing is no longer available. See internal/audit.
	ev := s.adminEvent(r, audit.Event{
		Action:     audit.ActionKeyGenerate,
		TargetKind: audit.TargetKey,
		Target:     key.Hash,
		Detail:     detail("alias", key.Alias, "scope", key.Scope),
	})
	if !s.recordAudit(w, r, ev) {
		return
	}
	if err := s.store.Put(r.Context(), key); err != nil {
		s.recordAuditFailure(r, ev, err)
		s.fail(w, r, err)
		return
	}

	s.log.Info("virtual key created", "alias", key.Alias, "hash", key.Hash)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"key":  plaintext,
		"info": key,
		"note": "This key is shown once and cannot be recovered. Store it now.",
	})
}

// handleKeyInfo returns metadata for one key, addressed by its hash.
func (s *Server) handleKeyInfo(w http.ResponseWriter, r *http.Request) {
	if !s.requireMaster(w, r) {
		return
	}
	s.keyInfo(w, r)
}

func (s *Server) keyInfo(w http.ResponseWriter, r *http.Request) {
	hash := r.URL.Query().Get("hash")
	if hash == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "the \"hash\" query parameter is required")
		return
	}
	key, err := s.store.Get(r.Context(), hash)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found_error", "no such key")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(key)
}

// handleKeyList returns every stored key's metadata.
func (s *Server) handleKeyList(w http.ResponseWriter, r *http.Request) {
	if !s.requireMaster(w, r) {
		return
	}
	s.keyList(w, r)
}

func (s *Server) keyList(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.List(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys, "count": len(keys)})
}

// handleKeyDelete revokes a key.
func (s *Server) handleKeyDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireMaster(w, r) {
		return
	}
	s.keyDelete(w, r)
}

func (s *Server) keyDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hash string `json:"hash"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || req.Hash == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "a \"hash\" field is required")
		return
	}
	ev := s.adminEvent(r, audit.Event{
		Action:     audit.ActionKeyDelete,
		TargetKind: audit.TargetKey,
		Target:     req.Hash,
	})
	if !s.recordAudit(w, r, ev) {
		return
	}
	if err := s.store.Delete(r.Context(), req.Hash); err != nil {
		s.recordAuditFailure(r, ev, err)
		s.fail(w, r, err)
		return
	}
	// Release the key's rate-limit counter and spend record; it can never be
	// used again, so retaining either would only leak memory and skew reports.
	s.auth.ForgetKey(r.Context(), req.Hash)
	// Both ledger implementations expose Forget; the interface deliberately does
	// not, since dropping a subject is an administrative action rather than
	// something the accounting path needs.
	if f, ok := s.ledger.(interface{ Forget(string) }); ok {
		f.Forget(req.Hash)
	}
	s.log.Info("virtual key deleted", "hash", req.Hash)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted", "hash": req.Hash})
}

// updateRequest is the body of POST /key/update.
//
// Every editable field is a pointer, so an omitted field means "leave this
// alone" and a present one means "set it to exactly this". A plain struct
// cannot express that difference: zero is a meaningful value for every one of
// these — an empty models list, an unlimited rpm, a cleared budget — and a
// partial update built on zero values would silently reset whatever the caller
// did not mention.
type updateRequest struct {
	Hash             string    `json:"hash"`
	Alias            *string   `json:"alias"`
	Models           *[]string `json:"models"`
	RPMLimit         *int      `json:"rpm_limit"`
	TPMLimit         *int      `json:"tpm_limit"`
	AllowPassthrough *bool     `json:"allow_passthrough"`
	// Blocked disables a key without deleting it, which is the difference
	// between suspending someone and losing their spend history: a deleted key
	// takes its ledger entry with it, a blocked one keeps answering "what did
	// this cost" while refusing to spend more.
	Blocked   *bool    `json:"blocked"`
	MaxBudget *float64 `json:"max_budget"`
	// BudgetDuration and Duration are Go duration strings. An empty string
	// clears the window and the expiry respectively, which is how a key is made
	// permanent again after having been given a lifetime.
	BudgetDuration *string `json:"budget_duration"`
	Duration       *string `json:"duration"`
	Scope          *string `json:"scope"`
}

// handleKeyUpdate edits a stored key in place.
//
// It exists because revoke-and-reissue is not an equivalent: a key's hash is
// what its spend, its budget window and its rate-limit counters are addressed
// by, so replacing a key to raise its budget resets the window it was spending
// against and hands its holder a new secret to install everywhere. Blocking,
// re-budgeting and re-scoping are all edits to a credential that stays the
// same credential.
func (s *Server) handleKeyUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireMaster(w, r) {
		return
	}
	s.keyUpdate(w, r)
}

func (s *Server) keyUpdate(w http.ResponseWriter, r *http.Request) {
	var req updateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "request body is not valid JSON")
		return
	}
	if req.Hash == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "a \"hash\" field is required")
		return
	}

	key, err := s.store.Get(r.Context(), req.Hash)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found_error", "no such key")
		return
	}

	// Edit a copy. The store hands back a pointer into its own state, and
	// mutating that before the request has been validated would leave a
	// rejected update half applied.
	updated := *key

	if req.Alias != nil {
		updated.Alias = *req.Alias
	}
	if req.Models != nil {
		updated.Models = *req.Models
	}
	if req.RPMLimit != nil {
		if *req.RPMLimit < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "rpm_limit must not be negative")
			return
		}
		updated.RPMLimit = *req.RPMLimit
	}
	if req.TPMLimit != nil {
		if *req.TPMLimit < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "tpm_limit must not be negative")
			return
		}
		updated.TPMLimit = *req.TPMLimit
	}
	if req.AllowPassthrough != nil {
		updated.AllowPassthrough = *req.AllowPassthrough
	}
	if req.Blocked != nil {
		updated.Blocked = *req.Blocked
	}
	if req.MaxBudget != nil {
		if *req.MaxBudget < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "max_budget must not be negative")
			return
		}
		updated.MaxBudget = *req.MaxBudget
	}
	if req.BudgetDuration != nil {
		d, err := parseOptionalDuration(*req.BudgetDuration)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error",
				"budget_duration must be empty or a positive Go duration string, for example \"720h\"")
			return
		}
		updated.BudgetDuration = d
	}
	// Checked against the merged key rather than the request, so raising a
	// budget to zero on a key that already has a window is refused for the same
	// reason setting both at once would be.
	if updated.BudgetDuration > 0 && updated.MaxBudget == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"budget_duration requires max_budget; a window with no cap limits nothing")
		return
	}
	if req.Duration != nil {
		d, err := parseOptionalDuration(*req.Duration)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error",
				"duration must be empty or a positive Go duration string, for example \"720h\"")
			return
		}
		if d == 0 {
			updated.ExpiresAt = nil
		} else {
			// Measured from now, not from the key's creation: an operator
			// extending a key means "another 30 days", and dating it from a
			// creation months ago would silently expire the key they just
			// renewed.
			t := time.Now().UTC().Add(d)
			updated.ExpiresAt = &t
		}
	}
	if req.Scope != nil {
		if *req.Scope != "" && s.cfg.Scope(*req.Scope) == nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error",
				"scope "+strconv.Quote(*req.Scope)+" is not declared under rbac")
			return
		}
		updated.Scope = *req.Scope
	}

	// Blocked is stated on every update rather than only when it changed: it is
	// the field that decides whether the credential still works, and a reader
	// scanning for when a key was suspended should not have to reconstruct that
	// from the absence of a mention.
	ev := s.adminEvent(r, audit.Event{
		Action:     audit.ActionKeyUpdate,
		TargetKind: audit.TargetKey,
		Target:     updated.Hash,
		Detail: detail("alias", updated.Alias, "scope", updated.Scope,
			"blocked", strconv.FormatBool(updated.Blocked)),
	})
	if !s.recordAudit(w, r, ev) {
		return
	}
	if err := s.store.Put(r.Context(), &updated); err != nil {
		s.recordAuditFailure(r, ev, err)
		s.fail(w, r, err)
		return
	}

	s.log.Info("virtual key updated", "alias", updated.Alias, "hash", updated.Hash,
		"blocked", updated.Blocked, "request_id", RequestIDFrom(r.Context()))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(&updated)
}

// parseOptionalDuration reads a Go duration string in which the empty string
// means "none" and anything non-positive is an error.
func parseOptionalDuration(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, errNotAPositiveDuration
	}
	return d, nil
}

var errNotAPositiveDuration = errors.New("not a positive duration")
