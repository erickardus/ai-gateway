package promptcache

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/jsonx"
)

// peek is what the server does before routing: Fingerprint reads the fields
// that walk already produced rather than re-parsing the body.
func peek(t testing.TB, body []byte) jsonx.Fields {
	fields, err := jsonx.Peek(body)
	if err != nil {
		return jsonx.Fields{}
	}
	return fields
}

// conversation renders a request whose prefix — system, tools and opening turns
// — is fixed, and whose tail grows as turns are appended.
func conversation(system string, turns ...string) []byte {
	messages := make([]any, 0, len(turns))
	for _, t := range turns {
		messages = append(messages, map[string]any{"role": "user", "content": t})
	}
	body, err := json.Marshal(map[string]any{
		"model":    "anthropic-claude",
		"system":   []any{map[string]any{"type": "text", "text": system}},
		"tools":    []any{map[string]any{"name": "read_file"}},
		"messages": messages,
	})
	if err != nil {
		panic(err)
	}
	return body
}

// The whole point of the fingerprint: it must survive the thing that changes on
// every turn, or a conversation is re-pinned to a new deployment each time and
// pays a cache write instead of a cache read.
func TestFingerprintIsStableAsAConversationGrows(t *testing.T) {
	first, ok := Fingerprint(core.FormatAnthropic, "m", peek(t, conversation("be helpful", "one", "two")))
	if !ok {
		t.Fatal("Fingerprint: ok = false, want true")
	}
	later, ok := Fingerprint(core.FormatAnthropic, "m", peek(t, conversation("be helpful", "one", "two", "three", "four")))
	if !ok {
		t.Fatal("Fingerprint on a longer conversation: ok = false")
	}
	if first != later {
		t.Errorf("fingerprint changed as the conversation grew: %s then %s", first, later)
	}
}

func TestFingerprintDiscriminates(t *testing.T) {
	base := conversation("be helpful", "one", "two")
	baseline, _ := Fingerprint(core.FormatAnthropic, "m", peek(t, base))

	tests := []struct {
		name  string
		model string
		body  []byte
	}{
		{"different system", "m", conversation("be terse", "one", "two")},
		{"different opening turn", "m", conversation("be helpful", "different", "two")},
		{"different model group", "other", base},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Fingerprint(core.FormatAnthropic, tt.model, peek(t, tt.body))
			if !ok {
				t.Fatal("ok = false, want true")
			}
			if got == baseline {
				t.Error("fingerprint matched a request with a different prefix")
			}
		})
	}

	// Two formats can carry the same bytes and mean different things, so they
	// must never share a pin.
	openai, _ := Fingerprint(core.FormatOpenAI, "m", peek(t, base))
	if openai == baseline {
		t.Error("fingerprint ignored the wire format")
	}
}

