package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/erickardus/ai-gateway/internal/auth"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/jsonx"
	"github.com/erickardus/ai-gateway/internal/provider"
	"github.com/erickardus/ai-gateway/internal/router"
)

// handleMessages serves the Anthropic Messages API.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.serveInference(w, r, "/v1/messages", core.FormatAnthropic)
}

// handleCountTokens serves Anthropic's token counting endpoint. It is optional
// for a gateway, but exposing it keeps Claude Code from spending an inference
// request to measure context.
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	s.serveInference(w, r, "/v1/messages/count_tokens", core.FormatAnthropic)
}

// handleChatCompletions serves the OpenAI Chat Completions API.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	s.serveInference(w, r, "/v1/chat/completions", core.FormatOpenAI)
}

// serveInference is the shared path for every inference endpoint: authenticate,
// read the body once, route, relay.
func (s *Server) serveInference(w http.ResponseWriter, r *http.Request, upstreamPath string, format core.Format) {
	ctx := r.Context()

	authCtx, err := s.auth.Authenticate(ctx, r.Header)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	body, err := s.readBody(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	fields, err := jsonx.Peek(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "request body is not valid JSON")
		return
	}
	if fields.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "request body is missing the \"model\" field")
		return
	}
	// Annotate the access log with who is calling and what they asked for. The
	// alias is used where a key has one; otherwise a redacted fingerprint, which
	// identifies the key across log lines without ever exposing it.
	if authCtx.Key != nil {
		label := authCtx.Key.Alias
		if label == "" {
			label = auth.Redact(authCtx.Credentials.Key)
		}
		AnnotateRequest(ctx, label, fields.Model)
	}

	if err := s.auth.AuthorizeModel(authCtx, fields.Model); err != nil {
		s.fail(w, r, err)
		return
	}
	if !s.router.HasGroup(fields.Model) {
		s.fail(w, r, fmt.Errorf("model %q: %w", fields.Model, core.ErrModelNotFound))
		return
	}

	// Charge the key's rate limit only now that the request is known to be one
	// the gateway will actually dispatch.
	if err := s.auth.Admit(authCtx); err != nil {
		s.fail(w, r, err)
		return
	}

	overrides := router.OverridesFromHeaders(r.Header)
	overrides.AllowPassthrough = authCtx.Key != nil && authCtx.Key.AllowPassthrough
	if fields.HasDisableFallbacks {
		overrides.DisableFallbacks = fields.DisableFallbacks
		// It is a gateway directive, not part of the provider's schema, so it
		// must not reach the upstream.
		stripped, err := jsonx.DeleteTopLevelKey(body, "disable_fallbacks")
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "could not process disable_fallbacks")
			return
		}
		body = stripped
	}

	req := &provider.Request{
		Path:   upstreamPath,
		Query:  r.URL.RawQuery,
		Body:   body,
		Format: format,
		Header: r.Header,
		Creds:  authCtx.Credentials,
		Stream: fields.Stream,
	}

	result, err := s.router.Route(ctx, fields.Model, req, overrides)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer result.Response.Body.Close()

	provider.SanitizeResponseHeaders(w.Header(), result.Response.Header)
	w.Header().Set("x-gateway-model-id", fields.Model)
	w.Header().Set("x-gateway-deployment", result.Deployment.ID())
	w.Header().Set("x-gateway-attempted-retries", strconv.Itoa(result.AttemptedRetries))
	w.Header().Set("x-gateway-attempted-fallbacks", strconv.Itoa(result.AttemptedFallback))
	w.WriteHeader(result.Response.StatusCode)

	usage, relayErr := provider.Relay(w, result.Response.Body)
	if relayErr != nil {
		// The status line is already sent, so the only thing left is to record
		// what happened.
		s.log.Warn("relay interrupted",
			"request_id", RequestIDFrom(ctx),
			"deployment", result.Deployment.ID(),
			"error", relayErr)
	}
	s.router.RecordUsage(ctx, result.Deployment, usage)
	if authCtx.Key != nil {
		s.auth.Limiter().AddTokens(authCtx.Key.Hash, usage.Total())
	}
}

