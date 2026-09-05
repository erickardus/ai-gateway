package promptcache

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Injection edits a request that a customer is paying for and a provider is
// about to tokenize, so the properties it must hold are stronger than "the
// output parses". These tests state them as properties over many bodies rather
// than as assertions about one.

// TestInjectionAddsBreakpointsAndNothingElse strips the breakpoints back out of
// an injected body and requires what is left to be the request that went in.
//
// It is the guarantee that matters most: a gateway that reshaped a request
// while marking it would change the prefix bytes the upstream caches on, which
// is the very thing injection is trying to make hittable — and would do it
// invisibly, since the request would still succeed.
func TestInjectionAddsBreakpointsAndNothingElse(t *testing.T) {
	for _, body := range injectableBodies() {
		out, changed, err := Inject([]byte(body), 0)
		if err != nil {
			t.Fatalf("Inject(%s): %v", body, err)
		}
		if !changed {
			continue
		}
		if !json.Valid(out) {
			t.Fatalf("Inject produced invalid JSON for %s: %s", body, out)
		}

		before := normalize(decode(t, []byte(body)))
		after := normalize(decode(t, out))
		if !reflect.DeepEqual(before, after) {
			t.Errorf("injection changed the request beyond its breakpoints\n in: %s\nout: %s", body, out)
		}
	}
}

// TestInjectionIsIdempotent proves a marked body is left alone on a second
// pass. Two gateways in a chain, or a retry that re-enters the path, must not
// accumulate breakpoints: the API caps how many a request may carry, and
// exceeding it turns an optimization into a 400.
func TestInjectionIsIdempotent(t *testing.T) {
	for _, body := range injectableBodies() {
		once, changed, err := Inject([]byte(body), 0)
		if err != nil || !changed {
			continue
		}
		twice, changedAgain, err := Inject(once, 0)
		if err != nil {
			t.Fatalf("second Inject: %v", err)
		}
		if changedAgain {
			t.Errorf("second injection changed an already-marked body: %s", twice)
		}
		if string(twice) != string(once) {
			t.Errorf("second injection rewrote the body\nfirst:  %s\nsecond: %s", once, twice)
		}
	}
}

// TestInjectionStaysUnderTheBreakpointCap counts what it places. Anthropic
// accepts at most four cache_control blocks per request, and the gateway's two
// must leave room for none of the caller's, since it only ever runs where the
// caller placed none.
func TestInjectionStaysUnderTheBreakpointCap(t *testing.T) {
	const cap = 4
	for _, body := range injectableBodies() {
		out, changed, err := Inject([]byte(body), 0)
		if err != nil || !changed {
			continue
		}
		if n := strings.Count(string(out), `"cache_control"`); n > cap {
			t.Errorf("placed %d breakpoints, the API accepts %d: %s", n, cap, out)
		}
	}
}

// TestInjectionNeverMarksATrailingMessage is the difference between saving
// money and spending it. A breakpoint on the last message is rewritten every
// turn, so it buys one cache write per turn and never a read — strictly worse
// than not caching at all.
func TestInjectionNeverMarksATrailingMessage(t *testing.T) {
	const body = `{"model":"m","system":"a system prompt long enough to be worth caching",` +
		`"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"second"},{"role":"user","content":"latest turn"}]}`

	out, changed, err := Inject([]byte(body), 0)
	if err != nil || !changed {
		t.Fatalf("Inject: changed=%v err=%v", changed, err)
	}

	var doc struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for i, m := range doc.Messages {
		if strings.Contains(string(m), "cache_control") {
			t.Errorf("message %d was marked; a breakpoint on a turn is rewritten on the next one: %s", i, m)
		}
	}
}

// TestInjectionIsSkippedWhenTheCallerMentionsCachingInProse guards the
// regression that a substring check would cause. A developer asking Claude Code
// about cache_control must not thereby switch off caching for the rest of that
// conversation.
func TestInjectionIsSkippedWhenTheCallerMentionsCachingInProse(t *testing.T) {
	const body = `{"model":"m","system":"You are a helpful assistant with a system prompt long enough to be cacheable",` +
		`"messages":[{"role":"user","content":"how does cache_control work in the Messages API?"}]}`

	out, changed, err := Inject([]byte(body), 0)
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if !changed {
		t.Fatal("injection was skipped because prompt text mentioned cache_control; the breakpoint is a member name, not a word in a message")
	}
	if !strings.Contains(string(out), `"cache_control":{"type":"ephemeral"}`) {
		t.Errorf("no breakpoint placed: %s", out)
	}
}

