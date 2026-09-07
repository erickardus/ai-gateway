package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erickardus/ai-gateway/internal/ccsettings"
)

// modelsServer stands in for a gateway serving the named groups.
func modelsServer(t *testing.T, ids ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("x-gateway-key") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		entries := make([]map[string]string, 0, len(ids))
		for _, id := range ids {
			entries = append(entries, map[string]string{"id": id, "description": "1 deployment"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": entries})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// settingsEnv reads back the env block this command wrote.
func settingsEnv(t *testing.T, dir string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	return doc.Env
}

// The whole point of the command: the four settings that make Claude Code talk
// to a gateway, written together and correctly, without anyone remembering that
// the key belongs in a header variable rather than a credential one.
func TestSetupWritesTheSettingsAGatewayNeeds(t *testing.T) {
	t.Setenv("GATEWAY_KEY", "sk-vk-TEST")
	srv := modelsServer(t, "kimi-k3", "claude-sonnet-5")
	dir := filepath.Join(t.TempDir(), ".claude")

	if err := runSetup([]string{"-yes", "-gateway", srv.URL, "-dir", dir, "-model", "kimi-k3"}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	env := settingsEnv(t, dir)
	want := map[string]string{
		ccsettings.EnvBaseURL:        srv.URL,
		ccsettings.EnvModel:          "kimi-k3",
		ccsettings.EnvSmallFastModel: "kimi-k3",
		ccsettings.EnvCustomHeaders:  "x-gateway-key: sk-vk-TEST",
	}
	for name, value := range want {
		if env[name] != value {
			t.Errorf("%s = %q, want %q", name, env[name], value)
		}
	}
	// The key must never land anywhere that displaces the subscription login.
	for _, forbidden := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		if _, ok := env[forbidden]; ok {
			t.Errorf("%s was written; that would replace the subscription with per-token billing", forbidden)
		}
	}
}

// A gateway model has to be registered or Claude Code cannot switch back to it,
// and a model Claude Code already knows must not be — the slot holds one name,
// and spending it on a name that never needed it is what makes the return trip
// impossible.
func TestSetupRegistersOnlyAModelClaudeCodeCannotResolve(t *testing.T) {
	t.Setenv("GATEWAY_KEY", "sk-vk-TEST")
	srv := modelsServer(t, "kimi-k3", "claude-sonnet-5")

	t.Run("gateway model is registered", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), ".claude")
		if err := runSetup([]string{"-yes", "-gateway", srv.URL, "-dir", dir, "-model", "kimi-k3"}); err != nil {
			t.Fatalf("setup: %v", err)
		}
		env := settingsEnv(t, dir)
		if env[ccsettings.EnvCustomModelOption] != "kimi-k3" {
			t.Errorf("%s = %q, want kimi-k3", ccsettings.EnvCustomModelOption, env[ccsettings.EnvCustomModelOption])
		}
		if env[ccsettings.EnvCustomModelOptionName] == "" {
			t.Error("the picker row was left unlabelled")
		}
	})

	t.Run("an anthropic model takes no slot", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), ".claude")
		if err := runSetup([]string{"-yes", "-gateway", srv.URL, "-dir", dir,
			"-model", "claude-sonnet-5", "-picker-model", "none"}); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if v, ok := settingsEnv(t, dir)[ccsettings.EnvCustomModelOption]; ok {
			t.Errorf("%s = %q, want it absent", ccsettings.EnvCustomModelOption, v)
		}
	})
}

