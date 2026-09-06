package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/erickardus/ai-gateway/internal/auth"
	"github.com/erickardus/ai-gateway/internal/cache"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/jsonx"
	"github.com/erickardus/ai-gateway/internal/metrics"
	"github.com/erickardus/ai-gateway/internal/promptcache"
	"github.com/erickardus/ai-gateway/internal/provider"
	"github.com/erickardus/ai-gateway/internal/router"
)

// streamUsageOptions is what a streamed OpenAI-compatible request needs in order
// to be billed at all. The field is inert on a non-streamed request, which is
// why it is only ever added to one that asked to stream.
var streamUsageOptions = []byte(`{"include_usage":true}`)

// pathMessages is Anthropic's inference endpoint, and pathCountTokens the
// endpoint that takes the same body to answer a different question. Both are
// served by serveInference below, and two of the things it does — asking the
// API to cache the conversation, and pinning the prefix — belong only to the
// one that actually runs the model.
const (
	pathMessages    = "/v1/messages"
	pathCountTokens = "/v1/messages/count_tokens"
)

// handleMessages serves the Anthropic Messages API.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.serveInference(w, r, pathMessages, core.FormatAnthropic)
}

// handleCountTokens serves Anthropic's token counting endpoint. It is optional
// for a gateway, but exposing it keeps Claude Code from spending an inference
// request to measure context.
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	s.serveInference(w, r, pathCountTokens, core.FormatAnthropic)
}

// handleChatCompletions serves the OpenAI Chat Completions API.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	s.serveInference(w, r, "/v1/chat/completions", core.FormatOpenAI)
}

