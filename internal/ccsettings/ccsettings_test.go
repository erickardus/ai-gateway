package ccsettings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("settings file is not valid JSON after writing: %v", err)
	}
	return doc
}

// The file belongs to the developer. Anything this package does not own has to
// come back out the way it went in, or a login would quietly discard settings
// that took someone a while to get right.
func TestSaveKeepsEverythingItDoesNotOwn(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "settings.json", `{
	  "model": "opus",
	  "permissions": {"deny": ["Bash(curl *)"]},
	  "env": {"CLAUDE_CODE_ENABLE_TELEMETRY": "1", "ANTHROPIC_MODEL": "old-model"},
	  "hooks": {"PostToolUse": [{"matcher": "Edit", "hooks": [{"type": "command", "command": "audit"}]}]}
	}`)

	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !f.SetEnv(map[string]string{
		EnvBaseURL:       "https://gw.example.com",
		EnvModel:         "anthropic-claude",
		EnvCustomHeaders: "x-gateway-key: sk-vk-abc",
	}) {
		t.Fatal("SetEnv reported no change")
	}
	if !f.EnsureHook("gateway login --renew --if-expiring") {
		t.Fatal("EnsureHook reported no change")
	}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}

	doc := readDoc(t, path)
	if doc["model"] != "opus" {
		t.Errorf("model = %v, want it preserved", doc["model"])
	}
	if _, ok := doc["permissions"]; !ok {
		t.Error("permissions block was dropped")
	}
	env := doc["env"].(map[string]any)
	if env["CLAUDE_CODE_ENABLE_TELEMETRY"] != "1" {
		t.Error("an unrelated env variable was dropped")
	}
	if env[EnvModel] != "anthropic-claude" {
		t.Errorf("%s = %v, want it updated", EnvModel, env[EnvModel])
	}
	if env[EnvCustomHeaders] != "x-gateway-key: sk-vk-abc" {
		t.Errorf("%s = %v", EnvCustomHeaders, env[EnvCustomHeaders])
	}
	hooks := doc["hooks"].(map[string]any)
	if _, ok := hooks["PostToolUse"]; !ok {
		t.Error("an unrelated hook was dropped")
	}
	if _, ok := hooks["SessionStart"]; !ok {
		t.Error("the renewal hook was not added")
	}
}

// login is expected to be run again. A hook appended per run would eventually
// mean a renewal check per stored copy.
func TestEnsureHookIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	const cmd = "gateway login --renew --if-expiring"

	for i := range 3 {
		f, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		added := f.EnsureHook(cmd)
		if i == 0 && !added {
			t.Fatal("the first EnsureHook added nothing")
		}
		if i > 0 && added {
			t.Fatalf("EnsureHook added a duplicate on run %d", i+1)
		}
		if err := f.Save(); err != nil {
			t.Fatal(err)
		}
	}

	doc := readDoc(t, path)
	groups := doc["hooks"].(map[string]any)["SessionStart"].([]any)
	if len(groups) != 1 {
		t.Fatalf("SessionStart has %d groups, want 1", len(groups))
	}
}

func TestRemoveHookLeavesNoScaffolding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	const cmd = "gateway login --renew --if-expiring"

	f, _ := Load(path)
	f.EnsureHook(cmd)
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}

	f, _ = Load(path)
	if !f.RemoveHook(cmd) {
		t.Fatal("RemoveHook removed nothing")
	}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	if _, ok := readDoc(t, path)["hooks"]; ok {
		t.Error("an empty hooks block was left behind")
	}
}

func TestRemoveHookKeepsOtherHooksInTheSameGroup(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "settings.json", `{"hooks": {"SessionStart": [
	  {"hooks": [{"type": "command", "command": "mine"}, {"type": "command", "command": "theirs"}]}
	]}}`)

	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !f.RemoveHook("mine") {
		t.Fatal("RemoveHook removed nothing")
	}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}

	groups := readDoc(t, path)["hooks"].(map[string]any)["SessionStart"].([]any)
	entries := groups[0].(map[string]any)["hooks"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["command"] != "theirs" {
		t.Errorf("entries = %v, want only the unrelated hook", entries)
	}
}

