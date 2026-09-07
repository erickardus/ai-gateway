// Package translate rewrites an inference request from one provider wire format
// into another, and rewrites the reply back again.
//
// It exists for one reason: a client speaks one format for its whole life.
// Claude Code speaks the Anthropic Messages API and nothing else, so without
// translation a fleet fronted by this gateway can offer it Anthropic-compatible
// upstreams only — and the OpenAI-compatible half of the ecosystem, which is
// most of it, is unreachable from the editor the gateway is built for.
//
// # What this costs, stated plainly
//
// Everywhere else the gateway edits a body: it splices one value and leaves
// every other byte identical. Translation is the opposite operation. It parses
// the whole document, rebuilds it, and thereby takes ownership of the fidelity
// of every field — including the fields that do not map, which are enumerated
// in Loss below rather than left to be discovered from a bill or a broken tool
// call. A translated request is not the caller's request. It is this package's
// best reconstruction of it.
//
// Three rules keep that ownership honest:
//
//  1. Translation is opt-in. With router.translation.enabled unset, an ingress
//     reaches only deployments of its own format and not one byte here runs.
//  2. A passthrough deployment is never translated. Its body must arrive as the
//     caller wrote it — Anthropic's gateway rules require it, and the endpoint
//     strips Claude Code's attribution block positionally — and its credential
//     belongs to the caller's own subscription, which must not be spent against
//     a body they did not write. Enforced in router.candidates and at load.
//  3. Usage is translated into the target format's own semantics rather than
//     copied across. The two formats disagree about whether a reported input
//     count includes cached tokens, and copying the number over would bill
//     every cached token twice. See usage.go in package provider for the rule
//     this preserves.
//
// # Unknown fields
//
// A field this package does not know about is dropped, never forwarded. The
// alternative — passing unrecognized members through — puts an Anthropic field
// name in front of an OpenAI server, which answers 400 and takes the whole
// request with it. Dropping degrades one capability; forwarding fails the call.
package translate

import (
	"errors"
	"fmt"
	"io"

	"github.com/erickardus/ai-gateway/internal/core"
)

// ErrUnsupported is returned for a format pair or an endpoint this package
// cannot translate. It is never a reason to guess: a caller that receives it
// must decline the route rather than send an untranslated body somewhere it
// will not be understood.
var ErrUnsupported = errors.New("translation not supported for this format pair")

// Paths this package knows. The gateway's own routes are the Anthropic Messages
// endpoint, its token-counting sibling, and OpenAI Chat Completions.
const (
	PathMessages       = "/v1/messages"
	PathCountTokens    = "/v1/messages/count_tokens"
	PathChatCompletion = "/v1/chat/completions"
)

// Supported reports whether a request that arrived in from can be rewritten for
// an upstream speaking to. A format translated to itself is supported and is a
// no-op, so callers need not special-case the common path.
func Supported(from, to core.Format) bool {
	if from == to {
		return true
	}
	switch {
	case from == core.FormatAnthropic && to == core.FormatOpenAI:
		return true
	case from == core.FormatOpenAI && to == core.FormatAnthropic:
		return true
	}
	return false
}

// UpstreamPath returns the path on a to-format upstream that answers a request
// which arrived at ingress, and whether such a path exists.
//
// Token counting has no answer in the OpenAI format. There is no endpoint that
// measures a prompt without running the model, and the honest reply to that is
// that this endpoint is not translatable — not an estimate the caller would
// then plan a context window around. A gateway that guessed here would have
// Claude Code trim conversations against a number nobody computed.
func UpstreamPath(ingress string, to core.Format) (string, bool) {
	switch to {
	case core.FormatAnthropic:
		switch ingress {
		case PathMessages, PathCountTokens:
			return ingress, true
		case PathChatCompletion:
			return PathMessages, true
		}
	case core.FormatOpenAI:
		switch ingress {
		case PathMessages:
			return PathChatCompletion, true
		case PathChatCompletion:
			return ingress, true
		case PathCountTokens:
			return "", false
		}
	}
	return "", false
}

// Options are the facts about one upstream that translation cannot infer from
// the request. Each exists because the OpenAI-compatible ecosystem disagrees
// with itself and the gateway cannot ask a base URL which dialect it speaks.
type Options struct {
	// MaxCompletionTokens emits the output cap as max_completion_tokens rather
	// than max_tokens. OpenAI's reasoning models refuse the older spelling, and
	// much of the compatible ecosystem has never implemented the newer one, so
	// there is no value that works everywhere and none is guessed.
	MaxCompletionTokens bool
	// KeepCacheControl carries Anthropic's cache_control markers onto a
	// translated OpenAI body, for an upstream whose explicit cache reads them.
	// It follows the deployment's supports_cache_control, and is off by
	// default because every other server answers 400 to the unknown member.
	KeepCacheControl bool
	// DefaultMaxTokens bounds a translated Anthropic request whose OpenAI
	// original named no cap. The Messages API requires max_tokens, so a
	// request that omitted one has to be given a number here or refused, and
	// refusing a request that every OpenAI server would have accepted is the
	// worse answer.
	DefaultMaxTokens int
}

