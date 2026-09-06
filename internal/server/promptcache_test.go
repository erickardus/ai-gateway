package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erickardus/ai-gateway/internal/config"
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
		"tools":    []any{map[string]any{"name": "read_file"}},
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
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, extraDeployments: 2,
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
	if n := strings.Count(string(forwarded), `"cache_control"`); n != 2 {
		t.Fatalf("upstream saw %d breakpoints, want 2:\n%s", n, forwarded)
	}
	if !json.Valid(forwarded) {
		t.Fatalf("forwarded body is not valid JSON: %s", forwarded)
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

	// 2000 cache-read tokens at $3.00/M input against $0.30/M cache read.
	const want = 2000 * (3.0 - 0.3) / 1_000_000
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