// These three are the reason the feature exists. Writing a gateway key beside
// any of them produces a configuration that works and silently bills the
// developer per token, so it is refused rather than papered over.
func TestConflictsFindsWhatDisplacesTheSubscription(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{name: "clean", body: `{"env": {"ANTHROPIC_BASE_URL": "https://gw"}}`, want: nil},
		{name: "apiKeyHelper", body: `{"apiKeyHelper": "~/bin/key.sh"}`, want: []string{"apiKeyHelper"}},
		{name: "api key", body: `{"env": {"ANTHROPIC_API_KEY": "sk-ant-api03-x"}}`, want: []string{"ANTHROPIC_API_KEY"}},
		{name: "auth token", body: `{"env": {"ANTHROPIC_AUTH_TOKEN": "tok"}}`, want: []string{"ANTHROPIC_AUTH_TOKEN"}},
		{
			name: "all of them",
			body: `{"apiKeyHelper": "x", "env": {"ANTHROPIC_API_KEY": "a", "ANTHROPIC_AUTH_TOKEN": "b"}}`,
			want: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "apiKeyHelper"},
		},
		{name: "empty values do not count", body: `{"apiKeyHelper": "", "env": {"ANTHROPIC_API_KEY": ""}}`, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := write(t, t.TempDir(), "settings.json", tc.body)
			f, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			got := f.Conflicts()
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("Conflicts() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Overwriting a file that failed to parse would discard settings the developer
// cannot get back.
func TestLoadRefusesToTouchUnparseableSettings(t *testing.T) {
	path := write(t, t.TempDir(), "settings.json", `{"env": {`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted a file it cannot safely rewrite")
	}
}

func TestLoadTreatsAMissingFileAsEmpty(t *testing.T) {
	f, err := Load(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatalf("a first login on a fresh machine must not be an error: %v", err)
	}
	if len(f.Conflicts()) != 0 {
		t.Error("an empty document reported conflicts")
	}
}

func TestSavedFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	f, _ := Load(path)
	f.SetEnv(map[string]string{EnvCustomHeaders: "x-gateway-key: sk-vk-secret"})
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != filePerm {
		t.Errorf("mode = %v, want %v: the file holds a credential", perm, os.FileMode(filePerm))
	}
}

func TestSetEnvReportsNoChangeWhenNothingMoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	vars := map[string]string{
		EnvBaseURL:       "https://gw.example.com",
		EnvCustomHeaders: "x-gateway-key: sk-vk-abc",
	}
	f, _ := Load(path)
	if !f.SetEnv(vars) {
		t.Fatal("the first SetEnv reported no change")
	}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}

	// A renewal that reissued nothing must not rewrite the file, which is what
	// keeps the SessionStart hook cheap.
	f, _ = Load(path)
	if f.SetEnv(vars) {
		t.Error("SetEnv reported a change when every value already matched")
	}
}

func TestRenewalDue(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		state State
		want  bool
	}{
		{
			name:  "far from expiry",
			state: State{ExpiresAt: now.Add(30 * 24 * time.Hour), RenewWithin: "168h"},
			want:  false,
		},
		{
			name:  "inside the window",
			state: State{ExpiresAt: now.Add(3 * 24 * time.Hour), RenewWithin: "168h"},
			want:  true,
		},
		{
			name:  "already expired",
			state: State{ExpiresAt: now.Add(-time.Hour), RenewWithin: "168h"},
			want:  true,
		},
		{
			name:  "an unreadable window still renews eventually",
			state: State{ExpiresAt: now.Add(time.Hour), RenewWithin: "nonsense"},
			want:  true,
		},
		{
			name:  "no expiry recorded",
			state: State{RenewWithin: "168h"},
			want:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.RenewalDue(now); got != tc.want {
				t.Errorf("RenewalDue = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := StatePath(t.TempDir())
	want := &State{
		Gateway: "https://gw.example.com", Identity: "dev@example.com",
		Subject: "user-1", Device: "laptop", Role: "platform-eng",
		ExpiresAt: time.Now().UTC().Truncate(time.Second), RenewWithin: "168h0m0s",
		RefreshToken: "refresh-token",
	}
	if err := SaveState(path, want); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != filePerm {
		t.Errorf("mode = %v, want %v: the file holds a refresh token", perm, os.FileMode(filePerm))
	}

	got, ok, err := LoadState(path)
	if err != nil || !ok {
		t.Fatalf("LoadState = %v, %v", ok, err)
	}
	if *got != *want {
		t.Errorf("state round trip lost data:\n got %+v\nwant %+v", *got, *want)
	}

	if err := DeleteState(path); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := LoadState(path); ok {
		t.Error("state survived deletion")
	}
	if err := DeleteState(path); err != nil {
		t.Errorf("deleting an absent state file must not be an error: %v", err)
	}
}
