// Package ccsettings reads and writes Claude Code's settings file.
//
// It exists because of a gap in what Claude Code can be told to do. A gateway
// key has to travel in ANTHROPIC_CUSTOM_HEADERS — every other way of handing
// Claude Code a credential writes Authorization or x-api-key and so displaces a
// claude.ai subscription login — and that variable is static. There is no
// helper command behind it the way there is behind apiKeyHelper. So the only
// way a developer's key can be provisioned without them pasting it somewhere is
// for their own tooling to write it into the settings file.
//
// The file belongs to the developer and holds settings this package knows
// nothing about, so it is edited rather than generated: the document is decoded
// into a generic map, the few keys this package owns are set, and everything
// else is written back untouched. The one cosmetic cost is key ordering, which
// JSON object decoding does not preserve.
package ccsettings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Settings file and directory permissions. The file carries a credential, so it
// is readable only by its owner, and the directory only traversable by them.
const (
	filePerm = 0o600
	dirPerm  = 0o700
)

// EnvVar names the variables this package owns. Everything else in the env
// block is left alone.
const (
	EnvBaseURL       = "ANTHROPIC_BASE_URL"
	EnvModel         = "ANTHROPIC_MODEL"
	EnvCustomHeaders = "ANTHROPIC_CUSTOM_HEADERS"
	// EnvSmallFastModel is the model Claude Code uses for its own background
	// work — session titles and the like — rather than for the developer's
	// turns. It is set separately because the cheap choice for that traffic is
	// rarely the model someone wants answering them.
	EnvSmallFastModel = "ANTHROPIC_SMALL_FAST_MODEL"
	// EnvCustomModelOption registers one model name that is not Claude Code's
	// own, so that /model and --model will resolve it.
	//
	// Without it a gateway model name is not merely absent from the picker: it
	// is unresolvable, and Claude Code answers an unresolvable name by falling
	// back to the saved default *silently*. The request succeeds against the
	// wrong model, which is worse than an error. There is one slot, so only one
	// such model can be selectable at a time.
	EnvCustomModelOption = "ANTHROPIC_CUSTOM_MODEL_OPTION"
	// EnvCustomModelOptionName and EnvCustomModelOptionDescription are how that
	// entry is labelled in the picker. Cosmetic, but a row reading
	// "kimi-k3-anthropic" next to "Sonnet 5" is a row nobody can tell the
	// purpose of six months later.
	EnvCustomModelOptionName        = "ANTHROPIC_CUSTOM_MODEL_OPTION_NAME"
	EnvCustomModelOptionDescription = "ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION"
)

// ownedVars are the variables this package writes and removes. SetEnv and
// ClearEnv both read it, so a variable cannot be added to one and forgotten in
// the other — which would leave a logout that tidies away a key but leaves the
// model selection it was written beside.
var ownedVars = []string{
	EnvBaseURL,
	EnvModel,
	EnvSmallFastModel,
	EnvCustomHeaders,
	EnvCustomModelOption,
	EnvCustomModelOptionName,
	EnvCustomModelOptionDescription,
}

// displacingVars are the settings that replace a claude.ai subscription login
// with a per-token credential. Writing a gateway key alongside any of them
// would produce a configuration that works and quietly bills the developer, so
// their presence is reported rather than overwritten: they may be there on
// purpose, and it is not this tool's place to decide.
var displacingVars = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}

// Dir returns Claude Code's configuration directory.
func Dir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".claude"), nil
}

// SettingsPath returns the user settings file within a configuration directory.
func SettingsPath(dir string) string { return filepath.Join(dir, "settings.json") }

// File is a settings document open for editing.
type File struct {
	path string
	doc  map[string]any
}

// Load reads a settings file. A file that does not exist yet loads as an empty
// document, so a first login on a fresh machine is not a special case.
func Load(path string) (*File, error) {
	f := &File{path: path, doc: map[string]any{}}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return f, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return f, nil
	}
	if err := json.Unmarshal(raw, &f.doc); err != nil {
		// Refusing is the only safe answer. Overwriting a file that failed to
		// parse would discard settings the developer cannot get back.
		return nil, fmt.Errorf("%s is not valid JSON, so it will not be modified: %w", path, err)
	}
	return f, nil
}

// Path returns the file's location.
func (f *File) Path() string { return f.path }

// Conflicts lists the settings in this file that would displace a claude.ai
// subscription login. An empty result means a gateway key can be written here
// and the subscription will still be the credential Claude Code uses.
func (f *File) Conflicts() []string {
	var found []string
	if v, ok := f.doc["apiKeyHelper"]; ok {
		if s, _ := v.(string); strings.TrimSpace(s) != "" {
			found = append(found, "apiKeyHelper")
		}
	}
	env := f.env(false)
	for _, name := range displacingVars {
		if v, ok := env[name]; ok {
			if s, _ := v.(string); strings.TrimSpace(s) != "" {
				found = append(found, name)
			}
		}
	}
	sort.Strings(found)
	return found
}

