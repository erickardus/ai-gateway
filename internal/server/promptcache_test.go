package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
)

// longSystem is past the injection threshold, so a test that expects a
// breakpoint is not silently exercising the "too short to cache" path.
var longSystem = strings.Repeat("You are a careful assistant working in a repository. ", 100)

// promptBody renders a conversation. The prefix — system, tools and the opening
// turns — is what a pin is derived from; appending turns is what a real client
// does between requests.
func promptBody(system string, turns ...string) string {
	messages := make([]any, 0, len(turns))
	for _, turn := range turns {
		messages = append(messages, map[string]any{"role": "user", "content": turn})
	}
	body, err := json.Marshal(map[string]any{
		"model":    "anthropic-claude",
		"system":   []any{map[string]any{"type": "text", "text": system}},
		"tools":    []any{map[string]any{"name": "read_file", "input_schema": map[string]any{"type": "object"}}},
		"messages": messages,
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

// The gateway's reason for pinning: a conversation must keep reaching the
// upstream holding its warm prompt cache even while the group is load balanced.
func TestPromptAffinityKeepsAConversationOnOneDeployment(t *testing.T) {
	// Upstreams that hold a real cache, because a pin follows the evidence that
	// one exists: an upstream reporting no cache activity is one with no warm
	// prefix to send the next turn back to.
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, extraDeployments: 2,
		upstreams: newCachingCluster(t, core.FormatAnthropic, 5_000, 3).handlers(),
	})
	body := promptBody("be helpful", "first turn", "answer")

	first := h.do(t, claudeCodeRequest("/v1/messages", body))
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", first.Code, first.Body.String())
	}
	pinned := first.Header().Get("x-gateway-deployment")
	// The opening turn has no pin to honour yet, which is reported as "new"
	// rather than as a miss.
	if got := first.Header().Get("x-gateway-prompt-affinity"); got != "new" {
		t.Errorf("first request affinity = %q, want new", got)
	}

	// A growing conversation keeps the same prefix, so it keeps the same pin.
	turns := []string{"first turn", "answer"}
	for i, turn := range []string{"second turn", "third turn", "fourth turn"} {
		turns = append(turns, turn)
		rec := h.do(t, claudeCodeRequest("/v1/messages", promptBody("be helpful", turns...)))
		if got := rec.Header().Get("x-gateway-deployment"); got != pinned {
			t.Fatalf("turn %d went to %s, want the pinned %s", i, got, pinned)
		}
		if got := rec.Header().Get("x-gateway-prompt-affinity"); got != "hit" {
			t.Fatalf("turn %d affinity = %q, want hit", i, got)
		}
	}
}

func TestPromptAffinityIsSilentWhenThereIsOneDeployment(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "api_key", allowPassthrough: true})

	rec := h.do(t, claudeCodeRequest("/v1/messages", promptBody("be helpful", "hi")))
	if got := rec.Header().Get("x-gateway-prompt-affinity"); got != "" {
		t.Errorf("affinity header = %q, want it absent with nothing to choose between", got)
	}
}

func TestPromptAffinityCanBeTurnedOff(t *testing.T) {
	off := false
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, extraDeployments: 2,
		promptCache: config.PromptCacheConfig{Affinity: &off},
	})

	rec := h.do(t, claudeCodeRequest("/v1/messages", promptBody("be helpful", "hi")))
	if got := rec.Header().Get("x-gateway-prompt-affinity"); got != "" {
		t.Errorf("affinity header = %q, want it absent when affinity is off", got)
	}
}

