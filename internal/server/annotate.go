package server

import (
	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/jsonx"
	"github.com/erickardus/ai-gateway/internal/promptcache"
	"github.com/erickardus/ai-gateway/internal/provider"
)

// streamUsageOptions is what a streamed OpenAI-compatible request needs in order
// to be billed at all. The field is inert on a non-streamed request, which is
// why it is only ever added to one that asked to stream.
var streamUsageOptions = []byte(`{"include_usage":true}`)

// annotationLevel is how much of what the gateway would add a deployment has
// been shown to accept. A deployment starts at annotateFull and is demoted one
// step each time it refuses, so a wrong guess costs the cheapest thing first.
//
// The order is by what losing each annotation costs. A cache breakpoint is an
// optimization: losing it means paying full price for a prefix that repeats.
// The streamed usage option is not an optimization at all — without it an
// OpenAI-compatible reply carries no usage anywhere, so the request is billed as
// nothing and charges nothing against a budget. Answering one refused breakpoint
// by also dropping the accounting would trade a smaller failure for a larger
// silent one.
type annotationLevel int

const (
	// annotateNone sends every deployment the request as it arrived.
	annotateNone annotationLevel = iota
	// annotateEssential adds only what billing depends on.
	annotateEssential
	// annotateFull adds everything the deployment's format allows.
	annotateFull
)

// annotator decides what each deployment receives of the annotations the gateway
// adds for its own benefit: a cache breakpoint, so an Anthropic upstream has
// something to cache, and a usage option, so a streamed OpenAI-compatible reply
// can be billed at all.
//
// It is consulted per deployment, after routing has chosen one. Which annotation
// applies is a fact about the upstream rather than about the request — its wire
// format decides which of the two is even meaningful, its auth mode decides
// whether its body may be touched, and its own history decides whether it takes
// an annotation at all — and none of that is known while there is still a group
// to choose from.
//
// One instance serves one request. The state that outlives a request, which
// deployments have refused an annotation, belongs to the Server.
type annotator struct {
	srv *Server
	// stream says the caller asked for a streamed reply, which is the only
	// request a usage option belongs on.
	stream bool
	// markConversation says this endpoint runs the model, so Anthropic's
	// automatic caching has a conversation to follow. Token counting takes the
	// same body to answer a different question and warms nothing, so asking it
	// to cache the conversation is at best inert.
	markConversation bool
	// requestID ties a skipped annotation to the request it was skipped on.
	requestID string
}

// annotatorFor builds the annotator for one request, or returns nil where
// nothing about this request could be annotated whatever deployment serves it.
//
// Nil is returned as the interface value rather than a nil *annotator, so the
// caller's check for one actually finds nothing.
func (s *Server) annotatorFor(format core.Format, stream, markConversation bool, requestID string) provider.Annotator {
	// With translation switched on, an ingress can reach a deployment of either
	// format, so neither shortcut below holds: which annotation applies is
	// decided by the deployment that ends up serving, and that is not known
	// here. Both shortcuts exist only to avoid allocating an annotator that
	// would find nothing to add, so standing them down costs an allocation on
	// requests that turn out to need nothing — against, if they were left in
	// place, a translated streamed reply reaching an OpenAI-compatible upstream
	// with no usage option on it, which is billed as nothing and charged
	// against no budget.
	crossFormat := s.cfg.Router.Translation.Enabled

	wantsUsage := stream && s.cfg.Observability.StreamUsageEnabled()
	// The usage option belongs only to an OpenAI-compatible upstream.
	if format == core.FormatAnthropic && !crossFormat {
		wantsUsage = false
	}
	// Injection needs somewhere to land. An anthropic deployment always reads a
	// breakpoint, so the global switch is the whole question there; an openai
	// one reads it only where its operator said so, and a fleet where none did
	// would otherwise allocate an annotator per request to decide it has nothing
	// to add.
	marksPrefix := s.cfg.PromptCache.Inject
	if format == core.FormatOpenAI && !crossFormat {
		marksPrefix = marksPrefix && s.marksOpenAIPrefix
	}
	if !wantsUsage && !marksPrefix {
		return nil
	}
	return &annotator{srv: s, stream: stream, markConversation: markConversation, requestID: requestID}
}

