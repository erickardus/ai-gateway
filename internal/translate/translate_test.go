package translate

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/erickardus/ai-gateway/internal/core"
)

// decode is a small helper so a test can assert on one field of a translated
// document without declaring a struct for the whole of it.
func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return m
}

func TestUpstreamPathCountTokensHasNoOpenAICounterpart(t *testing.T) {
	// There is no OpenAI endpoint that measures a prompt without running the
	// model. A gateway that invented an answer here would have Claude Code
	// trimming conversations against a number nobody computed, so the honest
	// reply is that the route does not exist.
	if _, ok := UpstreamPath(PathCountTokens, core.FormatOpenAI); ok {
		t.Fatal("count_tokens must not be routable to an openai deployment")
	}
	if got, ok := UpstreamPath(PathMessages, core.FormatOpenAI); !ok || got != PathChatCompletion {
		t.Fatalf("messages -> openai = %q, %v; want %q, true", got, ok, PathChatCompletion)
	}
	if got, ok := UpstreamPath(PathChatCompletion, core.FormatAnthropic); !ok || got != PathMessages {
		t.Fatalf("chat -> anthropic = %q, %v; want %q, true", got, ok, PathMessages)
	}
	if got, ok := UpstreamPath(PathCountTokens, core.FormatAnthropic); !ok || got != PathCountTokens {
		t.Fatalf("count_tokens -> anthropic = %q, %v; want it unchanged", got, ok)
	}
}

func TestAnthropicRequestToOpenAISystemAndSampling(t *testing.T) {
	in := []byte(`{
		"model":"grp","max_tokens":512,
		"system":[{"type":"text","text":"be terse"},{"type":"text","text":"and kind"}],
		"messages":[{"role":"user","content":"hi"}],
		"temperature":0.4,"top_p":0.9,"top_k":40,
		"stop_sequences":["END"],"stream":true,
		"metadata":{"user_id":"u1"}
	}`)
	out, err := Request(core.FormatAnthropic, core.FormatOpenAI, in, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got openAIRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}

	if len(got.Messages) != 2 || got.Messages[0].Role != "system" {
		t.Fatalf("want a leading system message, got %+v", got.Messages)
	}
	if text := openAIContentText(got.Messages[0].Content); text != "be terse\nand kind" {
		t.Fatalf("system blocks should flatten to one string, got %q", text)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 512 {
		t.Fatalf("max_tokens = %v, want 512", got.MaxTokens)
	}
	if got.MaxCompletionTokens != nil {
		t.Fatal("max_completion_tokens must not be emitted unless the deployment asked for it")
	}
	if len(got.Stop) != 1 || got.Stop[0] != "END" {
		t.Fatalf("stop = %v, want [END]", got.Stop)
	}
	if !got.Stream {
		t.Fatal("stream must survive")
	}
	// top_k and metadata have no counterpart and must be dropped rather than
	// forwarded, since an unknown member is a 400 on the whole request.
	if strings.Contains(string(out), "top_k") || strings.Contains(string(out), "metadata") {
		t.Fatalf("unmappable fields leaked into the translated body: %s", out)
	}
}

func TestAnthropicRequestToOpenAIMaxCompletionTokens(t *testing.T) {
	in := []byte(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := Request(core.FormatAnthropic, core.FormatOpenAI, in, Options{MaxCompletionTokens: true})
	if err != nil {
		t.Fatal(err)
	}
	var got openAIRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.MaxCompletionTokens == nil || *got.MaxCompletionTokens != 64 {
		t.Fatalf("max_completion_tokens = %v, want 64", got.MaxCompletionTokens)
	}
	if got.MaxTokens != nil {
		t.Fatal("the two spellings are one cap; only the requested one may be sent")
	}
}

func TestAnthropicRequestToOpenAIToolRoundTrip(t *testing.T) {
	// One assistant turn calling a tool, then a user turn carrying its result:
	// the shape Claude Code produces on every tool-using conversation.
	in := []byte(`{
		"model":"m","max_tokens":100,
		"messages":[
			{"role":"user","content":"weather?"},
			{"role":"assistant","content":[
				{"type":"text","text":"checking"},
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Oslo"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"4C"}]},
				{"type":"text","text":"thanks"}
			]}
		],
		"tools":[{"name":"get_weather","description":"look up weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
		"tool_choice":{"type":"any","disable_parallel_tool_use":true}
	}`)
	out, err := Request(core.FormatAnthropic, core.FormatOpenAI, in, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got openAIRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}

	wantRoles := []string{"user", "assistant", "tool", "user"}
	if len(got.Messages) != len(wantRoles) {
		t.Fatalf("got %d messages, want %d: %+v", len(got.Messages), len(wantRoles), got.Messages)
	}
	for i, want := range wantRoles {
		if got.Messages[i].Role != want {
			t.Fatalf("messages[%d].role = %q, want %q", i, got.Messages[i].Role, want)
		}
	}
	// The tool message must precede the rest of the user turn it was written
	// inside: OpenAI requires each result to answer the assistant turn that
	// called it, and a user message in between breaks that adjacency.
	if got.Messages[2].ToolCallID != "toolu_1" {
		t.Fatalf("tool_call_id = %q, want toolu_1", got.Messages[2].ToolCallID)
	}
	if text := openAIContentText(got.Messages[2].Content); text != "4C" {
		t.Fatalf("tool result text = %q, want 4C", text)
	}

	assistant := got.Messages[1]
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("want one tool call, got %+v", assistant.ToolCalls)
	}
	if assistant.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("tool name = %q", assistant.ToolCalls[0].Function.Name)
	}
	// The arguments document is the caller's own and must survive byte for
	// byte: re-encoding it through a map would reorder its keys, which is
	// invisible in a request and fatal in a prompt-cache prefix.
	if assistant.ToolCalls[0].Function.Arguments != `{"city":"Oslo"}` {
		t.Fatalf("arguments = %q, want the input document unchanged", assistant.ToolCalls[0].Function.Arguments)
	}

	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("tools = %+v", got.Tools)
	}
	if string(got.ToolChoice) != `"required"` {
		t.Fatalf("tool_choice = %s, want \"required\"", got.ToolChoice)
	}
	if got.ParallelToolCalls == nil || *got.ParallelToolCalls {
		t.Fatal("disable_parallel_tool_use must become parallel_tool_calls false")
	}
}