func TestInjectionMarksTheStablePrefix(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true,
		promptCache: config.PromptCacheConfig{Inject: true},
	})

	rec := h.do(t, claudeCodeRequest("/v1/messages", promptBody(longSystem, "hi")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	_, forwarded, _, _ := h.seen.get()
	// Three: the end of the tools, the end of the system prompt, and the
	// top-level field that follows the conversation. One short of the cap the
	// API imposes, which is the room a caller's own would have needed — and a
	// caller with any of its own is never injected into at all.
	if n := strings.Count(string(forwarded), `"cache_control"`); n != 3 {
		t.Fatalf("upstream saw %d breakpoints, want 3:\n%s", n, forwarded)
	}
	if !json.Valid(forwarded) {
		t.Fatalf("forwarded body is not valid JSON: %s", forwarded)
	}

	var doc struct {
		CacheControl json.RawMessage   `json:"cache_control"`
		System       []json.RawMessage `json:"system"`
		Tools        []json.RawMessage `json:"tools"`
		Messages     []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(forwarded, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(doc.CacheControl) != `{"type":"ephemeral"}` {
		t.Errorf("the conversation is not cached: %s", forwarded)
	}
	for _, section := range []struct {
		name   string
		blocks []json.RawMessage
	}{{"system", doc.System}, {"tools", doc.Tools}} {
		if !strings.Contains(string(section.blocks[len(section.blocks)-1]), "cache_control") {
			t.Errorf("the last %s block carries no breakpoint: %s", section.name, forwarded)
		}
	}
	for i, m := range doc.Messages {
		if strings.Contains(string(m), "cache_control") {
			t.Errorf("message %d was marked; a breakpoint on a turn is rewritten on the next: %s", i, m)
		}
	}
}

// Token counting takes the same body as inference and answers a different
// question. Asking it to cache the conversation is at best inert, so the field
// that does is confined to the endpoint it belongs to.
func TestCountTokensIsNotAskedToCacheTheConversation(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true,
		promptCache: config.PromptCacheConfig{Inject: true},
	})

	h.do(t, claudeCodeRequest("/v1/messages/count_tokens", promptBody(longSystem, "hi")))

	_, forwarded, _, _ := h.seen.get()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(forwarded, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := doc["cache_control"]; ok {
		t.Errorf("count_tokens was asked to cache the conversation: %s", forwarded)
	}
	// The explicit breakpoints still go on, since they describe the prompt
	// rather than ask the endpoint to do anything.
	if !strings.Contains(string(forwarded), "cache_control") {
		t.Errorf("no breakpoint reached count_tokens at all: %s", forwarded)
	}
}

func TestInjectionIsOffByDefault(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "api_key", allowPassthrough: true})

	h.do(t, claudeCodeRequest("/v1/messages", promptBody(longSystem, "hi")))

	_, forwarded, _, _ := h.seen.get()
	if strings.Contains(string(forwarded), "cache_control") {
		t.Errorf("a breakpoint was added without being asked for:\n%s", forwarded)
	}
}

// A caller that manages its own breakpoints must be forwarded byte for byte.
// Claude Code is exactly that caller.
func TestInjectionLeavesAnAlreadyMarkedBodyAlone(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true,
		promptCache: config.PromptCacheConfig{Inject: true},
	})
	body := `{"model":"anthropic-claude","system":[{"type":"text","text":"` + longSystem + `","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`

	h.do(t, claudeCodeRequest("/v1/messages", body))

	_, forwarded, _, _ := h.seen.get()
	if string(forwarded) != body {
		t.Errorf("body was rewritten:\ngot  %s\nwant %s", forwarded, body)
	}
}

// The OpenAI ingress must not grow Anthropic's breakpoints.
func TestInjectionIsAnthropicOnly(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true,
		promptCache: config.PromptCacheConfig{Inject: true},
	})
	body := `{"model":"anthropic-claude","messages":[{"role":"system","content":"` + longSystem + `"}]}`

	h.do(t, claudeCodeRequest("/v1/chat/completions", body))

	_, forwarded, _, _ := h.seen.get()
	if strings.Contains(string(forwarded), "cache_control") {
		t.Errorf("a breakpoint reached an OpenAI-format request:\n%s", forwarded)
	}
}

const cachedUsage = `{"id":"msg_1","type":"message","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":2000,"cache_creation_input_tokens":100}}`