// readBody reads the request body under the configured size limit.
func (s *Server) readBody(r *http.Request) ([]byte, error) {
	limited := http.MaxBytesReader(nil, r.Body, s.cfg.Server.MaxBodyBytes)

	// Presize from Content-Length where the client supplied a credible one:
	// io.ReadAll otherwise starts at 512 bytes and repeatedly reallocates,
	// churning several times the body size in garbage on every request.
	var buf bytes.Buffer
	if n := r.ContentLength; n > 0 && n <= s.cfg.Server.MaxBodyBytes {
		buf.Grow(int(n) + bytes.MinRead)
	}
	if _, err := buf.ReadFrom(limited); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, fmt.Errorf("body exceeds %d bytes: %w", s.cfg.Server.MaxBodyBytes, core.ErrBodyTooLarge)
		}
		return nil, fmt.Errorf("read request body: %w", err)
	}
	return buf.Bytes(), nil
}

// fail writes an error response, relaying an upstream error verbatim when there
// is one.
//
// Relaying matters: Claude Code inspects the upstream's own error wording to
// decide whether to retry with a capability disabled. A gateway that rewrapped
// those errors in its own envelope would break that recovery path even while
// preserving the status code.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var upstream *core.UpstreamError
	if errors.As(err, &upstream) {
		provider.SanitizeResponseHeaders(w.Header(), upstream.Header)
		w.Header().Del("Content-Length")
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(upstream.StatusCode)
		if len(upstream.Body) > 0 {
			_, _ = w.Write(upstream.Body)
		}
		s.log.Warn("upstream error relayed",
			"request_id", RequestIDFrom(r.Context()),
			"deployment", upstream.Deployment,
			"status", upstream.StatusCode)
		return
	}

	status := core.StatusFor(err)
	kind, message := classify(err, status)
	if status >= 500 {
		s.log.Error("request failed", "request_id", RequestIDFrom(r.Context()), "error", err)
	} else {
		s.log.Warn("request rejected", "request_id", RequestIDFrom(r.Context()), "error", err)
	}
	writeError(w, status, kind, message)
}

// classify turns an internal error into a client-safe type and message. Internal
// failures are deliberately opaque so implementation detail cannot leak.
func classify(err error, status int) (kind, message string) {
	switch {
	case errors.Is(err, core.ErrKeyInvalid):
		return "authentication_error", "invalid or missing virtual key"
	case errors.Is(err, core.ErrKeyBlocked):
		return "permission_error", "virtual key is blocked or expired"
	case errors.Is(err, core.ErrModelNotAllowed):
		return "permission_error", "this key is not permitted to call the requested model"
	case errors.Is(err, core.ErrPassthroughNotAllowed):
		return "permission_error", "this key is not permitted to use credential passthrough"
	case errors.Is(err, core.ErrUpstreamHostNotAllowed):
		return "permission_error", "the deployment's upstream host is not allowlisted"
	case errors.Is(err, core.ErrModelNotFound):
		return "not_found_error", "no such model"
	case errors.Is(err, core.ErrRateLimited):
		return "rate_limit_error", "rate limit exceeded"
	case errors.Is(err, core.ErrNoHealthyDeployment):
		return "api_error", "no healthy deployment available for this model"
	case errors.Is(err, core.ErrBodyTooLarge):
		return "invalid_request_error", "request body too large"
	}
	if status >= 500 {
		return "api_error", "internal server error"
	}
	return "invalid_request_error", "request could not be processed"
}

// errorEnvelope mirrors the Anthropic error shape, which Anthropic-format
// clients already know how to read.
type errorEnvelope struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, kind, message string) {
	var env errorEnvelope
	env.Type = "error"
	env.Error.Type = kind
	env.Error.Message = message

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(env)
}

// authHeaderNames is used by the key endpoints to find a presented credential.
func (s *Server) authHeaderNames() []string { return s.auth.HeaderNames() }
