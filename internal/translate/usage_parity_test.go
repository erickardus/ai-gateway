package translate_test

import (
	"io"
	"strings"
	"testing"

	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/provider"
	"github.com/erickardus/ai-gateway/internal/translate"
)

// The gateway bills from the reply the client receives, which on a translated
// request is a document this package wrote rather than one the upstream sent.
// That is only safe if the rewrite is exact: the ledger, the budget and the
// prompt-cache savings all read the same counters, and each format states them
// under a different convention. So the invariant these tests hold is a single
// sentence — reading the translated reply under the caller's format must give
// the same core.Usage as reading the original under the upstream's — and it is
// checked against the gateway's own parser rather than against a restatement of
// it here, so the two cannot drift apart.
//
// This lives in an external test package because package provider imports
// package translate, and the invariant is precisely that those two agree.

func TestUsageSurvivesTranslationToAnthropic(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage string
	}{
		{"plain", `{"prompt_tokens":1000,"completion_tokens":50}`},
		{"openai cached read", `{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":800}}`},
		{"deepseek top level hit", `{"prompt_tokens":1000,"completion_tokens":50,"prompt_cache_hit_tokens":600}`},
		{"qwen explicit write", `{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":300,"cache_creation_input_tokens":200}}`},
		{"minimax anthropic names", `{"prompt_tokens":900,"completion_tokens":10,"prompt_tokens_details":{"cache_read_input_tokens":400,"cache_creation_input_tokens":100}}`},
		{"over reported cache", `{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":9999}}`},
		{"no usage at all", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"id":"chatcmpl-1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]`
			if tc.usage != "" {
				body += `,"usage":` + tc.usage
			}
			body += `}`

			want, _ := provider.UsageFromBody([]byte(body), core.FormatOpenAI)

			out, err := translate.Response(core.FormatOpenAI, core.FormatAnthropic, []byte(body))
			if err != nil {
				t.Fatal(err)
			}
			got, _ := provider.UsageFromBody(out, core.FormatAnthropic)

			if got != want {
				t.Fatalf("translated reply bills differently from the original:\n got %+v\nwant %+v\n\n%s", got, want, out)
			}
		})
	}
}

func TestUsageSurvivesTranslationToOpenAI(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage string
	}{
		{"plain", `{"input_tokens":200,"output_tokens":20}`},
		{"read and write", `{"input_tokens":200,"output_tokens":20,"cache_read_input_tokens":800,"cache_creation_input_tokens":100}`},
		{"long cache tier", `{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":300,"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":200}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":` + tc.usage + `}`

			want, _ := provider.UsageFromBody([]byte(body), core.FormatAnthropic)

			out, err := translate.Response(core.FormatAnthropic, core.FormatOpenAI, []byte(body))
			if err != nil {
				t.Fatal(err)
			}
			got, _ := provider.UsageFromBody(out, core.FormatOpenAI)

			if got != want {
				t.Fatalf("translated reply bills differently from the original:\n got %+v\nwant %+v\n\n%s", got, want, out)
			}
		})
	}
}

// TestStreamedUsageSurvivesTranslation runs the same invariant over a streamed
// reply, which is the shape that actually matters: Claude Code streams every
// request, and a streamed reply that reports nothing is billed as nothing.
func TestStreamedUsageSurvivesTranslation(t *testing.T) {
	const reported = `{"prompt_tokens":5000,"completion_tokens":300,"prompt_tokens_details":{"cached_tokens":4200,"cache_creation_input_tokens":300}}`
	in := strings.Join([]string{
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: {"id":"chatcmpl-1","choices":[],"usage":` + reported + `}`,
		`data: [DONE]`,
		"",
	}, "\n\n")

	want, ok := provider.UsageFromBody([]byte(`{"usage":`+reported+`}`), core.FormatOpenAI)
	if !ok {
		t.Fatal("the fixture itself reports no usage")
	}

	translated, err := io.ReadAll(translate.Stream(core.FormatOpenAI, core.FormatAnthropic, io.NopCloser(strings.NewReader(in))))
	if err != nil {
		t.Fatal(err)
	}

	// Read it back the way the gateway does: every data: line, merged, under
	// the caller's format.
	var got core.Usage
	for _, line := range strings.Split(string(translated), "\n") {
		payload, found := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !found {
			continue
		}
		u, ok := provider.UsageFromBody([]byte(strings.TrimSpace(payload)), core.FormatAnthropic)
		if !ok {
			continue
		}
		got = maxUsage(got, u)
	}

	if got != want {
		t.Fatalf("streamed usage lost in translation:\n got %+v\nwant %+v\n\n%s", got, want, translated)
	}
}

// TestTranslatedStreamCarriesUsageEvenWhenTheClientNeverAsked guards the
// failure the annotator exists to prevent, from the other side: whatever the
// upstream reports has to reach the client's own message_delta, because that is
// the only place the gateway's sniffer will find it.
func TestTranslatedStreamCarriesUsageEvenWhenTheClientNeverAsked(t *testing.T) {
	in := "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"
	out, err := io.ReadAll(translate.Stream(core.FormatOpenAI, core.FormatAnthropic, io.NopCloser(strings.NewReader(in))))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"input_tokens":7`) {
		t.Fatalf("usage did not survive onto message_delta:\n%s", out)
	}
}

func maxUsage(a, b core.Usage) core.Usage {
	return core.Usage{
		InputTokens:        max(a.InputTokens, b.InputTokens),
		OutputTokens:       max(a.OutputTokens, b.OutputTokens),
		CacheReadTokens:    max(a.CacheReadTokens, b.CacheReadTokens),
		CacheWriteTokens:   max(a.CacheWriteTokens, b.CacheWriteTokens),
		CacheWrite1hTokens: max(a.CacheWrite1hTokens, b.CacheWrite1hTokens),
	}
}
