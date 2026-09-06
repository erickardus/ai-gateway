package sso

import (
	"github.com/erickardus/ai-gateway/internal/config"
)

// claimValues reads a claim as a list of strings.
//
// Providers are not consistent here: group membership arrives as an array from
// Okta and Keycloak, and a single-valued claim such as a department is a plain
// string. Both are read, and anything else yields nothing rather than an error,
// because a claim shaped unexpectedly should fail to match a role — which is
// refused — rather than fail the login in a way that looks like an outage.
func claimValues(raw map[string]any, claim string) []string {
	v, ok := raw[claim]
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// Role returns the first configured role that matches the identity.
//
// First match wins, so ordering in configuration is meaningful and a "*"
// catch-all belongs last. An identity matching nothing is refused: defaulting
// it to the narrowest role would silently admit anyone the provider will
// authenticate, which for a provider covering a whole company is everyone.
func (p *Provider) Role(id *Identity) (config.SSORole, bool) {
	for _, r := range p.cfg.Roles {
		if r.Match == "*" {
			return r, true
		}
		for _, v := range id.Roles {
			if v == r.Match {
				return r, true
			}
		}
	}
	return config.SSORole{}, false
}
