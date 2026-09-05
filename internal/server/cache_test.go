package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/erickardus/ai-gateway/internal/cache"
)

// countingUpstream reports how many times the provider was actually called.
func countingUpstream(hits *atomic.Int64, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}
}

func TestCacheHitSkipsUpstream(t *testing.T) {
	var hits atomic.Int64
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, cache: true,
		upstream: countingUpstream(&hits, `{"id":"m","usage":{"input_tokens":10,"output_tokens":5}}`),
	})
	body := `{"model":"anthropic-claude","messages":[{"role":"user","content":"hi"}]}`

	first := h.do(t, claudeCodeRequest("/v1/messages", body))
	if first.Code != http.StatusOK {
		t.Fatalf("first = %d", first.Code)
	}
	if got := first.Header().Get("x-gateway-cache"); got != "miss" {
		t.Errorf("first request cache header = %q, want miss", got)
	}

	second := h.do(t, claudeCodeRequest("/v1/messages", body))
	if second.Code != http.StatusOK {
		t.Fatalf("second = %d", second.Code)
	}
	if got := second.Header().Get("x-gateway-cache"); got != "hit" {
		t.Errorf("second request cache header = %q, want hit", got)
	}
	if second.Body.String() != first.Body.String() {
		t.Errorf("cached body differs:\n got: %s\nwant: %s", second.Body.String(), first.Body.String())
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream called %d times, want 1: the cache did not prevent the second call", n)
	}
}

// The isolation guarantee: one key must never be served another key's response.
func TestCacheIsolatedPerKeyByDefault(t *testing.T) {
	var hits atomic.Int64
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, cache: true,
		upstream: countingUpstream(&hits, `{"id":"m","usage":{"input_tokens":1,"output_tokens":1}}`),
	})
	h.addKey(t, "sk-vk-SECOND", "second-key")
	body := `{"model":"anthropic-claude","messages":[{"role":"user","content":"private"}]}`

	// First key warms the cache.
	if rec := h.do(t, claudeCodeRequest("/v1/messages", body)); rec.Code != http.StatusOK {
		t.Fatalf("first key = %d", rec.Code)
	}

	// A different key sending the identical request must NOT get a hit.
	req := claudeCodeRequest("/v1/messages", body)
	req.Header.Set("x-gateway-key", "sk-vk-SECOND")
	rec := h.do(t, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second key = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("x-gateway-cache"); got == "hit" {
		t.Fatal("a second key was served the first key's cached response")
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("upstream called %d times, want 2: the cache is not isolated per key", n)
	}
}

// With sharing explicitly configured, keys do reuse each other's responses.
func TestSharedScopeReusesAcrossKeys(t *testing.T) {
	var hits atomic.Int64
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, cache: true, cacheScope: cache.ScopeShared,
		upstream: countingUpstream(&hits, `{"id":"m","usage":{"input_tokens":1,"output_tokens":1}}`),
	})
	h.addKey(t, "sk-vk-SECOND", "second-key")
	body := `{"model":"anthropic-claude","messages":[{"role":"user","content":"shareable"}]}`

	h.do(t, claudeCodeRequest("/v1/messages", body))
	req := claudeCodeRequest("/v1/messages", body)
	req.Header.Set("x-gateway-key", "sk-vk-SECOND")
	rec := h.do(t, req)

	if got := rec.Header().Get("x-gateway-cache"); got != "hit" {
		t.Errorf("cache header = %q, want hit under the shared scope", got)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream called %d times, want 1 under the shared scope", n)
	}
}

// Different prompts must not collide.
func TestDifferentRequestsMissSeparately(t *testing.T) {
	var hits atomic.Int64
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, cache: true,
		upstream: countingUpstream(&hits, `{"id":"m","usage":{"input_tokens":1,"output_tokens":1}}`),
	})
	h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[{"role":"user","content":"one"}]}`))
	h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[{"role":"user","content":"two"}]}`))
	if n := hits.Load(); n != 2 {
		t.Errorf("upstream called %d times, want 2: different prompts must not share an entry", n)
	}
}