func TestPromptCacheMetrics(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, extraDeployments: 1,
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(cachedUsage))
		},
	})

	h.do(t, claudeCodeRequest("/v1/messages", promptBody("be helpful", "hi")))
	h.do(t, claudeCodeRequest("/v1/messages", promptBody("be helpful", "again")))

	scrape := h.do(t, httptest.NewRequest(http.MethodGet, "/metrics", nil)).Body.String()
	for _, want := range []string{
		`gateway_prompt_cache_tokens_total`,
		`outcome="read"`,
		`outcome="write"`,
		`gateway_prompt_cache_requests_total`,
		`gateway_prompt_affinity_total`,
	} {
		if !strings.Contains(scrape, want) {
			t.Errorf("scrape is missing %s:\n%s", want, scrape)
		}
	}
	if !strings.Contains(scrape, `gateway_prompt_affinity_total{model="anthropic-claude"`) {
		t.Errorf("affinity counter is not labelled by model:\n%s", scrape)
	}
}

// Savings are reported beside cost, not folded into it: cost is what was
// charged, and this is what was not.
func TestSpendReportsPromptCacheSavings(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, masterKey: "sk-master-TEST",
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(cachedUsage))
		},
	})

	h.do(t, claudeCodeRequest("/v1/messages", promptBody("be helpful", "hi")))

	req := httptest.NewRequest(http.MethodGet, "/spend/keys", nil)
	req.Header.Set("x-gateway-key", "sk-master-TEST")
	rec := h.do(t, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var report struct {
		Entries []struct {
			CacheSavings float64 `json:"cache_savings"`
			Cost         float64 `json:"cost"`
		} `json:"entries"`
		TotalCacheSavings float64 `json:"total_cache_savings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(report.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(report.Entries))
	}

	// 2000 cache-read tokens at $3.00/M input against $0.30/M cache read, less
	// the premium on the 100 tokens this request wrote: $3.75/M against the
	// $3.00/M they would have cost as input.
	const want = 2000*(3.0-0.3)/1_000_000 + 100*(3.0-3.75)/1_000_000
	if diff := report.Entries[0].CacheSavings - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("cache_savings = %v, want %v", report.Entries[0].CacheSavings, want)
	}
	if report.TotalCacheSavings != report.Entries[0].CacheSavings {
		t.Errorf("total_cache_savings = %v, want %v", report.TotalCacheSavings, report.Entries[0].CacheSavings)
	}
	if report.Entries[0].Cost == 0 {
		t.Error("cost = 0; savings must be reported beside what was actually charged, not instead of it")
	}
}

func TestHealthReportsPromptCacheConfiguration(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, extraDeployments: 1,
		promptCache: config.PromptCacheConfig{Inject: true},
	})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("x-gateway-key", testVirtualKey)
	rec := h.do(t, req)

	var health struct {
		PromptCache struct {
			Affinity        bool   `json:"affinity"`
			AffinityTTL     string `json:"affinity_ttl"`
			MaxInFlightLead int    `json:"affinity_max_in_flight_lead"`
			Inject          bool   `json:"inject"`
		} `json:"prompt_cache"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !health.PromptCache.Affinity {
		t.Error("affinity = false, want true for a group with several deployments")
	}
	if health.PromptCache.AffinityTTL != "5m0s" {
		t.Errorf("affinity_ttl = %q, want 5m0s", health.PromptCache.AffinityTTL)
	}
	if health.PromptCache.MaxInFlightLead != 4 {
		t.Errorf("affinity_max_in_flight_lead = %d, want 4", health.PromptCache.MaxInFlightLead)
	}
	if !health.PromptCache.Inject {
		t.Error("inject = false, want true")
	}
}

// A single-deployment gateway reports affinity as inactive rather than on: the
// setting is configured, but there is nothing for it to pin against.
func TestHealthDistinguishesConfiguredFromActiveAffinity(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "api_key", allowPassthrough: true})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("x-gateway-key", testVirtualKey)
	rec := h.do(t, req)

	var health struct {
		PromptCache struct {
			Affinity bool   `json:"affinity"`
			Note     string `json:"affinity_note"`
		} `json:"prompt_cache"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if health.PromptCache.Affinity {
		t.Error("affinity = true, want false with one deployment per group")
	}
	if health.PromptCache.Note == "" {
		t.Error("no note explaining why affinity is inactive")
	}
}

// claudeCodeBody renders a conversation the way Claude Code sends one: a
// breakpoint on the last system block, one on the last tool, and one on the last
// message. The third moves forward with every turn.
func claudeCodeBody(system string, turns ...string) string {
	mark := map[string]any{"type": "ephemeral"}
	messages := make([]any, 0, len(turns))
	for i, turn := range turns {
		block := map[string]any{"type": "text", "text": turn}
		if i == len(turns)-1 {
			block["cache_control"] = mark
		}
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		messages = append(messages, map[string]any{"role": role, "content": []any{block}})
	}
	body, err := json.Marshal(map[string]any{
		"model":    "anthropic-claude",
		"system":   []any{map[string]any{"type": "text", "text": system, "cache_control": mark}},
		"tools":    []any{map[string]any{"name": "read_file", "cache_control": mark}},
		"messages": messages,
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

// TestPromptAffinitySurvivesAMovingBreakpoint starts a conversation where a real
// one starts: a single user message, carrying the breakpoint because it is the
// only thing there is to mark.
//
// On the next turn that marker has moved to the newest message and the opening
// turn carries none. Nothing about the prompt has changed, and the upstream
// still holds it warm — but a fingerprint taken over the raw bytes changes, the
// conversation is re-pinned, and the deployment it warmed is now just one
// candidate among three. This is the shape of the traffic the gateway exists to
// carry, so it is the shape the pin has to survive.
func TestPromptAffinitySurvivesAMovingBreakpoint(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, extraDeployments: 2,
		upstreams: newCachingCluster(t, core.FormatAnthropic, 5_000, 3).handlers(),
	})

	first := h.do(t, claudeCodeRequest("/v1/messages", claudeCodeBody("be helpful", "opening question")))
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", first.Code, first.Body.String())
	}
	pinned := first.Header().Get("x-gateway-deployment")
	if got := first.Header().Get("x-gateway-prompt-affinity"); got != "new" {
		t.Fatalf("opening turn affinity = %q, want new", got)
	}

	turns := []string{"opening question"}
	for i, turn := range []string{"an answer", "second question", "another answer", "third question"} {
		turns = append(turns, turn)
		rec := h.do(t, claudeCodeRequest("/v1/messages", claudeCodeBody("be helpful", turns...)))
		if rec.Code != http.StatusOK {
			t.Fatalf("turn %d: status = %d", i+2, rec.Code)
		}
		if got := rec.Header().Get("x-gateway-prompt-affinity"); got != "hit" {
			t.Errorf("turn %d affinity = %q, want hit: the breakpoint moved but the prompt did not", i+2, got)
		}
		if got := rec.Header().Get("x-gateway-deployment"); got != pinned {
			t.Errorf("turn %d went to %s, leaving the warm cache on %s", i+2, got, pinned)
		}
	}
}

// TestAnUpstreamThatRefusesAnAnnotationStillServesTheRequest covers the risk
// injection creates rather than the money it saves.
//
// The gateway annotates a body for its own benefit: the caller asked for none of
// it. Which shapes a breakpoint may legally hang on differs between providers
// and changes over time — the legacy Bedrock integration refuses the top-level
// field outright — so a guess that turns out to be wrong must cost a round trip
// rather than the request.
func TestAnUpstreamThatRefusesAnAnnotationStillServesTheRequest(t *testing.T) {
	var calls atomic.Int64
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true,
		promptCache: config.PromptCacheConfig{Inject: true},
		upstream: func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			calls.Add(1)
			if strings.Contains(string(body), "cache_control") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"cache_control: unexpected field"}}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"msg_1","type":"message","usage":{"input_tokens":5,"output_tokens":9}}`)
		},
	})

	rec := h.do(t, claudeCodeRequest("/v1/messages", promptBody(longSystem, "hi")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s: the caller lost a request over an optimization it never asked for", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("upstream saw %d requests, want 2: the annotated attempt and the plain retry", got)
	}

	_, forwarded, _, _ := h.seen.get()
	if strings.Contains(string(forwarded), "cache_control") {
		t.Errorf("the retry carried the annotation that was just refused: %s", forwarded)
	}
	if logs := h.logBuf.String(); !strings.Contains(logs, "upstream rejected an annotated request") {
		t.Errorf("nothing in the log names the deployment paying an extra round trip:\n%s", logs)
	}
}

