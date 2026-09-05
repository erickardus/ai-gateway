package provider

import (
	"net/http"
	"strings"
	"testing"

	"github.com/erickardus/ai-gateway/internal/auth"
	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/testutil"
)

// A base URL that already carries the path's leading segment must not double it.
func TestBuildURLDoesNotDoublePrefix(t *testing.T) {
	tests := []struct {
		base, path, query, want string
	}{
		{"https://api.openai.com/v1", "/v1/chat/completions", "", "https://api.openai.com/v1/chat/completions"},
		{"https://api.anthropic.com", "/v1/messages", "beta=true", "https://api.anthropic.com/v1/messages?beta=true"},
		{"https://host/anthropic", "/v1/messages", "", "https://host/anthropic/v1/messages"},
		{"https://api.openai.com/v1/", "/v1/chat/completions", "", "https://api.openai.com/v1/chat/completions"},
		// A query on the base (Azure's api-version) must survive alongside the
		// caller's own.
		{"https://x.azure.com/openai?api-version=2024-02-01", "/v1/messages", "beta=true",
			"https://x.azure.com/openai/v1/messages?api-version=2024-02-01&beta=true"},
	}
	for _, tc := range tests {
		got, err := buildURL(tc.base, tc.path, tc.query)
		if err != nil {
			t.Errorf("buildURL(%q, %q): %v", tc.base, tc.path, err)
			continue
		}
		if got != tc.want {
			t.Errorf("buildURL(%q, %q, %q)\n got: %s\nwant: %s", tc.base, tc.path, tc.query, got, tc.want)
		}
	}
}

// Relaying must be gated on the value actually being a provider credential, not
// merely on which header it arrived in. Otherwise a caller who puts their
// virtual key in Authorization as well has it forwarded to the provider.
func TestPassthroughNeverRelaysAVirtualKey(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-vk-GATEWAYKEY")
	h.Set("x-gateway-key", "sk-vk-GATEWAYKEY")

	params := config.DeploymentParams{AuthMode: core.AuthModePassthrough, APIBase: "https://api.anthropic.com"}
	creds := auth.Credentials{Key: "sk-vk-GATEWAYKEY", ViaHeader: "X-Gateway-Key"}

	out := BuildUpstreamHeaders(h, params, creds, keyHeaders)
	if v := out.Get("Authorization"); v != "" {
		t.Fatalf("the gateway's own virtual key was relayed upstream: %q", v)
	}
}

// A real subscription token in the same position must still be relayed.
func TestPassthroughRelaysRealCredential(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-ant-oat01-REAL")
	h.Set("x-gateway-key", "sk-vk-KEY")

	params := config.DeploymentParams{AuthMode: core.AuthModePassthrough, APIBase: "https://api.anthropic.com"}
	creds := auth.Credentials{Key: "sk-vk-KEY", ViaHeader: "X-Gateway-Key"}

	out := BuildUpstreamHeaders(h, params, creds, keyHeaders)
	if v := out.Get("Authorization"); v != "Bearer sk-ant-oat01-REAL" {
		t.Fatalf("Authorization = %q, want the caller's real credential relayed", v)
	}
}

// A non-streaming body is a single JSON object with no trailing newline; usage
// must still be extracted or every token limit goes unenforced.
func TestUsageFromNonStreamingBody(t *testing.T) {
	body := `{"id":"msg_1","type":"message","usage":{"input_tokens":1234,"output_tokens":56}}`
	rec := testutil.NewSyncWriter()
	usage, err := Relay(rec, strings.NewReader(body), core.FormatAnthropic)
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if usage.InputTokens != 1234 || usage.OutputTokens != 56 {
		t.Errorf("usage = %+v, want 1234/56 from a body with no trailing newline", usage)
	}
	if rec.String() != body {
		t.Error("body was altered in relay")
	}
}
