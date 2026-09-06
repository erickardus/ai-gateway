// Package promptcache supports the provider-side prompt cache: the one an
// upstream keeps of a request's leading tokens, not the response cache in
// package cache.
//
// The two are unrelated and solve different problems. A response cache answers
// an identical request without calling the provider at all. A prompt cache is
// the provider's own: it stores the tokenized prefix of a request — tools,
// system blocks, earlier turns — so the next request sharing that prefix is
// billed at roughly a tenth of the input price and starts generating sooner.
//
// A load-balancing gateway is where that quietly stops working. A prompt cache
// lives on one upstream account, so spreading a conversation's turns across
// deployments turns every cache read into a cache write, which costs a premium
// rather than a discount. This package derives the fingerprint the router pins
// on, and — where the caller has not managed breakpoints itself — marks the
// stable prefix of a request so there is a cache to hit at all.
package promptcache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/jsonx"
)

// cacheControl is the breakpoint marker Anthropic reads. "ephemeral" is the
// only lifetime the Messages API defines without an explicit ttl, and its cache
// lives about five minutes, refreshed on each read.
var cacheControl = []byte(`{"type":"ephemeral"}`)

// controlKey is the member an already-annotated body carries. A caller that
// places its own breakpoints — Claude Code does — knows its prompt better than
// the gateway does, and the API caps how many breakpoints one request may carry,
// so the gateway adds none where any are already present.
const controlKey = "cache_control"

// controlMarker is the same name as it appears in an encoded document. It is a
// prefilter, not the test: finding these bytes only means the document might
// carry the member, since prompt text mentioning cache_control contains them
// too.
var controlMarker = []byte(`"` + controlKey + `"`)

// prefixLead is how many leading messages join the fingerprint, which differs by
// wire format because the two put their distinguishing turn in different places.
//
// Under the Anthropic format the first message is the opening user turn, which
// differs per conversation and is therefore enough on its own. Under OpenAI the
// first is often a system message shared by every caller, and the second is what
// tells two conversations apart.
//
// The count has to be one a conversation satisfies from its very first request,
// not merely later on. A lead of two under the Anthropic format would read one
// message on the opening turn and two on every turn after it, so the opening
// turn would fingerprint differently from the conversation it begins — and the
// upstream it warmed would be the one deployment the second turn had no reason
// to return to. An OpenAI conversation carrying a system message already holds
// two messages on its first request, so two is stable there from the start.
func prefixLead(format core.Format) int {
	if format == core.FormatAnthropic {
		return 1
	}
	return 2
}