func TestAnthropicRequestToOpenAIImageBecomesDataURI(t *testing.T) {
	in := []byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},
		{"type":"text","text":"what is this"}
	]}]}`)
	out, err := Request(core.FormatAnthropic, core.FormatOpenAI, in, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got openAIRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	var parts []openAIPart
	if err := json.Unmarshal(got.Messages[0].Content, &parts); err != nil {
		t.Fatalf("a turn holding an image must use the parts form: %v", err)
	}
	if len(parts) != 2 || parts[0].ImageURL == nil {
		t.Fatalf("parts = %+v", parts)
	}
	if want := "data:image/png;base64,AAAA"; parts[0].ImageURL.URL != want {
		t.Fatalf("image url = %q, want %q", parts[0].ImageURL.URL, want)
	}
}

func TestAnthropicRequestToOpenAICacheControlIsOptIn(t *testing.T) {
	in := []byte(`{"model":"m","max_tokens":10,"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":"hi"}]}`)

	plain, err := Request(core.FormatAnthropic, core.FormatOpenAI, in, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "cache_control") {
		t.Fatalf("the marker must not reach a server that did not declare it: %s", plain)
	}

	kept, err := Request(core.FormatAnthropic, core.FormatOpenAI, in, Options{KeepCacheControl: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(kept), "cache_control") {
		t.Fatalf("supports_cache_control must carry the marker across: %s", kept)
	}
}

func TestAnthropicThinkingBecomesReasoningEffort(t *testing.T) {
	for _, tc := range []struct {
		budget int
		want   string
	}{{1024, "low"}, {8000, "medium"}, {32000, "high"}} {
		in := []byte(`{"model":"m","max_tokens":100000,"messages":[{"role":"user","content":"hi"}],
			"thinking":{"type":"enabled","budget_tokens":` + itoa(tc.budget) + `}}`)
		out, err := Request(core.FormatAnthropic, core.FormatOpenAI, in, Options{})
		if err != nil {
			t.Fatal(err)
		}
		var got openAIRequest
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if got.ReasoningEffort != tc.want {
			t.Fatalf("budget %d -> %q, want %q", tc.budget, got.ReasoningEffort, tc.want)
		}
	}
}

func TestOpenAIRequestToAnthropicHoistsSystemAndCapsOutput(t *testing.T) {
	in := []byte(`{"model":"m","messages":[
		{"role":"system","content":"be terse"},
		{"role":"user","content":"hi"},
		{"role":"system","content":"also be kind"}
	]}`)
	out, err := Request(core.FormatOpenAI, core.FormatAnthropic, in, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got anthropicRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	var system string
	if err := json.Unmarshal(got.System, &system); err != nil {
		t.Fatal(err)
	}
	if system != "be terse\n\nalso be kind" {
		t.Fatalf("system = %q; every system message must be hoisted, in order", system)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v", got.Messages)
	}
	// max_tokens is required by the Messages API and absent from the original.
	// Refusing a request every OpenAI server would have accepted is the worse
	// answer, so a cap is supplied.
	if got.MaxTokens != defaultMaxTokens {
		t.Fatalf("max_tokens = %d, want the default %d", got.MaxTokens, defaultMaxTokens)
	}

	bounded, err := Request(core.FormatOpenAI, core.FormatAnthropic, in, Options{DefaultMaxTokens: 256})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bounded, &got); err != nil {
		t.Fatal(err)
	}
	if got.MaxTokens != 256 {
		t.Fatalf("configured default ignored: max_tokens = %d", got.MaxTokens)
	}
}

func TestOpenAIRequestToAnthropicMergesToolResultsIntoOneTurn(t *testing.T) {
	// Two parallel tool calls answered by two tool messages. The Messages API
	// refuses consecutive turns of the same role, so both results have to land
	// in one user turn or the whole conversation is rejected.
	in := []byte(`{"model":"m","messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":null,"tool_calls":[
			{"index":0,"id":"call_a","type":"function","function":{"name":"f","arguments":"{\"x\":1}"}},
			{"index":1,"id":"call_b","type":"function","function":{"name":"g","arguments":"{\"y\":2}"}}
		]},
		{"role":"tool","tool_call_id":"call_a","content":"4C"},
		{"role":"tool","tool_call_id":"call_b","content":"rain"}
	]}`)
	out, err := Request(core.FormatOpenAI, core.FormatAnthropic, in, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got anthropicRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("want user/assistant/user, got %d turns: %s", len(got.Messages), out)
	}
	if got.Messages[2].Role != "user" {
		t.Fatalf("results belong to a user turn, got %q", got.Messages[2].Role)
	}
	var results []anthropicBlock
	if err := json.Unmarshal(got.Messages[2].Content, &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].ToolUseID != "call_a" || results[1].ToolUseID != "call_b" {
		t.Fatalf("both results must merge into one turn, in order: %+v", results)
	}

	var calls []anthropicBlock
	if err := json.Unmarshal(got.Messages[1].Content, &calls); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0].Type != "tool_use" || string(calls[0].Input) != `{"x":1}` {
		t.Fatalf("tool calls = %+v", calls)
	}
}

func TestOpenAIToolChoiceNoneWithholdsTools(t *testing.T) {
	// The Messages API has no "none". Withholding the tools is the only
	// faithful way to say it: the model then cannot call one.
	in := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"none"}`)
	out, err := Request(core.FormatOpenAI, core.FormatAnthropic, in, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got anthropicRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 0 || got.ToolChoice != nil {
		t.Fatalf("tool_choice none must withhold the tools, got %+v / %+v", got.Tools, got.ToolChoice)
	}
}

