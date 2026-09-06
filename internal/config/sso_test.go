package config

import (
	"strings"
	"testing"
	"time"
)

// ssoConfig builds a config whose SSO block is valid, so a test can break one
// field at a time and see only that error.
const ssoConfig = minimalConfig + `
sso:
  issuer: https://example.okta.com
  client_id: gateway
  client_secret: secret
  redirect_url: https://gw.example.com/sso/callback
  roles:
    - match: platform-eng
      models: ["m"]
      allow_passthrough: true
`

func TestSSODefaults(t *testing.T) {
	cfg, err := Parse([]byte(ssoConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s := cfg.SSO
	if s.KeyDuration != DefaultSSOKeyDuration {
		t.Errorf("key_duration = %s, want %s", s.KeyDuration, DefaultSSOKeyDuration)
	}
	if s.RenewWithin != DefaultSSORenewWithin {
		t.Errorf("renew_within = %s, want %s", s.RenewWithin, DefaultSSORenewWithin)
	}
	if s.RoleClaim != DefaultSSORoleClaim {
		t.Errorf("role_claim = %q, want %q", s.RoleClaim, DefaultSSORoleClaim)
	}
	// A refresh token is what lets a renewal re-check the identity, so the
	// scope that buys one has to be asked for by default.
	if !contains(s.Scopes, "offline_access") {
		t.Errorf("scopes = %v, want offline_access among them", s.Scopes)
	}
	// The base URL is the gateway's own origin, already implied by the
	// redirect, and the model is unambiguous with one group configured.
	if s.BaseURL != "https://gw.example.com" {
		t.Errorf("base_url = %q, want it derived from redirect_url", s.BaseURL)
	}
	if s.Model != "m" {
		t.Errorf("model = %q, want the only configured group", s.Model)
	}
}

func TestSSODisabledByDefault(t *testing.T) {
	cfg, err := Parse([]byte(minimalConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.SSO.Enabled() {
		t.Error("SSO is enabled with no issuer configured")
	}
}

func TestSSOValidation(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "a block with no issuer turns nothing on and says so",
			yaml: minimalConfig + "\nsso:\n  client_id: gateway\n  redirect_url: https://gw/cb\n",
			want: "set sso.issuer",
		},
		{
			name: "plain http would let anything on the path mint identities",
			yaml: strings.Replace(ssoConfig, "https://example.okta.com", "http://example.okta.com", 1),
			want: "must be https",
		},
		{
			name: "loopback is exempt, for a fake provider on a laptop",
			yaml: strings.Replace(ssoConfig, "https://example.okta.com", "http://127.0.0.1:8080", 1),
			want: "",
		},
		{
			name: "no client id",
			yaml: strings.Replace(ssoConfig, "  client_id: gateway\n", "", 1),
			want: "sso.client_id",
		},
		{
			name: "no redirect url",
			yaml: strings.Replace(ssoConfig, "  redirect_url: https://gw.example.com/sso/callback\n", "", 1),
			want: "sso.redirect_url",
		},
		{
			name: "no roles admits nobody",
			yaml: minimalConfig + "\nsso:\n  issuer: https://example.okta.com\n  client_id: gateway\n  redirect_url: https://gw.example.com/cb\n",
			want: "sso.roles",
		},
		{
			name: "a duplicate match can never apply",
			yaml: ssoConfig + "    - match: platform-eng\n      models: [\"m\"]\n",
			want: "already matched by",
		},
		{
			name: "a role naming a model nothing serves could call nothing",
			yaml: strings.Replace(ssoConfig, `models: ["m"]`, `models: ["no-such-group"]`, 1),
			want: "no deployment serves",
		},
		{
			name: "a renewal window at the key's life renews every session",
			yaml: ssoConfig + "  key_duration: 24h\n  renew_within: 24h\n",
			want: "must be below sso.key_duration",
		},
		{
			name: "a budget window with no cap limits nothing",
			yaml: ssoConfig + "      budget_duration: 720h\n",
			want: "requires max_budget",
		},
		{
			name: "a negative rate limit",
			yaml: ssoConfig + "      rpm_limit: -1\n",
			want: "must be >= 0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Parse: %v", err)
			case tc.want == "":
				return
			case err == nil:
				t.Fatalf("Parse accepted a config it should refuse, wanted an error mentioning %q", tc.want)
			case !strings.Contains(err.Error(), tc.want):
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// Claude Code skips model discovery when its only credential is a custom
// header, so a name it can ask for has to be settled at load rather than
// discovered at runtime.
func TestSSOModelRequiredWhenAmbiguous(t *testing.T) {
	// The second deployment joins model_list, above the sso block.
	twoGroups := strings.Replace(ssoConfig, "\nsso:\n", `
  - model_name: n
    params:
      format: anthropic
      api_base: https://api.anthropic.com
      auth_mode: api_key
      auth_header: x-api-key
      api_key: k

sso:
`, 1)

	_, err := Parse([]byte(twoGroups))
	if err == nil {
		t.Fatal("Parse accepted two model groups with no sso.model to choose between them")
	}
	if !strings.Contains(err.Error(), "sso.model") {
		t.Errorf("error = %v, want it to name sso.model", err)
	}

	withModel := strings.Replace(twoGroups, "\n  roles:", "\n  model: m\n  roles:", 1)
	if _, err := Parse([]byte(withModel)); err != nil {
		t.Fatalf("Parse with sso.model set: %v", err)
	}
}

func TestSSORoleBudgetsSurviveParsing(t *testing.T) {
	cfg, err := Parse([]byte(ssoConfig + "      max_budget: 200\n      budget_duration: 720h\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	role := cfg.SSO.Roles[0]
	if role.MaxBudget != 200 {
		t.Errorf("max_budget = %v, want 200", role.MaxBudget)
	}
	if role.BudgetDuration != 720*time.Hour {
		t.Errorf("budget_duration = %s, want 720h", role.BudgetDuration)
	}
	if !role.AllowPassthrough {
		t.Error("allow_passthrough was lost; without it the key cannot use the subscription path")
	}
}

func contains(vs []string, want string) bool {
	for _, v := range vs {
		if v == want {
			return true
		}
	}
	return false
}

func TestJWTAuthParsesAndDefaults(t *testing.T) {
	cfg, err := Parse([]byte(ssoConfig + "  jwt_auth:\n    enabled: true\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	j := cfg.SSO.JWTAuth
	if !j.Enabled {
		t.Fatal("jwt_auth.enabled did not parse")
	}
	if len(j.Audiences) != 1 || j.Audiences[0] != "gateway" {
		t.Errorf("audiences = %v, want the client id", j.Audiences)
	}
	if j.TTL() != DefaultJWTAuthCacheTTL {
		t.Errorf("cache_ttl = %v, want %v", j.TTL(), DefaultJWTAuthCacheTTL)
	}
}

// jwt_auth is inert without an issuer, so enabling it alone turns nothing on
// and says nothing — the same oversight the sibling fields are guarded against.
func TestJWTAuthWithoutAnIssuerIsRefused(t *testing.T) {
	_, err := Parse([]byte(minimalConfig + `
sso:
  jwt_auth:
    enabled: true
`))
	if err == nil || !strings.Contains(err.Error(), "sso.issuer") {
		t.Fatalf("want a rejection naming sso.issuer, got %v", err)
	}
}

// A block written out but never enabled accepts no tokens, and nothing else
// would tell the operator that.
func TestJWTAuthConfiguredButNotEnabledIsRefused(t *testing.T) {
	_, err := Parse([]byte(ssoConfig + `  jwt_auth:
    audiences: ["api://gateway"]
`))
	if err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("want a rejection of the unenabled block, got %v", err)
	}
}

// A negative TTL expires every entry before it is read, so every request pays a
// full signature verification — the exact cost the field exists to avoid.
func TestJWTAuthNegativeCacheTTLIsRefused(t *testing.T) {
	_, err := Parse([]byte(ssoConfig + `  jwt_auth:
    enabled: true
    cache_ttl: -5s
`))
	if err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("want a rejection of the negative TTL, got %v", err)
	}
}

// An explicit zero means "verify every request", which is a configuration an
// operator may genuinely want and which this gateway honours rather than
// treating as an omitted field.
func TestJWTAuthCacheTTLZeroIsHonoured(t *testing.T) {
	cfg, err := Parse([]byte(ssoConfig + `  jwt_auth:
    enabled: true
    cache_ttl: 0s
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.SSO.JWTAuth.TTL(); got != 0 {
		t.Errorf("cache_ttl = %v, want an explicit 0 to survive", got)
	}
}

// There is no wildcard audience. Accepting "*" as a literal would leave an
// operator believing they had opened this up while every token was refused.
func TestJWTAuthWildcardAudienceIsRefused(t *testing.T) {
	_, err := Parse([]byte(ssoConfig + `  jwt_auth:
    enabled: true
    audiences: ["*"]
`))
	if err == nil || !strings.Contains(err.Error(), "not a wildcard") {
		t.Fatalf("want a rejection of the wildcard, got %v", err)
	}
}
