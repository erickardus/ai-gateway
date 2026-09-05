package config

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/erickardus/ai-gateway/internal/core"
)

// Validate checks the whole config and reports every problem at once, each
// naming the field path at fault. Reporting only the first error would make
// fixing a config a round trip per mistake.
func (c *Config) Validate() error {
	var errs []error

	if len(c.ModelList) == 0 {
		errs = append(errs, errors.New("model_list: at least one deployment is required"))
	}
	if !slices.Contains(KnownStrategies, c.Router.Strategy) {
		errs = append(errs, fmt.Errorf("router.strategy: unknown strategy %q (valid: %s)",
			c.Router.Strategy, strings.Join(KnownStrategies, ", ")))
	}
	if c.Router.NumRetries != nil && *c.Router.NumRetries < 0 {
		errs = append(errs, fmt.Errorf("router.num_retries: must be >= 0, got %d", *c.Router.NumRetries))
	}
	if c.Router.Cooldown.AllowedFails != nil && *c.Router.Cooldown.AllowedFails < 0 {
		errs = append(errs, fmt.Errorf("router.cooldown.allowed_fails: must be >= 0, got %d", *c.Router.Cooldown.AllowedFails))
	}
	if j := c.Router.Backoff.Jitter; j != nil && (*j < 0 || *j > 10) {
		errs = append(errs, fmt.Errorf("router.backoff.jitter: must be within [0,10], got %v", *j))
	}
	if c.Router.MaxFallbackHops < 1 {
		errs = append(errs, fmt.Errorf("router.max_fallback_hops: must be >= 1, got %d", c.Router.MaxFallbackHops))
	}
	if b := c.Router.LowestLatencyBuffer; b < 0 || b > 1 {
		errs = append(errs, fmt.Errorf("router.lowest_latency_buffer: must be within [0,1], got %v", b))
	}
	for _, d := range []struct {
		path  string
		value time.Duration
	}{
		{"server.read_header_timeout", c.Server.ReadHeaderTimeout},
		{"server.idle_timeout", c.Server.IdleTimeout},
		{"server.shutdown_grace", c.Server.ShutdownGrace},
		{"router.timeout", c.Router.Timeout},
		{"router.stream_timeout", c.Router.StreamTimeout},
		{"router.cooldown.period", c.Router.Cooldown.Period},
		{"router.backoff.initial", c.Router.Backoff.Initial},
		{"router.backoff.max", c.Router.Backoff.Max},
	} {
		if d.value < 0 {
			errs = append(errs, fmt.Errorf("%s: must not be negative, got %s", d.path, d.value))
		}
	}

	// A group must speak one wire format: the gateway does not translate, so a
	// mixed group would route some requests to an upstream expecting a
	// different schema.
	groupFormat := make(map[string]core.Format)
	for i := range c.ModelList {
		d := &c.ModelList[i]
		if seen, ok := groupFormat[d.ModelName]; ok && seen != d.Params.Format {
			errs = append(errs, fmt.Errorf(
				"model_list[%d].params.format: model group %q mixes %q and %q; a group must speak one format because the gateway does not translate between them",
				i, d.ModelName, seen, d.Params.Format))
		} else if !ok {
			groupFormat[d.ModelName] = d.Params.Format
		}
	}

	if c.Server.MaxBodyBytes <= 0 {
		errs = append(errs, fmt.Errorf("server.max_body_bytes: must be > 0, got %d", c.Server.MaxBodyBytes))
	}

	allowedHosts := make(map[string]bool, len(c.VirtualKeys.AllowedUpstreamHosts))
	for _, h := range c.VirtualKeys.AllowedUpstreamHosts {
		allowedHosts[NormalizeHost(h)] = true
	}

	seenIDs := make(map[string]int)
	groups := make(map[string]bool)

	for i := range c.ModelList {
		d := &c.ModelList[i]
		p := fmt.Sprintf("model_list[%d]", i)

		if d.ModelName == "" {
			errs = append(errs, fmt.Errorf("%s.model_name: required", p))
		}
		groups[d.ModelName] = true

		switch d.Params.Format {
		case core.FormatAnthropic, core.FormatOpenAI:
		default:
			errs = append(errs, fmt.Errorf("%s.params.format: must be %q or %q, got %q",
				p, core.FormatAnthropic, core.FormatOpenAI, d.Params.Format))
		}

		if d.Params.APIBase == "" {
			errs = append(errs, fmt.Errorf("%s.params.api_base: required", p))
		} else if u, err := url.Parse(d.Params.APIBase); err != nil {
			errs = append(errs, fmt.Errorf("%s.params.api_base: not a valid URL: %v", p, err))
		} else if u.Scheme != "http" && u.Scheme != "https" {
			errs = append(errs, fmt.Errorf("%s.params.api_base: scheme must be http or https, got %q", p, u.Scheme))
		} else if u.User != nil {
			// Credentials in the URL would be sent upstream outside the header
			// policy and could smuggle past the host allowlist.
			errs = append(errs, fmt.Errorf("%s.params.api_base: must not embed userinfo", p))
		}

		switch d.Params.AuthMode {
		case core.AuthModeAPIKey:
			if d.Params.APIKey == "" {
				errs = append(errs, fmt.Errorf("%s.params.api_key: required when auth_mode is api_key (unset or empty ${ENV} reference?)", p))
			}
			if d.Params.AuthHeader == "" {
				errs = append(errs, fmt.Errorf("%s.params.auth_header: required when auth_mode is api_key", p))
			}
		case core.AuthModePassthrough:
			if d.Params.APIKey != "" {
				errs = append(errs, fmt.Errorf("%s.params.api_key: must be empty when auth_mode is passthrough (the caller's own credential is relayed)", p))
			}
			// A passthrough deployment relays a caller credential upstream, so
			// its host must be explicitly allowlisted.
			if host := HostOf(d.Params.APIBase); host == "" {
				errs = append(errs, fmt.Errorf("%s.params.api_base: cannot determine host for allowlist check", p))
			} else if !allowedHosts[host] {
				errs = append(errs, fmt.Errorf(
					"%s.params.api_base: host %q is not in virtual_keys.allowed_upstream_hosts; a passthrough deployment relays the caller's credential, so its host must be explicitly allowed",
					p, host))
			}
		default:
			errs = append(errs, fmt.Errorf("%s.params.auth_mode: must be %q or %q, got %q",
				p, core.AuthModeAPIKey, core.AuthModePassthrough, d.Params.AuthMode))
		}

		if d.Weight != nil && *d.Weight < 0 {
			errs = append(errs, fmt.Errorf("%s.weight: must be >= 0, got %d", p, *d.Weight))
		}
		if d.RPM < 0 {
			errs = append(errs, fmt.Errorf("%s.rpm: must be >= 0, got %d", p, d.RPM))
		}
		if d.TPM < 0 {
			errs = append(errs, fmt.Errorf("%s.tpm: must be >= 0, got %d", p, d.TPM))
		}
		if prev, dup := seenIDs[d.id]; dup {
			errs = append(errs, fmt.Errorf("%s: duplicate deployment (same model_name, api_base and model as model_list[%d])", p, prev))
		}
		seenIDs[d.id] = i
	}

	for _, set := range []struct {
		name  string
		rules []FallbackRule
	}{
		{"router.fallbacks", c.Router.Fallbacks},
		{"router.context_window_fallbacks", c.Router.ContextWindowFallbacks},
		{"router.content_policy_fallbacks", c.Router.ContentPolicyFallbacks},
	} {
		for i, r := range set.rules {
			if !groups[r.From] {
				errs = append(errs, fmt.Errorf("%s[%d].from: no model group named %q", set.name, i, r.From))
			}
			if len(r.To) == 0 {
				errs = append(errs, fmt.Errorf("%s[%d].to: must name at least one model group", set.name, i))
			}
			for j, to := range r.To {
				if !groups[to] {
					errs = append(errs, fmt.Errorf("%s[%d].to[%d]: no model group named %q", set.name, i, j, to))
				}
				if to == r.From {
					errs = append(errs, fmt.Errorf("%s[%d].to[%d]: model group %q cannot fall back to itself", set.name, i, j, to))
				}
				if from, okFrom := groupFormat[r.From]; okFrom {
					if dst, okTo := groupFormat[to]; okTo && dst != from {
						errs = append(errs, fmt.Errorf(
							"%s[%d].to[%d]: %q speaks %q but %q speaks %q; a fallback must not cross wire formats",
							set.name, i, j, r.From, from, to, dst))
					}
				}
			}
		}
	}

	for i, k := range c.VirtualKeys.Keys {
		p := fmt.Sprintf("virtual_keys.keys[%d]", i)
		if k.Key == "" {
			errs = append(errs, fmt.Errorf("%s.key: required (unset or empty ${ENV} reference?)", p))
		}
		if k.RPMLimit < 0 {
			errs = append(errs, fmt.Errorf("%s.rpm_limit: must be >= 0, got %d", p, k.RPMLimit))
		}
		if k.TPMLimit < 0 {
			errs = append(errs, fmt.Errorf("%s.tpm_limit: must be >= 0, got %d", p, k.TPMLimit))
		}
	}

	if c.VirtualKeys.Store.Kind != "memory" && c.VirtualKeys.Store.Kind != "file" {
		errs = append(errs, fmt.Errorf("virtual_keys.store.kind: must be \"memory\" or \"file\", got %q", c.VirtualKeys.Store.Kind))
	}
	if c.VirtualKeys.Store.Kind == "file" && c.VirtualKeys.Store.Path == "" {
		errs = append(errs, errors.New("virtual_keys.store.path: required when store.kind is \"file\""))
	}

	return errors.Join(errs...)
}

// NormalizeHost lowercases a host and strips a trailing dot, so that
// "API.Anthropic.com." and "api.anthropic.com" compare equal in the allowlist.
func NormalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

// HostOf extracts the normalized hostname from a base URL, without its port.
func HostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return NormalizeHost(u.Hostname())
}
