package config

import (
	"strings"
	"testing"

	"github.com/erickardus/ai-gateway/internal/core"
)

// rbacYAML is a minimal gateway with one hierarchy, so the tests below vary
// only the part they are about.
func rbacYAML(rbac string) string {
	return `
model_list:
  - model_name: anthropic-claude
    params:
      format: anthropic
      api_base: https://api.anthropic.com
      model: anthropic-claude
      auth_mode: api_key
      auth_header: x-api-key
      api_key: k
    cost:
      input_per_1m: 3
      output_per_1m: 15
      cache_read_per_1m: 0.3
      cache_write_per_1m: 3.75
  - model_name: anthropic-claude-haiku
    params:
      format: anthropic
      api_base: https://api.anthropic.com
      model: claude-haiku
      auth_mode: api_key
      auth_header: x-api-key
      api_key: k
    cost:
      input_per_1m: 1
      output_per_1m: 5
      cache_read_per_1m: 0.1
      cache_write_per_1m: 1.25
` + rbac
}

// The nesting is the hierarchy: qualified ids and parent links are derived from
// it rather than written out, so there is one place a team's membership is
// stated.
func TestHierarchyFlattensToQualifiedIDs(t *testing.T) {
	cfg, err := Parse([]byte(rbacYAML(`
rbac:
  organizations:
    - id: acme
      max_budget: 1000
      budget_duration: 720h
      teams:
        - id: platform
          max_budget: 200
          budget_duration: 720h
          projects:
            - id: gateway
        - id: research
`)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	want := map[string]core.ScopeKind{
		"acme":                  core.ScopeOrganization,
		"acme/platform":         core.ScopeTeam,
		"acme/platform/gateway": core.ScopeProject,
		"acme/research":         core.ScopeTeam,
	}
	if got := len(cfg.Scopes()); got != len(want) {
		t.Fatalf("resolved %d scopes, want %d: %v", got, len(want), cfg.ScopeIDs())
	}
	for id, kind := range want {
		s := cfg.Scope(id)
		if s == nil {
			t.Fatalf("scope %q was not resolved; got %v", id, cfg.ScopeIDs())
		}
		if s.Kind != kind {
			t.Errorf("scope %q is a %s, want %s", id, s.Kind, kind)
		}
	}

	project := cfg.Scope("acme/platform/gateway")
	chain := project.Chain()
	if len(chain) != 3 {
		t.Fatalf("chain has %d levels, want 3: %v", len(chain), chain)
	}
	if chain[0].ID != "acme/platform/gateway" || chain[1].ID != "acme/platform" || chain[2].ID != "acme" {
		t.Errorf("chain must run innermost-first, got %s → %s → %s", chain[0].ID, chain[1].ID, chain[2].ID)
	}
	if got := project.Root().ID; got != "acme" {
		t.Errorf("Root() = %q, want %q", got, "acme")
	}
	if got := cfg.Scope("acme/platform").SpendSubject(); got != "team:acme/platform" {
		t.Errorf("SpendSubject() = %q, want the kind to be part of it", got)
	}
}

// An id carrying the separator would produce a qualified id that names a
// different scope than the nesting says.
func TestScopeSegmentRejectsTheSeparator(t *testing.T) {
	_, err := Parse([]byte(rbacYAML(`
rbac:
  organizations:
    - id: acme
      teams:
        - id: platform/gateway
`)))
	if err == nil || !strings.Contains(err.Error(), "must not contain") {
		t.Fatalf("want a rejection naming the separator, got %v", err)
	}
}

// The chain is an intersection, so a child listing a model its parent withholds
// has written something the hierarchy contradicts.
func TestChildCannotWidenItsParentsModels(t *testing.T) {
	_, err := Parse([]byte(rbacYAML(`
rbac:
  organizations:
    - id: acme
      models: ["anthropic-claude-haiku"]
      teams:
        - id: platform
          models: ["anthropic-claude"]
`)))
	if err == nil || !strings.Contains(err.Error(), "cannot widen its parent") {
		t.Fatalf("want a rejection explaining the intersection, got %v", err)
	}
}

// A key or a role pointing at a scope that does not exist fails at load, not on
// the request that happens to present it.
func TestDanglingScopeReferenceIsRefused(t *testing.T) {
	_, err := Parse([]byte(rbacYAML(`
rbac:
  organizations:
    - id: acme
      teams:
        - id: platform

virtual_keys:
  keys:
    - key: sk-vk-dev
      alias: dev
      scope: acme/pltform
`)))
	if err == nil || !strings.Contains(err.Error(), "acme/pltform") {
		t.Fatalf("want the mistyped scope named, got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "acme/platform") {
		t.Errorf("the error should list what was declared, got %q", err)
	}
}

// The same rule a key is held to: a window with nothing to cap limits nothing,
// and is far likelier to be a budget the operator believes they set.
func TestScopeBudgetWindowRequiresACap(t *testing.T) {
	_, err := Parse([]byte(rbacYAML(`
rbac:
  organizations:
    - id: acme
      budget_duration: 720h
`)))
	if err == nil || !strings.Contains(err.Error(), "budget_duration requires max_budget") {
		t.Fatalf("want a rejection of the capless window, got %v", err)
	}
}

// A model name nobody serves would present as a caller being refused something
// no key can reach.
func TestScopeModelMustNameAConfiguredGroup(t *testing.T) {
	_, err := Parse([]byte(rbacYAML(`
rbac:
  organizations:
    - id: acme
      models: ["anthropic-clyde"]
`)))
	if err == nil || !strings.Contains(err.Error(), "anthropic-clyde") {
		t.Fatalf("want the unknown model named, got %v", err)
	}
}

// Two scopes resolving to one path would make the second silently shadow the
// first, and its members' spend would land on the wrong pool.
func TestDuplicateScopePathIsRefused(t *testing.T) {
	_, err := Parse([]byte(rbacYAML(`
rbac:
  organizations:
    - id: acme
      teams:
        - id: platform
        - id: platform
`)))
	if err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Fatalf("want a duplicate rejection, got %v", err)
	}
}

// A gateway that declares no hierarchy resolves no scopes and pays for none of
// this.
func TestNoHierarchyResolvesNoScopes(t *testing.T) {
	cfg, err := Parse([]byte(rbacYAML("")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.RBAC.Enabled() {
		t.Error("RBAC.Enabled() should be false with no organizations declared")
	}
	if len(cfg.Scopes()) != 0 {
		t.Errorf("want no resolved scopes, got %v", cfg.ScopeIDs())
	}
	if cfg.Scope("") != nil || cfg.Scope("anything") != nil {
		t.Error("Scope() should answer nil rather than panic when nothing is configured")
	}
}
