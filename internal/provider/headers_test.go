package provider

import (
	"net/http"
	"testing"

	"github.com/erickardus/ai-gateway/internal/auth"
	"github.com/erickardus/ai-gateway/internal/config"
	"github.com/erickardus/ai-gateway/internal/core"
)

var keyHeaders = []string{"x-gateway-key", "x-litellm-api-key"}

// The headers a real Claude Code request carries on a subscription login.
func claudeCodeHeaders() http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-ant-oat01-SUBSCRIPTION")
	h.Set("anthropic-version", "2023-06-01")
	h.Set("anthropic-beta", "oauth-2025-04-20,claude-code-20250219")
	h.Set("user-agent", "claude-cli/2.1.230")
	h.Set("x-app", "cli")
	h.Set("x-claude-code-session-id", "sess-123")
	h.Set("x-gateway-key", "sk-vk-SECRET")
	h.Set("content-type", "application/json")
	h.Set("connection", "keep-alive")
	return h
}

func TestPassthroughPreservesSubscriptionCredential(t *testing.T) {
	creds := auth.Credentials{Key: "sk-vk-SECRET", ViaHeader: "X-Gateway-Key"}
	params := config.DeploymentParams{
		Format:   core.FormatAnthropic,
		APIBase:  "https://api.anthropic.com",
		AuthMode: core.AuthModePassthrough,
	}

	out := BuildUpstreamHeaders(claudeCodeHeaders(), params, creds, keyHeaders)

	if got := out.Get("Authorization"); got != "Bearer sk-ant-oat01-SUBSCRIPTION" {
		t.Errorf("Authorization = %q, want the caller's OAuth token relayed verbatim", got)
	}
	if got := out.Get("anthropic-beta"); got != "oauth-2025-04-20,claude-code-20250219" {
		t.Errorf("anthropic-beta = %q, want it forwarded verbatim (stripping the OAuth capability 401s the request)", got)
	}
	if got := out.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q", got)
	}
	if got := out.Get("user-agent"); got != "claude-cli/2.1.230" {
		t.Errorf("user-agent = %q, want it forwarded", got)
	}
	if got := out.Get("x-app"); got != "cli" {
		t.Errorf("x-app = %q, want it forwarded", got)
	}
	if got := out.Get("x-gateway-key"); got != "" {
		t.Errorf("the gateway's own virtual key leaked upstream in x-gateway-key: %q", got)
	}
	if got := out.Get("connection"); got != "" {
		t.Errorf("hop-by-hop header forwarded: %q", got)
	}
}

// If the caller authenticated via Authorization itself, that value is the
// gateway's own key and must not be relayed upstream.
func TestPassthroughDoesNotForwardTheAuthenticatingHeader(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-vk-THEGATEWAYKEY")
	creds := auth.Credentials{Key: "sk-vk-THEGATEWAYKEY", ViaHeader: "Authorization"}
	params := config.DeploymentParams{AuthMode: core.AuthModePassthrough, APIBase: "https://api.anthropic.com"}

	out := BuildUpstreamHeaders(h, params, creds, keyHeaders)
	if got := out.Get("Authorization"); got != "" {
		t.Fatalf("the gateway's virtual key was forwarded upstream: %q", got)
	}
}

func TestAPIKeyModeReplacesCallerCredentials(t *testing.T) {
	creds := auth.Credentials{Key: "sk-vk-SECRET", ViaHeader: "X-Gateway-Key"}
	params := config.DeploymentParams{
		Format:     core.FormatAnthropic,
		APIBase:    "https://api.anthropic.com",
		AuthMode:   core.AuthModeAPIKey,
		AuthHeader: "x-api-key",
		APIKey:     "sk-ant-api03-SERVERSIDE",
	}

	out := BuildUpstreamHeaders(claudeCodeHeaders(), params, creds, keyHeaders)

	if got := out.Get("x-api-key"); got != "sk-ant-api03-SERVERSIDE" {
		t.Errorf("x-api-key = %q, want the deployment's own credential", got)
	}
	if got := out.Get("Authorization"); got != "" {
		t.Errorf("the caller's subscription token leaked in api_key mode: %q", got)
	}
	if got := out.Get("anthropic-beta"); got == "" {
		t.Error("anthropic-beta must still be forwarded in api_key mode")
	}
}

func TestAPIKeyModeAppliesScheme(t *testing.T) {
	params := config.DeploymentParams{
		Format:     core.FormatOpenAI,
		AuthMode:   core.AuthModeAPIKey,
		AuthHeader: "authorization",
		AuthScheme: "Bearer",
		APIKey:     "sk-openai",
	}
	out := BuildUpstreamHeaders(http.Header{}, params, auth.Credentials{}, keyHeaders)
	if got := out.Get("Authorization"); got != "Bearer sk-openai" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer sk-openai")
	}
}

func TestGatewayOnlyHeadersAreStripped(t *testing.T) {
	h := claudeCodeHeaders()
	h.Set("x-litellm-num-retries", "5")
	h.Set("x-litellm-api-key", "sk-vk-other")
	params := config.DeploymentParams{AuthMode: core.AuthModePassthrough, APIBase: "https://api.anthropic.com"}
	out := BuildUpstreamHeaders(h, params, auth.Credentials{ViaHeader: "X-Gateway-Key"}, keyHeaders)

	for _, name := range []string{"x-litellm-num-retries", "x-litellm-api-key", "x-gateway-key", "host", "content-length"} {
		if got := out.Get(name); got != "" {
			t.Errorf("%s should not be forwarded, got %q", name, got)
		}
	}
}

// Unknown anthropic-* headers must pass through: capability headers are an open
// list that grows with each Claude Code release.
func TestUnknownAnthropicHeadersForwarded(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-some-future-capability", "on")
	params := config.DeploymentParams{AuthMode: core.AuthModePassthrough, APIBase: "https://api.anthropic.com"}
	out := BuildUpstreamHeaders(h, params, auth.Credentials{}, keyHeaders)
	if got := out.Get("anthropic-some-future-capability"); got != "on" {
		t.Errorf("unknown anthropic-* header was dropped: %q", got)
	}
}