func TestOpenAIToolWithoutParametersGetsAnEmptySchema(t *testing.T) {
	in := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"now"}}],"tool_choice":"required"}`)
	out, err := Request(core.FormatOpenAI, core.FormatAnthropic, in, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got anthropicRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 1 || len(got.Tools[0].InputSchema) == 0 {
		t.Fatalf("input_schema is required and must be synthesized: %+v", got.Tools)
	}
	if got.ToolChoice == nil || got.ToolChoice.Type != "any" {
		t.Fatalf("tool_choice = %+v, want any", got.ToolChoice)
	}
}

func TestReasoningEffortStandsDownWhenTheCapIsTooSmall(t *testing.T) {
	// Anthropic refuses a budget below 1024, and one that leaves no room for a
	// reply. A small cap therefore means no thinking rather than a request the
	// upstream rejects outright.
	in := []byte(`{"model":"m","max_tokens":512,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	out, err := Request(core.FormatOpenAI, core.FormatAnthropic, in, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got anthropicRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Thinking != nil {
		t.Fatalf("thinking must not be enabled under a 512-token cap: %+v", got.Thinking)
	}

	roomy := []byte(`{"model":"m","max_tokens":40000,"temperature":0.5,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	out, err = Request(core.FormatOpenAI, core.FormatAnthropic, roomy, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Thinking == nil || got.Thinking.BudgetTokens != 16384 {
		t.Fatalf("thinking = %+v, want a 16384-token budget", got.Thinking)
	}
	// Anthropic refuses temperature outright while thinking is on, so the knob
	// the caller set has to go rather than the reasoning they asked for.
	if got.Temperature != nil {
		t.Fatal("temperature must be dropped when extended thinking is enabled")
	}
}

func TestOpenAIResponseToAnthropicCarvesCachedTokensOutOfInput(t *testing.T) {
	// The single most expensive thing to get wrong. prompt_tokens includes the
	// cached tokens; input_tokens excludes them. Copying the number across
	// would bill every cached token twice — once at the full input rate inside
	// input_tokens, once at the cache rate beside it.
	in := []byte(`{"id":"chatcmpl-1","model":"gpt-5","choices":[
		{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1000,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":800}}}`)
	out, err := Response(core.FormatOpenAI, core.FormatAnthropic, in)
	if err != nil {
		t.Fatal(err)
	}
	var got anthropicResponse
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Usage.InputTokens != 200 {
		t.Fatalf("input_tokens = %d, want 1000 - 800", got.Usage.InputTokens)
	}
	if got.Usage.CacheReadInputTokens != 800 {
		t.Fatalf("cache_read_input_tokens = %d, want 800", got.Usage.CacheReadInputTokens)
	}
	if got.Usage.OutputTokens != 20 {
		t.Fatalf("output_tokens = %d", got.Usage.OutputTokens)
	}
	if got.Type != "message" || got.Role != "assistant" || got.StopReason != "end_turn" {
		t.Fatalf("envelope = %+v", got)
	}
	if len(got.Content) != 1 || got.Content[0].Type != "text" || got.Content[0].Text != "hello" {
		t.Fatalf("content = %+v", got.Content)
	}
	if !strings.HasPrefix(got.ID, "msg_") {
		t.Fatalf("id = %q, want an Anthropic-shaped identifier", got.ID)
	}
}

func TestUsageClampsAnUpstreamThatOverReportsItsCache(t *testing.T) {
	// A provider reporting more cached tokens than prompt tokens would
	// otherwise mint cache savings out of a typo upstream, and drive input
	// negative.
	got := usageToAnthropic(&openAIUsage{
		PromptTokens:        100,
		CompletionTokens:    5,
		PromptTokensDetails: &openAIPromptDetails{CachedTokens: 400, CacheCreationInputTokens: 400},
	})
	if got.InputTokens != 0 || got.CacheReadInputTokens != 100 || got.CacheCreationInputTokens != 0 {
		t.Fatalf("usage = %+v; every prompt token must be counted exactly once", got)
	}
}

func TestOpenAIResponseToAnthropicToolCalls(t *testing.T) {
	in := []byte(`{"id":"chatcmpl-2","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,
		"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},
		"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	out, err := Response(core.FormatOpenAI, core.FormatAnthropic, in)
	if err != nil {
		t.Fatal(err)
	}
	var got anthropicResponse
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", got.StopReason)
	}
	if len(got.Content) != 1 || got.Content[0].Type != "tool_use" {
		t.Fatalf("content = %+v", got.Content)
	}
	if got.Content[0].ID != "toolu_call_1" {
		t.Fatalf("tool id = %q", got.Content[0].ID)
	}
	if string(got.Content[0].Input) != `{"a":1}` {
		t.Fatalf("input = %s, want the arguments document unchanged", got.Content[0].Input)
	}
}

func TestAnthropicResponseToOpenAISumsCacheCountersIntoPromptTokens(t *testing.T) {
	in := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[
		{"type":"text","text":"hi"}],"stop_reason":"max_tokens",
		"usage":{"input_tokens":200,"output_tokens":20,"cache_read_input_tokens":800,"cache_creation_input_tokens":100}}`)
	out, err := Response(core.FormatAnthropic, core.FormatOpenAI, in)
	if err != nil {
		t.Fatal(err)
	}
	var got openAIResponse
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Usage.PromptTokens != 1100 {
		t.Fatalf("prompt_tokens = %d, want 200 + 800 + 100", got.Usage.PromptTokens)
	}
	if got.Usage.PromptTokensDetails == nil || got.Usage.PromptTokensDetails.CachedTokens != 800 {
		t.Fatalf("cached_tokens = %+v, want 800 broken out beside the total", got.Usage.PromptTokensDetails)
	}
	if got.Choices[0].FinishReason != "length" {
		t.Fatalf("finish_reason = %q, want length", got.Choices[0].FinishReason)
	}
}

func TestErrorEnvelopeIsRewrappedAndTheWordingSurvives(t *testing.T) {
	// Claude Code matches on the upstream's own error wording to decide whether
	// to retry with a capability disabled, so only the envelope may change.
	const message = "This model does not support the 'max_tokens' parameter"
	in := []byte(`{"error":{"message":"` + message + `","type":"invalid_request_error","code":"unsupported_parameter"}}`)
	out := Error(core.FormatOpenAI, core.FormatAnthropic, in)

	got := decode(t, out)
	if got["type"] != "error" {
		t.Fatalf("want an anthropic error envelope, got %s", out)
	}
	inner, _ := got["error"].(map[string]any)
	if inner["message"] != message {
		t.Fatalf("message = %v, want it byte for byte", inner["message"])
	}
	if inner["type"] != "invalid_request_error" {
		t.Fatalf("type = %v", inner["type"])
	}

	back := decode(t, Error(core.FormatAnthropic, core.FormatOpenAI, out))
	outer, _ := back["error"].(map[string]any)
	if outer["message"] != message {
		t.Fatalf("round trip lost the wording: %v", outer["message"])
	}
}

func TestErrorPassesThroughWhenItIsNotAnEnvelope(t *testing.T) {
	// An unparseable error is still better relayed than replaced.
	in := []byte(`<html>502 Bad Gateway</html>`)
	if got := Error(core.FormatOpenAI, core.FormatAnthropic, in); string(got) != string(in) {
		t.Fatalf("got %s, want the body unchanged", got)
	}
}

// ---------- streaming ----------

// collect drains a translated stream into one string.
func collect(t *testing.T, from, to core.Format, sse string) string {
	t.Helper()
	out, err := io.ReadAll(Stream(from, to, io.NopCloser(strings.NewReader(sse))))
	if err != nil {
		t.Fatalf("read translated stream: %v", err)
	}
	return string(out)
}

// events returns the decoded payload of every data: line, in order.
func events(t *testing.T, sse string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(sse, "\n") {
		payload := eventPayload([]byte(strings.TrimSpace(line)))
		if payload == nil {
			continue
		}
		out = append(out, decode(t, payload))
	}
	return out
}

func TestOpenAIStreamToAnthropicBracketsTextAndTools(t *testing.T) {
	// Text, then a tool call, in one reply. OpenAI leaves the boundary between
	// them implicit; Anthropic requires it to be stated, and a client that
	// receives a delta for a block that was never opened treats the stream as
	// corrupt.
	in := strings.Join([]string{
		`data: {"id":"chatcmpl-9","object":"chat.completion.chunk","model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","content":"Let me "}}]}`,
		`data: {"id":"chatcmpl-9","choices":[{"index":0,"delta":{"content":"check."}}]}`,
		`data: {"id":"chatcmpl-9","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":"}}]}}]}`,
		`data: {"id":"chatcmpl-9","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`data: {"id":"chatcmpl-9","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"id":"chatcmpl-9","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":40}}}`,
		`data: [DONE]`,
		"",
	}, "\n\n")

	out := collect(t, core.FormatOpenAI, core.FormatAnthropic, in)
	evs := events(t, out)

	var types []string
	for _, e := range evs {
		types = append(types, e["type"].(string))
	}
	// Text streams as it arrives; the tool call is assembled and emitted whole
	// at the end, so its arguments can never be spliced with another call's.
	want := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_delta",
		"content_block_stop",
		"content_block_start", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence:\n got %v\nwant %v\n\nstream:\n%s", types, want, out)
	}

	// Every event must also name itself on its own event: line, which is what
	// an Anthropic client dispatches on.
	if !strings.Contains(out, "event: message_start\n") {
		t.Fatalf("missing SSE event names:\n%s", out)
	}

	tool := evs[5]["content_block"].(map[string]any)
	if tool["type"] != "tool_use" || tool["id"] != "toolu_call_1" || tool["name"] != "f" {
		t.Fatalf("tool block = %v", tool)
	}
	if idx := evs[5]["index"].(float64); idx != 1 {
		t.Fatalf("the tool block must take the next index, got %v", idx)
	}
	// Both argument fragments arrive as one document, in order.
	if got := evs[6]["delta"].(map[string]any)["partial_json"]; got != `{"a":1}` {
		t.Fatalf("arguments = %v, want the fragments joined in order", got)
	}

	// Usage arrives after the finish reason on most OpenAI-compatible servers,
	// so the message may not be closed until the stream ends — otherwise every
	// streamed reply is billed as nothing.
	usage := evs[8]["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 10 {
		t.Fatalf("input_tokens = %v, want 50 - 40", usage["input_tokens"])
	}
	if usage["cache_read_input_tokens"].(float64) != 40 {
		t.Fatalf("cache_read_input_tokens = %v", usage["cache_read_input_tokens"])
	}
	if evs[8]["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v", evs[8]["delta"])
	}
}

func TestOpenAIStreamToAnthropicKeepsTwoToolCallsApart(t *testing.T) {
	// A server emitting two calls may interleave their argument fragments.
	// Keying on arrival order rather than the upstream's own index would splice
	// one call's arguments into the other's, producing two tool calls whose
	// inputs are both invalid JSON.
	in := strings.Join([]string{
		`data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"{\"x\":"}}]}}]}`,
		`data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"g","arguments":"{\"y\":2}"}}]}}]}`,
		`data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n")

	evs := events(t, collect(t, core.FormatOpenAI, core.FormatAnthropic, in))

	type block struct {
		id, name, args string
	}
	var blocks []block
	for _, e := range evs {
		switch e["type"] {
		case "content_block_start":
			cb := e["content_block"].(map[string]any)
			blocks = append(blocks, block{id: cb["id"].(string), name: cb["name"].(string)})
		case "content_block_delta":
			d := e["delta"].(map[string]any)
			if partial, ok := d["partial_json"].(string); ok {
				blocks[len(blocks)-1].args += partial
			}
		}
	}
	want := []block{
		{id: "toolu_a", name: "f", args: `{"x":1}`},
		{id: "toolu_b", name: "g", args: `{"y":2}`},
	}
	if len(blocks) != len(want) {
		t.Fatalf("want one block per call, got %+v", blocks)
	}
	for i, w := range want {
		if blocks[i] != w {
			t.Fatalf("block %d = %+v, want %+v", i, blocks[i], w)
		}
	}
}

func TestOpenAIStreamToAnthropicClosesAnAbandonedStream(t *testing.T) {
	// The upstream died after one delta and never sent [DONE]. A client holding
	// an unterminated message waits for an event that is never coming.
	in := "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"
	evs := events(t, collect(t, core.FormatOpenAI, core.FormatAnthropic, in))
	last := evs[len(evs)-1]
	if last["type"] != "message_stop" {
		t.Fatalf("stream must be closed on EOF, ended with %v", last["type"])
	}
}

func TestOpenAIStreamErrorBecomesAnAnthropicErrorEvent(t *testing.T) {
	in := "data: {\"error\":{\"message\":\"overloaded\",\"type\":\"server_error\"}}\n\n"
	out := collect(t, core.FormatOpenAI, core.FormatAnthropic, in)
	if !strings.Contains(out, "event: error\n") {
		t.Fatalf("want an anthropic error event:\n%s", out)
	}
	if !strings.Contains(out, "overloaded") {
		t.Fatalf("the upstream's wording must survive:\n%s", out)
	}
}

func TestAnthropicStreamToOpenAINumbersToolCallsSeparately(t *testing.T) {
	// Anthropic indexes text and tool calls in one sequence; OpenAI numbers
	// tool calls in a sequence of their own, and a client assembling arguments
	// keys on that number. Handing it Anthropic's index leaves a gap the client
	// reads as a missing call.
	in := strings.Join([]string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_7","model":"claude","usage":{"input_tokens":30,"output_tokens":0,"cache_read_input_tokens":10}}}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"f","input":{}}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":1}`,
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":12}}`,
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
		"",
	}, "\n\n")

	out := collect(t, core.FormatAnthropic, core.FormatOpenAI, in)
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("an OpenAI stream ends with its sentinel:\n%s", out)
	}
	// An OpenAI stream names no events; a client reading one would treat an
	// event: line as a protocol error.
	if strings.Contains(out, "event: ") {
		t.Fatalf("event names must not survive into an OpenAI stream:\n%s", out)
	}

	evs := events(t, out)
	var toolIndexes []float64
	var text strings.Builder
	for _, e := range evs {
		choices, _ := e["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
		if c, ok := delta["content"].(string); ok {
			text.WriteString(c)
		}
		for _, call := range toAnySlice(delta["tool_calls"]) {
			toolIndexes = append(toolIndexes, call.(map[string]any)["index"].(float64))
		}
	}
	if text.String() != "hi" {
		t.Fatalf("text = %q", text.String())
	}
	// The tool is Anthropic content block 1 and OpenAI tool call 0.
	for _, idx := range toolIndexes {
		if idx != 0 {
			t.Fatalf("tool call index = %v, want 0", idx)
		}
	}

	final := evs[len(evs)-1]
	usage := final["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) != 40 {
		t.Fatalf("prompt_tokens = %v, want 30 + 10", usage["prompt_tokens"])
	}
	if usage["completion_tokens"].(float64) != 12 {
		t.Fatalf("completion_tokens = %v", usage["completion_tokens"])
	}
}

func TestAnthropicPingBecomesAComment(t *testing.T) {
	// A ping carries no content, but it is the only traffic during a long
	// thinking pause, and a client counting silence against a timeout needs the
	// bytes to keep arriving.
	in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"m\"}}\n\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n"
	out := collect(t, core.FormatAnthropic, core.FormatOpenAI, in)
	if !strings.Contains(out, ": ping\n\n") {
		t.Fatalf("a ping must still put bytes on the wire:\n%s", out)
	}
}

func TestNonStreamingBodyIsTranslatedWhole(t *testing.T) {
	in := `{"id":"chatcmpl-3","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`
	out, err := ReadResponse(core.FormatOpenAI, core.FormatAnthropic, strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if decode(t, out)["type"] != "message" {
		t.Fatalf("want a translated message, got %s", out)
	}

	// A reply that cannot be rewritten is an error rather than a body, so the
	// caller can still answer with a status the client understands. Relaying
	// the original would hand a client a document its own format does not
	// define, which reads to it as a corrupt reply from its own provider.
	if _, err := ReadResponse(core.FormatOpenAI, core.FormatAnthropic, strings.NewReader("not json")); err == nil {
		t.Fatal("an untranslatable reply must surface as an error")
	}
}

func TestSameFormatIsNotTouched(t *testing.T) {
	// The ordinary path must stay the zero-copy relay it was.
	body := io.NopCloser(strings.NewReader("anything at all"))
	if got := Stream(core.FormatAnthropic, core.FormatAnthropic, body); got != body {
		t.Fatal("a same-format relay must hand back the very same reader")
	}
	in := []byte(`{"model":"m"}`)
	out, err := Request(core.FormatOpenAI, core.FormatOpenAI, in, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if &out[0] != &in[0] {
		t.Fatal("a same-format request must share the caller's slice")
	}
}

func toAnySlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
