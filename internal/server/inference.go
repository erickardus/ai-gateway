package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/erickardus/ai-gateway/internal/translate"
)

// pathMessages is Anthropic's inference endpoint, and pathCountTokens the
// endpoint that takes the same body to answer a different question. Both are
// served by serveInference below, and two of the things it does — asking the
// API to cache the conversation, and pinning the prefix — belong only to the
// one that actually runs the model.
const (
	pathMessages    = "/v1/messages"
	pathCountTokens = "/v1/messages/count_tokens"
)

// translatedHeader names the format pair a translated request crossed, as
// "openai->anthropic". It is set only where translation actually happened.
//
// A caller can otherwise not tell: the whole point of a translated reply is
// that it is indistinguishable from a native one, which makes a fidelity
// problem impossible to attribute from the client side. This is the header that
// says "this answer was rebuilt" before anyone has to guess.
const translatedHeader = "x-gateway-translated"

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
		s.reject(r, &obs, rejectUnauthenticated, started)
		s.failAs(w, r, err, format)
		return
	}
	// SpendSubject rather than Hash: an SSO key is reissued over its owner's
	// life, and billing each reissue separately would reset their budget.
	obs.keyHash, obs.keyAlias = authCtx.Key.SpendSubject(), authCtx.Key.Alias
	// The pools this caller draws on. Resolved here rather than at accounting
	// time because the key that named them is in hand now, and the request may
	// yet fail in a way that still has to be charged to them.
	obs.scopes, obs.scopeAliases = authCtx.ScopeSubjects(), authCtx.ScopeAliases()

	body, err := s.readBody(r)
	if err != nil {
		s.reject(r, &obs, "body_too_large", started)
		s.failAs(w, r, err, format)
		return
	}
	obs.requestBytes = int64(len(body))

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
		s.failAs(w, r, err, format)
		return
	}
	if !s.router.HasGroup(fields.Model) {
		s.reject(r, &obs, "model_unknown", started)
		s.failAs(w, r, fmt.Errorf("model %q: %w", fields.Model, core.ErrModelNotFound), format)
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
			obs.responseBytes = int64(len(entry.Body))
			s.metrics.RecordResponseCache(fields.Model, metrics.OutcomeHit)
			s.serveFromCache(w, entry, fields.Model, format)
			s.metrics.Observe(obs.toResult(0, 0, 0))
			// recordTraffic rather than record: a hit is a served request and
			// belongs in the traffic view, but obs.deployment is the sentinel
			// "cache" and record would take that for a real upstream and write
			// a ledger entry for an answer nobody was billed for.
			s.recordTraffic(r, obs, 0, 0, false)
			return
		}
		s.metrics.RecordResponseCache(fields.Model, metrics.OutcomeMiss)
	}

	// Refuse a key that has already spent its budget before incurring more cost.
	if err := s.auth.CheckBudget(ctx, authCtx, s.ledger); err != nil {
		s.reject(r, &obs, "budget_exceeded", started)
		s.failAs(w, r, err, format)
		return
	}

	// Charge the key's rate limit only now that the request is known to be one
	// the gateway will actually dispatch.
	if err := s.auth.Admit(ctx, authCtx); err != nil {
		s.reject(r, &obs, "rate_limited", started)
		s.failAs(w, r, err, format)
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

	// Body is now the request as the caller wrote it, less the gateway
	// directives that must never reach an upstream. Everything the gateway adds
	// for its own benefit — a cache breakpoint so there is something to cache, a
	// usage option so a streamed reply can be billed — is added per deployment
	// instead, once routing has chosen one.
	//
	// It has to be. Which annotation an upstream will take is a fact about that
	// upstream: a passthrough deployment must receive the caller's bytes
	// untouched, breakpoints are an Anthropic construct, and a server may simply
	// refuse a field it does not recognize. Deciding here, where the group is
	// still a set of candidates, would mean annotating all of them or none.
	req := &provider.Request{
		Path:  upstreamPath,
		Query: r.URL.RawQuery,
		Body:  body,
		// The third argument says this endpoint actually runs the model, which
		// is what gives Anthropic's automatic caching a conversation to follow.
		// Token counting is the one endpoint that does not: it takes the same
		// body to answer a different question and warms nothing. It is written
		// as "not token counting" rather than "is /v1/messages" because a chat
		// completion runs the model too, and once translation can send one to
		// an Anthropic upstream, naming the endpoint instead of the property
		// silently withholds the conversation breakpoint from exactly those
		// requests — the growing history, which is the expensive half.
		Annotate: s.annotatorFor(format, fields.Stream, upstreamPath != pathCountTokens, RequestIDFrom(ctx)),
		Format:   format,
		Header:   r.Header,
		Creds:    authCtx.Credentials,
		Stream:   fields.Stream,
	}

	// An upstream that refuses an annotated body is retried with the request as
	// it arrived by the attempt that annotated it, which is the only place that
	// knows which deployment refused and can stop annotating it.
	result, err := s.router.Route(ctx, fields.Model, req, overrides)
	if err != nil {
		obs.outcome = metrics.OutcomeGateway
		var upstream *core.UpstreamError
		if errors.As(err, &upstream) {
			obs.outcome, obs.deployment = metrics.OutcomeUpstream, upstream.Deployment
		}
		obs.latency = time.Since(started)
		s.record(r, obs)
		s.failAs(w, r, err, format)
		return
	}
	defer result.Response.Body.Close()

	obs.deployment = result.Deployment.ID()
	obs.format, obs.streaming = format, fields.Stream
	obs.retries, obs.fallbacks = result.AttemptedRetries, result.AttemptedFallback
	obs.promptAffinity = result.PromptAffinity
	obs.statusClass = statusClass(result.Response.StatusCode)
	s.metrics.InFlightAdd(obs.model, obs.deployment, 1)
	defer s.metrics.InFlightAdd(obs.model, obs.deployment, -1)

	// A deployment speaking another wire format answered in that format, so the
	// reply has to be rewritten on its way out.
	//
	// A non-streamed one is rewritten here, before a single response header is
	// written. There is no way to rewrite half a JSON document, so it has to be
	// held whole in any case — and holding it before the status line is
	// committed is what lets a reply this gateway cannot convert be reported as
	// a gateway error, rather than reaching the caller as a 200 carrying a
	// document their own format does not define. A streamed reply cannot be
	// treated this way and is converted event by event during the relay: the
	// status is sent with the first chunk, and buffering it would turn every
	// long generation into the 300-second silence Claude Code aborts on.
	obs.upstreamFormat = result.Response.Format
	upstreamBody := io.Reader(result.Response.Body)
	if result.Response.Format != format && !fields.Stream {
		rewritten, terr := translate.ReadResponse(result.Response.Format, format, result.Response.Body)
		if terr != nil {
			obs.outcome = metrics.OutcomeGateway
			obs.latency = time.Since(started)
			s.record(r, obs)
			s.log.Error("upstream reply could not be translated for the caller",
				"request_id", RequestIDFrom(ctx),
				"deployment", result.Deployment.ID(),
				"from", result.Response.Format, "to", format,
				"error", terr)
			writeError(w, http.StatusBadGateway, "api_error",
				"the upstream replied in a format the gateway could not translate for this request")
			return
		}
		upstreamBody = bytes.NewReader(rewritten)
	}

	provider.SanitizeResponseHeaders(w.Header(), result.Response.Header)
	// The upstream's own Content-Length describes a document that has just been
	// rebuilt into a different one and would truncate it. The header naming the
	// pair is what makes a translated request identifiable from the client side
	// rather than only from the gateway's logs.
	if result.Response.Format != format {
		w.Header().Del("Content-Length")
		w.Header().Set(translatedHeader, string(result.Response.Format)+"->"+string(format))
	}
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

	// The relay reads the reply in the caller's own format, whether it was
	// rewritten above or is being rewritten event by event here. Everything
	// downstream — the usage sniffer, the response cache — therefore sees
	// exactly what the client sees, which is deliberate: the translator
	// restates the upstream's token counters under the caller's format's
	// convention, so the reply the client reads and the figure the gateway
	// bills come from one document rather than from two parses that could
	// disagree.
	if result.Response.Format != format && fields.Stream {
		upstreamBody = translate.Stream(result.Response.Format, format, result.Response.Body)
	}
	relayed, relayErr := provider.Relay(relayTarget, upstreamBody, format)
	usage := relayed.Usage
	obs.responseBytes = relayed.Bytes
	// Time to first token is measured from when the caller's request arrived,
	// not from when the relay began: the queueing, routing and retrying that
	// happened in between is latency the caller waited through.
	if !relayed.FirstChunkAt.IsZero() {
		obs.timeToFirstToken = relayed.FirstChunkAt.Sub(started)
	}
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
		s.auth.RecordTokens(context.WithoutCancel(ctx), authCtx, usage.Total())
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
	obs.streamCompleted = relayErr == nil
	if relayErr != nil {
		obs.outcome = metrics.OutcomeUpstream
	}
	obs.latency = time.Since(started)
	// Throughput is measured after the first chunk so it describes generation
	// rather than the queueing that preceded it — the two move independently,
	// and averaging them together hides both.
	//
	// Only for a streamed reply. A single-shot response's "first chunk" is its
	// whole body, so the interval after it is the time to write one buffer —
	// microseconds — and dividing output tokens by that reports millions of
	// tokens per second into a histogram whose largest bucket is 1000. One such
	// observation moves p99 permanently.
	if obs.streaming && !relayed.FirstChunkAt.IsZero() && usage.OutputTokens > 0 {
		if generating := time.Since(relayed.FirstChunkAt).Seconds(); generating > 0 {
			obs.throughput = float64(usage.OutputTokens) / generating
		}
	}
	s.record(r, obs)
}

