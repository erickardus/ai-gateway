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
	"encoding/json"
	"strconv"
	"time"

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

// The roles an OpenAI-format conversation may open with that are not a turn of
// the conversation itself. "developer" is the newer spelling of "system" and
// occupies the same position.
const (
	roleSystem    = "system"
	roleDeveloper = "developer"
)

// prefixLead is how many leading messages join the fingerprint.
//
// The count has to be one a conversation satisfies from its very first request,
// not merely later on. A lead of two against a conversation that opens with a
// single message reads one message on that first turn and two on every turn
// after it, so the opening turn fingerprints differently from the conversation
// it begins — and the upstream it warmed is the one deployment the second turn
// has no reason to return to. The cost of that is one cache write per
// conversation, paid quietly.
//
// So the lead is decided by what the opening message is rather than by the wire
// format alone. Under the Anthropic format the first message is the opening
// user turn, which differs per conversation and is enough on its own. Under
// OpenAI the first message is usually a system prompt many callers share, and
// the second — the first user turn — is what tells two conversations apart;
// but a caller that sends no system message has a first request holding one
// message where its second holds three, which is the instability above. Reading
// the second message only when a system message precedes it is stable in both
// cases, because a system message is present from the first request or not at
// all.
func prefixLead(format core.Format, firstRole string) int {
	if format == core.FormatAnthropic {
		return 1
	}
	if firstRole == roleSystem || firstRole == roleDeveloper {
		return 2
	}
	return 1
}

// LongCacheLifetime is how long Anthropic's extended cache entry lives. A caller
// opts into it per breakpoint, paying twice base input to write rather than
// 1.25x, in exchange for an entry that survives a gap the default five-minute
// one would not.
const LongCacheLifetime = time.Hour

// longTTL is the lifetime a one-hour breakpoint names.
const longTTL = "1h"

// DeclaresLongCacheTTL reports whether the request asks for the one-hour prompt
// cache.
//
// It decides how long a prefix pin should live. A pin exists to send the next
// turn back to the upstream holding the warm entry, so a pin that lapses before
// that entry does hands the conversation back to the load balancer while the
// cache it paid two times input to write is still sitting there — and the turn
// that lands elsewhere pays that premium again.
//
// Only the tools and the system prompt are read. Both are small, both are
// already walked to fingerprint the request, and the API requires longer-lived
// entries to appear before shorter-lived ones — so a one-hour breakpoint that
// coexists with any five-minute breakpoint is in one of them. Scanning the
// message history for the remaining case would cost a walk over the largest part
// of every request, which is the expense the fingerprint itself is shaped to
// avoid.
func DeclaresLongCacheTTL(fields jsonx.Fields) bool {
	return declaresLongTTL(fields.Tools) || declaresLongTTL(fields.System)
}

