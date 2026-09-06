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
	if c.PromptCache.Inject && len(c.ModelList) > 0 {
		// A breakpoint is only ever placed on an Anthropic deployment that is
		// not a passthrough one: breakpoints are an Anthropic construct, and a
		// passthrough deployment must forward the caller's body unchanged, since
		// Anthropic's gateway rules require it and the endpoint strips Claude
		// Code's attribution block positionally.
		//
		// Both are decided per deployment, at dispatch, so a mixed fleet is
		// served correctly: the deployments that can take a breakpoint get one
		// and the rest are sent the request as it arrived. What is refused here
		// is the fleet where that set is empty, which is a setting that says one
		// thing and does nothing — the same reason a partial cost model is
		// refused rather than quietly completed.
		injectable := 0
		for i := range c.ModelList {
			d := &c.ModelList[i]
			if d.Params.Format == core.FormatAnthropic && d.Params.AuthMode != core.AuthModePassthrough {
				injectable++
			}
		}
		if injectable == 0 {
			errs = append(errs, errors.New(
				"prompt_cache.inject: no deployment can carry a cache breakpoint, so this does nothing; breakpoints are placed only on an anthropic deployment that is not a passthrough one, since a passthrough deployment must forward the caller's body unchanged"))
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
	cost := d.Cost
	if cost.Zero() {
		return nil
	}

	base := rateSet{
		input:        cost.InputPer1M,
		output:       cost.OutputPer1M,
		cacheRead:    cost.CacheReadPer1M,
		cacheWrite:   cost.CacheWritePer1M,
		cacheWrite1h: cost.CacheWrite1hPer1M,
	}
	errs := validateRates(path+".cost", base, d.Params.Format, false)
	return append(errs, validateLongContext(path, d, base)...)
}

// rateSet is one tier of a deployment's price list. The base rates and the
// long-context rates are the same five figures under different keys, and the
// relationships between them — a read below input, a write above it — hold
// within a tier rather than across tiers, so they are checked per tier.
type rateSet struct {
	input, output, cacheRead, cacheWrite, cacheWrite1h float64
}

// validateRates checks one tier's internal consistency. block is the key path
// the operator would edit, so an error names the line rather than the concept.
//
// required says whether an input price must be present. It is optional on the
// base tier — a cost model charging output alone is a deliberate enough oddity
// to leave alone — and mandatory on a long-context tier, which exists only to
// override the base and cannot do so without one.
func validateRates(block string, r rateSet, format core.Format, required bool) []error {
	var errs []error
	for _, f := range []struct {
		name  string
		value float64
	}{
		{"input_per_1m", r.input},
		{"output_per_1m", r.output},
		{"cache_read_per_1m", r.cacheRead},
		{"cache_write_per_1m", r.cacheWrite},
		{"cache_write_1h_per_1m", r.cacheWrite1h},
	} {
		if f.value < 0 {
			errs = append(errs, fmt.Errorf("%s.%s: must be >= 0, got %v", block, f.name, f.value))
		}
	}

	if r.input <= 0 {
		if required {
			errs = append(errs, fmt.Errorf(
				"%s.input_per_1m: required; a tier with no input price of its own cannot price the tokens it exists to charge differently",
				block))
		}
		// Without an input price there is nothing to price cache reads against.
		return errs
	}

	if r.cacheRead <= 0 {
		errs = append(errs, fmt.Errorf(
			"%s.cache_read_per_1m: required once input_per_1m is set; leaving it zero prices every cached token as free, which overstates this deployment's savings and understates its cost by the whole of its cache traffic",
			block))
	} else if r.cacheRead >= r.input {
		errs = append(errs, fmt.Errorf(
			"%s.cache_read_per_1m: must be below input_per_1m (got %v against %v); a cache read costing as much as fresh input means prompt caching saves nothing, which no provider charges and a gateway cannot report",
			block, r.cacheRead, r.input))
	}

	// A cache write is an Anthropic construct: it is charged at a premium over
	// input and reported as its own counter. OpenAI-compatible providers cache
	// automatically and charge nothing to write, so requiring a price there
	// would be inventing one.
	if format == core.FormatAnthropic {
		if r.cacheWrite <= 0 {
			errs = append(errs, fmt.Errorf(
				"%s.cache_write_per_1m: required on an anthropic deployment once input_per_1m is set; a cache write costs a premium over input, and pricing it at zero hides the one cost that makes bad cache routing expensive",
				block))
		} else if r.cacheWrite <= r.input {
			errs = append(errs, fmt.Errorf(
				"%s.cache_write_per_1m: must exceed input_per_1m (got %v against %v); writing the cache is charged at a premium, and a price at or below input makes a cache miss look free",
				block, r.cacheWrite, r.input))
		}
		if h := r.cacheWrite1h; h > 0 && h < r.cacheWrite {
			errs = append(errs, fmt.Errorf(
				"%s.cache_write_1h_per_1m: must be at least cache_write_per_1m (got %v against %v); the longer-lived cache is the more expensive one to write",
				block, h, r.cacheWrite))
		}
	}
	return errs
}

// validateLongContext checks the higher tier a provider charges above a prompt
// size.
//
// The tier is refused rather than defaulted when it is incomplete, for the
// reason the base tier is: a rate it leaves unset falls back to the base rate,
// so a tier naming only its input price would charge the premium on ordinary
// input and the small-request price on everything else. That is a bill nobody
// can reconstruct, on the largest requests a deployment serves.
func validateLongContext(path string, d *Deployment, base rateSet) []error {
	long := d.Cost.LongContext
	if long == nil {
		return nil
	}
	block := path + ".cost.long_context"

	if base.input <= 0 {
		return []error{fmt.Errorf(
			"%s: requires %s.cost.input_per_1m; a higher tier is an override of the base rates and there are none to override",
			block, path)}
	}

	var errs []error
	if long.AbovePromptTokens <= 0 {
		errs = append(errs, fmt.Errorf(
			"%s.above_prompt_tokens: must be > 0, got %d; without a threshold there is nothing to decide which tier a request falls into",
			block, long.AbovePromptTokens))
	}

	tier := rateSet{
		input:        long.InputPer1M,
		output:       long.OutputPer1M,
		cacheRead:    long.CacheReadPer1M,
		cacheWrite:   long.CacheWritePer1M,
		cacheWrite1h: long.CacheWrite1hPer1M,
	}
	errs = append(errs, validateRates(block, tier, d.Params.Format, true)...)

	// An output price is required only where the base tier has one to be
	// mispriced against: a deployment charging nothing for output at either
	// size is coherent, one that charges for it at the small size and silently
	// falls back to that rate at the large size is not.
	if base.output > 0 && tier.output <= 0 {
		errs = append(errs, fmt.Errorf(
			"%s.output_per_1m: required once %s.cost.output_per_1m is set; a tier that leaves it unset bills long-context output at the small-request rate",
			block, path))
	}

	// Every rate this tier names must be at least its base counterpart. A tier
	// is what a provider charges *extra* above a threshold, so a cheaper rate
	// here is a transposed pair of blocks rather than a deliberate discount —
	// and it would report the largest requests as the cheapest ones.
	for _, f := range []struct {
		name       string
		tier, base float64
	}{
		{"input_per_1m", tier.input, base.input},
		{"output_per_1m", tier.output, base.output},
		{"cache_read_per_1m", tier.cacheRead, base.cacheRead},
		{"cache_write_per_1m", tier.cacheWrite, base.cacheWrite},
		{"cache_write_1h_per_1m", tier.cacheWrite1h, base.cacheWrite1h},
	} {
		if f.tier > 0 && f.base > 0 && f.tier < f.base {
			errs = append(errs, fmt.Errorf(
				"%s.%s: must be at least %s.cost.%s (got %v against %v); the long-context tier is the more expensive one, so a lower rate here is the two blocks written the wrong way round",
				block, f.name, path, f.name, f.tier, f.base))
		}
	}
	return errs
}