// TestFingerprintReadsTheTurnThatDiscriminates covers the one place the two wire
// formats need different treatment.
//
// Under the Anthropic format the opening user turn is the distinguishing one, so
// the fingerprint stops there and two requests that differ only from the second
// turn onwards share a pin. That is deliberate rather than a loss of precision:
// they carry the same system prompt, the same tools and the same opening turn,
// which is the prefix an upstream actually holds warm, so the same deployment is
// where both of them belong. Reading further would buy nothing and would cost
// the property the whole feature rests on — a first turn carries one message
// where every later turn carries three, so a two-message lead would fingerprint
// a conversation's opening request differently from its own second one.
//
// Under OpenAI the first message is a system prompt many callers share, so it is
// the second that has to be read.
func TestFingerprintReadsTheTurnThatDiscriminates(t *testing.T) {
	anthropic := func(second string) string {
		fp, ok := Fingerprint(core.FormatAnthropic, "m", peek(t, conversation("be helpful", "one", second)))
		if !ok {
			t.Fatal("Fingerprint: ok = false")
		}
		return fp
	}
	if anthropic("two") != anthropic("different") {
		t.Error("an Anthropic request was re-pinned over a turn behind the cached prefix")
	}

	openai := func(second string) string {
		body, err := json.Marshal(map[string]any{
			"model": "openai-gpt",
			"messages": []any{
				map[string]any{"role": "system", "content": "be helpful"},
				map[string]any{"role": "user", "content": second},
			},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		fp, ok := Fingerprint(core.FormatOpenAI, "m", peek(t, body))
		if !ok {
			t.Fatal("Fingerprint: ok = false")
		}
		return fp
	}
	if openai("one") == openai("another") {
		t.Error("two OpenAI conversations behind one shared system message share a pin")
	}
}

// Prompt text must not leak into routing state, which with Redis configured
// leaves the process entirely.
func TestFingerprintCarriesNoPromptText(t *testing.T) {
	got, ok := Fingerprint(core.FormatAnthropic, "m", peek(t, conversation("a distinctive secret", "one")))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "distinctive") {
		t.Errorf("fingerprint contains prompt text: %s", got)
	}
	if len(got) != 64 {
		t.Errorf("fingerprint = %q, want a 64-character hex digest", got)
	}
}

func TestFingerprintDeclinesRequestsWithNoPrefix(t *testing.T) {
	for _, body := range []string{
		`{"model":"m"}`,
		`{"model":"m","messages":[]}`,
		`{not json`,
	} {
		if got, ok := Fingerprint(core.FormatAnthropic, "m", peek(t, []byte(body))); ok {
			t.Errorf("%s: ok = true (%s), want false", body, got)
		}
	}
}

// A request without an explicit "system" still has a cacheable prefix in its
// tools and opening turns, which is exactly the OpenAI shape.
func TestFingerprintUsesLeadingMessagesWhenThereIsNoSystemBlock(t *testing.T) {
	a := []byte(`{"messages":[{"role":"system","content":"shared"},{"role":"user","content":"one"},{"role":"user","content":"x"}]}`)
	b := []byte(`{"messages":[{"role":"system","content":"shared"},{"role":"user","content":"two"},{"role":"user","content":"x"}]}`)

	fa, ok := Fingerprint(core.FormatOpenAI, "m", peek(t, a))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	fb, _ := Fingerprint(core.FormatOpenAI, "m", peek(t, b))
	if fa == fb {
		t.Error("two conversations sharing only a system message got the same pin")
	}
}

const longSystem = "You are a careful assistant. " +
	"Repeat this guidance until it is long enough to be worth caching upstream. " +
	"Repeat this guidance until it is long enough to be worth caching upstream. " +
	"Repeat this guidance until it is long enough to be worth caching upstream. "

func TestInjectMarksToolsAndSystem(t *testing.T) {
	body := []byte(`{"model":"m","system":[{"type":"text","text":"` + longSystem + `"}],"tools":[{"name":"a"},{"name":"b"}],"messages":[]}`)

	got, changed, err := Inject(body, 0)
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if !json.Valid(got) {
		t.Fatalf("result is not valid JSON: %s", got)
	}
	if n := strings.Count(string(got), `"cache_control"`); n != 2 {
		t.Errorf("placed %d breakpoints, want 2 (end of tools, end of system)", n)
	}
	// The breakpoint belongs at the end of the prefix, not the start: a
	// breakpoint on the first tool caches only that tool.
	if !strings.Contains(string(got), `{"name":"b","cache_control":{"type":"ephemeral"}}`) {
		t.Errorf("breakpoint is not on the last tool: %s", got)
	}
	if strings.Contains(string(got), `{"name":"a","cache_control"`) {
		t.Errorf("breakpoint placed on a tool that is not last: %s", got)
	}
	if !strings.Contains(string(got), `"messages":[]`) {
		t.Errorf("messages were altered: %s", got)
	}
}

// The Messages API accepts a bare string system prompt, which has nowhere to
// hang a breakpoint. Promoting it to the single text block it is defined to
// equal is what makes that request cacheable at all.
func TestInjectPromotesAStringSystemPrompt(t *testing.T) {
	body := []byte(`{"model":"m","system":` + jsonString(longSystem) + `}`)

	got, changed, err := Inject(body, 0)
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}

	var decoded struct {
		System []struct {
			Type         string `json:"type"`
			Text         string `json:"text"`
			CacheControl *struct {
				Type string `json:"type"`
			} `json:"cache_control"`
		} `json:"system"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("result does not decode: %v (%s)", err, got)
	}
	if len(decoded.System) != 1 {
		t.Fatalf("system has %d blocks, want 1", len(decoded.System))
	}
	if decoded.System[0].Type != "text" || decoded.System[0].Text != longSystem {
		t.Errorf("system text changed: %+v", decoded.System[0])
	}
	if decoded.System[0].CacheControl == nil || decoded.System[0].CacheControl.Type != "ephemeral" {
		t.Errorf("no ephemeral breakpoint: %+v", decoded.System[0])
	}
}

func TestInjectLeavesRequestsAlone(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		minBytes int
	}{
		{
			// A caller that placed its own breakpoints knows where its prompt
			// repeats, and the API caps how many a request may carry.
			name:     "caller already marked the prompt",
			body:     `{"system":[{"type":"text","text":"` + longSystem + `","cache_control":{"type":"ephemeral"}}]}`,
			minBytes: 0,
		},
		{
			name:     "prefix too short to cache upstream",
			body:     `{"system":[{"type":"text","text":"hi"}]}`,
			minBytes: 4096,
		},
		{"no prefix at all", `{"model":"m","messages":[]}`, 0},
		{"system is an empty array", `{"system":[]}`, 0},
		{"system is an unexpected shape", `{"system":[42]}`, 0},
		{"tools is not an array", `{"tools":{"name":"a"}}`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed, err := Inject([]byte(tt.body), tt.minBytes)
			if err != nil {
				t.Fatalf("Inject: %v", err)
			}
			if changed {
				t.Errorf("changed = true, want false: %s", got)
			}
			if string(got) != tt.body {
				t.Errorf("body was edited: %s", got)
			}
		})
	}
}

// A body the editor cannot read is forwarded exactly as it arrived. An
// optimization that cannot be applied is not a reason to refuse a request.
func TestInjectReturnsTheOriginalBodyOnError(t *testing.T) {
	body := []byte(`{"system":`)
	got, changed, err := Inject(body, 0)
	if err == nil {
		t.Error("err = nil, want an error for a malformed body")
	}
	if changed {
		t.Error("changed = true, want false")
	}
	if string(got) != string(body) {
		t.Errorf("body = %s, want it returned unchanged", got)
	}
}

// Injection must not disturb the bytes it is not there to change: the upstream
// cache keys on them.
func TestInjectPreservesEverythingElse(t *testing.T) {
	body := []byte("{\n  \"model\": \"m\",\n  \"temperature\": 1.50,\n  \"system\": [{\"type\":\"text\",\"text\":\"" + longSystem + "\"}]\n}")

	got, changed, err := Inject(body, 0)
	if err != nil || !changed {
		t.Fatalf("Inject: changed=%v err=%v", changed, err)
	}
	if !strings.Contains(string(got), `"temperature": 1.50`) {
		t.Errorf("an unrelated number was renormalized: %s", got)
	}
	if !strings.HasPrefix(string(got), "{\n  \"model\": \"m\",") {
		t.Errorf("unrelated formatting changed: %s", got)
	}
}

func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// The fingerprint runs on every request of a load-balanced group, so what it
// costs on a realistically large body is worth keeping in view.
func BenchmarkFingerprint(b *testing.B) {
	fields := peek(b, benchBody())
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := Fingerprint(core.FormatAnthropic, "anthropic-claude", fields); !ok {
			b.Fatal("ok = false")
		}
	}
}

// The walk Fingerprint reads from is one the router performs anyway, but it is
// the cost that grows with body size, so it is measured alongside.
func BenchmarkPeekAndFingerprint(b *testing.B) {
	body := benchBody()
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		fields, err := jsonx.Peek(body)
		if err != nil {
			b.Fatalf("Peek: %v", err)
		}
		if _, ok := Fingerprint(core.FormatAnthropic, "anthropic-claude", fields); !ok {
			b.Fatal("ok = false")
		}
	}
}

func BenchmarkInject(b *testing.B) {
	body := benchBody()
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, changed, err := Inject(body, 4096); err != nil || !changed {
			b.Fatalf("changed=%v err=%v", changed, err)
		}
	}
}

// benchBody approximates a Claude Code request: a large system prompt, a dozen
// tool definitions, and a conversation of some length.
func benchBody() []byte {
	tools := make([]any, 0, 12)
	for i := range 12 {
		tools = append(tools, map[string]any{
			"name":         "tool_" + strings.Repeat("x", i+1),
			"description":  strings.Repeat("what this tool does. ", 40),
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
		})
	}
	messages := make([]any, 0, 40)
	for i := range 40 {
		messages = append(messages, map[string]any{
			"role":    []string{"user", "assistant"}[i%2],
			"content": strings.Repeat("a turn of conversation. ", 60),
		})
	}
	body, err := json.Marshal(map[string]any{
		"model":    "anthropic-claude",
		"system":   []any{map[string]any{"type": "text", "text": strings.Repeat("system guidance. ", 500)}},
		"tools":    tools,
		"messages": messages,
	})
	if err != nil {
		panic(err)
	}
	return body
}

// TestFingerprintIgnoresAMovingBreakpoint is the property affinity depends on
// for the traffic this gateway primarily carries.
//
// Claude Code, the SDKs' automatic caching and the documented multi-turn
// pattern all mark the last message of the conversation, so the breakpoint
// walks forward as turns accumulate while the prompt behind it does not change.
// A fingerprint that moved with it would call every turn a new prefix: each one
// would be free to land on a cold deployment, and the conversation would pay a
// cache write per turn on the largest prefix it has.
func TestFingerprintIgnoresAMovingBreakpoint(t *testing.T) {
	// The system block and the tool table are marked once and never move; the
	// marker on the trailing message is the one that walks.
	turn := func(messages string) []byte {
		return []byte(`{"model":"m",` +
			`"system":[{"type":"text","text":"S","cache_control":{"type":"ephemeral"}}],` +
			`"tools":[{"name":"read_file","cache_control":{"type":"ephemeral"}}],` +
			`"messages":[` + messages + `]}`)
	}
	marked := func(text string) string {
		return `{"role":"user","content":[{"type":"text","text":"` + text + `","cache_control":{"type":"ephemeral"}}]}`
	}
	plain := func(text string) string {
		return `{"role":"user","content":[{"type":"text","text":"` + text + `"}]}`
	}

	bodies := [][]byte{
		turn(marked("one")),
		turn(plain("one") + "," + plain("two") + "," + marked("three")),
		turn(plain("one") + "," + plain("two") + "," + plain("three") + "," + marked("four")),
	}

	fingerprints := make([]string, len(bodies))
	for i, body := range bodies {
		fields, err := jsonx.Peek(body)
		if err != nil {
			t.Fatalf("turn %d: Peek: %v", i+1, err)
		}
		fp, ok := Fingerprint(core.FormatAnthropic, "m", fields)
		if !ok {
			t.Fatalf("turn %d: no fingerprint for a request carrying a system prompt", i+1)
		}
		fingerprints[i] = fp
	}

	for i, fp := range fingerprints[1:] {
		if fp != fingerprints[0] {
			t.Errorf("turn %d fingerprinted as %s, turn 1 as %s: the same conversation must pin to one deployment",
				i+2, fp[:12], fingerprints[0][:12])
		}
	}
}

// TestFingerprintStillSeparatesDifferentPrompts guards the other half: ignoring
// breakpoints must not blur two conversations into one pin, which would
// concentrate unrelated traffic on a single deployment.
func TestFingerprintStillSeparatesDifferentPrompts(t *testing.T) {
	body := func(system string) []byte {
		return []byte(`{"model":"m","system":[{"type":"text","text":"` + system +
			`","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`)
	}
	fingerprint := func(b []byte) string {
		fields, err := jsonx.Peek(b)
		if err != nil {
			t.Fatalf("Peek: %v", err)
		}
		fp, ok := Fingerprint(core.FormatAnthropic, "m", fields)
		if !ok {
			t.Fatal("no fingerprint")
		}
		return fp
	}
	if fingerprint(body("one prompt")) == fingerprint(body("another prompt")) {
		t.Error("two different system prompts share a fingerprint")
	}
}