// env returns the env block, creating it when create is set.
func (f *File) env(create bool) map[string]any {
	if v, ok := f.doc["env"]; ok {
		if m, ok := v.(map[string]any); ok {
			return m
		}
	}
	m := map[string]any{}
	if create {
		f.doc["env"] = m
	}
	return m
}

// SetEnv applies the variables the gateway supplied, and reports whether
// anything changed. Nothing changing is the ordinary case on a renewal that did
// not need to reissue, and lets the caller skip the write entirely.
func (f *File) SetEnv(vars map[string]string) bool {
	env := f.env(true)
	changed := false
	for _, name := range ownedVars {
		want, ok := vars[name]
		if !ok {
			continue
		}
		if have, _ := env[name].(string); have == want {
			continue
		}
		env[name] = want
		changed = true
	}
	return changed
}

// EnsureHook adds a SessionStart hook running command, unless one is already
// there, and reports whether it added anything.
//
// Idempotence is the whole job: login is expected to be run again, and a hook
// appended once per run would eventually start a renewal check per stored copy.
func (f *File) EnsureHook(command string) bool {
	hooks, _ := f.doc["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	groups, _ := hooks["SessionStart"].([]any)

	for _, g := range groups {
		group, _ := g.(map[string]any)
		entries, _ := group["hooks"].([]any)
		for _, e := range entries {
			entry, _ := e.(map[string]any)
			if cmd, _ := entry["command"].(string); cmd == command {
				return false
			}
		}
	}

	groups = append(groups, map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": command}},
	})
	hooks["SessionStart"] = groups
	f.doc["hooks"] = hooks
	return true
}

// RemoveHook drops a SessionStart hook running command, and reports whether it
// removed anything. Empty groups left behind by the removal go too, so signing
// out does not leave scaffolding in the file.
func (f *File) RemoveHook(command string) bool {
	hooks, _ := f.doc["hooks"].(map[string]any)
	if hooks == nil {
		return false
	}
	groups, _ := hooks["SessionStart"].([]any)
	kept := make([]any, 0, len(groups))
	removed := false

	for _, g := range groups {
		group, _ := g.(map[string]any)
		if group == nil {
			kept = append(kept, g)
			continue
		}
		entries, _ := group["hooks"].([]any)
		keptEntries := make([]any, 0, len(entries))
		for _, e := range entries {
			entry, _ := e.(map[string]any)
			if cmd, _ := entry["command"].(string); cmd == command {
				removed = true
				continue
			}
			keptEntries = append(keptEntries, e)
		}
		if len(keptEntries) == 0 && len(entries) > 0 {
			continue
		}
		group["hooks"] = keptEntries
		kept = append(kept, group)
	}

	if !removed {
		return false
	}
	if len(kept) == 0 {
		delete(hooks, "SessionStart")
	} else {
		hooks["SessionStart"] = kept
	}
	if len(hooks) == 0 {
		delete(f.doc, "hooks")
	}
	return true
}

// GatewayKey returns the virtual key already recorded in this file, or "".
//
// It is what lets a second run reconfigure a project without the developer
// finding their credential again — and without it passing through a shell
// history on the way. The header name is not assumed: it is configurable on the
// gateway, so whatever name is present is accepted and only the value is read.
func (f *File) GatewayKey() string {
	raw, _ := f.env(false)[EnvCustomHeaders].(string)
	var first string
	for _, line := range strings.Split(raw, "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if value == "" {
			continue
		}
		// Claude Code allows several headers here. Preferring the one whose
		// name says "key" keeps an unrelated header from being mistaken for a
		// credential; the first header is the fallback because the gateway
		// writes exactly one.
		if strings.Contains(strings.ToLower(name), "key") {
			return value
		}
		if first == "" {
			first = value
		}
	}
	return first
}

// RemoveEnv deletes the named variables, and reports whether anything changed.
// Only variables this package owns may be removed, so a caller cannot use it to
// strip settings that belong to the developer.
func (f *File) RemoveEnv(names ...string) bool {
	env := f.env(false)
	changed := false
	for _, name := range names {
		if !slices.Contains(ownedVars, name) {
			continue
		}
		if _, ok := env[name]; ok {
			delete(env, name)
			changed = true
		}
	}
	if len(env) == 0 {
		delete(f.doc, "env")
	}
	return changed
}

// ClearEnv removes the variables this package owns, and reports whether
// anything changed.
func (f *File) ClearEnv() bool {
	env := f.env(false)
	changed := false
	for _, name := range ownedVars {
		if _, ok := env[name]; ok {
			delete(env, name)
			changed = true
		}
	}
	if len(env) == 0 {
		delete(f.doc, "env")
	}
	return changed
}

// Save writes the document back.
func (f *File) Save() error {
	raw, err := json.MarshalIndent(f.doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	return WriteFileAtomic(f.path, append(raw, '\n'))
}

// WriteFileAtomic writes a private file through a temporary one in the same
// directory, so an interrupted write cannot leave a truncated settings file
// behind — which for this file would mean a developer's whole Claude Code
// configuration, not just the part written here.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".settings-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(filePerm); err != nil {
		tmp.Close()
		return fmt.Errorf("set permissions on %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
