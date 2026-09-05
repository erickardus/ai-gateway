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

	documented := []struct {
		field string
		got   any
		want  any
	}{
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
		{"prompt_cache.inject", cfg.PromptCache.Inject, false},
		{"prompt_cache.inject_min_bytes", cfg.PromptCache.InjectMinBytes, 4096},

		{"observability.log_level", cfg.Observability.LogLevel, "info"},
		{"observability.log_format", cfg.Observability.LogFormat, "text"},
		{"observability.metrics", cfg.Observability.Metrics, false},
		{"observability.spend_flush_interval", cfg.Observability.SpendFlushInterval.String(), "30s"},
	}

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
		"/v1/models", "/api/hello", "/metrics",
		"/health", "/health/liveliness", "/health/readiness",
		"/key/generate", "/key/info", "/key/list", "/key/delete",
		"/spend/keys", "/spend/deployments", "/cache/purge",
	} {
		if !strings.Contains(string(docs), route) {
			t.Errorf("docs/configuration.md does not mention route %s", route)
		}
		if !strings.Contains(string(server), route) {
			t.Errorf("docs list route %s but the server does not register it", route)
		}
	}
}
