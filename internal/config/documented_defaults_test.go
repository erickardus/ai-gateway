package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDocumentedDefaultsMatchCode pins the defaults that docs/configuration.md
// publishes.
//
// A reference that quietly drifts from the code is worse than none: someone
// reads it, configures against it, and gets different behaviour. Changing a
// default should therefore break this test, as a prompt to update the table
// alongside it.
// documentedDefault is one row of the reference table, paired with the value
// the code actually produces.
type documentedDefault struct {
	field string
	got   any
	want  any
}

func TestDocumentedDefaultsMatchCode(t *testing.T) {
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

	documented := []documentedDefault{
		{"server.addr", cfg.Server.Addr, "0.0.0.0:4000"},
		{"server.read_header_timeout", cfg.Server.ReadHeaderTimeout.String(), "30s"},
		{"server.idle_timeout", cfg.Server.IdleTimeout.String(), "2m0s"},
		{"server.shutdown_grace", cfg.Server.ShutdownGrace.String(), "30s"},
		{"server.max_body_bytes", cfg.Server.MaxBodyBytes, int64(33554432)},

		{"router.strategy", cfg.Router.Strategy, "weighted-shuffle"},
		{"router.num_retries", cfg.Router.Retries(), 2},
		{"router.timeout", cfg.Router.Timeout.String(), "10m0s"},
		{"router.stream_timeout", cfg.Router.StreamTimeout.String(), "1m0s"},
		{"router.cooldown.allowed_fails", cfg.Router.Cooldown.Fails(), 3},
		{"router.cooldown.period", cfg.Router.Cooldown.Period.String(), "30s"},
		{"router.backoff.initial", cfg.Router.Backoff.Initial.String(), "500ms"},
		{"router.backoff.max", cfg.Router.Backoff.Max.String(), "8s"},
		{"router.backoff.jitter", cfg.Router.Backoff.JitterFactor(), 0.75},
		{"router.max_fallback_hops", cfg.Router.MaxFallbackHops, 5},

		{"model_list[].weight", cfg.ModelList[0].Share(), 1},
		{"model_list[].params.model", cfg.ModelList[0].Params.Model, "m"},

		{"virtual_keys.store.kind", cfg.VirtualKeys.Store.Kind, "memory"},

		{"redis.key_prefix", cfg.Redis.KeyPrefix, "ai-gateway"},
		{"redis.timeout", cfg.Redis.Timeout.String(), "250ms"},

		{"cache.enabled", cfg.Cache.Enabled, false},
		{"cache.ttl", cfg.Cache.TTL.String(), "5m0s"},
		{"cache.scope", string(cfg.Cache.Scope), "key"},
		{"cache.max_entries", cfg.Cache.MaxEntries, 1000},
		{"cache.max_entry_bytes", cfg.Cache.MaxEntryBytes, int64(1048576)},

		{"prompt_cache.affinity", cfg.PromptCache.AffinityEnabled(), true},
		{"prompt_cache.affinity_ttl", cfg.PromptCache.AffinityTTL.String(), "5m0s"},
		{"prompt_cache.affinity_max_in_flight_lead", cfg.PromptCache.MaxInFlightLead(), 4},
		{"prompt_cache.inject", cfg.PromptCache.Inject, false},
		{"prompt_cache.inject_min_bytes", cfg.PromptCache.InjectMinBytes, 4096},

		{"audit.enabled", cfg.Audit.On(), true},
		{"audit.sink", cfg.Audit.SinkKind(), "stdout"},

		{"observability.log_level", cfg.Observability.LogLevel, "info"},
		{"observability.log_format", cfg.Observability.LogFormat, "text"},
		{"observability.metrics", cfg.Observability.Metrics, false},
		{"observability.spend_flush_interval", cfg.Observability.SpendFlushInterval.String(), "30s"},
		{"observability.stream_usage", cfg.Observability.StreamUsageEnabled(), true},
	}

	// The SSO defaults only exist once SSO is on, so they are read from a config
	// that enables it.
	ssoCfg, err := Parse([]byte(ssoConfig))
	if err != nil {
		t.Fatalf("Parse sso config: %v", err)
	}
	documented = append(documented,
		documentedDefault{"sso.key_duration", ssoCfg.SSO.KeyDuration.String(), "720h0m0s"},
		documentedDefault{"sso.renew_within", ssoCfg.SSO.RenewWithin.String(), "168h0m0s"},
		documentedDefault{"sso.role_claim", ssoCfg.SSO.RoleClaim, "groups"},
	)

	// The jwt_auth defaults only exist once it is enabled, and the audience
	// default is derived from the client id rather than written down.
	jwtCfg, err := Parse([]byte(ssoConfig + "  jwt_auth:\n    enabled: true\n"))
	if err != nil {
		t.Fatalf("Parse jwt_auth config: %v", err)
	}
	documented = append(documented,
		documentedDefault{"sso.jwt_auth.enabled", ssoCfg.SSO.JWTAuth.Enabled, false},
		documentedDefault{"sso.jwt_auth.cache_ttl", jwtCfg.SSO.JWTAuth.TTL().String(), "1m0s"},
		documentedDefault{"sso.jwt_auth.audiences", strings.Join(jwtCfg.SSO.JWTAuth.Audiences, ","), jwtCfg.SSO.ClientID},
	)

	for _, d := range documented {
		if d.got != d.want {
			t.Errorf("%s = %v, but docs/configuration.md says %v — update both together", d.field, d.got, d.want)
		}
	}

	// The default key headers are documented as an ordered pair.
	want := []string{"x-gateway-key", "x-litellm-api-key"}
	if len(cfg.VirtualKeys.HeaderNames) != len(want) {
		t.Fatalf("virtual_keys.header_names = %v, want %v", cfg.VirtualKeys.HeaderNames, want)
	}
	for i, name := range want {
		if cfg.VirtualKeys.HeaderNames[i] != name {
			t.Errorf("virtual_keys.header_names[%d] = %q, want %q", i, cfg.VirtualKeys.HeaderNames[i], name)
		}
	}
}

