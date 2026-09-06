package core

import (
	"fmt"
	"strings"
	"time"
)

// ScopeKind names one level of the entitlement hierarchy. The three exist
// because they answer different questions: an organisation is who pays, a team
// is who shares an allowance, and a project is what the spend is attributed to.
type ScopeKind string

const (
	// ScopeOrganization is the root of a hierarchy. It has no parent.
	ScopeOrganization ScopeKind = "organization"
	// ScopeTeam sits under an organisation.
	ScopeTeam ScopeKind = "team"
	// ScopeProject sits under a team and is the level a key attaches to.
	ScopeProject ScopeKind = "project"
)

// ScopeSeparator joins the levels of a fully qualified scope id, so
// "acme/platform/gateway" names the gateway project of the platform team of the
// acme organisation. The path is the id: it is unique by construction, and it
// reads as the hierarchy it describes without a lookup.
const ScopeSeparator = "/"

// Scope is one node of the entitlement hierarchy — an organisation, a team or a
// project — and the subject a pooled budget accumulates under.
//
// The distinction from a role is the whole reason this type exists. A role maps
// an identity to entitlements, so every identity matching it gets its own
// independent budget and ten developers on one role can spend ten times the cap
// their operator wrote. A scope is a *subject in the ledger*: every key beneath
// it records its spend against the same entry, so the cap is one pool the
// members draw down together.
//
// Entitlements narrow going down. A scope's limits bind every key beneath it
// regardless of what the key itself declares, so a parent can never be widened
// by a child — which is what makes the hierarchy a control rather than a
// default.
type Scope struct {
	// ID is the fully qualified path, e.g. "acme/platform/gateway".
	ID   string    `json:"id"`
	Kind ScopeKind `json:"kind"`
	// Alias is a human label for reports; the ID is what everything keys on.
	Alias string `json:"alias,omitempty"`
	// Parent is the enclosing scope, nil at an organisation.
	Parent *Scope `json:"-"`

	// Models restricts what may be called beneath this scope. Empty means "no
	// restriction here", which is not the same as "anything": a parent's list
	// still applies, because the chain is an intersection.
	Models []string `json:"models,omitempty"`
	// RPMLimit and TPMLimit are allowances shared by every key beneath this
	// scope, not granted to each of them. Zero is unlimited.
	RPMLimit int `json:"rpm_limit,omitempty"`
	TPMLimit int `json:"tpm_limit,omitempty"`
	// MaxBudget caps billable spend across every key beneath this scope within
	// BudgetDuration. Zero is unlimited.
	MaxBudget float64 `json:"max_budget,omitempty"`
	// BudgetDuration is the window MaxBudget applies over; zero means forever.
	BudgetDuration time.Duration `json:"budget_duration,omitempty"`
	// Blocked disables every key beneath this scope at once, which is what
	// suspending a team means when its members hold keys the operator has
	// never seen.
	Blocked bool `json:"blocked,omitempty"`
}

// SpendSubject is the ledger entry this scope's pooled spend accumulates under.
//
// The kind is part of it so that a team and a project sharing a path prefix
// cannot draw on one another's window, and so that a report reads as a
// hierarchy rather than as a list of paths.
func (s *Scope) SpendSubject() string { return string(s.Kind) + ":" + s.ID }

// Chain returns this scope and its ancestors, innermost first.
//
// Order is what makes it usable for enforcement: the caller walks outward, so
// the narrowest limit is consulted first and an error names the scope closest
// to the key that refused it.
func (s *Scope) Chain() []*Scope {
	if s == nil {
		return nil
	}
	// Three levels is the whole hierarchy, so the chain never grows: the
	// capacity is exact rather than a guess.
	out := make([]*Scope, 0, 3)
	for n := s; n != nil; n = n.Parent {
		out = append(out, n)
	}
	return out
}

// Usable reports whether anything beneath this scope may be used at all. A
// block anywhere in the chain blocks everything below it.
func (s *Scope) Usable() error {
	for _, n := range s.Chain() {
		if n.Blocked {
			return fmt.Errorf("%s %q is blocked: %w", n.Kind, n.ID, ErrKeyBlocked)
		}
	}
	return nil
}

// AllowsModel reports whether the chain permits the named model group.
//
// Every scope that names a list must permit it: the chain is an intersection,
// so a team cannot grant its members a model its organisation withheld. A scope
// naming no list abstains rather than granting everything, which is why an
// empty Models is skipped instead of returning true.
func (s *Scope) AllowsModel(model string) bool {
	for _, n := range s.Chain() {
		if len(n.Models) == 0 {
			continue
		}
		if !matchAny(n.Models, model) {
			return false
		}
	}
	return true
}

// Denier returns the innermost scope in the chain that withholds the model, for
// an error message that names the level an operator would edit. It returns nil
// where the chain permits it.
func (s *Scope) Denier(model string) *Scope {
	for _, n := range s.Chain() {
		if len(n.Models) > 0 && !matchAny(n.Models, model) {
			return n
		}
	}
	return nil
}

// Root returns the outermost scope in the chain.
func (s *Scope) Root() *Scope {
	if s == nil {
		return nil
	}
	n := s
	for n.Parent != nil {
		n = n.Parent
	}
	return n
}

// matchAny reports whether any pattern in the list matches, using the same
// trailing-"*" rule as a key's own allowlist.
func matchAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if matchPattern(p, s) {
			return true
		}
	}
	return false
}

// ScopeKindAt returns the kind a scope at the given depth has, and whether the
// depth is one the hierarchy defines. Depth is the number of separators in the
// path: 0 is an organisation, 2 a project.
func ScopeKindAt(depth int) (ScopeKind, bool) {
	switch depth {
	case 0:
		return ScopeOrganization, true
	case 1:
		return ScopeTeam, true
	case 2:
		return ScopeProject, true
	default:
		return "", false
	}
}

// ScopeDepth returns how many levels deep a fully qualified id is.
func ScopeDepth(id string) int { return strings.Count(id, ScopeSeparator) }

// JoinScope builds a child's fully qualified id from its parent's.
func JoinScope(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + ScopeSeparator + child
}
