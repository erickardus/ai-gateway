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
	wantsUsage := stream && s.cfg.Observability.StreamUsageEnabled()
	// Both annotations are format-specific, and an ingress only ever reaches
	// deployments speaking its own format, so a request in one format can never
	// pick up the other's annotation.
	if format == core.FormatAnthropic {
		wantsUsage = false
	}
	if !wantsUsage && !s.cfg.PromptCache.Inject {
		return nil
	}
	return &annotator{srv: s, stream: stream, markConversation: markConversation, requestID: requestID}
}

// Annotate implements provider.Annotator.
func (a *annotator) Annotate(dep *config.Deployment, base []byte) ([]byte, bool) {
	if !a.appliesTo(dep) {
		return base, false
	}
	switch dep.Params.Format {
	case core.FormatOpenAI:
		return a.askForStreamedUsage(base)
	case core.FormatAnthropic:
		return a.markCacheablePrefix(base)
	}
	return base, false
}

// appliesTo reports whether this deployment may be sent an annotated body at all.
func (a *annotator) appliesTo(dep *config.Deployment) bool {
	// A passthrough deployment forwards the caller's own credential to an
	// upstream that bills their subscription, and its body must arrive as they
	// sent it: Anthropic's gateway rules require it, and the endpoint strips
	// Claude Code's system-prompt attribution block positionally, which only
	// works on an array that was not spliced. Standing down here rather than at
	// load is what lets one gateway front both subscription and API traffic and
	// still annotate the half that benefits.
	if dep.Params.AuthMode == core.AuthModePassthrough {
		return false
	}
	// An upstream that has already refused an annotation is not offered another.
	// The round trip that established it is worth paying once; paying it on
	// every request, which is what a purely per-request decision costs, is what
	// makes a wrong guess expensive rather than merely wasteful.
	_, refused := a.srv.refusesAnnotation.Load(dep.ID())
	return !refused
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
	if _, seen := a.srv.refusesAnnotation.LoadOrStore(dep.ID(), true); seen {
		return
	}
	a.srv.log.Warn("upstream rejected an annotated request, so this deployment will not be annotated again",
		"deployment", dep.ID(),
		"cost", "one round trip, paid once per deployment per process")
}