// TestDocsReferenceRealEndpoints guards against the configuration reference
// listing a route the server does not serve.
func TestDocsReferenceRealEndpoints(t *testing.T) {
	docs, err := os.ReadFile(filepath.Join("..", "..", "docs", "configuration.md"))
	if err != nil {
		t.Skipf("configuration reference not readable: %v", err)
	}
	server, err := os.ReadFile(filepath.Join("..", "..", "internal", "server", "server.go"))
	if err != nil {
		t.Fatalf("read server: %v", err)
	}

	for _, route := range []string{
		"/v1/messages", "/v1/messages/count_tokens", "/v1/chat/completions",
		"/v1/models", "/api/hello", "/metrics", "/spend/scopes",
		"/health", "/health/liveliness", "/health/readiness",
		"/key/generate", "/key/info", "/key/list", "/key/delete",
		"/spend/keys", "/spend/deployments", "/cache/purge",
		"/sso/login", "/sso/callback", "/sso/exchange", "/sso/renew",
	} {
		if !strings.Contains(string(docs), route) {
			t.Errorf("docs/configuration.md does not mention route %s", route)
		}
		if !strings.Contains(string(server), route) {
			t.Errorf("docs list route %s but the server does not register it", route)
		}
	}
}

// TestDocumentedOTLPDefaultsMatchCode pins the exporter defaults published in
// docs/configuration.md and docs/observability.md.
//
// They are checked separately from the rest because they apply only once an
// endpoint turns the exporter on: with none set, the whole block stays zero so
// that a gateway exporting nothing does not carry a half-configured exporter.
func TestDocumentedOTLPDefaultsMatchCode(t *testing.T) {
	// The defaults read the standard OpenTelemetry variables, so the ambient
	// environment has to be cleared or the assertions depend on the machine.
	for _, k := range []string{
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "OTEL_EXPORTER_OTLP_PROTOCOL",
		"OTEL_EXPORTER_OTLP_METRICS_HEADERS", "OTEL_EXPORTER_OTLP_HEADERS",
		"OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES",
	} {
		t.Setenv(k, "")
	}

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
    endpoint: http://localhost:4318
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	o := cfg.Observability.OTLP
	documented := []struct {
		field string
		got   any
		want  any
	}{
		{"observability.otlp.protocol", o.Protocol, "http/protobuf"},
		{"observability.otlp.interval", o.Interval.String(), "1m0s"},
		{"observability.otlp.timeout", o.Timeout.String(), "10s"},
		{"observability.otlp.compress", o.CompressEnabled(), true},
		{"observability.otlp.service_name", o.ServiceName, "ai-gateway"},
	}
	for _, d := range documented {
		if d.got != d.want {
			t.Errorf("%s = %v, but the docs say %v — update both together", d.field, d.got, d.want)
		}
	}

	// With no endpoint the block stays empty, so nothing half-configured is
	// carried by a gateway that exports nothing.
	off, err := Parse([]byte(`
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
	if off.Observability.OTLP.Enabled() {
		t.Error("the exporter reported itself enabled with no endpoint configured")
	}
	if off.Observability.OTLP.Protocol != "" || off.Observability.OTLP.Interval != 0 {
		t.Errorf("defaults were applied to a disabled exporter: %+v", off.Observability.OTLP)
	}
}