// FuzzInject holds the properties against arbitrary input. The body is a
// request from an authenticated caller, but "authenticated" is not "well
// behaved", and a malformed edit reaches the provider as a rewritten prompt.
func FuzzInject(f *testing.F) {
	for _, body := range injectableBodies() {
		f.Add(body, 0)
	}
	f.Add(`{"system":[],"tools":[]}`, 0)
	f.Add(`{"system":123,"tools":"x"}`, 0)
	f.Add(`{"messages":[{"cache_control":{}}]}`, 0)
	f.Add(`not json`, 4096)

	f.Fuzz(func(t *testing.T, body string, minBytes int) {
		if minBytes < 0 {
			return
		}
		out, changed, err := Inject([]byte(body), minBytes)
		if err != nil {
			// A body that could not be edited must be forwarded exactly as it
			// arrived: an optimization is never a reason to alter a request.
			if string(out) != body || changed {
				t.Fatalf("body altered on error: %s", out)
			}
			return
		}
		if !changed {
			if string(out) != body {
				t.Fatalf("body altered while reporting no change: %s", out)
			}
			return
		}
		if !json.Valid(out) {
			t.Fatalf("invalid JSON produced from %q: %s", body, out)
		}
		before, after := normalize(decodeOrSkip(t, []byte(body))), normalize(decodeOrSkip(t, out))
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("injection changed the request beyond its breakpoints\n in: %s\nout: %s", body, out)
		}
		if n := strings.Count(string(out), `"cache_control"`); n > 4 {
			t.Fatalf("placed %d breakpoints: %s", n, out)
		}
	})
}

// injectableBodies is a spread of request shapes the Messages API accepts,
// covering both system forms, tools present and absent, and the escapes and
// unicode a real prompt carries.
func injectableBodies() []string {
	return []string{
		`{"model":"m","system":"a plain string system prompt"}`,
		`{"model":"m","system":[{"type":"text","text":"block one"},{"type":"text","text":"block two"}]}`,
		`{"model":"m","tools":[{"name":"read","input_schema":{"type":"object"}}],"system":"prompt"}`,
		`{"model":"m","tools":[{"name":"a"},{"name":"b"}],"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","system":"quotes \" backslashes \\ and unicode — ✓","messages":[{"role":"user","content":"x"}]}`,
		`{"model":"m","system":[{"type":"text","text":"only block"}],"messages":[{"role":"user","content":"x"}],"max_tokens":1024,"temperature":0.7}`,
		`{"model":"m","system":"prompt","tools":[]}`,
		`{"model":"m","messages":[{"role":"user","content":"no system, no tools"}]}`,
	}
}

func decode(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

func decodeOrSkip(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Skip("input is not decodable JSON")
	}
	return v
}

// normalize reduces a decoded request to the form both sides of the comparison
// must share: every breakpoint removed, and a system prompt written either as a
// string or as the single text block the API defines it to equal collapsed to
// the same value.
//
// The collapse is applied to both sides rather than undone on one, because the
// promotion is the one structural change injection is allowed to make and the
// two spellings are the same request.
func normalize(node any) any {
	switch n := node.(type) {
	case map[string]any:
		delete(n, "cache_control")
		for k, v := range n {
			n[k] = normalize(v)
		}
		if system, ok := n["system"]; ok {
			if text, promoted := demoteSystemBlock(system); promoted {
				n["system"] = text
			}
		}
		return n
	case []any:
		for i, v := range n {
			n[i] = normalize(v)
		}
		return n
	}
	return node
}

// demoteSystemBlock recognizes the promotion injection performs on a string
// system prompt and reverses it, so the comparison sees the request the caller
// sent.
func demoteSystemBlock(system any) (string, bool) {
	blocks, ok := system.([]any)
	if !ok || len(blocks) != 1 {
		return "", false
	}
	block, ok := blocks[0].(map[string]any)
	if !ok || len(block) != 2 || block["type"] != "text" {
		return "", false
	}
	text, ok := block["text"].(string)
	return text, ok
}
