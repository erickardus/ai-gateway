package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erickardus/ai-gateway/internal/audit"
	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/metrics"
	"github.com/erickardus/ai-gateway/internal/sso"
)

// recordingSink captures what the handlers wrote, and can be told to fail.
//
// It is a sink rather than a spy on the file one because the question these
// tests ask is about the handlers: what they record, and what they do when
// recording is impossible. Where the bytes land is internal/audit's business.
type recordingSink struct {
	buf  bytes.Buffer
	sink *audit.WriterSink
	// fail, when set, is returned instead of writing. It is what a full disk
	// looks like from a handler's point of view.
	fail error
}

func newRecordingSink() *recordingSink {
	s := &recordingSink{}
	s.sink = audit.NewWriterSink(&s.buf)
	return s
}

func (s *recordingSink) Record(ctx context.Context, e audit.Event) (audit.Record, error) {
	if s.fail != nil {
		return audit.Record{}, s.fail
	}
	return s.sink.Record(ctx, e)
}

func (s *recordingSink) Close() error { return s.sink.Close() }

// records decodes what has been written so far.
func (s *recordingSink) records(t *testing.T) []audit.Record {
	t.Helper()
	var out []audit.Record
	for _, line := range strings.Split(strings.TrimSpace(s.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec audit.Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode audit record %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// find returns the first record for an action, failing the test if there is
// none.
func (s *recordingSink) find(t *testing.T, action string) audit.Record {
	t.Helper()
	for _, rec := range s.records(t) {
		if rec.Action == action {
			return rec
		}
	}
	t.Fatalf("no %s record was written; got %+v", action, s.records(t))
	return audit.Record{}
}

// auditHarness is a console-enabled gateway recording to a sink under the
// test's control.
func auditHarness(t *testing.T) (*harness, *recordingSink) {
	t.Helper()
	h := newUIHarness(t)
	sink := newRecordingSink()
	// Attached after Handler() was built, which is safe because the handlers
	// read the field per request rather than closing over it.
	h.srv.UseAudit(sink)
	return h, sink
}

func masterKeyRequest(method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("x-gateway-key", testMasterKey)
	req.Header.Set("User-Agent", "gateway-test/1.0")
	return req
}

// The key lifecycle must be recorded from both surfaces, with the actor, the
// target and the outcome the record is supposed to carry.
func TestKeyLifecycleIsAudited(t *testing.T) {
	tests := []struct {
		name string
		// do performs the action and returns the storage hash it acted on.
		do     func(t *testing.T, h *harness) string
		action string
		via    string
	}{
		{
			name:   "generated through the management API",
			action: audit.ActionKeyGenerate,
			via:    "api",
			do: func(t *testing.T, h *harness) string {
				rec := h.do(t, masterKeyRequest(http.MethodPost, "/key/generate", `{"alias":"finance"}`))
				if rec.Code != http.StatusCreated {
					t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
				}
				return hashOfIssuedKey(t, rec.Body.Bytes())
			},
		},
		{
			name:   "generated through the console",
			action: audit.ActionKeyGenerate,
			via:    "console",
			do: func(t *testing.T, h *harness) string {
				cookie := h.signIn(t)
				rec := h.uiPost(t, "/ui/api/keys/generate", `{"alias":"finance"}`, cookie)
				if rec.Code != http.StatusCreated {
					t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
				}
				return hashOfIssuedKey(t, rec.Body.Bytes())
			},
		},
		{
			name:   "blocked through the console",
			action: audit.ActionKeyUpdate,
			via:    "console",
			do: func(t *testing.T, h *harness) string {
				cookie := h.signIn(t)
				created := h.uiPost(t, "/ui/api/keys/generate", `{"alias":"finance"}`, cookie)
				hash := hashOfIssuedKey(t, created.Body.Bytes())
				rec := h.uiPost(t, "/ui/api/keys/update", `{"hash":"`+hash+`","blocked":true}`, cookie)
				if rec.Code != http.StatusOK {
					t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
				}
				return hash
			},
		},
		{
			name:   "deleted through the management API",
			action: audit.ActionKeyDelete,
			via:    "api",
			do: func(t *testing.T, h *harness) string {
				created := h.do(t, masterKeyRequest(http.MethodPost, "/key/generate", `{"alias":"finance"}`))
				hash := hashOfIssuedKey(t, created.Body.Bytes())
				rec := h.do(t, masterKeyRequest(http.MethodPost, "/key/delete", `{"hash":"`+hash+`"}`))
				if rec.Code != http.StatusOK {
					t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
				}
				return hash
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, sink := auditHarness(t)
			hash := tc.do(t, h)

			var rec audit.Record
			for _, candidate := range sink.records(t) {
				if candidate.Action == tc.action && candidate.Target == hash {
					rec = candidate
				}
			}
			if rec.Action == "" {
				t.Fatalf("no %s record for %s; got %+v", tc.action, hash, sink.records(t))
			}
			if rec.Outcome != audit.OutcomeSuccess {
				t.Errorf("outcome = %q, want %q", rec.Outcome, audit.OutcomeSuccess)
			}
			if rec.TargetKind != audit.TargetKey {
				t.Errorf("target_kind = %q, want %q", rec.TargetKind, audit.TargetKey)
			}
			if rec.Actor.Kind != audit.ActorMasterKey {
				t.Errorf("actor kind = %q, want %q — every console session is still the master key",
					rec.Actor.Kind, audit.ActorMasterKey)
			}
			if rec.Detail["via"] != tc.via {
				t.Errorf("detail via = %q, want %q", rec.Detail["via"], tc.via)
			}
			if rec.RequestID == "" {
				t.Error("record carries no request ID, so it cannot be joined to the access log")
			}
			if rec.SourceIP == "" {
				t.Error("record carries no source address")
			}
			if rec.UserAgent != "gateway-test/1.0" && tc.via == "api" {
				t.Errorf("user agent = %q, want the caller's", rec.UserAgent)
			}
		})
	}
}

// A sink that will not take the record must stop the mutation, not merely
// complain about it. This is the whole failure posture in one assertion.
func TestASinkFailureRefusesTheMutation(t *testing.T) {
	h, sink := auditHarness(t)
	sink.fail = errors.New("audit log is full")

	before, err := h.store.List(context.Background())
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}

	rec := h.do(t, masterKeyRequest(http.MethodPost, "/key/generate", `{"alias":"finance"}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500 — an unrecordable action must be refused", rec.Code)
	}

	after, err := h.store.List(context.Background())
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("the key store grew from %d to %d despite the refusal", len(before), len(after))
	}

	// The refusal is a 500 like any other from the outside, so the counter is
	// the only thing that says the gateway currently cannot be administered.
	if got := counterValue(t, h.metrics, metrics.MAuditFailures); got != 1 {
		t.Errorf("%s = %v, want 1", metrics.MAuditFailures, got)
	}
}

// The same posture for the console's own session: a sign-in that cannot be
// recorded does not hand back a session.
func TestASinkFailureRefusesASignIn(t *testing.T) {
	h, sink := auditHarness(t)
	sink.fail = errors.New("audit log is full")

	req := httptest.NewRequest(http.MethodPost, "/ui/api/session",
		strings.NewReader(`{"master_key":"`+testMasterKey+`"}`))
	rec := h.do(t, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "gw_ui_session" && c.Value != "" {
			t.Fatal("a sign-in that could not be recorded still issued a session")
		}
	}
}

// Sign-in, refusal and sign-out are the console's own lifecycle, and a refused
// sign-in is the one of the three an auditor asks about first.
func TestConsoleSessionsAreAudited(t *testing.T) {
	h, sink := auditHarness(t)

	refused := h.do(t, httptest.NewRequest(http.MethodPost, "/ui/api/session",
		strings.NewReader(`{"master_key":"sk-master-WRONG"}`)))
	if refused.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", refused.Code)
	}
	rec := sink.find(t, audit.ActionConsoleSignIn)
	if rec.Outcome != audit.OutcomeRefused {
		t.Errorf("outcome = %q, want %q", rec.Outcome, audit.OutcomeRefused)
	}
	if rec.Actor.Kind != audit.ActorUnauthenticated {
		t.Errorf("actor kind = %q, want %q — nothing authenticated", rec.Actor.Kind, audit.ActorUnauthenticated)
	}

	cookie := h.signIn(t)
	out := h.uiPost(t, "/ui/api/session/delete", "", cookie)
	if out.Code != http.StatusOK {
		t.Fatalf("sign out: status %d", out.Code)
	}

	var outcomes []string
	for _, r := range sink.records(t) {
		if r.Action == audit.ActionConsoleSignIn || r.Action == audit.ActionConsoleSignOut {
			outcomes = append(outcomes, r.Action+"/"+r.Outcome)
		}
	}
	want := []string{
		audit.ActionConsoleSignIn + "/" + audit.OutcomeRefused,
		audit.ActionConsoleSignIn + "/" + audit.OutcomeSuccess,
		audit.ActionConsoleSignOut + "/" + audit.OutcomeSuccess,
	}
	if strings.Join(outcomes, ",") != strings.Join(want, ",") {
		t.Errorf("session records = %v, want %v", outcomes, want)
	}
}

// Signing out is the one action a failed audit write must not refuse: refusing
// would leave a live console session behind, which is worse than an unrecorded
// sign-out.
func TestASinkFailureStillSignsTheOperatorOut(t *testing.T) {
	h, sink := auditHarness(t)
	cookie := h.signIn(t)
	sink.fail = errors.New("audit log is full")

	rec := h.uiPost(t, "/ui/api/session/delete", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 — a sign-out must not be blocked by the audit log", rec.Code)
	}
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == "gw_ui_session" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("the session cookie was not cleared")
	}
}

// The endpoints that take no credential must not let a stranger write records:
// an audit write fsyncs, and the console's sign-out and the management
// endpoints' 401s are both reachable by anyone who can open a socket.
func TestUnauthenticatedCallersCannotFillTheAuditLog(t *testing.T) {
	h, sink := auditHarness(t)

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/key/generate", strings.NewReader(`{}`)),
		httptest.NewRequest(http.MethodPost, "/key/delete", strings.NewReader(`{"hash":"x"}`)),
		httptest.NewRequest(http.MethodPost, "/ui/api/session/delete", nil),
	} {
		h.do(t, req)
	}
	if got := sink.records(t); len(got) != 0 {
		t.Fatalf("unauthenticated requests wrote %d audit record(s): %+v", len(got), got)
	}
}

// Purging the cache is an administrative action with a cost to every caller,
// and nothing in the cache afterwards says it happened.
func TestCachePurgeIsAudited(t *testing.T) {
	h := newHarness(t, harnessOpts{
		authMode: "api_key", masterKey: testMasterKey, cache: true, ui: true,
	})
	sink := newRecordingSink()
	h.srv.UseAudit(sink)

	rec := h.do(t, masterKeyRequest(http.MethodPost, "/cache/purge", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if got := sink.find(t, audit.ActionCachePurge); got.TargetKind != audit.TargetCache {
		t.Errorf("target_kind = %q, want %q", got.TargetKind, audit.TargetCache)
	}
}

// The one thing an audit log must never become is a second place the gateway's
// secrets live. It is read by more people than anything else the gateway
// produces.
func TestAuditRecordsCarryNoSecrets(t *testing.T) {
	h, sink := auditHarness(t)

	cookie := h.signIn(t)
	created := h.uiPost(t, "/ui/api/keys/generate", `{"alias":"finance"}`, cookie)
	if created.Code != http.StatusCreated {
		t.Fatalf("generate: status %d, body %s", created.Code, created.Body.String())
	}
	var issued struct {
		Key  string `json:"key"`
		Info struct {
			Hash string `json:"hash"`
		} `json:"info"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &issued); err != nil {
		t.Fatalf("decode the issued key: %v", err)
	}
	h.uiPost(t, "/ui/api/keys/update", `{"hash":"`+issued.Info.Hash+`","blocked":true}`, cookie)
	h.uiPost(t, "/ui/api/keys/delete", `{"hash":"`+issued.Info.Hash+`"}`, cookie)
	h.do(t, httptest.NewRequest(http.MethodPost, "/ui/api/session",
		strings.NewReader(`{"master_key":"`+testMasterKey+`"}`)))

	written := sink.buf.String()
	for _, secret := range []string{
		issued.Key,     // the plaintext of a key the gateway issued
		testMasterKey,  // the credential that bought the session
		testVirtualKey, // a key the harness configured
	} {
		if strings.Contains(written, secret) {
			t.Errorf("the audit log contains a credential (%s...)", secret[:8])
		}
	}
	// The hash is the whole point: it is what joins a record to a key listing,
	// and it is useless to whoever steals the log.
	if !strings.Contains(written, issued.Info.Hash) {
		t.Error("the audit log names no key hash, so its records cannot be joined to anything")
	}
}

// An SSO grant is the one action whose actor is a person rather than a
// credential, which is the case the actor model exists for.
func TestSSOGrantIsAuditedAgainstTheIdentity(t *testing.T) {
	h := newSSOHarness(t, nil)
	sink := newRecordingSink()
	h.srv.UseAudit(sink)

	verifier, _ := sso.NewToken()
	_, grant := h.login(t, verifier)
	if got := grant["_status"]; got != float64(http.StatusOK) {
		t.Fatalf("exchange status = %v, grant = %v", got, grant)
	}

	rec := sink.find(t, audit.ActionSSOGrant)
	if rec.Actor.Kind != audit.ActorSSOSubject {
		t.Errorf("actor kind = %q, want %q", rec.Actor.Kind, audit.ActorSSOSubject)
	}
	if rec.Actor.ID == "" {
		t.Error("the record names no provider subject, so it does not identify a person")
	}
	if rec.Actor.Display != "dev@example.com" {
		t.Errorf("actor display = %q, want dev@example.com", rec.Actor.Display)
	}
	if rec.TargetKind != audit.TargetKey || rec.Target == "" {
		t.Errorf("target = %q/%q, want the hash of the key that was issued", rec.TargetKind, rec.Target)
	}
	if rec.Detail["role"] != "platform-eng" {
		t.Errorf("detail role = %q, want platform-eng", rec.Detail["role"])
	}

	// The key the developer was handed, and the refresh token that renews it,
	// must be nowhere in the record.
	env := grant["env"].(map[string]any)
	key := strings.TrimPrefix(env["ANTHROPIC_CUSTOM_HEADERS"].(string), "x-gateway-key: ")
	written := sink.buf.String()
	if strings.Contains(written, key) {
		t.Error("the audit log contains the plaintext key the grant issued")
	}
	if refresh, _ := grant["refresh_token"].(string); refresh != "" && strings.Contains(written, refresh) {
		t.Error("the audit log contains the grant's refresh token")
	}
}

// An identity the provider vouched for, refused by this gateway's own rules, is
// the SSO event an auditor asks about.
func TestSSORefusalIsAudited(t *testing.T) {
	h := newSSOHarness(t, func(c *config.SSOConfig) {
		c.Roles = []config.SSORole{{Match: "nobody-at-all", Models: []string{"anthropic-claude"}}}
	})
	sink := newRecordingSink()
	h.srv.UseAudit(sink)

	verifier, _ := sso.NewToken()
	if _, grant := h.login(t, verifier); grant["error"] == nil {
		t.Fatalf("the login was not refused: %v", grant)
	}

	rec := sink.find(t, audit.ActionSSOGrant)
	if rec.Outcome != audit.OutcomeRefused {
		t.Errorf("outcome = %q, want %q", rec.Outcome, audit.OutcomeRefused)
	}
	if rec.TargetKind != audit.TargetIdentity {
		t.Errorf("target_kind = %q, want %q — nothing was issued", rec.TargetKind, audit.TargetIdentity)
	}
	if rec.Reason == "" {
		t.Error("a refusal with no reason says nothing an auditor can act on")
	}
}

// hashOfIssuedKey reads the storage hash out of a /key/generate response.
func hashOfIssuedKey(t *testing.T, body []byte) string {
	t.Helper()
	var issued struct {
		Info struct {
			Hash string `json:"hash"`
		} `json:"info"`
	}
	if err := json.Unmarshal(body, &issued); err != nil {
		t.Fatalf("decode the issued key: %v", err)
	}
	if issued.Info.Hash == "" {
		t.Fatalf("no hash in %s", body)
	}
	return issued.Info.Hash
}

// counterValue reads one unlabelled-or-labelled counter's total out of the
// registry, summed across its label sets.
func counterValue(t *testing.T, reg *metrics.Registry, name string) float64 {
	t.Helper()
	var total float64
	for _, family := range reg.Collect() {
		if family.Name != name {
			continue
		}
		for _, p := range family.Points {
			total += p.Value
		}
	}
	return total
}