// statusClass reduces an upstream status to its class. The full code would put
// an unbounded dimension on a metric — a provider is free to invent one — where
// the class is what an alert actually keys on: a 5xx is the upstream's fault, a
// 4xx is the request's.
func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	default:
		return ""
	}
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
//
// It does not go through record: there is no spend to account for, and a ledger
// entry with no deployment would be a charge against a request that never made
// one. It does reach the traffic buffer, because a refusal is the thing an
// operator most often needs to see one of — a budget exhausted, a key blocked, a
// model that is not in the group they think it is — and the counter that already
// tallies rejections cannot say whose.
func (s *Server) reject(r *http.Request, obs *observation, reason string, started time.Time) {
	obs.outcome = metrics.OutcomeRejected
	obs.rejectReason = reason
	obs.latency = time.Since(started)
	s.metrics.Observe(obs.toResult(0, 0, 0))

	// An unauthenticated refusal is counted but not kept.
	//
	// It is the one rejection decided before any credential is verified, so it
	// is reachable by anyone who can open a socket — and the ring is a fixed
	// thousand entries shared with the traffic an operator opened the page to
	// read. Recording it would let an anonymous client evict the whole buffer
	// in well under a second. The rejection counter still tallies these by
	// reason, so the signal survives; only the per-request record does not.
	if reason != rejectUnauthenticated {
		s.recordTraffic(r, *obs, 0, 0, false)
	}
}

