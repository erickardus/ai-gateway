package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/metrics"
	"github.com/erickardus/ai-gateway/internal/spend"
)

// usageUpstream replies with a realistic Anthropic usage block, including the
// cache counters Claude Code traffic actually carries.
func usageUpstream(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"id":"msg_1","type":"message","usage":{"input_tokens":1000,"output_tokens":500,"cache_read_input_tokens":20000,"cache_creation_input_tokens":2000}}`)
}

func TestSpendRecordedForBillableDeployment(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "api_key", allowPassthrough: true, upstream: usageUpstream})

	rec := h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	keys, err := h.ledger.Keys(context.Background())
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d key summaries, want 1", len(keys))
	}
	got := keys[0]
	if got.InputTokens != 1000 || got.OutputTokens != 500 {
		t.Errorf("tokens = %d/%d, want 1000/500", got.InputTokens, got.OutputTokens)
	}
	if got.CacheReadTokens != 20000 || got.CacheWriteTokens != 2000 {
		t.Errorf("cache tokens = %d/%d, want 20000/2000", got.CacheReadTokens, got.CacheWriteTokens)
	}
	// input 1000@3 + output 500@15 + cacheRead 20000@0.30 + cacheWrite 2000@3.75
	// = 0.003 + 0.0075 + 0.006 + 0.0075 = 0.024
	if got.Cost < 0.0239 || got.Cost > 0.0241 {
		t.Errorf("cost = %v, want ~0.024", got.Cost)
	}
	if got.BillableRequests != 1 {
		t.Errorf("billable requests = %d, want 1", got.BillableRequests)
	}
}

// Passthrough traffic bills the caller's subscription, so the operator's ledger
// must show usage but no cost.
func TestPassthroughRecordsUsageWithoutCost(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "passthrough", allowPassthrough: true, upstream: usageUpstream})

	if rec := h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	keys, _ := h.ledger.Keys(context.Background())
	got := keys[0]
	if got.Requests != 1 {
		t.Errorf("requests = %d, want 1", got.Requests)
	}
	if got.InputTokens != 1000 {
		t.Errorf("usage must still be recorded, got %d input tokens", got.InputTokens)
	}
	if got.Cost != 0 {
		t.Errorf("cost = %v, want 0: the caller's subscription was billed, not the operator", got.Cost)
	}
	if got.BillableRequests != 0 {
		t.Errorf("billable requests = %d, want 0", got.BillableRequests)
	}
}

func TestBudgetEnforced(t *testing.T) {
	// A budget small enough that one request exhausts it.
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, upstream: usageUpstream, maxBudget: 0.02,
	})

	if rec := h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`)); rec.Code != http.StatusOK {
		t.Fatalf("first request = %d, want 200", rec.Code)
	}

	// The next one is refused: 402, because retrying sooner will not help.
	rec := h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("second request = %d, want 402; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "budget") {
		t.Errorf("error body should mention the budget: %s", rec.Body.String())
	}
}

// A budget must not apply to traffic the operator does not pay for.
func TestBudgetIgnoresPassthroughTraffic(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "passthrough", allowPassthrough: true, upstream: usageUpstream, maxBudget: 0.0001,
	})
	for i := range 5 {
		if rec := h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`)); rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200: subscription traffic must not consume an operator budget", i+1, rec.Code)
		}
	}
}

func TestSpendEndpointsRequireMasterKey(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "api_key", allowPassthrough: true, upstream: usageUpstream, masterKey: "sk-master-X"})
	h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))

	for _, path := range []string{"/spend/keys", "/spend/deployments"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("x-gateway-key", testVirtualKey)
		if rec := h.do(t, req); rec.Code != http.StatusUnauthorized {
			t.Errorf("virtual key on %s = %d, want 401", path, rec.Code)
		}

		req = httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("x-gateway-key", "sk-master-X")
		rec := h.do(t, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("master key on %s = %d", path, rec.Code)
		}
		var payload struct {
			Count     int     `json:"count"`
			TotalCost float64 `json:"total_cost"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if payload.Count != 1 {
			t.Errorf("%s count = %d, want 1", path, payload.Count)
		}
		if payload.TotalCost <= 0 {
			t.Errorf("%s total_cost = %v, want > 0", path, payload.TotalCost)
		}
	}
}

