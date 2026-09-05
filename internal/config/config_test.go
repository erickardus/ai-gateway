package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shipped example must load through the real code path, so a broken example
// fails CI rather than a user's first run.
func TestExampleConfigLoads(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-example")
	t.Setenv("OPENAI_API_KEY", "sk-openai-example")
	t.Setenv("GATEWAY_MASTER_KEY", "sk-master-example")
	t.Setenv("DEV_KEY", "sk-vk-example")

	path := filepath.Join("..", "..", "config", "gateway.example.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("the shipped example config does not load: %v", err)
	}

	if got := len(cfg.Groups()); got != 3 {
		t.Errorf("model groups = %d, want 3", got)
	}
	group := cfg.Groups()["anthropic-claude"]
	if len(group) != 2 {
		t.Fatalf("anthropic-claude has %d deployments, want 2", len(group))
	}
	if group[0].Params.AuthMode != "passthrough" {
		t.Errorf("first deployment auth_mode = %q, want passthrough", group[0].Params.AuthMode)
	}
	if group[0].ID() == group[1].ID() {
		t.Error("deployments in a group must have distinct IDs")
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("PRESENT", "value")
	t.Setenv("EMPTY", "")

	tests := []struct{ in, want string }{
		{"${PRESENT}", "value"},
		{"${MISSING}", ""},
		{"${MISSING:-fallback}", "fallback"},
		{"${PRESENT:-fallback}", "value"},
		{"${EMPTY:-fallback}", "fallback"},
		{"prefix-${PRESENT}-suffix", "prefix-value-suffix"},
		{"no references", "no references"},
	}
	for _, tc := range tests {
		if got := string(ExpandEnv([]byte(tc.in))); got != tc.want {
			t.Errorf("ExpandEnv(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

const minimalConfig = `
model_list:
  - model_name: m
    params:
      format: anthropic
      api_base: https://api.anthropic.com
      auth_mode: api_key
      auth_header: x-api-key
      api_key: k
`

func TestDefaultsApplied(t *testing.T) {
	cfg, err := Parse([]byte(minimalConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Router.Strategy != StrategyWeightedShuffle {
		t.Errorf("strategy = %q", cfg.Router.Strategy)
	}
	if got := cfg.Router.Retries(); got != DefaultNumRetries {
		t.Errorf("num_retries = %d, want %d", got, DefaultNumRetries)
	}
	if got := cfg.Router.Cooldown.Fails(); got != DefaultAllowedFails {
		t.Errorf("allowed_fails = %d, want %d", got, DefaultAllowedFails)
	}
	if cfg.Server.Addr != DefaultAddr {
		t.Errorf("addr = %q", cfg.Server.Addr)
	}
}

// An explicit allowed_fails of 0 means "eject on the first failure" and must not
// be mistaken for an unset field.
func TestExplicitZeroAllowedFailsIsHonoured(t *testing.T) {
	cfg, err := Parse([]byte(minimalConfig + "\nrouter:\n  cooldown:\n    allowed_fails: 0\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Router.Cooldown.Fails(); got != 0 {
		t.Errorf("allowed_fails = %d, want 0: an explicit zero must survive defaulting", got)
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "unknown strategy",
			yaml: minimalConfig + "\nrouter:\n  strategy: nonsense\n",
			want: "router.strategy",
		},
		{
			name: "passthrough host not allowlisted",
			yaml: "model_list:\n  - model_name: m\n    params:\n      format: anthropic\n      api_base: https://evil.example.com\n      auth_mode: passthrough\n",
			want: "allowed_upstream_hosts",
		},
		{
			name: "passthrough must not carry a key",
			yaml: "virtual_keys:\n  allowed_upstream_hosts: [api.anthropic.com]\nmodel_list:\n  - model_name: m\n    params:\n      format: anthropic\n      api_base: https://api.anthropic.com\n      auth_mode: passthrough\n      api_key: leaked\n",
			want: "must be empty when auth_mode is passthrough",
		},
		{
			// Injection rewrites the request body, and the passthrough path
			// exists to forward one unchanged.
			name: "prompt cache injection alongside passthrough",
			yaml: "virtual_keys:\n  allowed_upstream_hosts: [api.anthropic.com]\nprompt_cache:\n  inject: true\nmodel_list:\n  - model_name: m\n    params:\n      format: anthropic\n      api_base: https://api.anthropic.com\n      auth_mode: passthrough\n",
			want: "prompt_cache.inject",
		},
		{
			name: "prompt cache injection on an openai deployment",
			yaml: "prompt_cache:\n  inject: true\nmodel_list:\n  - model_name: m\n    params:\n      format: openai\n      api_base: https://api.openai.com\n      auth_mode: api_key\n      auth_header: authorization\n      api_key: k\n",
			want: "cache breakpoints are an Anthropic construct",
		},
		{
			name: "negative prompt cache affinity ttl",
			yaml: minimalConfig + "\nprompt_cache:\n  affinity_ttl: -1s\n",
			want: "prompt_cache.affinity_ttl",
		},
		{
			name: "negative prompt cache in-flight lead",
			yaml: minimalConfig + "\nprompt_cache:\n  affinity_max_in_flight_lead: -1\n",
			want: "prompt_cache.affinity_max_in_flight_lead",
		},
		{
			name: "negative prompt cache inject minimum",
			yaml: minimalConfig + "\nprompt_cache:\n  inject_min_bytes: -1\n",
			want: "prompt_cache.inject_min_bytes",
		},
		{
			name: "api_key mode needs a credential",
			yaml: "model_list:\n  - model_name: m\n    params:\n      format: anthropic\n      api_base: https://api.anthropic.com\n      auth_mode: api_key\n",
			want: "params.api_key",
		},
		{
			name: "fallback names an unknown group",
			yaml: minimalConfig + "\nrouter:\n  fallbacks:\n    - from: m\n      to: [ghost]\n",
			want: "no model group named \"ghost\"",
		},
		{
			name: "fallback to itself",
			yaml: minimalConfig + "\nrouter:\n  fallbacks:\n    - from: m\n      to: [m]\n",
			want: "cannot fall back to itself",
		},
		{
			name: "api_base with embedded credentials",
			yaml: "model_list:\n  - model_name: m\n    params:\n      format: anthropic\n      api_base: https://user:pass@api.anthropic.com\n      auth_mode: api_key\n      auth_header: x-api-key\n      api_key: k\n",
			want: "must not embed userinfo",
		},
		{
			name: "empty model list",
			yaml: "router:\n  strategy: least-busy\n",
			want: "model_list",
		},
		{
			name: "file store without a path",
			yaml: minimalConfig + "\nvirtual_keys:\n  store:\n    kind: file\n",
			want: "virtual_keys.store.path",
		},
		{
			name: "negative weight",
			yaml: "model_list:\n  - model_name: m\n    weight: -5\n    params:\n      format: anthropic\n      api_base: https://api.anthropic.com\n      auth_mode: api_key\n      auth_header: x-api-key\n      api_key: k\n",
			want: "weight",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// Validation reports every problem at once, not just the first.
func TestValidationAggregatesErrors(t *testing.T) {
	_, err := Parse([]byte("router:\n  strategy: bogus\n  num_retries: -1\nmodel_list:\n  - model_name: m\n    params:\n      format: klingon\n      api_base: https://api.anthropic.com\n      auth_mode: api_key\n"))
	if err == nil {
		t.Fatal("expected errors")
	}
	msg := err.Error()
	for _, want := range []string{"router.strategy", "router.num_retries", "params.format", "params.api_key"} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error is missing %q:\n%s", want, msg)
		}
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	if _, err := Parse([]byte(minimalConfig + "\ntyop: true\n")); err == nil {
		t.Fatal("an unknown top-level field should be rejected so typos surface")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(os.TempDir(), "definitely-absent-gateway.yaml")); err == nil {
		t.Fatal("expected an error for a missing config file")
	}
}

func TestHostOfNormalizes(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://API.Anthropic.com/v1", "api.anthropic.com"},
		{"https://api.anthropic.com.", "api.anthropic.com"},
		{"https://api.anthropic.com:8443", "api.anthropic.com"},
		{"not a url with spaces", ""},
	}
	for _, tc := range tests {
		if got := HostOf(tc.in); got != tc.want {
			t.Errorf("HostOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// An explicit zero must survive defaulting for every field where zero is a
// meaningful setting — the defect this project criticises LiteLLM for.
func TestExplicitZerosAreHonoured(t *testing.T) {
	cfg, err := Parse([]byte(`
model_list:
  - model_name: m
    weight: 0
    params:
      format: anthropic
      api_base: https://api.anthropic.com
      auth_mode: api_key
      auth_header: x-api-key
      api_key: k
router:
  num_retries: 0
  backoff:
    jitter: 0
  cooldown:
    allowed_fails: 0
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Router.Retries(); got != 0 {
		t.Errorf("num_retries = %d, want 0: an explicit zero means never retry", got)
	}
	if got := cfg.Router.Backoff.JitterFactor(); got != 0 {
		t.Errorf("jitter = %v, want 0: an explicit zero means deterministic backoff", got)
	}
	if got := cfg.Router.Cooldown.Fails(); got != 0 {
		t.Errorf("allowed_fails = %d, want 0", got)
	}
	if got := cfg.ModelList[0].Share(); got != 0 {
		t.Errorf("weight = %d, want 0: an explicit zero drains the deployment", got)
	}
}

// Omitted fields still receive their defaults.
func TestOmittedFieldsStillDefault(t *testing.T) {
	cfg, err := Parse([]byte(minimalConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Router.Retries() != DefaultNumRetries {
		t.Errorf("num_retries = %d, want %d", cfg.Router.Retries(), DefaultNumRetries)
	}
	if cfg.Router.Backoff.JitterFactor() != DefaultBackoffJitter {
		t.Errorf("jitter = %v", cfg.Router.Backoff.JitterFactor())
	}
	if cfg.ModelList[0].Share() != 1 {
		t.Errorf("weight = %d, want 1", cfg.ModelList[0].Share())
	}
}

// A negative duration would disable the feature it configures rather than
// tightening it, so it must be rejected.
func TestNegativeDurationsRejected(t *testing.T) {
	for _, field := range []string{
		"router:\n  timeout: -5s\n",
		"router:\n  stream_timeout: -1s\n",
		"router:\n  cooldown:\n    period: -1s\n",
		"router:\n  backoff:\n    initial: -1s\n",
		"server:\n  shutdown_grace: -1s\n",
	} {
		_, err := Parse([]byte(minimalConfig + field))
		if err == nil {
			t.Errorf("negative duration accepted:\n%s", field)
			continue
		}
		if !strings.Contains(err.Error(), "must not be negative") {
			t.Errorf("unexpected error for %q: %v", field, err)
		}
	}
}

// The gateway does not translate between wire formats, so a group must not mix
// them and a fallback must not cross them.
func TestFormatHomogeneityEnforced(t *testing.T) {
	mixed := `
model_list:
  - model_name: g
    params: {format: anthropic, api_base: "https://api.anthropic.com", auth_mode: api_key, auth_header: x-api-key, api_key: k}
  - model_name: g
    params: {format: openai, api_base: "https://api.openai.com/v1", auth_mode: api_key, auth_header: authorization, api_key: k}
`
	if _, err := Parse([]byte(mixed)); err == nil || !strings.Contains(err.Error(), "mixes") {
		t.Errorf("a mixed-format group should be rejected, got %v", err)
	}

	crossFallback := `
model_list:
  - model_name: a
    params: {format: anthropic, api_base: "https://api.anthropic.com", auth_mode: api_key, auth_header: x-api-key, api_key: k}
  - model_name: o
    params: {format: openai, api_base: "https://api.openai.com/v1", auth_mode: api_key, auth_header: authorization, api_key: k}
router:
  fallbacks:
    - from: a
      to: [o]
`
	if _, err := Parse([]byte(crossFallback)); err == nil || !strings.Contains(err.Error(), "cross wire formats") {
		t.Errorf("a cross-format fallback should be rejected, got %v", err)
	}
}