// rejectUnauthenticated names the refusal that precedes authentication, and so
// precedes knowing who to attribute it to.
const rejectUnauthenticated = "unauthenticated"

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

// fail writes an error response for a caller whose format is not known, which
// is every endpoint but inference.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.failAs(w, r, err, "")
}

// failAs writes an error response, relaying an upstream error verbatim when
// there is one.
//
// Relaying matters: Claude Code inspects the upstream's own error wording to
// decide whether to retry with a capability disabled. A gateway that rewrapped
// those errors in its own envelope would break that recovery path even while
// preserving the status code.
//
// A translated request is the one case where something has to change, and only
// the envelope does. The refusal came back in the upstream's format, and a
// client handed an envelope its own format does not define finds no message in
// it at all — so it reads a refusal it could have acted on as a corrupt reply.
// The message text inside is carried across byte for byte, which is what that
// matching depends on.
func (s *Server) failAs(w http.ResponseWriter, r *http.Request, err error, format core.Format) {
	var upstream *core.UpstreamError
	if errors.As(err, &upstream) {
		provider.SanitizeResponseHeaders(w.Header(), upstream.Header)
		w.Header().Del("Content-Length")
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		body := upstream.Body
		if format != "" && upstream.Format != "" && upstream.Format != format {
			body = translate.Error(upstream.Format, format, body)
			w.Header().Set(translatedHeader, string(upstream.Format)+"->"+string(format))
		}
		w.WriteHeader(upstream.StatusCode)
		if len(body) > 0 {
			_, _ = w.Write(body)
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
		// Which level withheld it, where a scope is what did. "This key is not
		// permitted" is true either way but leaves a caller whose key does list
		// the model with nowhere to go: widening the key's allowlist, the
		// obvious next step, changes nothing.
		var scoped *core.ScopeModelError
		if errors.As(err, &scoped) {
			return "permission_error", string(scoped.Kind) + " " + scoped.ID +
				" is not permitted to call the requested model"
		}
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
		// operator rather than the caller. Which subject ran out is not a figure
		// and is the difference between an actionable message and a misleading
		// one: a developer refused by their team's pool has not exhausted
		// anything of their own, and telling them they have sends them to ask
		// the wrong person for more.
		var scoped *core.ScopeBudgetError
		if errors.As(err, &scoped) {
			return "budget_error", "the shared budget for " + string(scoped.Kind) + " " +
				scoped.ID + " is exhausted for the current window"
		}
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