// serveInference is the shared path for every inference endpoint: authenticate,
// read the body once, route, relay.
func (s *Server) serveInference(w http.ResponseWriter, r *http.Request, upstreamPath string, format core.Format) {
	ctx := r.Context()
	started := time.Now()
	obs := observation{}

	authCtx, err := s.auth.Authenticate(ctx, r.Header)
	if err != nil {
		s.reject(r, &obs, "unauthenticated", started)
		s.fail(w, r, err)
		return
	}
	obs.keyHash, obs.keyAlias = authCtx.Key.Hash, authCtx.Key.Alias

	body, err := s.readBody(r)
	if err != nil {
		s.reject(r, &obs, "body_too_large", started)
		s.fail(w, r, err)
		return
	}

	fields, err := jsonx.Peek(body)
	if err != nil {
		s.reject(r, &obs, "malformed_body", started)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "request body is not valid JSON")
		return
	}
	if fields.Model == "" {
		s.reject(r, &obs, "missing_model", started)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "request body is missing the \"model\" field")
		return
	}
	obs.model = fields.Model
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
		s.reject(r, &obs, "model_forbidden", started)
		s.fail(w, r, err)
		return
	}
	if !s.router.HasGroup(fields.Model) {
		s.reject(r, &obs, "model_unknown", started)
		s.fail(w, r, fmt.Errorf("model %q: %w", fields.Model, core.ErrModelNotFound))
		return
	}

	// A cache hit calls no upstream, so it is served before rate limits and
	// budgets are charged: it consumes neither provider capacity nor money, and
	// billing for it would be charging twice for one answer.
	cacheKey, cacheable := s.cacheKeyFor(authCtx, format, fields.Model, body)
	if cacheable {
		if entry, hit, err := s.cache.Get(ctx, cacheKey); err != nil {
			s.log.Warn("cache lookup failed", "error", err, "request_id", RequestIDFrom(ctx))
		} else if hit {
			obs.deployment, obs.outcome = "cache", metrics.OutcomeCacheHit
			obs.usage, obs.latency = entry.Usage, time.Since(started)
			s.serveFromCache(w, entry, fields.Model, format)
			s.metrics.Observe(obs.toResult(0))
			return
		}
	}

	// Refuse a key that has already spent its budget before incurring more cost.
	if err := s.auth.CheckBudget(ctx, authCtx, s.ledger); err != nil {
		s.reject(r, &obs, "budget_exceeded", started)
		s.fail(w, r, err)
		return
	}

	// Charge the key's rate limit only now that the request is known to be one
	// the gateway will actually dispatch.
	if err := s.auth.Admit(authCtx); err != nil {
		s.reject(r, &obs, "rate_limited", started)
		s.fail(w, r, err)
		return
	}

	overrides := router.OverridesFromHeaders(r.Header)
	overrides.AllowPassthrough = authCtx.Key != nil && authCtx.Key.AllowPassthrough
	overrides.PromptPrefix, overrides.PromptPinTTL = s.promptPrefix(format, fields, upstreamPath != pathCountTokens)
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

	// Everything below annotates the request for the gateway's own benefit
	// rather than the caller's. The body as it stood before is kept so a request
	// an upstream rejects because of an annotation can be served without it: the
	// shapes a breakpoint or a stream option may legally be attached to differ
	// between providers and change over time, and a gateway that guessed wrong
	// would otherwise turn an optimization into a failed request.
	unannotated := body
	annotated := false

	// Ask an OpenAI-compatible upstream to report usage on a streamed reply.
	// Without stream_options.include_usage the final chunk carries no usage at
	// all, so the request is billed as nothing: no cost, no budget charge, no
	// rate-limit tokens, and a prompt cache whose reads are invisible. A caller
	// that set stream_options itself is left alone — AddMember reports the
	// member already present and changes no byte — since it has said what it
	// wants and the gateway's accounting is not worth overriding it for.
	if format == core.FormatOpenAI && fields.Stream && s.cfg.Observability.StreamUsageEnabled() {
		asked, added, err := jsonx.AddMember(body, "stream_options", streamUsageOptions)
		if err != nil {
			// As with injection below: a request the gateway could not annotate
			// is forwarded as it arrived rather than refused.
			s.log.Warn("stream usage request skipped", "error", err, "request_id", RequestIDFrom(ctx))
		} else if added {
			body, annotated = asked, true
		}
	}

	// Mark the cacheable prefix last, once every gateway directive has been
	// stripped: injection edits the body, and editing one that is about to
	// change again would place a breakpoint against bytes the upstream never
	// sees. Configuration refuses injection alongside any passthrough
	// deployment, so no rewritten body can reach the path that must forward one
	// unchanged.
	if s.cfg.PromptCache.Inject && format == core.FormatAnthropic {
		// The top-level breakpoint is Anthropic's automatic caching, a field of
		// the Messages request. Token counting takes the same body but answers a
		// different question, and asking it to cache anything is at best inert,
		// so only the inference path carries it.
		injected, changed, err := promptcache.Inject(body, s.cfg.PromptCache.InjectMinBytes, upstreamPath == pathMessages)
		if err != nil {
			// The request is forwarded exactly as it arrived. An optimization
			// that could not be applied is not a reason to refuse a request.
			s.log.Warn("prompt cache injection skipped", "error", err, "request_id", RequestIDFrom(ctx))
		} else if changed {
			body, annotated = injected, true
		}
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
	if err != nil && annotated && rejectedAsBadRequest(err) {
		// An upstream that refuses the annotated body is refusing something the
		// caller never asked for, so it gets one more chance with the request as
		// it arrived. Anything still failing is the caller's own 400 and is
		// relayed as such.
		s.warnAnnotationRejected(err)
		req.Body = unannotated
		result, err = s.router.Route(ctx, fields.Model, req, overrides)
	}
	if err != nil {
		obs.outcome = metrics.OutcomeGateway
		var upstream *core.UpstreamError
		if errors.As(err, &upstream) {
			obs.outcome, obs.deployment = metrics.OutcomeUpstream, upstream.Deployment
		}
		obs.latency = time.Since(started)
		s.record(r, obs)
		s.fail(w, r, err)
		return
	}
	defer result.Response.Body.Close()

	obs.deployment = result.Deployment.ID()
	obs.format, obs.streaming = format, fields.Stream
	obs.retries, obs.fallbacks = result.AttemptedRetries, result.AttemptedFallback
	obs.promptAffinity = result.PromptAffinity
	s.metrics.InFlightAdd(obs.model, obs.deployment, 1)
	defer s.metrics.InFlightAdd(obs.model, obs.deployment, -1)

	provider.SanitizeResponseHeaders(w.Header(), result.Response.Header)
	if cacheable {
		w.Header().Set(cacheHeader, "miss")
	}
	w.Header().Set("x-gateway-model-id", fields.Model)
	w.Header().Set("x-gateway-deployment", result.Deployment.ID())
	w.Header().Set("x-gateway-attempted-retries", strconv.Itoa(result.AttemptedRetries))
	w.Header().Set("x-gateway-attempted-fallbacks", strconv.Itoa(result.AttemptedFallback))
	if result.PromptAffinity != router.AffinityOff {
		// Whether the request reached the deployment holding its warm prefix is
		// visible per request, not only in aggregate: a cache-affinity problem
		// is otherwise only discoverable from a bill.
		w.Header().Set(promptAffinityHeader, result.PromptAffinity)
	}
	w.WriteHeader(result.Response.StatusCode)

	// Tee the relay when the response is a candidate for caching. The client
	// still receives every chunk as it arrives; the copy is only written to the
	// cache once the stream completes successfully.
	var relayTarget http.ResponseWriter = w
	var tee *teeWriter
	if cacheable && s.cacheable(result.Response.StatusCode, nil) {
		tee = &teeWriter{ResponseWriter: w, capture: &bytes.Buffer{}, limit: s.cfg.Cache.MaxEntryBytes}
		relayTarget = tee
	}

	usage, relayErr := provider.Relay(relayTarget, result.Response.Body, format)
	if relayErr != nil {
		// The status line is already sent, so the only thing left is to record
		// what happened.
		s.log.Warn("relay interrupted",
			"request_id", RequestIDFrom(ctx),
			"deployment", result.Deployment.ID(),
			"error", relayErr)
	}
	// Detached for the same reason as record: a client that disconnected must
	// still have its usage counted against the deployment and its key.
	s.router.RecordUsage(context.WithoutCancel(ctx), result.Deployment, usage)
	// Pin the prefix only now: whether this deployment holds a warm copy of it
	// is something only the response says.
	s.router.RecordPrefix(context.WithoutCancel(ctx), result, usage)
	if authCtx.Key != nil {
		s.auth.Limiter().AddTokens(authCtx.Key.Hash, usage.Total())
	}

	// Store only a stream that completed. A truncated response replayed for the
	// TTL would hand every later caller the same broken answer.
	if tee != nil && relayErr == nil && !tee.Overflowed() {
		entry := &cache.Entry{
			Status: result.Response.StatusCode,
			Header: cacheableHeaders(result.Response.Header),
			Body:   tee.capture.Bytes(),
			Usage:  usage,
			// SSE is replayed through the relay path rather than written whole.
			Streaming: fields.Stream,
			StoredAt:  time.Now().UTC(),
		}
		if err := s.cache.Put(context.WithoutCancel(ctx), cacheKey, entry, s.cfg.Cache.TTL); err != nil {
			s.log.Warn("cache store failed", "error", err, "request_id", RequestIDFrom(ctx))
		}
	}

	obs.usage = usage
	obs.outcome = metrics.OutcomeSuccess
	if relayErr != nil {
		obs.outcome = metrics.OutcomeUpstream
	}
	obs.latency = time.Since(started)
	s.record(r, obs)
}