func TestMetricsEndpoint(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "api_key", allowPassthrough: true, upstream: usageUpstream})
	h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))
	// A rejected request too, so the reason label is exercised.
	h.do(t, claudeCodeRequest("/v1/messages", `{"model":"nope","messages":[]}`))

	rec := h.do(t, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}
	out := rec.Body.String()
	for _, want := range []string{
		`gateway_requests_total{model="anthropic-claude"`,
		`outcome="success"`,
		`gateway_tokens_total{model="anthropic-claude"`,
		`gateway_cost_total{model="anthropic-claude"`,
		`gateway_rejections_total{model="nope",outcome="model_unknown"} 1`,
		`gateway_request_duration_seconds_bucket`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output is missing %q:\n%s", want, out)
		}
	}
	// No credential may appear in a scrape.
	if strings.Contains(out, testVirtualKey) || strings.Contains(out, "sk-ant-oat01") {
		t.Error("a credential leaked into the metrics output")
	}
}

// In-flight must return to zero once requests complete.
func TestInFlightSettlesToZero(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "api_key", allowPassthrough: true, upstream: usageUpstream})
	for range 5 {
		h.do(t, claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`))
	}
	rec := h.do(t, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, "gateway_in_flight") && !strings.HasSuffix(line, " 0") {
			t.Errorf("in-flight did not settle: %q", line)
		}
	}
}

// A client that hangs up must still be charged. Accounting runs after the
// response is relayed, so using the request context would let anyone dodge a
// budget by disconnecting.
// Accounting must survive the client hanging up. It runs after the response has
// been relayed, so if it used the request context a disconnecting client would
// escape their budget entirely.
func TestAccountingSurvivesClientDisconnect(t *testing.T) {
	h := newHarness(t, harnessOpts{authMode: "api_key", allowPassthrough: true, upstream: usageUpstream})

	// The local ledger ignores context, so on its own it cannot demonstrate the
	// bug. A context-honouring ledger — as the Redis one is — makes the failure
	// visible: on the request context the write is abandoned.
	strict := &ctxSensitiveLedger{inner: h.ledger}
	h.setLedger(t, strict)

	// A request whose client has already gone away.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := claudeCodeRequest("/v1/messages", `{"model":"anthropic-claude","messages":[]}`).WithContext(ctx)

	h.srv.record(req, observation{
		model:      "anthropic-claude",
		deployment: h.deploymentID,
		keyHash:    "k1",
		keyAlias:   "dev",
		usage:      core.Usage{InputTokens: 1000, OutputTokens: 500},
		outcome:    metrics.OutcomeSuccess,
		latency:    time.Millisecond,
	})

	if n := strict.abandoned.Load(); n > 0 {
		t.Fatalf("accounting abandoned %d time(s) on a cancelled context: a disconnecting client escapes their budget", n)
	}
	if strict.recorded.Load() == 0 {
		t.Fatal("nothing was recorded")
	}

	// And the cost actually landed, priced for a billable deployment.
	keys, _ := h.ledger.Keys(context.Background())
	if len(keys) == 0 || keys[0].Cost <= 0 {
		t.Errorf("expected a priced entry, got %+v", keys)
	}
}

// ctxSensitiveLedger refuses writes on a cancelled context, the way a
// network-backed store does.
type ctxSensitiveLedger struct {
	inner     spend.Store
	recorded  atomic.Int64
	abandoned atomic.Int64
}

func (l *ctxSensitiveLedger) Record(ctx context.Context, e spend.Entry) error {
	if err := ctx.Err(); err != nil {
		l.abandoned.Add(1)
		return err
	}
	l.recorded.Add(1)
	return l.inner.Record(ctx, e)
}

func (l *ctxSensitiveLedger) Spends(ctx context.Context, subjects []spend.Subject) ([]float64, error) {
	return l.inner.Spends(ctx, subjects)
}
func (l *ctxSensitiveLedger) Keys(ctx context.Context) ([]spend.Summary, error) {
	return l.inner.Keys(ctx)
}
func (l *ctxSensitiveLedger) Scopes(ctx context.Context) ([]spend.Summary, error) {
	return l.inner.Scopes(ctx)
}
func (l *ctxSensitiveLedger) Deployments(ctx context.Context) ([]spend.Summary, error) {
	return l.inner.Deployments(ctx)
}

// A streamed Chat Completions reply carries usage only where the request asked
// for it. Without that, a deployment serving nothing but streamed traffic — the
// normal case for a chat UI — is recorded as costing nothing at all, and no
// error, header or metric says otherwise.
func TestStreamedOpenAIRequestsAskForUsage(t *testing.T) {
	no := false
	cases := []struct {
		name        string
		path        string
		format      core.Format
		body        string
		streamUsage *bool
		want        bool
	}{
		{
			name:   "streamed openai request",
			path:   "/v1/chat/completions",
			format: core.FormatOpenAI,
			body:   `{"model":"openai-gpt","stream":true,"messages":[]}`,
			want:   true,
		},
		{
			name:   "non-streamed openai request",
			path:   "/v1/chat/completions",
			format: core.FormatOpenAI,
			body:   `{"model":"openai-gpt","messages":[]}`,
			// A whole-body reply reports usage unasked, so there is nothing to
			// add and no extra chunk to justify.
			want: false,
		},
		{
			name:   "streamed anthropic request",
			path:   "/v1/messages",
			format: core.FormatAnthropic,
			body:   `{"model":"anthropic-claude","stream":true,"messages":[]}`,
			// Anthropic reports usage on message_start regardless, and
			// stream_options is not part of its schema.
			want: false,
		},
		{
			name:        "turned off",
			path:        "/v1/chat/completions",
			format:      core.FormatOpenAI,
			body:        `{"model":"openai-gpt","stream":true,"messages":[]}`,
			streamUsage: &no,
			want:        false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, harnessOpts{
				authMode: "api_key", allowPassthrough: true,
				format: tc.format, streamUsage: tc.streamUsage,
			})
			if rec := h.do(t, claudeCodeRequest(tc.path, tc.body)); rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			_, forwarded, _, _ := h.seen.get()
			if got := askedForStreamUsage(forwarded); got != tc.want {
				t.Errorf("request asked for stream usage = %v, want %v: %s", got, tc.want, forwarded)
			}
		})
	}
}

// A caller that sent stream_options has said what it wants, and the gateway's
// own accounting is not a reason to overrule it. The one-off warning is what
// makes the resulting unbilled traffic visible instead.
func TestCallerStreamOptionsAreLeftAlone(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true, format: core.FormatOpenAI,
	})
	body := `{"model":"openai-gpt","stream":true,"stream_options":{"include_usage":false},"messages":[]}`

	if rec := h.do(t, claudeCodeRequest("/v1/chat/completions", body)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	_, forwarded, _, _ := h.seen.get()
	if !strings.Contains(string(forwarded), `"stream_options":{"include_usage":false}`) {
		t.Errorf("the caller's own stream_options did not survive: %s", forwarded)
	}
	if strings.Count(string(forwarded), "stream_options") != 1 {
		t.Errorf("stream_options appears more than once: %s", forwarded)
	}
}

// TestAnUnbilledStreamIsReported covers what is left once the request can no
// longer be annotated: a streamed reply that reported nothing is recorded as
// costing nothing, and the only way an operator learns of it is this line.
func TestAnUnbilledStreamIsReported(t *testing.T) {
	no := false
	h := newHarness(t, harnessOpts{
		authMode: "api_key", allowPassthrough: true,
		format: core.FormatOpenAI, streamUsage: &no,
		upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
		},
	})

	for i := 0; i < 3; i++ {
		if rec := h.do(t, claudeCodeRequest("/v1/chat/completions",
			`{"model":"openai-gpt","stream":true,"messages":[]}`)); rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
	}

	logs := h.logBuf.String()
	if !strings.Contains(logs, "streamed reply reported no usage") {
		t.Errorf("nothing in the log says the deployment was billed nothing:\n%s", logs)
	}
	// It is a fact about the deployment, not the request, so it is said once
	// however much traffic goes unbilled.
	if got := strings.Count(logs, "streamed reply reported no usage"); got != 1 {
		t.Errorf("warned %d times, want 1", got)
	}
}
