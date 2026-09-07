package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
			// A passthrough deployment must forward the caller's body unchanged,
			// so it never carries a breakpoint. A fleet holding nothing else is
			// a setting that does nothing, which is refused rather than served.
			name: "prompt cache injection where every deployment is passthrough",
			yaml: "virtual_keys:\n  allowed_upstream_hosts: [api.anthropic.com]\nprompt_cache:\n  inject: true\nmodel_list:\n  - model_name: m\n    params:\n      format: anthropic\n      api_base: https://api.anthropic.com\n      auth_mode: passthrough\n",
			want: "no deployment can carry a cache breakpoint",
		},
		{
			// Breakpoints are an Anthropic construct, so an all-openai fleet is
			// the same empty set.
			name: "prompt cache injection where every deployment is openai",
			yaml: "prompt_cache:\n  inject: true\nmodel_list:\n  - model_name: m\n    params:\n      format: openai\n      api_base: https://api.openai.com\n      auth_mode: api_key\n      auth_header: authorization\n      api_key: k\n",
			want: "no deployment can carry a cache breakpoint",
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

// Injection is decided per deployment, at dispatch, so a fleet that mixes
// deployments which can carry a breakpoint with deployments which cannot is
// served rather than refused.
//
// This is the arrangement the gateway exists for: one gateway fronting Claude
// Code, whose subscription traffic must reach the upstream byte for byte, and
// plain API callers, who place no breakpoints of their own and get no caching
// without one. Refusing the combination at load — which is what a single body
// fixed before routing forces — meant the operator had to choose which half of
// their traffic to serve properly.
func TestInjectionIsAllowedAlongsidePassthrough(t *testing.T) {
	const mixed = `
virtual_keys:
  allowed_upstream_hosts: [api.anthropic.com]
prompt_cache:
  inject: true
model_list:
  - model_name: claude
    params:
      format: anthropic
      api_base: https://api.anthropic.com
      auth_mode: passthrough
  - model_name: claude-api
    params:
      format: anthropic
      api_base: https://api.anthropic.com/v1
      auth_mode: api_key
      auth_header: x-api-key
      api_key: k
      model: claude-sonnet-4
`
	cfg, err := Parse([]byte(mixed))
	if err != nil {
		t.Fatalf("a mixed fleet was refused, so an operator must still choose which half of their traffic to serve properly: %v", err)
	}
	if !cfg.PromptCache.Inject {
		t.Error("inject was silently turned off rather than applied to the deployments that can take it")
	}
}

// An openai deployment whose upstream reads a cache breakpoint — Alibaba's Qwen
// is the one that does — makes injection meaningful on a fleet holding no
// anthropic deployment at all.
func TestInjectionIsAllowedOnAnOpenAIUpstreamThatReadsTheMarker(t *testing.T) {
	const qwen = `
prompt_cache:
  inject: true
model_list:
  - model_name: qwen
    params:
      format: openai
      api_base: https://dashscope.aliyuncs.com/compatible-mode/v1
      model: qwen-max
      auth_mode: api_key
      auth_header: authorization
      auth_scheme: Bearer
      api_key: k
      supports_cache_control: true
`
	if _, err := Parse([]byte(qwen)); err != nil {
		t.Fatalf("an explicit-cache openai fleet was refused, so its callers only ever get the implicit cache: %v", err)
	}

	// Without the declaration the same fleet marks nothing, which is a setting
	// that says one thing and does nothing.
	if _, err := Parse([]byte(strings.Replace(qwen, "      supports_cache_control: true\n", "", 1))); err == nil {
		t.Error("injection was accepted on a fleet where no deployment reads a breakpoint")
	}
}

// The capability is only meaningful where a body is both OpenAI-format and
// annotated at all.
func TestCacheControlCapabilityIsRefusedWhereItCannotApply(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{
			name: "on an anthropic deployment, which always reads it",
			yaml: "model_list:\n  - model_name: m\n    params:\n      format: anthropic\n      api_base: https://api.anthropic.com\n      auth_mode: api_key\n      auth_header: x-api-key\n      api_key: k\n      supports_cache_control: true\n",
			want: "only meaningful on an openai deployment",
		},
		{
			name: "on a passthrough deployment, which is never annotated",
			yaml: "virtual_keys:\n  allowed_upstream_hosts: [api.openai.com]\nmodel_list:\n  - model_name: m\n    params:\n      format: openai\n      api_base: https://api.openai.com\n      auth_mode: passthrough\n      supports_cache_control: true\n",
			want: "forwards the caller's body unchanged",
		},
	} {
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

// Without translation the gateway cannot carry a request between wire formats,
// so a group must not mix them and a fallback must not cross them.
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

// With translation switched on, both arrangements are the point rather than a
// mistake: one public model name spanning both halves of a fleet, and a group
// in one format falling back to a group in the other.
func TestFormatHomogeneityRelaxedByTranslation(t *testing.T) {
	const enable = "\nrouter:\n  translation:\n    enabled: true\n"

	mixed := `
model_list:
  - model_name: g
    params: {format: anthropic, api_base: "https://api.anthropic.com", auth_mode: api_key, auth_header: x-api-key, api_key: k}
  - model_name: g
    params: {format: openai, api_base: "https://api.openai.com/v1", auth_mode: api_key, auth_header: authorization, api_key: k}
` + enable
	if _, err := Parse([]byte(mixed)); err != nil {
		t.Errorf("a mixed-format group should be allowed with translation on: %v", err)
	}

	crossFallback := `
model_list:
  - model_name: a
    params: {format: anthropic, api_base: "https://api.anthropic.com", auth_mode: api_key, auth_header: x-api-key, api_key: k}
  - model_name: o
    params: {format: openai, api_base: "https://api.openai.com/v1", auth_mode: api_key, auth_header: authorization, api_key: k}
router:
  translation:
    enabled: true
  fallbacks:
    - from: a
      to: [o]
`
	if _, err := Parse([]byte(crossFallback)); err != nil {
		t.Errorf("a cross-format fallback should be allowed with translation on: %v", err)
	}

	// A passthrough deployment sharing a group with another format is not an
	// error and not a hole: it is simply unreachable from an ingress of that
	// other format, which the router enforces at selection.
	withPassthrough := `
virtual_keys:
  allowed_upstream_hosts: ["api.anthropic.com"]
model_list:
  - model_name: g
    params: {format: anthropic, api_base: "https://api.anthropic.com", auth_mode: passthrough}
  - model_name: g
    params: {format: openai, api_base: "https://api.openai.com/v1", auth_mode: api_key, auth_header: authorization, api_key: k}
` + enable
	if _, err := Parse([]byte(withPassthrough)); err != nil {
		t.Errorf("a subscription deployment must still be allowed beside a translated one: %v", err)
	}

	negative := `
model_list:
  - model_name: g
    params: {format: openai, api_base: "https://api.openai.com/v1", auth_mode: api_key, auth_header: authorization, api_key: k}
router:
  translation:
    enabled: true
    default_max_tokens: -1
`
	if _, err := Parse([]byte(negative)); err == nil || !strings.Contains(err.Error(), "default_max_tokens") {
		t.Errorf("a negative default cap should be rejected, got %v", err)
	}
}

// The exporter is the one component whose own failures nothing else reports: a
// wrong endpoint produces no bad responses and no wrong numbers, just an absence
// that looks exactly like having configured nothing. So the checkable parts are
// checked at load, where the operator is still looking.
func TestOTLPValidation(t *testing.T) {
	tests := []struct {
		name    string
		otlp    string
		wantErr string
	}{
		{
			name: "a complete exporter",
			otlp: "endpoint: http://localhost:4318\n    protocol: http/json\n    interval: 30s\n    timeout: 5s",
		},
		{
			name: "no exporter at all",
			otlp: "",
		},
		{
			name:    "configured but never turned on",
			otlp:    "protocol: http/protobuf\n    interval: 30s",
			wantErr: "configured without an endpoint",
		},
		{
			name:    "a scheme that names no transport",
			otlp:    "endpoint: otlp://collector:4317",
			wantErr: "scheme must be http or https",
		},
		{
			name:    "grpc, which this exporter does not speak",
			otlp:    "endpoint: http://localhost:4317\n    protocol: grpc",
			wantErr: "grpc is not implemented",
		},
		{
			name:    "an unknown protocol",
			otlp:    "endpoint: http://localhost:4318\n    protocol: thrift",
			wantErr: "must be \"http/protobuf\" or \"http/json\"",
		},
		{
			name:    "a timeout that outlasts the interval",
			otlp:    "endpoint: http://localhost:4318\n    interval: 5s\n    timeout: 10s",
			wantErr: "must be below observability.otlp.interval",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, k := range []string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT"} {
				t.Setenv(k, "")
			}
			body := `
model_list:
  - model_name: m
    params:
      format: anthropic
      api_base: https://api.anthropic.com
      auth_mode: api_key
      auth_header: x-api-key
      api_key: k
`
			if tt.otlp != "" {
				body += "observability:\n  otlp:\n    " + tt.otlp + "\n"
			}
			_, err := Parse([]byte(body))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Parse: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("Parse succeeded, want an error containing %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("Parse error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// The standard OpenTelemetry variables are how a collector sidecar configures
// every other component in a fleet. A gateway that read only its own YAML would
// be the one process needing to be told separately.
func TestOTLPReadsStandardEnvironment(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	t.Setenv("OTEL_SERVICE_NAME", "gateway-eu")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "api-key=abc123,x-tenant=acme")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=staging,cloud.region=eu-west-1")

	cfg, err := Parse([]byte(`
model_list:
  - model_name: m
    params:
      format: anthropic
      api_base: https://api.anthropic.com
      auth_mode: api_key
      auth_header: x-api-key
      api_key: k
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	o := cfg.Observability.OTLP
	if !o.Enabled() || o.Endpoint != "http://collector:4318" {
		t.Fatalf("endpoint = %q, want it read from OTEL_EXPORTER_OTLP_ENDPOINT", o.Endpoint)
	}
	if o.ServiceName != "gateway-eu" {
		t.Errorf("service_name = %q, want gateway-eu", o.ServiceName)
	}
	if o.Headers["api-key"] != "abc123" || o.Headers["x-tenant"] != "acme" {
		t.Errorf("headers = %v, want both pairs parsed", o.Headers)
	}
	if o.ResourceAttributes["cloud.region"] != "eu-west-1" {
		t.Errorf("resource attributes = %v, want cloud.region parsed", o.ResourceAttributes)
	}
}

// The config file is the more specific statement, so it wins — but only where
// it speaks. A file naming one header must not discard the rest.
func TestOTLPConfigOverridesEnvironmentPerKey(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://from-env:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "api-key=from-env,x-tenant=acme")

	cfg, err := Parse([]byte(`
model_list:
  - model_name: m
    params:
      format: anthropic
      api_base: https://api.anthropic.com
      auth_mode: api_key
      auth_header: x-api-key
      api_key: k
observability:
  otlp:
    endpoint: http://from-file:4318
    headers:
      api-key: from-file
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	o := cfg.Observability.OTLP
	if o.Endpoint != "http://from-file:4318" {
		t.Errorf("endpoint = %q, want the config file to win", o.Endpoint)
	}
	if o.Headers["api-key"] != "from-file" {
		t.Errorf("api-key = %q, want the config file to win", o.Headers["api-key"])
	}
	if o.Headers["x-tenant"] != "acme" {
		t.Errorf("x-tenant = %q, want the environment's header kept alongside", o.Headers["x-tenant"])
	}
}

// The postgres key store is what lets a fleet share one set of keys, so every
// half-written version of it is refused rather than resolved into a different
// store than the operator wrote.

func TestPostgresKeyStoreWithoutADSNIsRefused(t *testing.T) {
	_, err := Parse([]byte(minimalConfig + `
virtual_keys:
  store:
    kind: postgres
`))
	if err == nil || !strings.Contains(err.Error(), "virtual_keys.store.dsn") {
		t.Fatalf("want a rejection naming virtual_keys.store.dsn, got %v", err)
	}
}

// The likely cause of an empty dsn is a ${VAR} that was never set in the
// environment, which would otherwise start a gateway pointed at nothing.
func TestPostgresKeyStoreWithAnUnsetEnvDSNIsRefused(t *testing.T) {
	_, err := Parse([]byte(minimalConfig + `
virtual_keys:
  store:
    kind: postgres
    dsn: ${GATEWAY_TEST_UNSET_DSN}
`))
	if err == nil || !strings.Contains(err.Error(), "virtual_keys.store.dsn") {
		t.Fatalf("want a rejection naming virtual_keys.store.dsn, got %v", err)
	}
}

// A dsn beside a file store means the keys are not where one of those two lines
// says they are, and leaves a database password in a file that has no use for
// it.
func TestADSNOnAFileKeyStoreIsRefused(t *testing.T) {
	_, err := Parse([]byte(minimalConfig + `
virtual_keys:
  store:
    kind: file
    path: ./data/keys.json
    dsn: postgres://gateway:secret@db:5432/gateway
`))
	if err == nil || !strings.Contains(err.Error(), "virtual_keys.store.dsn") {
		t.Fatalf("want a rejection naming virtual_keys.store.dsn, got %v", err)
	}
}

func TestPostgresKeyStoreParses(t *testing.T) {
	t.Setenv("GATEWAY_TEST_DSN", "postgres://gateway:secret@db:5432/gateway?sslmode=require")
	cfg, err := Parse([]byte(minimalConfig + `
virtual_keys:
  store:
    kind: postgres
    dsn: ${GATEWAY_TEST_DSN}
    timeout: 500ms
    max_conns: 4
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s := cfg.VirtualKeys.Store
	if s.DSN != "postgres://gateway:secret@db:5432/gateway?sslmode=require" {
		t.Errorf("dsn = %q, want the expanded environment value", s.DSN)
	}
	if s.Timeout != 500*time.Millisecond {
		t.Errorf("timeout = %s, want 500ms", s.Timeout)
	}
	if s.MaxConns != 4 {
		t.Errorf("max_conns = %d, want 4", s.MaxConns)
	}
}
