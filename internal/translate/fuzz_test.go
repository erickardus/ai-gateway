package translate_test

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/provider"
	"github.com/erickardus/ai-gateway/internal/translate"
)

// The rest of this gateway fuzzes the paths where a bad input becomes a wrong
// bill rather than a failed request — the breakpoint detector, the body
// rewriter, the usage parser. Translation is now one of those paths, and it is
// the widest: it parses a whole document from an upstream nobody here controls
// and rebuilds it, and the rebuilt document is what the ledger, the budget and
// the caller all read.
//
// So two properties are fuzzed. That no input crashes or hangs the gateway,
// and that no input makes a translated reply bill differently from the original.

// FuzzTranslateUsageAccounting is the money one. Whatever counters an upstream
// reports, reading the translated reply under the caller's format must give the
// same core.Usage as reading the original under the upstream's — because the
// two formats disagree about whether a reported input figure includes cached
// tokens, and getting it wrong bills every cached token twice.
func FuzzTranslateUsageAccounting(f *testing.F) {
	f.Add(1000, 50, 800, 0)
	f.Add(0, 0, 0, 0)
	f.Add(100, 5, 9999, 9999)
	f.Add(-1, -1, -1, -1)
	f.Add(1<<40, 1<<40, 1<<39, 1<<38)

	f.Fuzz(func(t *testing.T, prompt, completion, cached, written int) {
		body, err := json.Marshal(map[string]any{
			"id":    "chatcmpl-f",
			"model": "m",
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "hi"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens":     prompt,
				"completion_tokens": completion,
				"prompt_tokens_details": map[string]any{
					"cached_tokens":               cached,
					"cache_creation_input_tokens": written,
				},
			},
		})
		if err != nil {
			t.Skip()
		}

		want, _ := provider.UsageFromBody(body, core.FormatOpenAI)

		out, err := translate.Response(core.FormatOpenAI, core.FormatAnthropic, body)
		if err != nil {
			t.Fatalf("a well-formed reply must translate: %v", err)
		}
		got, _ := provider.UsageFromBody(out, core.FormatAnthropic)
		if got != want {
			t.Fatalf("translated reply bills differently:\n got %+v\nwant %+v\n\n%s", got, want, out)
		}

		// No counter may go negative on the way across, whatever the upstream
		// reported. A negative would subtract from a bill.
		if got.InputTokens < 0 || got.OutputTokens < 0 || got.CacheReadTokens < 0 || got.CacheWriteTokens < 0 {
			t.Fatalf("negative usage from translation: %+v", got)
		}
	})
}

// FuzzTranslateRequest holds that no request body, however malformed, can crash
// the gateway or produce something that is not a JSON document — which is what
// would be put on the wire to an upstream.
func FuzzTranslateRequest(f *testing.F) {
	f.Add(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	f.Add(`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"x"}]}]}`)
	f.Add(`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[]}]}]}`)
	f.Add(`{"messages":[{"role":"assistant","tool_calls":[{"index":0,"function":{"arguments":"{"}}]}]}`)
	f.Add(`{"model":null,"messages":null,"tools":[]}`)
	f.Add(``)
	f.Add(`[]`)

	pairs := [][2]core.Format{
		{core.FormatAnthropic, core.FormatOpenAI},
		{core.FormatOpenAI, core.FormatAnthropic},
	}
	f.Fuzz(func(t *testing.T, body string) {
		for _, pair := range pairs {
			out, err := translate.Request(pair[0], pair[1], []byte(body), translate.Options{})
			if err != nil {
				continue
			}
			if !json.Valid(out) {
				t.Fatalf("%s->%s produced invalid JSON from %q:\n%s", pair[0], pair[1], body, out)
			}
		}
	})
}

// FuzzTranslateResponse holds the same for a reply, in both directions, and for
// the error envelopes — which are relayed on the path a client uses to decide
// whether to retry.
func FuzzTranslateResponse(f *testing.F) {
	f.Add(`{"id":"c","choices":[{"index":0,"message":{"content":"hi"},"finish_reason":"stop"}]}`)
	f.Add(`{"id":"m","content":[{"type":"tool_use","id":"t","name":"f","input":{"a":1}}],"stop_reason":"tool_use"}`)
	f.Add(`{"error":{"message":"boom","type":"server_error"}}`)
	f.Add(`{"choices":[]}`)
	f.Add(``)

	pairs := [][2]core.Format{
		{core.FormatOpenAI, core.FormatAnthropic},
		{core.FormatAnthropic, core.FormatOpenAI},
	}
	f.Fuzz(func(t *testing.T, body string) {
		for _, pair := range pairs {
			if out, err := translate.Response(pair[0], pair[1], []byte(body)); err == nil && !json.Valid(out) {
				t.Fatalf("%s->%s produced invalid JSON from %q:\n%s", pair[0], pair[1], body, out)
			}
			// An error envelope is never refused: an unparseable error is
			// still better relayed than replaced, so the only property is that
			// something comes back and nothing panics.
			translate.Error(pair[0], pair[1], []byte(body))
		}
	})
}

// FuzzTranslateStream holds that an arbitrary upstream stream cannot hang the
// translator or leave a client waiting on a message that is never closed. A
// stream that goes quiet is the failure Claude Code reports as a 300-second
// timeout, so "always terminates" is a property worth a fuzz target.
func FuzzTranslateStream(f *testing.F) {
	f.Add("data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	f.Add("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\"}}]}}]}\n\n")
	f.Add("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	f.Add("data:\n\n:comment\n\n")
	f.Add("")

	f.Fuzz(func(t *testing.T, sse string) {
		toAnthropic, err := io.ReadAll(translate.Stream(core.FormatOpenAI, core.FormatAnthropic,
			io.NopCloser(strings.NewReader(sse))))
		if err != nil {
			t.Fatalf("reading a translated stream must not fail: %v", err)
		}
		// Whatever the upstream sent, a message that was opened is closed.
		if strings.Contains(string(toAnthropic), "event: message_start") &&
			!strings.Contains(string(toAnthropic), "event: message_stop") {
			t.Fatalf("a started message was never closed:\n%s", toAnthropic)
		}

		toOpenAI, err := io.ReadAll(translate.Stream(core.FormatAnthropic, core.FormatOpenAI,
			io.NopCloser(strings.NewReader(sse))))
		if err != nil {
			t.Fatalf("reading a translated stream must not fail: %v", err)
		}
		if strings.Contains(string(toOpenAI), `"chat.completion.chunk"`) &&
			!strings.Contains(string(toOpenAI), "data: [DONE]") {
			t.Fatalf("a started stream was never terminated:\n%s", toOpenAI)
		}
	})
}