// Fingerprint identifies the cacheable prefix of a request, or returns false
// when the body has none worth pinning on.
//
// It covers the parts of a request that stay byte-identical as a conversation
// grows — the tool definitions, the system blocks and the opening turns — and
// deliberately not the trailing messages, which change on every turn and would
// make the fingerprint useless as an affinity key.
//
// Cache breakpoints are excluded from the hash. They are metadata saying where
// a prefix ends rather than prompt text an upstream tokenizes, and every client
// that places its own walks the last one forward as the conversation grows —
// Claude Code does, and so does the SDKs' automatic caching. Hashing them would
// make each turn of one conversation a different prefix, which is the precise
// condition this whole package exists to prevent.
//
// The model group and wire format are included so that two groups sharing a
// prompt do not share a pin, and the whole thing is hashed rather than kept, so
// prompt text never reaches routing state, a log line or Redis.
//
// It takes the Fields the router already peeked rather than the body, so the
// document is validated and walked once per request rather than three times.
func Fingerprint(format core.Format, model string, fields jsonx.Fields) (string, bool) {
	h := sha256.New()
	// Length-prefix every component: without it a system block ending in the
	// bytes of a tool definition could hash like a different request.
	write := func(part []byte) {
		h.Write([]byte(strconv.Itoa(len(part))))
		h.Write([]byte(":"))
		h.Write(part)
	}
	write([]byte(format))
	write([]byte(model))
	write(stripBreakpoints(fields.System))
	write(stripBreakpoints(fields.Tools))

	lead := fields.LeadingMessages(prefixLead(format))
	for _, message := range lead {
		write(stripBreakpoints(message))
	}

	// A request with no prefix at all — no system, no tools, no messages — has
	// nothing for an upstream to cache, so pinning it would only concentrate
	// load for no benefit.
	if len(fields.System) == 0 && len(fields.Tools) == 0 && len(lead) == 0 {
		return "", false
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// stripBreakpoints removes cache_control members from a value so that a
// breakpoint moving through a conversation does not change its fingerprint.
//
// The byte scan is the same prefilter alreadyMarked uses, and for the same
// reason: the bytes appear in any conversation that discusses prompt caching,
// so their presence only means the structural walk is worth running. A value
// that cannot be walked is hashed as it arrived — a fingerprint that is stable
// for a request shape the gateway does not recognize is still better than none.
func stripBreakpoints(value []byte) []byte {
	if !bytes.Contains(value, controlMarker) {
		return value
	}
	stripped, changed, err := jsonx.StripObjectKey(value, controlKey)
	if err != nil || !changed {
		return value
	}
	return stripped
}

// Inject marks the stable prefix of an Anthropic request as cacheable, and
// reports whether it changed anything.
//
// Breakpoints go at the end of the tool definitions and at the end of the
// system prompt, which together are the part of a request that does not change
// from one turn to the next. Trailing messages are deliberately left alone: a
// breakpoint there is rewritten on every turn, so it buys one cache write for
// every cache read.
//
// It is skipped entirely when the body already carries a breakpoint of its own.
// The API caps how many a request may hold, and a caller that placed its own
// knows where its prompt actually repeats.
//
// minBytes suppresses breakpoints on prefixes too short for a provider to
// cache. Anthropic ignores a breakpoint below its minimum rather than
// rejecting it, so the threshold is an economy, not a correctness requirement.
//
// The body is edited by splicing, never by re-encoding: a re-marshalled request
// would reorder keys and renormalize numbers, which changes the very prefix
// bytes the upstream cache keys on.
func Inject(body []byte, minBytes int) ([]byte, bool, error) {
	if marked, err := alreadyMarked(body); err != nil {
		return body, false, err
	} else if marked {
		return body, false, nil
	}

	values, err := jsonx.RawTopLevel(body, "system", "tools")
	if err != nil {
		return body, false, err
	}
	system, tools := values[0], values[1]
	if len(system)+len(tools) < minBytes {
		return body, false, nil
	}

	// Both breakpoints are decided before either is spliced, so the document is
	// walked once to read and once to write rather than once per edit. Each
	// walk revalidates the whole body, and on the request bodies this runs
	// against that is what editing actually costs.
	marks := make(map[string][]byte, 2)
	if marked, ok, err := markLastElement(tools); err != nil {
		return body, false, err
	} else if ok {
		marks["tools"] = marked
	}
	if marked, ok, err := markSystem(system); err != nil {
		return body, false, err
	} else if ok {
		marks["system"] = marked
	}
	if len(marks) == 0 {
		return body, false, nil
	}

	out, err := jsonx.SetTopLevelValues(body, marks)
	if err != nil {
		return body, false, err
	}
	return out, true, nil
}

// markSystem places a breakpoint on the system prompt in either of the two
// shapes the Messages API accepts.
//
// The string form has nowhere to attach one, so it is promoted to the single
// text block it is defined to be equivalent to. The original string's bytes are
// reused verbatim rather than decoded and re-encoded, so the text an upstream
// tokenizes is unchanged.
func markSystem(system []byte) ([]byte, bool, error) {
	if len(system) == 0 {
		return nil, false, nil
	}
	if system[0] == '"' {
		out := append([]byte(`[{"type":"text","text":`), system...)
		out = append(out, `,"cache_control":`...)
		out = append(out, cacheControl...)
		out = append(out, "}]"...)
		return out, true, nil
	}
	return markLastElement(system)
}

// markLastElement adds a breakpoint to the final element of a JSON array,
// which is where a cacheable prefix ends.
func markLastElement(array []byte) ([]byte, bool, error) {
	if len(array) == 0 {
		return nil, false, nil
	}
	spans, err := jsonx.Elements(array)
	if err != nil {
		// A value in an unexpected shape is not an error worth failing a
		// request over: the request is forwarded exactly as it arrived, just
		// without the optimization.
		return nil, false, nil
	}
	if len(spans) == 0 {
		return nil, false, nil
	}

	last := spans[len(spans)-1]
	element := array[last[0]:last[1]]
	if len(element) == 0 || element[0] != '{' {
		return nil, false, nil
	}

	marked, added, err := jsonx.AddMember(element, "cache_control", cacheControl)
	if err != nil || !added {
		return nil, false, err
	}

	out := make([]byte, 0, len(array)-len(element)+len(marked))
	out = append(out, array[:last[0]]...)
	out = append(out, marked...)
	out = append(out, array[last[1]:]...)
	return out, true, nil
}

// alreadyMarked reports whether the caller placed a breakpoint of its own.
//
// A substring search answers a different question than the one being asked. The
// bytes of "cache_control" appear in any conversation that discusses prompt
// caching — a developer reading these docs through Claude Code, a diff touching
// this file — and reading that as "the caller manages its own breakpoints" turns
// injection off for the rest of that conversation. The result is a silently
// larger bill on exactly the traffic that talks about caching.
//
// So the cheap scan is only a prefilter: bytes absent means the member is
// certainly absent, and bytes present buys a structural walk that distinguishes
// a member name from prompt text. The walk costs a pass over the body, which is
// why it runs on the small fraction of requests that get past the prefilter.
func alreadyMarked(body []byte) (bool, error) {
	if !bytes.Contains(body, controlMarker) {
		return false, nil
	}
	return jsonx.ContainsObjectKey(body, controlKey)
}