// A 400 the caller earned is still a 400. Retrying it once costs a round trip;
// hiding it would cost the caller their error.
func TestACallersOwnBadRequestIsStillRelayed(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true,
		promptCache: config.PromptCacheConfig{Inject: true},
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: required"}}`)
		},
	})

	rec := h.do(t, claudeCodeRequest("/v1/messages", promptBody(longSystem, "hi")))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "max_tokens: required") {
		t.Errorf("the upstream's own wording did not reach the caller: %s", rec.Body.String())
	}
}

// TestAffinityIsSkippedForAGroupWithNothingToChoose covers a fleet holding both
// kinds of group at once.
//
// Fingerprinting a prompt is not free — it hashes the system blocks, the tool
// definitions and the opening turn of every request — and it buys nothing for a
// group with one deployment, where every request already lands on the same
// upstream and therefore the same cache. Asking the question per group rather
// than per fleet is what keeps a single balanced group from imposing that cost
// on all the others.
func TestAffinityIsSkippedForAGroupWithNothingToChoose(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true,
		extraDeployments: 2, soloGroup: true,
	})

	solo := strings.Replace(promptBody("be helpful", "first turn", "answer"),
		`"model":"anthropic-claude"`, `"model":"`+soloModel+`"`, 1)
	rec := h.do(t, claudeCodeRequest("/v1/messages", solo))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("x-gateway-prompt-affinity"); got != "" {
		t.Errorf("affinity = %q, want no pin: this group has one deployment", got)
	}

	// The balanced group in the same fleet is unaffected.
	balanced := h.do(t, claudeCodeRequest("/v1/messages", promptBody("be helpful", "first turn", "answer")))
	if got := balanced.Header().Get("x-gateway-prompt-affinity"); got != "new" {
		t.Errorf("affinity = %q on the balanced group, want new", got)
	}
}