// A streamed response is replayed as the same event sequence.
func TestStreamingResponseIsCachedAndReplayed(t *testing.T) {
	var hits atomic.Int64
	sse := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":4,\"output_tokens\":11}}\n\n"

	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, cache: true,
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			rc := http.NewResponseController(w)
			for _, chunk := range strings.SplitAfter(sse, "\n\n") {
				if chunk == "" {
					continue
				}
				w.Write([]byte(chunk))
				rc.Flush()
			}
		},
	})
	body := `{"model":"anthropic-claude","stream":true,"messages":[]}`

	first := h.do(t, claudeCodeRequest("/v1/messages", body))
	second := h.do(t, claudeCodeRequest("/v1/messages", body))

	if got := second.Header().Get("x-gateway-cache"); got != "hit" {
		t.Fatalf("second stream cache header = %q, want hit", got)
	}
	if second.Body.String() != first.Body.String() {
		t.Errorf("replayed stream differs:\n got: %q\nwant: %q", second.Body.String(), first.Body.String())
	}
	if !strings.Contains(second.Body.String(), "event: ping") {
		t.Error("the replayed stream lost its ping event")
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream called %d times, want 1", n)
	}
}

// An error must not be cached: replaying a transient failure for the whole TTL
// would turn a blip into a sticky outage.
func TestErrorsAreNotCached(t *testing.T) {
	var hits atomic.Int64
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, cache: true,
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error"}}`))
		},
	})
	body := `{"model":"anthropic-claude","messages":[]}`

	if rec := h.do(t, claudeCodeRequest("/v1/messages", body)); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	afterFirst := hits.Load()

	rec := h.do(t, claudeCodeRequest("/v1/messages", body))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("x-gateway-cache"); got == "hit" {
		t.Fatal("an error response was served from cache")
	}
	// The exact count depends on retries; what matters is that the second
	// request reached the upstream at all rather than replaying a stored error.
	if hits.Load() <= afterFirst {
		t.Errorf("the second request made no upstream call: a transient error was cached and would stick for the whole TTL")
	}
}

// A cache hit calls no upstream and costs nothing, so it must not be billed.
func TestCacheHitIsNotBilled(t *testing.T) {
	var hits atomic.Int64
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, cache: true,
		upstream: countingUpstream(&hits, `{"id":"m","usage":{"input_tokens":1000,"output_tokens":500}}`),
	})
	body := `{"model":"anthropic-claude","messages":[]}`

	h.do(t, claudeCodeRequest("/v1/messages", body))
	keys, _ := h.ledger.Keys(context.Background())
	afterFirst := keys[0].Cost

	h.do(t, claudeCodeRequest("/v1/messages", body)) // served from cache
	keys, _ = h.ledger.Keys(context.Background())
	if keys[0].Cost != afterFirst {
		t.Errorf("cost went from %v to %v on a cache hit; a hit calls no upstream and must not be billed", afterFirst, keys[0].Cost)
	}
	if keys[0].Requests != 1 {
		t.Errorf("requests = %d, want 1: only the upstream call is accounted", keys[0].Requests)
	}
}

func TestCachePurgeRequiresMasterKey(t *testing.T) {
	var hits atomic.Int64
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, cache: true, masterKey: "sk-master-P",
		upstream: countingUpstream(&hits, `{"id":"m","usage":{"input_tokens":1,"output_tokens":1}}`),
	})
	body := `{"model":"anthropic-claude","messages":[]}`
	h.do(t, claudeCodeRequest("/v1/messages", body))

	req := httptest.NewRequest(http.MethodPost, "/cache/purge", nil)
	req.Header.Set("x-gateway-key", testVirtualKey)
	if rec := h.do(t, req); rec.Code != http.StatusUnauthorized {
		t.Errorf("virtual key on /cache/purge = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/cache/purge", nil)
	req.Header.Set("x-gateway-key", "sk-master-P")
	if rec := h.do(t, req); rec.Code != http.StatusOK {
		t.Fatalf("master key on /cache/purge = %d", rec.Code)
	}

	// After a purge the next request reaches the upstream again.
	h.do(t, claudeCodeRequest("/v1/messages", body))
	if n := hits.Load(); n != 2 {
		t.Errorf("upstream called %d times, want 2 after a purge", n)
	}
}

// With caching off nothing is stored and no header is set.
func TestCacheDisabledByDefault(t *testing.T) {
	var hits atomic.Int64
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true,
		upstream: countingUpstream(&hits, `{"id":"m","usage":{"input_tokens":1,"output_tokens":1}}`),
	})
	body := `{"model":"anthropic-claude","messages":[]}`
	for range 3 {
		h.do(t, claudeCodeRequest("/v1/messages", body))
	}
	if n := hits.Load(); n != 3 {
		t.Errorf("upstream called %d times, want 3: caching should be off by default", n)
	}
}