func declaresLongTTL(value []byte) bool {
	if !bytes.Contains(value, controlMarker) {
		return false
	}
	values, err := jsonx.MemberValues(value, controlKey)
	if err != nil {
		return false
	}
	for _, v := range values {
		var control struct {
			TTL string `json:"ttl"`
		}
		if json.Unmarshal(v, &control) == nil && control.TTL == longTTL {
			return true
		}
	}
	return false
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

	lead := fields.LeadingMessages(prefixLead(format, fields.FirstMessageRole()))
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

// Inject marks the cacheable parts of an Anthropic request, and reports whether
// it changed anything.
//
// Explicit breakpoints go at the end of the tool definitions and at the end of
// the system prompt, which together are the part of a request that does not
// change from one turn to the next. Those two are read points that survive
// whatever happens later in the conversation.
//
// They do not, on their own, cache the conversation. Render order is tools, then
// system, then messages, so a breakpoint at the end of the system prompt caches
// everything before it and nothing after — and the messages are what grow. A
// caller with a modest system prompt and a long history would re-read the whole
// history at full price on every turn while the gateway reported that caching
// was working.
//
// So, where the request can carry it, injection also sets the top-level
// cache_control field. That is Anthropic's automatic caching: the API places a
// breakpoint on the last cacheable block and walks it forward as the
// conversation grows, which is precisely the marker a gateway cannot maintain
// itself — one written into the body would be rewritten every turn, buying a
// cache write for each read. Together the three are the combination the API
// documents for an agent loop: a guaranteed read point on the expensive static
// prefix, plus automatic caching for the tail.
//
// Three of the four breakpoints a request may hold, which is deliberate. The two
// documented ways of combining an explicit marker with the automatic one are
// both refused with a 400: all four slots already taken, and an explicit marker
// on the last block whose lifetime differs from the top-level field's. Neither
// can arise here, because injection runs only on a body carrying no breakpoint
// of its own and every marker it places has the same default lifetime.
//
// It is skipped entirely when the body already carries a breakpoint. A caller
// that placed its own knows where its prompt actually repeats, and the API caps
// how many a request may hold.
//
// minBytes suppresses breakpoints on prompts too short for a provider to cache.
// It is measured over the whole prompt — tools, system and messages — because
// that is what the automatic breakpoint would cache; measuring only the static
// prefix would skip exactly the caller this is for, whose history is long and
// whose system prompt is not. Anthropic ignores a breakpoint below its own
// minimum rather than rejecting it, so the threshold is an economy, not a
// correctness requirement.
//
// The body is edited by splicing, never by re-encoding: a re-marshalled request
// would reorder keys and renormalize numbers, which changes the very prefix
// bytes the upstream cache keys on.
func Inject(body []byte, minBytes int, markConversation bool) ([]byte, bool, error) {
	if marked, err := alreadyMarked(body); err != nil {
		return body, false, err
	} else if marked {
		return body, false, nil
	}

	values, err := jsonx.RawTopLevel(body, "system", "tools", "messages")
	if err != nil {
		return body, false, err
	}
	system, tools, messages := values[0], values[1], values[2]
	if len(system)+len(tools)+len(messages) < minBytes {
		return body, false, nil
	}

	// Both explicit breakpoints are decided before either is spliced, so the
	// document is walked once to read and once to write rather than once per
	// edit. Each walk revalidates the whole body, and on the request bodies this
	// runs against that is what editing actually costs.
	marks := make(map[string][]byte, 2)
	if marked, ok, err := markLastElement(tools, markable); err != nil {
		return body, false, err
	} else if ok {
		marks["tools"] = marked
	}
	if marked, ok, err := markSystem(system); err != nil {
		return body, false, err
	} else if ok {
		marks["system"] = marked
	}

	out, changed := body, false
	if len(marks) > 0 {
		out, err = jsonx.SetTopLevelValues(out, marks)
		if err != nil {
			return body, false, err
		}
		changed = true
	}
	// The automatic breakpoint is only worth a slot where there is a
	// conversation for it to follow. With no messages it would land on the
	// system block this already marked explicitly, which the API treats as a
	// no-op.
	if markConversation && hasElements(messages) {
		asked, added, err := jsonx.AddMember(out, controlKey, cacheControl)
		if err != nil {
			return body, false, err
		}
		if added {
			out, changed = asked, true
		}
	}
	if !changed {
		return body, false, nil
	}
	return out, true, nil
}

// hasElements reports whether a JSON array holds at least one element. An empty
// messages array is not a conversation, and a breakpoint set to follow one that
// is not there would land on the system prompt already marked explicitly.
func hasElements(array []byte) bool {
	if len(array) == 0 {
		return false
	}
	spans, err := jsonx.Elements(array)
	return err == nil && len(spans) > 0
}

// markable reports whether a tool definition is one a breakpoint is known to be
// accepted on.
//
// A custom tool declares an input_schema. The rest of what may appear in the
// tools array does not: server tools such as web search or code execution are
// named by type alone, an MCP toolset names a server, and both are shapes the
// gateway has no way to validate a breakpoint against. Marking one to find out
// costs a 400 on a request that would otherwise have been served.
//
// Skipping it loses nothing, which is what makes this the cheap answer rather
// than a compromise: tools render before the system prompt, so the breakpoint at
// the end of the system prompt already caches every tool in the list. The tools
// marker is a second read point for a caller whose system prompt changes while
// its tools do not, and a caller mixing server tools into that position simply
// does not get one.
func markable(tool []byte) bool {
	ok, err := jsonx.HasMember(tool, "input_schema")
	return err == nil && ok
}

// markSystem places a breakpoint on a system prompt, in either of the two shapes
// it may arrive in: a bare string, or an array of typed content blocks.
//
// The string form has nowhere to attach one, so it is promoted to the single
// text block it is defined to be equivalent to. The original string's bytes are
// reused verbatim rather than decoded and re-encoded, so the text an upstream
// tokenizes is unchanged.
//
// Both formats need this and the two shapes are identical, so it serves both:
// Anthropic's top-level system field, and the content of the leading system
// message of an OpenAI-format conversation. Alibaba's documentation for the
// latter says the same thing in its own words — "for a system message, you must
// change the content field to an array and add the cache_control field".
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
	// Every block a system prompt may hold is a text block, so there is nothing
	// to discriminate on the way there is among tools.
	return markLastElement(system, nil)
}