// A registration left behind names a model this project no longer uses, and the
// picker would go on offering it.
func TestSetupClearsAStaleRegistration(t *testing.T) {
	t.Setenv("GATEWAY_KEY", "sk-vk-TEST")
	srv := modelsServer(t, "kimi-k3", "claude-sonnet-5")
	dir := filepath.Join(t.TempDir(), ".claude")

	if err := runSetup([]string{"-yes", "-gateway", srv.URL, "-dir", dir, "-model", "kimi-k3"}); err != nil {
		t.Fatalf("first setup: %v", err)
	}
	if err := runSetup([]string{"-yes", "-gateway", srv.URL, "-dir", dir,
		"-model", "claude-sonnet-5", "-picker-model", "none"}); err != nil {
		t.Fatalf("second setup: %v", err)
	}

	env := settingsEnv(t, dir)
	for _, name := range []string{
		ccsettings.EnvCustomModelOption,
		ccsettings.EnvCustomModelOptionName,
		ccsettings.EnvCustomModelOptionDescription,
	} {
		if v, ok := env[name]; ok {
			t.Errorf("%s = %q survived, want it removed", name, v)
		}
	}
}

// Reconfiguring a project should not make someone find their credential again,
// nor put it through a shell history a second time.
func TestSetupReusesTheKeyAlreadyWritten(t *testing.T) {
	t.Setenv("GATEWAY_KEY", "")
	srv := modelsServer(t, "kimi-k3", "claude-sonnet-5")
	dir := filepath.Join(t.TempDir(), ".claude")

	if err := runSetup([]string{"-yes", "-gateway", srv.URL, "-dir", dir,
		"-model", "kimi-k3", "-key", "sk-vk-FIRSTRUN"}); err != nil {
		t.Fatalf("first setup: %v", err)
	}
	if err := runSetup([]string{"-yes", "-gateway", srv.URL, "-dir", dir, "-model", "claude-sonnet-5"}); err != nil {
		t.Fatalf("second setup without a key: %v", err)
	}
	if got := settingsEnv(t, dir)[ccsettings.EnvCustomHeaders]; got != "x-gateway-key: sk-vk-FIRSTRUN" {
		t.Errorf("custom headers = %q, want the key from the first run", got)
	}
}

// A typo should be caught here, where the answer names what is actually served,
// rather than becoming a 404 at the developer's first prompt.
func TestSetupRefusesAModelTheGatewayDoesNotServe(t *testing.T) {
	t.Setenv("GATEWAY_KEY", "sk-vk-TEST")
	srv := modelsServer(t, "kimi-k3")
	dir := filepath.Join(t.TempDir(), ".claude")

	err := runSetup([]string{"-yes", "-gateway", srv.URL, "-dir", dir, "-model", "kimi-k4"})
	if err == nil {
		t.Fatal("a model the gateway does not serve was accepted")
	}
	if !strings.Contains(err.Error(), "kimi-k3") {
		t.Errorf("the refusal should name what is served, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "settings.json")); statErr == nil {
		t.Error("a refused run still wrote a settings file")
	}
}

// Writing a gateway key beside a credential that displaces the subscription
// produces a configuration that works and quietly bills per token.
func TestSetupRefusesToWriteBesideADisplacingCredential(t *testing.T) {
	t.Setenv("GATEWAY_KEY", "sk-vk-TEST")
	srv := modelsServer(t, "kimi-k3")
	dir := filepath.Join(t.TempDir(), ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	existing := `{"env":{"ANTHROPIC_API_KEY":"sk-ant-SOMETHING"}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runSetup([]string{"-yes", "-gateway", srv.URL, "-dir", dir, "-model", "kimi-k3"})
	if err == nil {
		t.Fatal("setup wrote a gateway key beside ANTHROPIC_API_KEY")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("the refusal should name the conflict, got %v", err)
	}
}

func TestNeedsRegistration(t *testing.T) {
	for id, want := range map[string]bool{
		"kimi-k3-anthropic":         true,
		"gpt":                       true,
		"claude-sonnet-5":           false,
		"claude-haiku-4-5-20251001": false,
		"CLAUDE-OPUS-5":             false,
	} {
		if got := needsRegistration(id); got != want {
			t.Errorf("needsRegistration(%q) = %t, want %t", id, got, want)
		}
	}
}

func TestPrettyLabel(t *testing.T) {
	if got := prettyLabel("kimi-k3-anthropic"); got != "Kimi K3 Anthropic" {
		t.Errorf("prettyLabel = %q", got)
	}
}