// Request rewrites a request body that arrived in from into the body an
// upstream speaking to expects. from == to returns the body unchanged, sharing
// the caller's slice: nothing here mutates it.
func Request(from, to core.Format, body []byte, opts Options) ([]byte, error) {
	if from == to {
		return body, nil
	}
	switch {
	case from == core.FormatAnthropic && to == core.FormatOpenAI:
		return anthropicRequestToOpenAI(body, opts)
	case from == core.FormatOpenAI && to == core.FormatAnthropic:
		return openAIRequestToAnthropic(body, opts)
	}
	return nil, fmt.Errorf("%s to %s: %w", from, to, ErrUnsupported)
}

// Response rewrites a complete, non-streamed response body produced by an
// upstream speaking from into the shape a client speaking to expects.
func Response(from, to core.Format, body []byte) ([]byte, error) {
	if from == to {
		return body, nil
	}
	switch {
	case from == core.FormatOpenAI && to == core.FormatAnthropic:
		return openAIResponseToAnthropic(body)
	case from == core.FormatAnthropic && to == core.FormatOpenAI:
		return anthropicResponseToOpenAI(body)
	}
	return nil, fmt.Errorf("%s to %s: %w", from, to, ErrUnsupported)
}

// Stream wraps a streamed upstream response body so that reading it yields the
// same reply in the client's format.
//
// It is a reader rather than a rewritten buffer because a streamed reply must
// keep streaming: Claude Code reads tokens as they arrive and aborts a stream
// that goes silent for 300 seconds, so a translator that buffered a whole reply
// before emitting it would turn every long generation into a timeout. Each SSE
// event is converted and released as it lands.
//
// from == to returns body itself, so the ordinary same-format path stays the
// zero-copy relay it was.
//
// A stream is the one shape where a translation failure cannot be reported as
// one: by the time an unreadable event arrives the status line is long sent.
// Whatever was already relayed stands, and the message is closed so the client
// is not left waiting on an event that is never coming. A non-streamed reply
// has no such constraint, which is why ReadResponse exists separately.
func Stream(from, to core.Format, body io.ReadCloser) io.ReadCloser {
	if from == to {
		return body
	}
	return newStreamReader(from, to, body)
}

// ReadResponse reads a complete non-streamed reply and rewrites it for the
// client's format, returning an error rather than a body it could not convert.
//
// It is eager, and the caller is expected to use it before committing a status
// line. There is no way to rewrite half a JSON document, so this shape has to
// be held whole anyway — and holding it before the status is sent is what turns
// an untranslatable reply into an honest gateway error instead of a 200 whose
// body the client cannot parse.
func ReadResponse(from, to core.Format, body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxBufferedBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read upstream body for translation: %w", err)
	}
	if len(raw) > maxBufferedBodyBytes {
		return nil, fmt.Errorf("upstream body exceeds the %d bytes this gateway will buffer to translate", maxBufferedBodyBytes)
	}
	return Response(from, to, raw)
}

// Error rewrites an upstream error envelope into the shape a client speaking to
// can parse, preserving the upstream's own message text verbatim.
//
// The wording is preserved deliberately. Claude Code matches on an upstream's
// own error wording to decide whether to retry with a capability disabled, so
// rewriting the message would break a recovery path that the status code alone
// does not carry. Only the envelope around it changes, because an Anthropic
// client reading an OpenAI error envelope finds no message at all.
//
// A body that is not a recognizable error envelope is returned unchanged: an
// unparseable error is still better relayed than replaced.
func Error(from, to core.Format, body []byte) []byte {
	if from == to || len(body) == 0 {
		return body
	}
	switch {
	case from == core.FormatOpenAI && to == core.FormatAnthropic:
		return openAIErrorToAnthropic(body)
	case from == core.FormatAnthropic && to == core.FormatOpenAI:
		return anthropicErrorToOpenAI(body)
	}
	return body
}

// Loss enumerates what a translation between two formats cannot carry, as
// operator-facing sentences.
//
// It is exported and it is checked at load rather than left in a document,
// because the whole hazard of translation is that its failures are silent: a
// dropped top_k changes sampling and nothing says so, and a request whose
// extended thinking was dropped simply comes back shallower. The gateway logs
// this once per configured cross-format route at startup so the trade is
// visible before traffic is on it, rather than inferred afterwards from a
// change in output quality.
func Loss(from, to core.Format) []string {
	switch {
	case from == core.FormatAnthropic && to == core.FormatOpenAI:
		return []string{
			"top_k is dropped: the OpenAI Chat Completions API has no equivalent, so sampling will differ",
			"thinking is translated to reasoning_effort by budget, and the returned reasoning is not signed; a signed thinking block cannot be reconstructed, so multi-turn extended thinking degrades to plain text",
			"cache_control markers are dropped unless the deployment sets supports_cache_control, since most OpenAI-compatible servers refuse an unknown member",
			"metadata is dropped: it is an Anthropic construct with no chat-completions counterpart",
			"a tool_result carrying images is reduced to its text, because an OpenAI tool message takes no image parts",
			"document blocks are dropped: there is no chat-completions counterpart",
		}
	case from == core.FormatOpenAI && to == core.FormatAnthropic:
		return []string{
			"n, presence_penalty, frequency_penalty, logit_bias, logprobs and seed are dropped: the Messages API has none of them",
			"response_format is dropped, including json_schema; a caller relying on structured output will get prose",
			"max_tokens is required by the Messages API and defaulted when the request omits one, so an unbounded OpenAI request becomes a bounded Anthropic one",
			"tool_choice \"none\" is translated by withholding the tools, since the Messages API has no such value",
			"a system message that is not the first message is hoisted to the top-level system field, changing its position in the conversation",
		}
	}
	return nil
}