// markLastElement adds a breakpoint to the final element of a JSON array, which
// is where a cacheable prefix ends. A nil eligible marks whatever is there; one
// that returns false leaves the array alone rather than marking something the
// upstream might reject.
func markLastElement(array []byte, eligible func([]byte) bool) ([]byte, bool, error) {
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
	if eligible != nil && !eligible(element) {
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

// InjectOpenAI marks the cacheable prefix of a request in the OpenAI wire format,
// for a provider whose explicit cache reads Anthropic's cache_control marker.
//
// Most of the OpenAI-compatible ecosystem has nothing to mark: OpenAI, Kimi, GLM
// and DeepSeek all cache automatically, above a minimum prefix length, and a
// marker would be ignored or refused. Alibaba's Qwen is the exception. It caches
// implicitly too, but its *explicit* cache — the one with the higher hit ratio —
// is entered by marking a content block, and without a marker a caller only ever
// gets the implicit one.
//
// So this is not the Anthropic injection with a different gate on it. It marks
// one place rather than three, because an OpenAI-format body has only one of
// them:
//
//   - There is no top-level system field. The system prompt is the leading
//     message, and its content is what carries the marker.
//   - There is no top-level cache_control field, which is Anthropic's automatic
//     caching and an Anthropic construct. Nothing here walks a breakpoint
//     forward as the conversation grows, so a long history is re-read at full
//     price behind the prefix marker — the limitation roadmap.md records.
//   - The tools are not marked. An OpenAI tool declares no input_schema, so the
//     shape check that makes marking one safe under Anthropic does not apply,
//     and the provider documents the cache block as running from the start of
//     the messages array rather than from the tools.
//
// It stands down where the conversation does not open with a system or developer
// message. A marker on the first user turn would cache a prefix that changes
// with every conversation, which buys a write and no read.
//
// minBytes is measured over the messages and the tools together, and the
// provider's own minimum is 1024 tokens.
func InjectOpenAI(body []byte, minBytes int) ([]byte, bool, error) {
	if marked, err := alreadyMarked(body); err != nil {
		return body, false, err
	} else if marked {
		return body, false, nil
	}

	values, err := jsonx.RawTopLevel(body, "messages", "tools")
	if err != nil {
		return body, false, err
	}
	messages, tools := values[0], values[1]
	if len(messages)+len(tools) < minBytes {
		return body, false, nil
	}

	spans, err := jsonx.Elements(messages)
	if err != nil || len(spans) == 0 {
		// A value in an unexpected shape is forwarded as it arrived, without
		// the optimization, rather than failing the request.
		return body, false, nil
	}
	lead := messages[spans[0][0]:spans[0][1]]
	if !opensWithSystemPrompt(lead) {
		return body, false, nil
	}

	marked, ok, err := markMessageContent(lead)
	if err != nil || !ok {
		return body, false, err
	}

	edited := make([]byte, 0, len(messages)-len(lead)+len(marked))
	edited = append(edited, messages[:spans[0][0]]...)
	edited = append(edited, marked...)
	edited = append(edited, messages[spans[0][1]:]...)

	out, err := jsonx.SetTopLevelValues(body, map[string][]byte{"messages": edited})
	if err != nil {
		return body, false, err
	}
	return out, true, nil
}

// opensWithSystemPrompt reports whether a message is the system or developer turn
// an OpenAI-format conversation opens with, which is the part of it that stays
// fixed as the conversation grows.
func opensWithSystemPrompt(message []byte) bool {
	if len(message) == 0 || message[0] != '{' {
		return false
	}
	values, err := jsonx.RawTopLevel(message, "role")
	if err != nil || len(values[0]) == 0 {
		return false
	}
	var role string
	if json.Unmarshal(values[0], &role) != nil {
		return false
	}
	return role == roleSystem || role == roleDeveloper
}

// markMessageContent places a breakpoint on a message's content, promoting a
// bare string to the single text block it is equivalent to where it has to.
func markMessageContent(message []byte) ([]byte, bool, error) {
	values, err := jsonx.RawTopLevel(message, "content")
	if err != nil {
		return nil, false, err
	}
	marked, ok, err := markSystem(values[0])
	if err != nil || !ok {
		return nil, false, err
	}
	out, err := jsonx.SetTopLevelValues(message, map[string][]byte{"content": marked})
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}