// Annotate implements provider.Annotator.
func (a *annotator) Annotate(dep *config.Deployment, base []byte) ([]byte, bool) {
	// A passthrough deployment forwards the caller's own credential to an
	// upstream that bills their subscription, and its body must arrive as they
	// sent it: Anthropic's gateway rules require it, and the endpoint strips
	// Claude Code's system-prompt attribution block positionally, which only
	// works on an array that was not spliced. Standing down here rather than at
	// load is what lets one gateway front both subscription and API traffic and
	// still annotate the half that benefits.
	if dep.Params.AuthMode == core.AuthModePassthrough {
		return base, false
	}
	level := a.srv.levelFor(dep.ID())
	if level == annotateNone {
		return base, false
	}

	// The deployment's format, never the caller's: on a translated request the
	// body reaching this point has already been rewritten for this upstream, so
	// the annotation that belongs on it is the one that upstream reads.
	switch dep.Params.Format {
	case core.FormatOpenAI:
		// The usage option first: it is what billing depends on, so it is the
		// part that survives a demotion.
		body, changed := a.askForStreamedUsage(base)
		// A breakpoint only where the operator has named an upstream that reads
		// one. Most of this ecosystem caches automatically and would ignore or
		// refuse the marker, and the gateway cannot ask an arbitrary
		// OpenAI-compatible base URL which kind it is.
		if level >= annotateFull && dep.Params.SupportsCacheControl {
			if marked, ok := a.markOpenAIPrefix(body); ok {
				body, changed = marked, true
			}
		}
		return body, changed
	case core.FormatAnthropic:
		if level < annotateFull {
			return base, false
		}
		return a.markCacheablePrefix(base)
	}
	return base, false
}

// levelFor reports how much this deployment has been shown to accept. A
// deployment nothing is known about accepts everything, which is what makes the
// first refusal the thing that teaches the gateway otherwise.
func (s *Server) levelFor(id string) annotationLevel {
	if v, ok := s.annotationLevel.Load(id); ok {
		return v.(annotationLevel)
	}
	return annotateFull
}

// askForStreamedUsage adds stream_options.include_usage to a streamed
// OpenAI-compatible request that did not set stream_options itself.
//
// Without it the reply carries no usage anywhere, so the request is billed as
// nothing: no cost, no budget charge, no rate-limit tokens, and a prompt cache
// whose reads are invisible. A caller that set stream_options has said what it
// wants and is left alone, byte for byte.
func (a *annotator) askForStreamedUsage(base []byte) ([]byte, bool) {
	if !a.stream || !a.srv.cfg.Observability.StreamUsageEnabled() {
		return base, false
	}
	asked, added, err := jsonx.AddMember(base, "stream_options", streamUsageOptions)
	if err != nil {
		// A request the gateway could not annotate is forwarded as it arrived
		// rather than refused.
		a.srv.log.Warn("stream usage request skipped", "error", err, "request_id", a.requestID)
		return base, false
	}
	if !added {
		return base, false
	}
	return asked, true
}

// markOpenAIPrefix marks the leading system message of an OpenAI-format request,
// for an upstream whose explicit cache reads the marker.
func (a *annotator) markOpenAIPrefix(base []byte) ([]byte, bool) {
	if !a.srv.cfg.PromptCache.Inject {
		return base, false
	}
	injected, changed, err := promptcache.InjectOpenAI(base, a.srv.cfg.PromptCache.InjectMinBytes)
	if err != nil {
		a.srv.log.Warn("prompt cache injection skipped", "error", err, "request_id", a.requestID)
		return base, false
	}
	return injected, changed
}

// markCacheablePrefix marks the stable prefix of an Anthropic request, where the
// caller has not marked one itself.
func (a *annotator) markCacheablePrefix(base []byte) ([]byte, bool) {
	if !a.srv.cfg.PromptCache.Inject {
		return base, false
	}
	injected, changed, err := promptcache.Inject(base, a.srv.cfg.PromptCache.InjectMinBytes, a.markConversation)
	if err != nil {
		// As above: the request is forwarded exactly as it arrived. An
		// optimization that could not be applied is not a reason to refuse it.
		a.srv.log.Warn("prompt cache injection skipped", "error", err, "request_id", a.requestID)
		return base, false
	}
	if !changed {
		return base, false
	}
	return injected, true
}

// Refused implements provider.Annotator.
//
// It is reached only once the upstream has proved the annotation was the cause:
// the annotated body was refused and the same request as it arrived was served.
// A 400 the caller earned fails both bodies and never reaches here, so one
// malformed request cannot switch the optimization off for everyone.
//
// The memory is per process rather than persisted. What an upstream accepts can
// change with a release in either direction, and a restart is the cheapest way
// to re-ask: it costs one round trip on one request.
func (a *annotator) Refused(dep *config.Deployment) {
	a.srv.annotationMu.Lock()
	current := a.srv.levelFor(dep.ID())
	if current == annotateNone {
		a.srv.annotationMu.Unlock()
		return
	}
	next := current - 1
	a.srv.annotationLevel.Store(dep.ID(), next)
	a.srv.annotationMu.Unlock()

	a.srv.log.Warn("upstream rejected an annotated request, so less will be added to this deployment's requests from now on",
		"deployment", dep.ID(),
		"now_adds", next.describe(),
		"cost", "one round trip per step, paid once per deployment per process")
}

// describe names a level in terms of what it still adds, so the log line an
// operator reads says what the gateway will do rather than which enum it holds.
func (l annotationLevel) describe() string {
	switch l {
	case annotateEssential:
		return "only what billing depends on: stream_options.include_usage"
	case annotateFull:
		return "everything"
	default:
		return "nothing; requests are forwarded exactly as they arrive"
	}
}
