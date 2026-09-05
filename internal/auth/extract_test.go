package auth

import (
	"net/http"
	"strings"
	"testing"
)

var defaultHeaders = []string{"x-gateway-key", "x-litellm-api-key"}

func TestExtractGatewayKey(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		wantKey string
		wantVia string
		wantOK  bool
	}{
		{
			name:    "custom header",
			headers: map[string]string{"x-gateway-key": "sk-vk-abc"},
			wantKey: "sk-vk-abc", wantVia: "X-Gateway-Key", wantOK: true,
		},
		{
			name:    "custom header with Bearer scheme",
			headers: map[string]string{"x-gateway-key": "Bearer sk-vk-abc"},
			wantKey: "sk-vk-abc", wantVia: "X-Gateway-Key", wantOK: true,
		},
		{
			name:    "litellm compatibility header",
			headers: map[string]string{"x-litellm-api-key": "Bearer sk-vk-compat"},
			wantKey: "sk-vk-compat", wantVia: "X-Litellm-Api-Key", wantOK: true,
		},
		{
			name: "custom header wins over Authorization",
			headers: map[string]string{
				"x-gateway-key": "sk-vk-win",
				"Authorization": "Bearer sk-vk-lose",
			},
			wantKey: "sk-vk-win", wantVia: "X-Gateway-Key", wantOK: true,
		},
		{
			name:    "plain Authorization is accepted when it is not an upstream credential",
			headers: map[string]string{"Authorization": "Bearer sk-vk-plain"},
			wantKey: "sk-vk-plain", wantVia: "Authorization", wantOK: true,
		},
		{
			name:    "x-api-key is accepted when it is not an upstream credential",
			headers: map[string]string{"x-api-key": "sk-vk-xak"},
			wantKey: "sk-vk-xak", wantVia: "X-Api-Key", wantOK: true,
		},

		// The cases the whole design depends on.
		{
			name:    "subscription OAuth token in Authorization is NEVER a gateway key",
			headers: map[string]string{"Authorization": "Bearer sk-ant-oat01-SUBSCRIPTION"},
			wantOK:  false,
		},
		{
			name:    "Anthropic API key in Authorization is NEVER a gateway key",
			headers: map[string]string{"Authorization": "Bearer sk-ant-api03-CONSOLE"},
			wantOK:  false,
		},
		{
			name:    "Anthropic API key in x-api-key is NEVER a gateway key",
			headers: map[string]string{"x-api-key": "sk-ant-api03-CONSOLE"},
			wantOK:  false,
		},
		{
			name: "the real Claude Code subscription shape: OAuth passes through, custom header authenticates",
			headers: map[string]string{
				"Authorization":  "Bearer sk-ant-oat01-SUBSCRIPTION",
				"x-gateway-key":  "sk-vk-real",
				"anthropic-beta": "oauth-2025-04-20",
			},
			wantKey: "sk-vk-real", wantVia: "X-Gateway-Key", wantOK: true,
		},
		{
			name:    "no credentials at all",
			headers: map[string]string{},
			wantOK:  false,
		},
		{
			name:    "empty header value is ignored",
			headers: map[string]string{"x-gateway-key": "   "},
			wantOK:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tc.headers {
				h.Set(k, v)
			}
			got, ok := ExtractGatewayKey(h, defaultHeaders)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got %+v)", ok, tc.wantOK, got)
			}
			if !tc.wantOK {
				return
			}
			if got.Key != tc.wantKey {
				t.Errorf("Key = %q, want %q", got.Key, tc.wantKey)
			}
			if got.ViaHeader != tc.wantVia {
				t.Errorf("ViaHeader = %q, want %q", got.ViaHeader, tc.wantVia)
			}
		})
	}
}

// A subscription token must never be mistaken for a gateway key regardless of
// casing or scheme spelling.
func TestUpstreamCredentialNeverExtracted(t *testing.T) {
	for _, v := range []string{
		"sk-ant-oat01-x", "Bearer sk-ant-oat01-x", "bearer sk-ant-oat01-x",
		"BEARER sk-ant-oat01-x", "  Bearer   sk-ant-oat01-x  ",
		"sk-ant-api03-x", "Bearer sk-ant-api03-x",
	} {
		for _, hdr := range []string{"Authorization", "x-api-key"} {
			h := http.Header{}
			h.Set(hdr, v)
			if creds, ok := ExtractGatewayKey(h, defaultHeaders); ok {
				t.Errorf("%s: %q was extracted as gateway key %q", hdr, v, creds.Key)
			}
		}
	}
}

func TestRedactNeverLeaksSecret(t *testing.T) {
	secret := "sk-vk-SUPERSECRETVALUE1234567890"
	got := Redact(secret)
	if strings.Contains(got, "SUPERSECRET") {
		t.Fatalf("Redact leaked the secret: %q", got)
	}
	if Redact("") != "" {
		t.Error("empty stays empty")
	}
	if r := Redact("short"); strings.Contains(r, "short") {
		t.Errorf("short secret leaked: %q", r)
	}
}

func TestIsGatewayKeyHeader(t *testing.T) {
	if !IsGatewayKeyHeader("X-Gateway-Key", defaultHeaders) {
		t.Error("case-insensitive match expected")
	}
	if IsGatewayKeyHeader("Authorization", defaultHeaders) {
		t.Error("Authorization is not a gateway key header")
	}
}

// Padded and oddly-cased schemes must not let an upstream credential slip
// through as a gateway key.
func TestUpstreamCredentialEvasionAttempts(t *testing.T) {
	evasions := []string{
		"  Bearer   sk-ant-oat01-x  ",
		"\tBearer\tsk-ant-oat01-x",
		"BeArEr sk-ant-oat01-x",
		"bEaReR   sk-ant-api03-x",
		"  sk-ant-oat01-x",
	}
	for _, v := range evasions {
		for _, hdr := range []string{"Authorization", "x-api-key"} {
			h := http.Header{}
			h.Set(hdr, v)
			if creds, ok := ExtractGatewayKey(h, defaultHeaders); ok {
				t.Errorf("%s: %q evaded the upstream-credential check and was read as key %q", hdr, v, creds.Key)
			}
		}
	}
}