// promptPrefix fingerprints the cacheable prefix of a request and says how long
// a pin on it should live, or returns empty when nothing would come of pinning
// it at all.
//
// Fingerprinting is skipped where it cannot pay: with affinity off, or in a
// group that has nothing to choose between, hashing the system blocks and tool
// definitions of every request would cost real time for a preference with no
// decision to make.
//
// inference says whether this endpoint actually runs the model, and token
// counting is the one that does not. It takes the same body as an inference
// request and warms nothing, so a pin taken from it names a deployment holding
// no warm prefix, and refreshing an existing pin from it keeps a conversation
// pointed at an upstream on the strength of a request that never reached the
// model. Claude Code counts tokens on most turns, so this is the ordinary case
// rather than an edge.
//
// The lifetime is the caller's rather than the operator's wherever the caller
// declared one. A conversation using the one-hour cache holds an entry that
// outlives a five-minute pin, and the turn that arrives after the pin lapses
// would write that entry again somewhere else, at twice base input.
func (s *Server) promptPrefix(format core.Format, fields jsonx.Fields, inference bool) (string, time.Duration) {
	if !inference || !s.cfg.PromptCache.AffinityEnabled() || !s.pinnable[fields.Model] {
		return "", 0
	}
	fingerprint, ok := promptcache.Fingerprint(format, fields.Model, fields)
	if !ok {
		return "", 0
	}
	if promptcache.DeclaresLongCacheTTL(fields) {
		return fingerprint, promptcache.LongCacheLifetime
	}
	return fingerprint, 0
}

// reject records a request refused before dispatch. Such a request consumed no
// upstream capacity, so it carries a reason rather than a deployment.
func (s *Server) reject(r *http.Request, obs *observation, reason string, started time.Time) {
	obs.outcome = metrics.OutcomeRejected
	obs.rejectReason = reason
	obs.latency = time.Since(started)
	s.metrics.Observe(obs.toResult(0))
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
	case errors.Is(err, core.ErrBudgetExceeded):
		// Say what happened without disclosing the figures, which belong to the
		// operator rather than the caller.
		return "budget_error", "this key has exhausted its budget for the current window"
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

// rejectedAsBadRequest reports whether an upstream refused the request outright,
// which is the answer an annotation it does not accept produces.
func rejectedAsBadRequest(err error) bool {
	var upstream *core.UpstreamError
	return errors.As(err, &upstream) && upstream.StatusCode == http.StatusBadRequest
}

// warnAnnotationRejected reports, once per deployment, that an upstream refused
// a body the gateway had annotated.
//
// The request itself is retried without the annotation, so nothing is lost but
// one round trip — and that round trip is paid on every request until the
// configuration changes, which is why it is worth a line naming the deployment.
// The likely causes are an upstream that predates automatic caching, such as the
// legacy Bedrock integration, or an OpenAI-compatible server strict about fields
// it does not recognize.
func (s *Server) warnAnnotationRejected(err error) {
	var upstream *core.UpstreamError
	if !errors.As(err, &upstream) {
		return
	}
	if _, seen := s.rejectedAnnotation.LoadOrStore(upstream.Deployment, true); seen {
		return
	}
	s.log.Warn("upstream rejected an annotated request; retrying without the annotation costs a round trip on every request until it is turned off",
		"deployment", upstream.Deployment,
		"remedy", "unset prompt_cache.inject or observability.stream_usage for this deployment's group")
}