// TestPromptCacheMetricsCarryTheirValues goes past the label names the test
// above checks for.
//
// These counters are how an operator sees the prompt cache working without
// reading a bill, so the figures have to be the provider's own. A scrape naming
// the right series with the wrong numbers is worse than a missing one: it looks
// like an answer.
func TestPromptCacheMetricsCarryTheirValues(t *testing.T) {
	const (
		read    = 20_000
		write5m = 3_000
		write1h = 7_000
	)
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true,
		pricing: &core.Pricing{
			InputPer1M: 3, OutputPer1M: 15, CacheReadPer1M: 0.3,
			CacheWritePer1M: 3.75, CacheWrite1hPer1M: 6,
		},
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"usage":{"input_tokens":10,"output_tokens":20,`+
				`"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,`+
				`"cache_creation":{"ephemeral_5m_input_tokens":%d,"ephemeral_1h_input_tokens":%d}}}`,
				read, write5m+write1h, write5m, write1h)
		},
	})

	if rec := h.do(t, claudeCodeRequest("/v1/messages", promptBody(longSystem, "hi"))); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	scrape := h.do(t, httptest.NewRequest(http.MethodGet, "/metrics", nil)).Body.String()
	for _, want := range []struct {
		outcome string
		tokens  int
	}{
		{"read", read},
		// The write total, with the long tier inside it rather than beside it.
		{"write", write5m + write1h},
		{"write_1h", write1h},
	} {
		line := fmt.Sprintf(`outcome="%s"} %d`, want.outcome, want.tokens)
		if !strings.Contains(scrape, line) {
			t.Errorf("scrape has no %s counter reading %d:\n%s", want.outcome, want.tokens, scrape)
		}
	}

	// A request that read from the cache counts as a hit, not a miss: the
	// hit-rate series is what says caching is working at all.
	if !strings.Contains(scrape, `gateway_prompt_cache_requests_total{model="anthropic-claude",deployment="`+h.deploymentID+`",outcome="hit"} 1`) {
		t.Errorf("no hit recorded for a request that read the cache:\n%s", scrape)
	}
	if strings.Contains(scrape, `outcome="miss"} 1`) {
		t.Errorf("a cache read was counted as a miss:\n%s", scrape)
	}
}

