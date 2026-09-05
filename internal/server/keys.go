package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/erickardus/ai-gateway/internal/auth"
	"github.com/erickardus/ai-gateway/internal/core"
)

// keyView is the safe representation of a key. It never carries the plaintext,
// which exists only in the response that first issued it.
type keyView struct {
	Hash             string     `json:"hash"`
	Alias            string     `json:"alias,omitempty"`
	Models           []string   `json:"models,omitempty"`
	RPMLimit         int        `json:"rpm_limit,omitempty"`
	TPMLimit         int        `json:"tpm_limit,omitempty"`
	AllowPassthrough bool       `json:"allow_passthrough"`
	Blocked          bool       `json:"blocked"`
	CreatedAt        time.Time  `json:"created_at"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
}

func viewOf(k *core.Key) keyView {
	return keyView{
		Hash: k.Hash, Alias: k.Alias, Models: k.Models,
		RPMLimit: k.RPMLimit, TPMLimit: k.TPMLimit,
		AllowPassthrough: k.AllowPassthrough, Blocked: k.Blocked,
		CreatedAt: k.CreatedAt, ExpiresAt: k.ExpiresAt,
	}
}

// requireMaster gates the management endpoints on the master key. It reports
// whether the request may proceed.
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
}

// handleKeyGenerate mints a new virtual key. The plaintext is returned exactly
// once here and never stored.
func (s *Server) handleKeyGenerate(w http.ResponseWriter, r *http.Request) {
	if !s.requireMaster(w, r) {
		return
	}

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
	}
	if err := s.store.Put(r.Context(), key); err != nil {
		s.fail(w, r, err)
		return
	}

	s.log.Info("virtual key created", "alias", key.Alias, "hash", key.Hash)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"key":  plaintext,
		"info": viewOf(key),
		"note": "This key is shown once and cannot be recovered. Store it now.",
	})
}

// handleKeyInfo returns metadata for one key, addressed by its hash.
func (s *Server) handleKeyInfo(w http.ResponseWriter, r *http.Request) {
	if !s.requireMaster(w, r) {
		return
	}
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
	_ = json.NewEncoder(w).Encode(viewOf(key))
}

// handleKeyList returns every stored key's metadata.
func (s *Server) handleKeyList(w http.ResponseWriter, r *http.Request) {
	if !s.requireMaster(w, r) {
		return
	}
	keys, err := s.store.List(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	views := make([]keyView, 0, len(keys))
	for _, k := range keys {
		views = append(views, viewOf(k))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": views, "count": len(views)})
}

// handleKeyDelete revokes a key.
func (s *Server) handleKeyDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireMaster(w, r) {
		return
	}
	var req struct {
		Hash string `json:"hash"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || req.Hash == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "a \"hash\" field is required")
		return
	}
	if err := s.store.Delete(r.Context(), req.Hash); err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("virtual key deleted", "hash", req.Hash)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted", "hash": req.Hash})
}
