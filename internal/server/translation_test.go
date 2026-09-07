package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/erickardus/ai-gateway/internal/core"
)

// These are the acceptance tests for the feature's whole point: a client that
// speaks one wire format and nothing else — Claude Code speaks the Anthropic
// Messages API and nothing else — reaching an upstream that speaks the other,
// through one endpoint and one virtual key.
//
// They are written at the server boundary rather than against the translator
// because what has to hold is end to end: the request has to arrive at the
// upstream in its own dialect, the reply has to come back in the caller's, and
// the tokens have to land in the ledger exactly once on the way past.

// openAIUpstream answers a chat-completions call, recording enough for a test
// to assert the request really was rewritten.
func openAIUpstream(t *testing.T, reply string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, reply)
	}
}

func TestAnthropicClientReachesAnOpenAIUpstream(t *testing.T) {
	const reply = `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-5",
		"choices":[{"index":0,"message":{"role":"assistant","content":"4 degrees"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":800}}}`

	h := newHarness(t, harnessOpts{
		format:      core.FormatOpenAI,
		authMode:    "api_key",
		translation: true,
		upstream:    openAIUpstream(t, reply),
	})

	body := `{"model":"openai-gpt","max_tokens":256,"system":"be terse",
		"messages":[{"role":"user","content":"weather?"}]}`
	rec := h.do(t, claudeCodeRequest("/v1/messages", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// The upstream must have been called on its own endpoint, with its own
	// schema. Anything else and the rewrite did not happen.
	_, sent, path, _ := h.seen.get()
	if path != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want the chat-completions endpoint", path)
	}
	var upstream struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		MaxTokens int `json:"max_tokens"`
		System    any `json:"system"`
	}
	if err := json.Unmarshal(sent, &upstream); err != nil {
		t.Fatalf("upstream body is not a chat-completions request: %v\n%s", err, sent)
	}
	if upstream.System != nil {
		t.Fatalf("the anthropic system field must not survive: %s", sent)
	}
	if len(upstream.Messages) != 2 || upstream.Messages[0].Role != "system" {
		t.Fatalf("system prompt must become the leading message: %s", sent)
	}
	if upstream.MaxTokens != 256 {
		t.Fatalf("max_tokens = %d, want 256", upstream.MaxTokens)
	}

	// And the caller must get their own format back.
	if got := rec.Header().Get("x-gateway-translated"); got != "openai->anthropic" {
		t.Fatalf("x-gateway-translated = %q", got)
	}
	var out struct {
		Type       string `json:"type"`
		Role       string `json:"role"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens     int `json:"input_tokens"`
			OutputTokens    int `json:"output_tokens"`
			CacheReadTokens int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("reply is not an anthropic message: %v\n%s", err, rec.Body.String())
	}
	if out.Type != "message" || out.Role != "assistant" || out.StopReason != "end_turn" {
		t.Fatalf("reply envelope = %+v", out)
	}
	if len(out.Content) != 1 || out.Content[0].Text != "4 degrees" {
		t.Fatalf("content = %+v", out.Content)
	}

	// The cached tokens must be charged once, at the cache rate. Reporting the
	// upstream's prompt_tokens as input would bill all 1000 at the full rate
	// and then bill 800 of them again as a cache read.
	if out.Usage.InputTokens != 200 || out.Usage.CacheReadTokens != 800 {
		t.Fatalf("usage = %+v; cached tokens must be carved out of the input", out.Usage)
	}

	keys, err := h.ledger.Keys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d ledger rows, want 1", len(keys))
	}
	if keys[0].InputTokens != 200 || keys[0].CacheReadTokens != 800 || keys[0].OutputTokens != 50 {
		t.Fatalf("ledger row = %+v; it must agree with the reply the caller read", keys[0])
	}
}

func TestCrossFormatRoutingIsRefusedWhileTranslationIsOff(t *testing.T) {
	// The default. An operator who has not opted in gets the behaviour the
	// gateway has always had, and an error that names the switch rather than
	// leaving them to guess which of the two things to change.
	h := newHarness(t, harnessOpts{
		format:   core.FormatOpenAI,
		authMode: "api_key",
		upstream: openAIUpstream(t, `{"id":"x"}`),
	})

	rec := h.do(t, claudeCodeRequest("/v1/messages",
		`{"model":"openai-gpt","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, sent, _, _ := h.seen.get(); sent != nil {
		t.Fatalf("no upstream may be called: %s", sent)
	}
	if logs := h.logBuf.String(); !strings.Contains(logs, "router.translation.enabled") {
		t.Fatalf("the refusal must name the switch that would allow it:\n%s", logs)
	}
}

func TestPassthroughDeploymentIsNeverTranslated(t *testing.T) {
	// The rule that protects a developer's own subscription. A passthrough
	// deployment relays the caller's credential and must receive the body they
	// wrote; a translated request is neither. So an OpenAI-format caller cannot
	// reach one even with translation switched on.
	h := newHarness(t, harnessOpts{
		format:           core.FormatAnthropic,
		authMode:         "passthrough",
		allowPassthrough: true,
		translation:      true,
	})

	req := claudeCodeRequest("/v1/chat/completions",
		`{"model":"anthropic-claude","messages":[{"role":"user","content":"hi"}]}`)
	rec := h.do(t, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, sent, _, _ := h.seen.get(); sent != nil {
		t.Fatalf("a passthrough upstream must not be reached by a translated request: %s", sent)
	}
}

func TestPassthroughSurvivesInsideAMixedGroup(t *testing.T) {
	// The arrangement the feature is for: one group holding a subscription
	// deployment for Claude Code and an OpenAI one for everything else. The
	// passthrough member must still serve an Anthropic caller untouched.
	h := newHarness(t, harnessOpts{
		format:           core.FormatAnthropic,
		authMode:         "passthrough",
		allowPassthrough: true,
		translation:      true,
		extraDeployments: 1,
		extraFormat:      core.FormatOpenAI,
		extraAuthMode:    core.AuthModeAPIKey,
	})

	const body = `{"model":"anthropic-claude","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	rec := h.do(t, claudeCodeRequest("/v1/messages", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// Whichever member served it, an Anthropic caller got an Anthropic reply.
	if !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("reply is not JSON: %s", rec.Body.String())
	}
}

func TestCountTokensIsNotTranslated(t *testing.T) {
	// There is no OpenAI endpoint that measures a prompt without running the
	// model. Answering anyway would have Claude Code trimming conversations
	// against a number nobody computed.
	h := newHarness(t, harnessOpts{
		format:      core.FormatOpenAI,
		authMode:    "api_key",
		translation: true,
		upstream:    openAIUpstream(t, `{"id":"x"}`),
	})

	rec := h.do(t, claudeCodeRequest("/v1/messages/count_tokens",
		`{"model":"openai-gpt","messages":[{"role":"user","content":"hi"}]}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, sent, _, _ := h.seen.get(); sent != nil {
		t.Fatalf("no upstream may be called: %s", sent)
	}
}

func TestTranslatedStreamIsRelayedAndBilled(t *testing.T) {
	// The shape that actually matters: Claude Code streams every request, and a
	// streamed reply the gateway cannot read is billed as nothing.
	h := newHarness(t, harnessOpts{
		format:      core.FormatOpenAI,
		authMode:    "api_key",
		translation: true,
		upstream: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			rc := http.NewResponseController(w)
			for _, chunk := range []string{
				`{"id":"chatcmpl-2","object":"chat.completion.chunk","model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","content":"hel"}}]}`,
				`{"id":"chatcmpl-2","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
				`{"id":"chatcmpl-2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`{"id":"chatcmpl-2","choices":[],"usage":{"prompt_tokens":40,"completion_tokens":7}}`,
			} {
				io.WriteString(w, "data: "+chunk+"\n\n")
				rc.Flush()
			}
			io.WriteString(w, "data: [DONE]\n\n")
			rc.Flush()
		},
	})

	rec := h.do(t, claudeCodeRequest("/v1/messages",
		`{"model":"openai-gpt","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	out := rec.Body.String()
	for _, want := range []string{
		"event: message_start\n",
		"event: content_block_start\n",
		`"text_delta"`,
		"event: message_stop\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in the translated stream:\n%s", want, out)
		}
	}
	// The upstream's own sentinel belongs to its format and must not leak into
	// a stream the caller reads as Anthropic SSE.
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("the OpenAI stream terminator leaked through:\n%s", out)
	}

	// The gateway asks an OpenAI-compatible upstream for usage on a streamed
	// reply. That annotation has to be added to the translated body, not the
	// one the caller sent, or it is thrown away rebuilding the document.
	_, sent, _, _ := h.seen.get()
	if !strings.Contains(string(sent), `"include_usage":true`) {
		t.Fatalf("stream_options must be added after translation:\n%s", sent)
	}

	keys, err := h.ledger.Keys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].InputTokens != 40 || keys[0].OutputTokens != 7 {
		t.Fatalf("streamed usage was not billed: %+v", keys)
	}
}

func TestTranslatedUpstreamErrorIsRewrappedForTheCaller(t *testing.T) {
	// The upstream refused in its own format. A client handed an envelope its
	// own format does not define finds no message in it and reads a refusal it
	// could have acted on as a corrupt reply.
	const message = "This model's maximum context length is 128000 tokens"
	h := newHarness(t, harnessOpts{
		format:      core.FormatOpenAI,
		authMode:    "api_key",
		translation: true,
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"`+message+`","type":"invalid_request_error","code":"context_length_exceeded"}}`)
		},
	})

	rec := h.do(t, claudeCodeRequest("/v1/messages",
		`{"model":"openai-gpt","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the upstream's own: %s", rec.Code, rec.Body.String())
	}

	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("error is not an anthropic envelope: %v\n%s", err, rec.Body.String())
	}
	if env.Type != "error" {
		t.Fatalf("envelope = %+v", env)
	}
	// The wording is what Claude Code matches on to decide whether to retry
	// with a capability disabled, so only the envelope may change.
	if env.Error.Message != message {
		t.Fatalf("message = %q, want it byte for byte", env.Error.Message)
	}
}

func TestOpenAIClientReachesAnAnthropicUpstream(t *testing.T) {
	// The other direction, which is what lets a team's OpenAI-SDK tooling reach
	// the Claude deployments the same fleet already serves.
	const reply = `{"id":"msg_9","type":"message","role":"assistant","model":"claude","content":[
		{"type":"text","text":"hi there"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":12,"output_tokens":3}}`

	h := newHarness(t, harnessOpts{
		format:      core.FormatAnthropic,
		authMode:    "api_key",
		translation: true,
		upstream:    openAIUpstream(t, reply),
	})

	req := claudeCodeRequest("/v1/chat/completions",
		`{"model":"anthropic-claude","messages":[{"role":"system","content":"be terse"},{"role":"user","content":"hi"}]}`)
	rec := h.do(t, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	_, sent, path, _ := h.seen.get()
	if path != "/v1/messages" {
		t.Fatalf("upstream path = %q", path)
	}
	var upstream struct {
		System    string `json:"system"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(sent, &upstream); err != nil {
		t.Fatalf("upstream body is not a messages request: %v\n%s", err, sent)
	}
	if upstream.System != "be terse" {
		t.Fatalf("system message must be hoisted: %s", sent)
	}
	if len(upstream.Messages) != 1 {
		t.Fatalf("the system message must leave the conversation: %s", sent)
	}
	// The Messages API requires a cap and Chat Completions does not, so one has
	// to be supplied rather than the request refused.
	if upstream.MaxTokens <= 0 {
		t.Fatalf("max_tokens must be supplied: %s", sent)
	}

	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("reply is not a chat completion: %v\n%s", err, rec.Body.String())
	}
	if out.Object != "chat.completion" || len(out.Choices) != 1 {
		t.Fatalf("reply = %+v", out)
	}
	if out.Choices[0].Message.Content != "hi there" || out.Choices[0].FinishReason != "stop" {
		t.Fatalf("choice = %+v", out.Choices[0])
	}
	if out.Usage.PromptTokens != 12 {
		t.Fatalf("prompt_tokens = %d", out.Usage.PromptTokens)
	}
}

func TestUntranslatableReplyIsAGatewayError(t *testing.T) {
	// The upstream answered 200 with something this gateway cannot convert. The
	// reply is not relayed: a client handed a document its own format does not
	// define reads a corrupt answer from its own provider rather than a gateway
	// fault, and has no status code to act on. A non-streamed reply is rewritten
	// before the status line is sent precisely so this can be reported.
	h := newHarness(t, harnessOpts{
		format:      core.FormatOpenAI,
		authMode:    "api_key",
		translation: true,
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, "<html>not json at all</html>")
		},
	})

	rec := h.do(t, claudeCodeRequest("/v1/messages",
		`{"model":"openai-gpt","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Type != "error" {
		t.Fatalf("want an anthropic error envelope, got %s", rec.Body.String())
	}
	// The upstream's own body must not have leaked into it.
	if strings.Contains(rec.Body.String(), "<html>") {
		t.Fatalf("the untranslatable body was relayed: %s", rec.Body.String())
	}
}
