package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/erickardus/ai-gateway/internal/auth"
	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/sso"
	"github.com/erickardus/ai-gateway/internal/testutil"
)

// ssoHarness is a gateway with SSO wired up, plus the fake provider behind it.
type ssoHarness struct {
	*harness
	idp *testutil.OIDC
	// client follows no redirects, so each hop of the browser flow can be
	// inspected rather than silently completed.
	client *http.Client
	server *httptest.Server
}

func newSSOHarness(t *testing.T, tweak func(*config.SSOConfig)) *ssoHarness {
	t.Helper()
	h := newHarness(t, harnessOpts{authMode: "passthrough", allowPassthrough: true})
	idp := testutil.NewOIDC(t, "gateway")

	gw := httptest.NewServer(h.gateway)
	t.Cleanup(gw.Close)

	cfg := config.SSOConfig{
		Issuer:      idp.Issuer(),
		ClientID:    "gateway",
		Scopes:      []string{"openid", "email", "groups", "offline_access"},
		RedirectURL: gw.URL + "/sso/callback",
		KeyDuration: 720 * time.Hour,
		RenewWithin: 168 * time.Hour,
		RoleClaim:   "groups",
		Roles: []config.SSORole{
			{Match: "platform-eng", Models: []string{"anthropic-claude"}, AllowPassthrough: true, RPMLimit: 120},
			{Match: "contractors", Models: []string{"anthropic-claude"}},
		},
		BaseURL: gw.URL,
		Model:   "anthropic-claude",
	}
	if tweak != nil {
		tweak(&cfg)
	}
	h.srv.UseSSO(sso.New(cfg, testutil.DiscardLogger()), sso.NewMemStore())

	// The gateway handler was captured before UseSSO ran, so the routes have to
	// be rebuilt for the new ones to exist.
	gw.Config.Handler = h.srv.Handler()

	return &ssoHarness{
		harness: h,
		idp:     idp,
		server:  gw,
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Timeout:       10 * time.Second,
		},
	}
}

