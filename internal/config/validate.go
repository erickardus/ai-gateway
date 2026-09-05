package config

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/erickardus/ai-gateway/internal/cache"
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
		{"observability.spend_flush_interval", c.Observability.SpendFlushInterval},
		{"redis.timeout", c.Redis.Timeout},
		{"cache.ttl", c.Cache.TTL},
		{"prompt_cache.affinity_ttl", c.PromptCache.AffinityTTL},
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

	if c.Cache.Enabled {
		switch c.Cache.Scope {
		case cache.ScopeKey, cache.ScopeShared:
		default:
			errs = append(errs, fmt.Errorf("cache.scope: must be %q or %q, got %q", cache.ScopeKey, cache.ScopeShared, c.Cache.Scope))
		}
		if c.Cache.MaxEntries < 1 {
			errs = append(errs, fmt.Errorf("cache.max_entries: must be >= 1, got %d", c.Cache.MaxEntries))
		}
		if c.Cache.MaxEntryBytes < 1 {
			errs = append(errs, fmt.Errorf("cache.max_entry_bytes: must be >= 1, got %d", c.Cache.MaxEntryBytes))
		}
		if c.Cache.Shared && !c.Redis.Enabled() {
			errs = append(errs, errors.New("cache.shared: requires redis.addr, since a shared cache lives in Redis"))
		}

		// A passthrough deployment's response was generated under one caller's
		// personal subscription. Serving it to a different caller would hand
		// them output someone else paid for, so the combination is refused
		// rather than left as a footgun.
		if c.Cache.Scope == cache.ScopeShared {
			for i := range c.ModelList {
				if c.ModelList[i].Params.AuthMode == core.AuthModePassthrough {
					errs = append(errs, fmt.Errorf(
						"cache.scope: %q cannot be used while model_list[%d] is a passthrough deployment; its responses are generated under the caller's own subscription and must not be served to other keys",
						cache.ScopeShared, i))
					break
				}
			}
		}
	}

	if l := c.PromptCache.AffinityMaxInFlightLead; l != nil && *l < 0 {
		errs = append(errs, fmt.Errorf("prompt_cache.affinity_max_in_flight_lead: must be >= 0, got %d", *l))
	}
	if c.PromptCache.InjectMinBytes < 0 {
		errs = append(errs, fmt.Errorf("prompt_cache.inject_min_bytes: must be >= 0, got %d", c.PromptCache.InjectMinBytes))
	}
	if c.PromptCache.Inject {
		// Injection edits the request body. The passthrough path exists to
		// forward a body unchanged — Anthropic's gateway rules require it, and
		// the endpoint strips Claude Code's attribution block positionally — so
		// the combination is refused at load rather than left to surprise an
		// operator whose subscription traffic starts being rewritten.
		for i := range c.ModelList {
			if c.ModelList[i].Params.AuthMode == core.AuthModePassthrough {
				errs = append(errs, fmt.Errorf(
					"prompt_cache.inject: cannot be enabled while model_list[%d] is a passthrough deployment; injection rewrites the request body and a passthrough deployment must forward it unchanged",
					i))
				break
			}
		}
		for i := range c.ModelList {
			if c.ModelList[i].Params.Format != core.FormatAnthropic {
				errs = append(errs, fmt.Errorf(
					"prompt_cache.inject: model_list[%d] speaks %q; cache breakpoints are an Anthropic construct and are only placed on anthropic deployments",
					i, c.ModelList[i].Params.Format))
				break
			}
		}
	}

	if c.Redis.DB < 0 {
		errs = append(errs, fmt.Errorf("redis.db: must be >= 0, got %d", c.Redis.DB))
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
			if !d.Cost.Zero() {
				errs = append(errs, fmt.Errorf(
					"%s.cost: must be empty when auth_mode is passthrough; the upstream bills the caller's own subscription rather than the operator, so a price here would invent a charge nobody receives", p))
			}
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

		errs = append(errs, validatePricing(p, d)...)

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
		if k.MaxBudget < 0 {
			errs = append(errs, fmt.Errorf("%s.max_budget: must be >= 0, got %v", p, k.MaxBudget))
		}
		if k.BudgetDuration < 0 {
			errs = append(errs, fmt.Errorf("%s.budget_duration: must not be negative, got %s", p, k.BudgetDuration))
		}
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

// validatePricing refuses a cost model that would misreport what prompt caching
// costs.
//
// The failure this prevents is quiet. A deployment priced with input and output
// alone loads, serves traffic and reports a cost — one that charges every cached
// token as if it were ordinary input, and reports savings of exactly zero on the
// traffic prompt caching exists for. Nothing errors and nothing looks wrong
// until the provider's invoice disagrees with the gateway's own reports, which
// is the point at which the number is too late to be useful.
//
// So a partially specified cost model fails at load, in the same way injection
// alongside passthrough does. A deployment with no cost block at all is
// untouched: "unpriced" is a coherent state, and it is the one passthrough
// requires.
func validatePricing(path string, d *Deployment) []error {
	var errs []error
	cost := d.Cost
	if cost.Zero() {
		return nil
	}

	for _, f := range []struct {
		name  string
		value float64
	}{
		{"input_per_1m", cost.InputPer1M},
		{"output_per_1m", cost.OutputPer1M},
		{"cache_read_per_1m", cost.CacheReadPer1M},
		{"cache_write_per_1m", cost.CacheWritePer1M},
		{"cache_write_1h_per_1m", cost.CacheWrite1hPer1M},
	} {
		if f.value < 0 {
			errs = append(errs, fmt.Errorf("%s.cost.%s: must be >= 0, got %v", path, f.name, f.value))
		}
	}

	if cost.InputPer1M <= 0 {
		// Without an input price there is nothing to price cache reads against,
		// and a cost model that charges output alone is a deliberate enough
		// oddity to leave alone.
		return errs
	}

	if cost.CacheReadPer1M <= 0 {
		errs = append(errs, fmt.Errorf(
			"%s.cost.cache_read_per_1m: required once input_per_1m is set; leaving it zero prices every cached token as free, which overstates this deployment's savings and understates its cost by the whole of its cache traffic",
			path))
	} else if cost.CacheReadPer1M >= cost.InputPer1M {
		errs = append(errs, fmt.Errorf(
			"%s.cost.cache_read_per_1m: must be below input_per_1m (got %v against %v); a cache read costing as much as fresh input means prompt caching saves nothing, which no provider charges and a gateway cannot report",
			path, cost.CacheReadPer1M, cost.InputPer1M))
	}

	// A cache write is an Anthropic construct: it is charged at a premium over
	// input and reported as its own counter. OpenAI-compatible providers cache
	// automatically and charge nothing to write, so requiring a price there
	// would be inventing one.
	if d.Params.Format == core.FormatAnthropic {
		if cost.CacheWritePer1M <= 0 {
			errs = append(errs, fmt.Errorf(
				"%s.cost.cache_write_per_1m: required on an anthropic deployment once input_per_1m is set; a cache write costs a premium over input, and pricing it at zero hides the one cost that makes bad cache routing expensive",
				path))
		} else if cost.CacheWritePer1M <= cost.InputPer1M {
			errs = append(errs, fmt.Errorf(
				"%s.cost.cache_write_per_1m: must exceed input_per_1m (got %v against %v); writing the cache is charged at a premium, and a price at or below input makes a cache miss look free",
				path, cost.CacheWritePer1M, cost.InputPer1M))
		}
		if h := cost.CacheWrite1hPer1M; h > 0 && h < cost.CacheWritePer1M {
			errs = append(errs, fmt.Errorf(
				"%s.cost.cache_write_1h_per_1m: must be at least cache_write_per_1m (got %v against %v); the longer-lived cache is the more expensive one to write",
				path, h, cost.CacheWritePer1M))
		}
	}
	return errs
}
