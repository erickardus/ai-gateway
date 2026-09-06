package config

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/erickardus/ai-gateway/internal/core"
)

// RBACConfig declares the entitlement hierarchy: organisations, the teams
// inside them, and the projects inside those.
//
// It is nested rather than flat with parent references because the shape is the
// documentation. An operator reading it sees who is inside what without
// resolving ids, and a team cannot be declared under an organisation that does
// not exist. The cost is a flattening pass at load, which is where the
// fully qualified ids and the parent pointers come from.
type RBACConfig struct {
	Organizations []OrgConfig `yaml:"organizations"`
}

// Enabled reports whether any hierarchy is declared. With none, keys are flat
// and every scope check is skipped.
func (r RBACConfig) Enabled() bool { return len(r.Organizations) > 0 }

// ScopeLimits are the entitlements every level shares. They are declared
// identically at each level so that moving a budget from a team to its
// organisation is an edit rather than a rewrite.
type ScopeLimits struct {
	// Alias is a human label for reports.
	Alias string `yaml:"alias"`
	// Models restricts what may be called beneath this scope. Empty abstains
	// rather than granting everything: a parent's list still binds, because the
	// chain is an intersection.
	Models []string `yaml:"models"`
	// RPMLimit and TPMLimit are allowances shared by every key beneath this
	// scope, not handed to each of them.
	RPMLimit int `yaml:"rpm_limit"`
	TPMLimit int `yaml:"tpm_limit"`
	// MaxBudget caps billable spend across every key beneath this scope within
	// BudgetDuration. This is the pool.
	MaxBudget float64 `yaml:"max_budget"`
	// BudgetDuration is the window MaxBudget applies over; zero means forever.
	BudgetDuration time.Duration `yaml:"budget_duration"`
	// Blocked disables every key beneath this scope at once.
	Blocked bool `yaml:"blocked"`
}

// OrgConfig is one organisation.
type OrgConfig struct {
	// ID is one path segment, unique among organisations. The fully qualified
	// id of an organisation is just its ID.
	ID          string `yaml:"id"`
	ScopeLimits `yaml:",inline"`
	Teams       []TeamConfig `yaml:"teams"`
}

// TeamConfig is one team inside an organisation. A team is the level a shared
// budget usually belongs to: it is the smallest grouping that outlives the
// people in it.
type TeamConfig struct {
	ID          string `yaml:"id"`
	ScopeLimits `yaml:",inline"`
	Projects    []ProjectConfig `yaml:"projects"`
}

// ProjectConfig is one project inside a team.
type ProjectConfig struct {
	ID          string `yaml:"id"`
	ScopeLimits `yaml:",inline"`
}

// Scopes returns the flattened hierarchy, keyed by fully qualified id. It is
// resolved once at load; the map is shared and must not be mutated.
func (c *Config) Scopes() map[string]*core.Scope { return c.scopes }

// Scope returns one scope by fully qualified id, or nil.
//
// An empty id returns nil rather than an error: a key belonging to no hierarchy
// is the ordinary case, and callers treat a nil scope as "no scope checks".
func (c *Config) Scope(id string) *core.Scope {
	if id == "" || c.scopes == nil {
		return nil
	}
	return c.scopes[id]
}

// ScopeIDs returns every declared scope id in sorted order, for error messages
// that can name what was available.
func (c *Config) ScopeIDs() []string {
	out := make([]string, 0, len(c.scopes))
	for id := range c.scopes {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// resolveScopes flattens the declared hierarchy into addressable scopes with
// parent pointers.
//
// It runs before validation and reports only the errors that would make the map
// itself wrong — a malformed or duplicated id. Everything else that can be
// wrong about a hierarchy is a question about its contents rather than its
// shape, and belongs in Validate where it can be reported alongside the rest of
// the configuration rather than aborting at the first fault.
func (c *Config) resolveScopes() error {
	c.scopes = make(map[string]*core.Scope)
	if !c.RBAC.Enabled() {
		return nil
	}

	add := func(path string, kind core.ScopeKind, lim ScopeLimits, parent *core.Scope, where string) (*core.Scope, error) {
		if _, dup := c.scopes[path]; dup {
			return nil, fmt.Errorf("%s: scope %q is declared twice", where, path)
		}
		s := &core.Scope{
			ID: path, Kind: kind, Alias: lim.Alias, Parent: parent,
			Models: lim.Models, RPMLimit: lim.RPMLimit, TPMLimit: lim.TPMLimit,
			MaxBudget: lim.MaxBudget, BudgetDuration: lim.BudgetDuration,
			Blocked: lim.Blocked,
		}
		c.scopes[path] = s
		return s, nil
	}

	for i, org := range c.RBAC.Organizations {
		where := fmt.Sprintf("rbac.organizations[%d]", i)
		if err := validSegment(org.ID, where); err != nil {
			return err
		}
		orgScope, err := add(org.ID, core.ScopeOrganization, org.ScopeLimits, nil, where)
		if err != nil {
			return err
		}
		for j, team := range org.Teams {
			twhere := fmt.Sprintf("%s.teams[%d]", where, j)
			if err := validSegment(team.ID, twhere); err != nil {
				return err
			}
			teamScope, err := add(core.JoinScope(org.ID, team.ID), core.ScopeTeam, team.ScopeLimits, orgScope, twhere)
			if err != nil {
				return err
			}
			for k, proj := range team.Projects {
				pwhere := fmt.Sprintf("%s.projects[%d]", twhere, k)
				if err := validSegment(proj.ID, pwhere); err != nil {
					return err
				}
				if _, err := add(core.JoinScope(teamScope.ID, proj.ID), core.ScopeProject, proj.ScopeLimits, teamScope, pwhere); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// validSegment checks one level's id. A segment carrying the separator would
// produce a fully qualified id that names a different scope than the nesting
// says, so it is refused rather than escaped.
func validSegment(id, where string) error {
	switch {
	case id == "":
		return fmt.Errorf("%s.id: required", where)
	case strings.Contains(id, core.ScopeSeparator):
		return fmt.Errorf("%s.id: %q must not contain %q; the hierarchy is expressed by nesting, and the qualified id is built from it",
			where, id, core.ScopeSeparator)
	case strings.TrimSpace(id) != id:
		return fmt.Errorf("%s.id: %q has leading or trailing whitespace", where, id)
	}
	return nil
}