// A request that read nothing back is a miss, which is the other half of the
// ratio. Counting it as a hit would make the series say caching worked on
// exactly the traffic where it did not.
func TestARequestThatReadNothingCountsAsAMiss(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true,
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"usage":{"input_tokens":10,"output_tokens":20,`+
				`"cache_read_input_tokens":0,"cache_creation_input_tokens":5000}}`)
		},
	})

	h.do(t, claudeCodeRequest("/v1/messages", promptBody(longSystem, "hi")))

	scrape := h.do(t, httptest.NewRequest(http.MethodGet, "/metrics", nil)).Body.String()
	if !strings.Contains(scrape, `outcome="miss"} 1`) {
		t.Errorf("a request that read nothing was not counted as a miss:\n%s", scrape)
	}
	if strings.Contains(scrape, `gateway_prompt_cache_requests_total{model="anthropic-claude",deployment="`+h.deploymentID+`",outcome="hit"}`) {
		t.Errorf("a request that read nothing was counted as a hit:\n%s", scrape)
	}
}

// Token counting warms nothing. It takes the same body as an inference request
// and never reaches the model, so a pin taken from it names a deployment holding
// no warm prefix — and refreshing an existing pin from it keeps a conversation
// pointed at an upstream on the strength of a request that ran nothing. Claude
// Code counts tokens on most turns, so this is the ordinary case rather than an
// edge.
func TestTokenCountingLeavesNoPin(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, extraDeployments: 2,
		// Upstreams that report a cache write on every request, so nothing but
		// the endpoint itself can be what withholds the pin.
		upstreams: newCachingCluster(t, core.FormatAnthropic, 5_000, 3).handlers(),
	})
	body := promptBody("be helpful", "first turn", "answer")

	counted := h.do(t, claudeCodeRequest("/v1/messages/count_tokens", body))
	if counted.Code != http.StatusOK {
		t.Fatalf("count_tokens: status = %d, body = %s", counted.Code, counted.Body.String())
	}
	if got := counted.Header().Get("x-gateway-prompt-affinity"); got != "" {
		t.Errorf("count_tokens reported affinity %q, want none: it consults no pin", got)
	}

	// The conversation's first real turn is still its first: it must find no
	// pin to honour, and be free to land wherever the balancer sends it.
	first := h.do(t, claudeCodeRequest("/v1/messages", body))
	if got := first.Header().Get("x-gateway-prompt-affinity"); got != "new" {
		t.Errorf("first inference turn affinity = %q, want new: token counting pinned the prefix", got)
	}
}

// A prefix the provider did not cache has no warm copy anywhere, so pinning it
// concentrates that traffic on one deployment and buys nothing back. An
// Anthropic caller sending no breakpoints, with injection off, is that case on
// every request it makes.
func TestAnUncachedPrefixIsNotPinned(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, extraDeployments: 2,
	})
	body := promptBody("be helpful", "first turn", "answer")

	if rec := h.do(t, claudeCodeRequest("/v1/messages", body)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// The default upstream reports no cache counters at all, which is what an
	// unmarked prompt below the provider's minimum looks like.
	second := h.do(t, claudeCodeRequest("/v1/messages", body))
	if got := second.Header().Get("x-gateway-prompt-affinity"); got != "new" {
		t.Errorf("second turn affinity = %q, want new: nothing was cached for a pin to point at", got)
	}
}