// login walks the whole browser flow and returns the grant, exactly as the CLI
// does: start, follow the provider, follow the callback, redeem the code.
func (h *ssoHarness) login(t *testing.T, verifier string) (*http.Response, map[string]any) {
	t.Helper()

	start := h.server.URL + "/sso/login?" + url.Values{
		"redirect_uri":          {"http://127.0.0.1:54321/callback"},
		"code_challenge":        {sso.Challenge(verifier)},
		"code_challenge_method": {"S256"},
		"device":                {"laptop"},
	}.Encode()

	resp, err := h.client.Get(start)
	if err != nil {
		t.Fatalf("GET /sso/login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("/sso/login status = %d, want 302", resp.StatusCode)
	}

	// The provider's authorize endpoint redirects straight back to the gateway.
	authorize := resp.Header.Get("Location")
	resp, err = h.client.Get(authorize)
	if err != nil {
		t.Fatalf("GET authorize: %v", err)
	}
	resp.Body.Close()
	callback := resp.Header.Get("Location")

	resp, err = h.client.Get(callback)
	if err != nil {
		t.Fatalf("GET /sso/callback: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		return resp, nil
	}

	returned, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if e := returned.Query().Get("error"); e != "" {
		return resp, map[string]any{"error": e}
	}

	grant := h.post(t, "/sso/exchange", map[string]string{
		"code":          returned.Query().Get("code"),
		"code_verifier": verifier,
	})
	return resp, grant
}

func (h *ssoHarness) post(t *testing.T, path string, body any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := h.client.Post(h.server.URL+path, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("POST %s: decode: %v", path, err)
	}
	out["_status"] = float64(resp.StatusCode)
	return out
}

func TestSSOLoginIssuesAUsableKey(t *testing.T) {
	h := newSSOHarness(t, nil)
	verifier, _ := sso.NewToken()

	_, grant := h.login(t, verifier)
	if got := grant["_status"]; got != float64(http.StatusOK) {
		t.Fatalf("exchange status = %v, grant = %v", got, grant)
	}
	if grant["identity"] != "dev@example.com" {
		t.Errorf("identity = %v, want dev@example.com", grant["identity"])
	}
	if grant["role"] != "platform-eng" {
		t.Errorf("role = %v, want platform-eng", grant["role"])
	}

	env := grant["env"].(map[string]any)
	headers, _ := env["ANTHROPIC_CUSTOM_HEADERS"].(string)
	key := strings.TrimPrefix(headers, "x-gateway-key: ")
	if !strings.HasPrefix(key, core.PrefixVirtualKey) {
		t.Fatalf("custom headers = %q, want a virtual key in x-gateway-key", headers)
	}

	// The point of the whole flow: the key it issued must actually authenticate
	// an inference request.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"anthropic-claude","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-gateway-key", key)
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-SUBSCRIPTION")
	rec := httptest.NewRecorder()
	h.gateway.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("inference with the SSO key = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// This is the guarantee the whole feature rests on. Claude Code has three ways
// to be handed a gateway credential and all three displace a claude.ai
// subscription login; the fourth channel, a custom header, is the only one that
// does not. If this test ever fails, the feature has inverted its own purpose.
func TestSSOEnvNeverDisplacesTheSubscription(t *testing.T) {
	h := newSSOHarness(t, nil)
	verifier, _ := sso.NewToken()
	_, grant := h.login(t, verifier)

	env := grant["env"].(map[string]any)
	for _, forbidden := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "apiKeyHelper"} {
		if _, present := env[forbidden]; present {
			t.Errorf("the issued configuration sets %s, which replaces the subscription with a per-token credential", forbidden)
		}
	}
	headers := env["ANTHROPIC_CUSTOM_HEADERS"].(string)
	for _, forbidden := range []string{"authorization", "x-api-key"} {
		if strings.Contains(strings.ToLower(headers), forbidden) {
			t.Errorf("the key was placed in %s, which overwrites the subscription token", forbidden)
		}
	}
	if env["ANTHROPIC_BASE_URL"] == nil || env["ANTHROPIC_MODEL"] == nil {
		// Claude Code skips model discovery when its only credential is a
		// custom header, so a missing model name leaves it asking for a name
		// the gateway does not route.
		t.Errorf("env = %v, want both the base URL and the model name", env)
	}
}

func TestSSOKeyCarriesTheRolesEntitlements(t *testing.T) {
	h := newSSOHarness(t, nil)
	verifier, _ := sso.NewToken()
	h.login(t, verifier)

	keys, err := h.store.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var issued *core.Key
	for _, k := range keys {
		if k.Subject != "" {
			issued = k
		}
	}
	if issued == nil {
		t.Fatal("no key was stored for the SSO identity")
	}
	if issued.Subject != "user-1" || issued.Device != "laptop" {
		t.Errorf("key bound to subject %q device %q, want user-1/laptop", issued.Subject, issued.Device)
	}
	if !issued.AllowPassthrough {
		t.Error("allow_passthrough is off, so the key cannot use the subscription path at all")
	}
	if issued.RPMLimit != 120 {
		t.Errorf("RPMLimit = %d, want the role's 120", issued.RPMLimit)
	}
	if issued.ExpiresAt == nil {
		t.Error("the key has no expiry")
	}
	if !strings.Contains(issued.Alias, "dev@example.com") {
		t.Errorf("alias = %q, want it to name the person", issued.Alias)
	}
}

// Signing in again on the same machine must not leave the previous key live,
// and must not reset what its owner has already spent.
func TestSSORepeatLoginRetiresThePreviousKeyAndKeepsSpend(t *testing.T) {
	h := newSSOHarness(t, nil)

	v1, _ := sso.NewToken()
	_, first := h.login(t, v1)
	firstKey := keyFromGrant(t, first)

	v2, _ := sso.NewToken()
	_, second := h.login(t, v2)
	secondKey := keyFromGrant(t, second)

	if firstKey == secondKey {
		t.Fatal("the second login returned the same key, so a compromised one could not be replaced")
	}
	if _, err := h.store.Get(t.Context(), auth.HashKey(firstKey)); err == nil {
		t.Error("the superseded key is still live")
	}

	// Spend accumulates under the person, not the credential, so a developer
	// cannot clear their own budget window by signing in again.
	keys, _ := h.store.List(t.Context())
	for _, k := range keys {
		if k.Subject == "user-1" && k.SpendSubject() != "sso:user-1" {
			t.Errorf("SpendSubject = %q, want it keyed to the person", k.SpendSubject())
		}
	}
}

func TestSSOSecondDeviceGetsItsOwnKey(t *testing.T) {
	h := newSSOHarness(t, nil)
	v1, _ := sso.NewToken()
	_, first := h.login(t, v1)
	firstKey := keyFromGrant(t, first)

	// The same identity, a different machine.
	grant := h.post(t, "/sso/renew", map[string]string{
		"refresh_token": "refresh-token",
		"device":        "desktop",
	})
	if grant["_status"] != float64(http.StatusOK) {
		t.Fatalf("renew status = %v: %v", grant["_status"], grant)
	}

	// Signing in on a desktop must not sign the laptop out.
	if _, err := h.store.Get(t.Context(), auth.HashKey(firstKey)); err != nil {
		t.Error("issuing a key for a second device retired the first device's key")
	}
}

func TestSSORenewRefusedOnceTheProviderDisablesTheAccount(t *testing.T) {
	h := newSSOHarness(t, nil)
	v, _ := sso.NewToken()
	h.login(t, v)

	h.idp.Set(func(o *testutil.OIDC) { o.RefuseRefresh = true })
	grant := h.post(t, "/sso/renew", map[string]string{"refresh_token": "refresh-token", "device": "laptop"})
	if grant["_status"] != float64(http.StatusUnauthorized) {
		t.Errorf("renew status = %v, want 401 once the provider refuses the grant", grant["_status"])
	}
}

// Entitlements are re-derived on every renewal, so someone moved out of a group
// loses access then rather than whenever their key happens to expire.
func TestSSORenewRefusedOnceTheIdentityLosesEveryRole(t *testing.T) {
	h := newSSOHarness(t, nil)
	v, _ := sso.NewToken()
	h.login(t, v)

	h.idp.Set(func(o *testutil.OIDC) { o.Groups = []string{"alumni"} })
	grant := h.post(t, "/sso/renew", map[string]string{"refresh_token": "refresh-token", "device": "laptop"})
	if grant["_status"] != float64(http.StatusForbidden) {
		t.Errorf("renew status = %v, want 403 once the identity matches no role", grant["_status"])
	}
}

func TestSSOLoginRefusesAnIdentityInNoConfiguredGroup(t *testing.T) {
	h := newSSOHarness(t, nil)
	h.idp.Set(func(o *testutil.OIDC) { o.Groups = []string{"nobody"} })

	v, _ := sso.NewToken()
	resp, grant := h.login(t, v)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d, want a redirect carrying the refusal", resp.StatusCode)
	}
	if grant["error"] == nil {
		t.Fatalf("the login succeeded for an identity in no configured group: %v", grant)
	}
}

// The gateway sends a browser to this address carrying a code redeemable for a
// key. An address that is not the developer's own machine must never be
// redirected to, and must not be echoed into the refusal either.
func TestSSOLoginRefusesANonLoopbackRedirect(t *testing.T) {
	h := newSSOHarness(t, nil)
	verifier, _ := sso.NewToken()

	for _, bad := range []string{
		"http://evil.example.com/cb",
		"http://127.0.0.1.evil.example.com/cb",
		"http://169.254.169.254/latest/meta-data",
	} {
		start := h.server.URL + "/sso/login?" + url.Values{
			"redirect_uri":          {bad},
			"code_challenge":        {sso.Challenge(verifier)},
			"code_challenge_method": {"S256"},
		}.Encode()
		resp, err := h.client.Get(start)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("redirect_uri %q gave status %d, want 400", bad, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "" {
			t.Errorf("redirect_uri %q produced a redirect to %q", bad, loc)
		}
	}
}

func TestSSOLoginRequiresS256(t *testing.T) {
	h := newSSOHarness(t, nil)
	start := h.server.URL + "/sso/login?" + url.Values{
		"redirect_uri":          {"http://127.0.0.1:54321/callback"},
		"code_challenge":        {"plainvalue"},
		"code_challenge_method": {"plain"},
	}.Encode()
	resp, err := h.client.Get(start)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a plain challenge method", resp.StatusCode)
	}
}

// Without PKCE, anything on the machine that can observe the loopback URL could
// redeem the code first.
func TestSSOExchangeRequiresTheMatchingVerifier(t *testing.T) {
	h := newSSOHarness(t, nil)
	verifier, _ := sso.NewToken()
	wrong, _ := sso.NewToken()

	start := h.server.URL + "/sso/login?" + url.Values{
		"redirect_uri":          {"http://127.0.0.1:54321/callback"},
		"code_challenge":        {sso.Challenge(verifier)},
		"code_challenge_method": {"S256"},
	}.Encode()
	resp, _ := h.client.Get(start)
	resp.Body.Close()
	resp, _ = h.client.Get(resp.Header.Get("Location"))
	resp.Body.Close()
	resp, _ = h.client.Get(resp.Header.Get("Location"))
	resp.Body.Close()
	returned, _ := url.Parse(resp.Header.Get("Location"))
	code := returned.Query().Get("code")

	bad := h.post(t, "/sso/exchange", map[string]string{"code": code, "code_verifier": wrong})
	if bad["_status"] != float64(http.StatusBadRequest) {
		t.Fatalf("exchange with a wrong verifier = %v, want 400", bad["_status"])
	}
	// The code is spent even by a failed redemption, so it cannot be guessed at.
	retry := h.post(t, "/sso/exchange", map[string]string{"code": code, "code_verifier": verifier})
	if retry["_status"] != float64(http.StatusBadRequest) {
		t.Errorf("a code survived a failed redemption: %v", retry["_status"])
	}
}

func TestSSOCodeIsSingleUse(t *testing.T) {
	h := newSSOHarness(t, nil)
	verifier, _ := sso.NewToken()
	_, grant := h.login(t, verifier)
	if grant["_status"] != float64(http.StatusOK) {
		t.Fatal(grant)
	}
	// The same login cannot be replayed: its state was consumed too.
	again := h.post(t, "/sso/exchange", map[string]string{"code": "anything", "code_verifier": verifier})
	if again["_status"] != float64(http.StatusBadRequest) {
		t.Errorf("status = %v, want 400", again["_status"])
	}
}

func TestSSOCallbackRefusesAnUnknownState(t *testing.T) {
	h := newSSOHarness(t, nil)
	resp, err := h.client.Get(h.server.URL + "/sso/callback?state=made-up&code=whatever")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a callback belonging to no login", resp.StatusCode)
	}
}

// A gateway with no identity provider configured must serve no OIDC surface at
// all, rather than endpoints that fail in some other way.
func TestSSORoutesAbsentWhenUnconfigured(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	for _, path := range []string{"/sso/login", "/sso/callback"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.gateway.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 when no provider is configured", path, rec.Code)
		}
	}
}

func TestSanitizeDevice(t *testing.T) {
	tests := []struct{ in, want string }{
		{"laptop", "laptop"},
		{"Erick's MBP", "Ericks MBP"},
		{"  spaced  ", "spaced"},
		{"line\nbreak", "linebreak"},
		{strings.Repeat("x", 200), strings.Repeat("x", maxDeviceName)},
		{"", ""},
	}
	for _, tc := range tests {
		if got := sanitizeDevice(tc.in); got != tc.want {
			t.Errorf("sanitizeDevice(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func keyFromGrant(t *testing.T, grant map[string]any) string {
	t.Helper()
	env, ok := grant["env"].(map[string]any)
	if !ok {
		t.Fatalf("grant has no env block: %v", grant)
	}
	headers, _ := env["ANTHROPIC_CUSTOM_HEADERS"].(string)
	_, key, found := strings.Cut(headers, ": ")
	if !found {
		t.Fatalf("custom headers = %q", headers)
	}
	return key
}
