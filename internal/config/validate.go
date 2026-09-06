package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
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
		{"observability.otlp.interval", c.Observability.OTLP.Interval},
		{"observability.otlp.timeout", c.Observability.OTLP.Timeout},
		{"ui.session_ttl", c.UI.SessionTTL},
	} {
		if d.value < 0 {
			errs = append(errs, fmt.Errorf("%s: must not be negative, got %s", d.path, d.value))
		}
	}
	errs = append(errs, validateUI(c)...)
	errs = append(errs, validateAudit(c)...)
	errs = append(errs, validateOTLP(c.Observability.OTLP)...)
	errs = append(errs, validateSSO(c)...)
	errs = append(errs, validateRBAC(c)...)

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
			if d.Params.AuthMode == core.AuthModePassthrough {
				continue
			}
			// An anthropic deployment always reads the marker; an openai one
			// only where the operator has said its upstream does.
			if d.Params.Format == core.FormatAnthropic || d.Params.SupportsCacheControl {
				injectable++
			}
		}
		if injectable == 0 {
			errs = append(errs, errors.New(
				"prompt_cache.inject: no deployment can carry a cache breakpoint, so this does nothing; a breakpoint is placed only on an anthropic deployment, or on an openai one whose params.supports_cache_control says its upstream reads the marker, and never on a passthrough deployment, which must forward the caller's body unchanged"))
		}
	}

	// A capability that can never apply is a setting that says one thing and
	// does nothing, which is refused here for the reason prompt_cache.inject on
	// an unmarkable fleet is.
	for i := range c.ModelList {
		d := &c.ModelList[i]
		if !d.Params.SupportsCacheControl {
			continue
		}
		switch {
		case d.Params.Format != core.FormatOpenAI:
			errs = append(errs, fmt.Errorf(
				"model_list[%d].params.supports_cache_control: only meaningful on an openai deployment; an %s one always reads the marker",
				i, d.Params.Format))
		case d.Params.AuthMode == core.AuthModePassthrough:
			errs = append(errs, fmt.Errorf(
				"model_list[%d].params.supports_cache_control: a passthrough deployment forwards the caller's body unchanged, so it is never annotated",
				i))
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

	store := c.VirtualKeys.Store
	switch store.Kind {
	case "memory", "file", "postgres":
	default:
		errs = append(errs, fmt.Errorf("virtual_keys.store.kind: must be \"memory\", \"file\" or \"postgres\", got %q", store.Kind))
	}
	if store.Kind == "file" && store.Path == "" {
		errs = append(errs, errors.New("virtual_keys.store.path: required when store.kind is \"file\""))
	}
	if store.Kind == "postgres" && store.DSN == "" {
		errs = append(errs, errors.New("virtual_keys.store.dsn: required when store.kind is \"postgres\" (unset or empty ${ENV} reference?)"))
	}
	// A dsn on any other kind is refused rather than ignored. The two ways to
	// arrive here are an operator who set the connection string and forgot the
	// kind, and one who switched back to a file store and left the dsn behind;
	// in both cases silence would leave keys somewhere other than where the
	// file says, and the second case leaves a live database credential in a
	// file that now has no use for it.
	if store.Kind != "postgres" && store.DSN != "" {
		errs = append(errs, fmt.Errorf("virtual_keys.store.dsn: set while store.kind is %q, which never connects to a database; use kind: postgres or remove the dsn", store.Kind))
	}
	// And the mirror of it. Switching a file store to postgres means editing
	// the line below the path, so leaving the path behind is the likely
	// mistake rather than an unlikely one — and it leaves a config naming a
	// file of keys that the gateway does not read, which is the same
	// misdirection in the other direction.
	if store.Kind == "postgres" && store.Path != "" {
		errs = append(errs, fmt.Errorf("virtual_keys.store.path: %q is set while store.kind is \"postgres\", which keeps no file; remove the path or use kind: file", store.Path))
	}
	if store.Timeout < 0 {
		errs = append(errs, fmt.Errorf("virtual_keys.store.timeout: must not be negative, got %s", store.Timeout))
	}
	if store.MaxConns < 0 {
		errs = append(errs, fmt.Errorf("virtual_keys.store.max_conns: must be >= 0, got %d", store.MaxConns))
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
		writesFree:   cost.CacheWritesFree,
	}
	var errs []error
	// The flag says what an absent write price means, so there has to be a
	// price list for it to say it about. On its own it prices nothing and would
	// be silently ignored.
	if cost.CacheWritesFree && cost.InputPer1M <= 0 {
		errs = append(errs, fmt.Errorf(
			"%s.cost.cache_writes_free: requires %s.cost.input_per_1m; on its own it prices nothing, since it only says what an absent write price means",
			path, path))
	}
	errs = append(errs, validateRates(path+".cost", base, d.Params.Format, false)...)
	return append(errs, validateLongContext(path, d, base)...)
}

// rateSet is one tier of a deployment's price list. The base rates and the
// long-context rates are the same five figures under different keys, and the
// relationships between them — a read below input, a write above it — hold
// within a tier rather than across tiers, so they are checked per tier.
type rateSet struct {
	input, output, cacheRead, cacheWrite, cacheWrite1h float64
	// writesFree carries cost.cache_writes_free into the per-tier checks. It is
	// declared once on the base block and applies to every tier: a tier is an
	// override of the base rates, and one that reintroduced a write charge
	// would have to name a price, which is refused as a contradiction.
	writesFree bool
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
	// A declared-free write and a priced one are contradictory statements about
	// the same tier, and there is no defensible way to pick between them.
	if r.writesFree && r.cacheWrite > 0 {
		errs = append(errs, fmt.Errorf(
			"%s.cache_write_per_1m: must not be set alongside cache_writes_free (got %v); the two say different things about what a write costs",
			block, r.cacheWrite))
	}

	if format == core.FormatAnthropic {
		switch {
		case r.writesFree:
			// An Anthropic-compatible upstream that is not Anthropic. The rule
			// below asserts a fact about Anthropic's price list, not about the
			// wire format, so it does not apply here.
		case r.cacheWrite <= 0:
			errs = append(errs, fmt.Errorf(
				"%s.cache_write_per_1m: required on an anthropic deployment once input_per_1m is set; a cache write costs a premium over input, and pricing it at zero hides the one cost that makes bad cache routing expensive. Set cache_writes_free instead if this upstream is Anthropic-compatible but charges nothing to write",
				block))
		case r.cacheWrite <= r.input:
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
		// Declared once on the base block and inherited: a provider that
		// charges nothing to write does not start charging above a prompt-size
		// threshold, and a tier naming a write price alongside it is refused as
		// the contradiction it is.
		writesFree: base.writesFree,
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

// validateOTLP refuses an exporter configuration that would fail silently.
//
// A metrics exporter is the one component whose own failures nothing else
// reports: a wrong endpoint or an unsupported protocol produces no bad
// responses and no wrong numbers, just an absence that looks exactly like
// having configured nothing at all. So the checkable parts are checked at load,
// where the operator is still looking.
func validateOTLP(o OTLPConfig) []error {
	if !o.Enabled() {
		// Everything else in the block is inert without an endpoint, and a
		// block written in full but missing the one field that turns it on is
		// far likelier to be an oversight than a deliberate staging area.
		if o.Protocol != "" || len(o.Headers) > 0 || o.Interval > 0 || len(o.ResourceAttributes) > 0 {
			return []error{errors.New(
				"observability.otlp: configured without an endpoint, so nothing is exported; set observability.otlp.endpoint or OTEL_EXPORTER_OTLP_ENDPOINT")}
		}
		return nil
	}

	var errs []error
	u, err := url.Parse(o.Endpoint)
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("observability.otlp.endpoint: not a valid URL: %v", err))
	case u.Scheme != "http" && u.Scheme != "https":
		errs = append(errs, fmt.Errorf(
			"observability.otlp.endpoint: scheme must be http or https, got %q; this is OTLP over HTTP, and a bare host or an otlp:// URL names no transport", u.Scheme))
	case u.Host == "":
		errs = append(errs, fmt.Errorf("observability.otlp.endpoint: no host in %q", o.Endpoint))
	}

	switch o.Protocol {
	case "", "http/protobuf", "http/json":
	case "grpc":
		errs = append(errs, errors.New(
			"observability.otlp.protocol: grpc is not implemented; this exporter speaks OTLP over HTTP, and every collector accepts http/protobuf on port 4318"))
	default:
		errs = append(errs, fmt.Errorf(
			"observability.otlp.protocol: must be \"http/protobuf\" or \"http/json\", got %q", o.Protocol))
	}

	// A timeout at or above the interval leaves a slow collector with exports
	// overlapping, each holding the snapshot it started with.
	if o.Timeout > 0 && o.Interval > 0 && o.Timeout >= o.Interval {
		errs = append(errs, fmt.Errorf(
			"observability.otlp.timeout: must be below observability.otlp.interval (got %s against %s), or a slow collector leaves exports overlapping",
			o.Timeout, o.Interval))
	}
	return errs
}

// validateSSO checks the SSO block. Everything here is refused at load rather
// than at first login, because a login is the one moment a developer is
// watching and the worst time to discover the gateway was misconfigured.
func validateSSO(c *Config) []error {
	o := c.SSO
	if !o.Enabled() {
		// A block written out but missing the issuer turns nothing on. That is
		// far likelier to be an oversight than a deliberate staging area, and
		// the endpoints would be absent with no explanation.
		if o.ClientID != "" || o.ClientSecret != "" || o.RedirectURL != "" || len(o.Roles) > 0 ||
			o.JWTAuth.Enabled {
			return []error{errors.New(
				"sso: configured without an issuer, so no SSO endpoints are served; set sso.issuer")}
		}
		return nil
	}

	var errs []error

	u, err := url.Parse(o.Issuer)
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("sso.issuer: not a valid URL: %v", err))
	case u.Host == "":
		errs = append(errs, fmt.Errorf("sso.issuer: no host in %q", o.Issuer))
	case u.Scheme == "http" && !isLoopbackHost(u.Hostname()):
		// An ID token is only as trustworthy as the transport that delivered
		// the keys it was verified against. Loopback is exempt so a fake
		// provider can be run in a test or on a laptop.
		errs = append(errs, fmt.Errorf(
			"sso.issuer: must be https, got %q; the discovery document and JWKS are fetched over it, so plain HTTP would let anything on the path mint identities", o.Issuer))
	case u.Scheme != "http" && u.Scheme != "https":
		errs = append(errs, fmt.Errorf("sso.issuer: scheme must be https, got %q", u.Scheme))
	}

	if o.ClientID == "" {
		errs = append(errs, errors.New("sso.client_id: required when sso.issuer is set"))
	}
	if o.RedirectURL == "" {
		errs = append(errs, errors.New(
			"sso.redirect_url: required; it is the gateway's own /sso/callback address, and must be the URI registered at the provider"))
	} else if ru, err := url.Parse(o.RedirectURL); err != nil || ru.Scheme == "" || ru.Host == "" {
		errs = append(errs, fmt.Errorf("sso.redirect_url: must be an absolute URL, got %q", o.RedirectURL))
	}

	if o.KeyDuration <= 0 {
		errs = append(errs, fmt.Errorf("sso.key_duration: must be positive, got %s", o.KeyDuration))
	}
	if o.RenewWithin <= 0 {
		errs = append(errs, fmt.Errorf("sso.renew_within: must be positive, got %s", o.RenewWithin))
	}
	// A renewal window at or beyond the key's life means every check renews,
	// which turns a rare round trip into one per session start.
	if o.KeyDuration > 0 && o.RenewWithin >= o.KeyDuration {
		errs = append(errs, fmt.Errorf(
			"sso.renew_within: must be below sso.key_duration (got %s against %s), or every session start renews", o.RenewWithin, o.KeyDuration))
	}

	if o.RoleClaim == "" {
		errs = append(errs, errors.New("sso.role_claim: must name a claim"))
	}

	groups := c.Groups()
	if len(o.Roles) == 0 {
		errs = append(errs, errors.New(
			"sso.roles: at least one role is required; an authenticated identity with no role is refused, so SSO with no roles admits nobody"))
	}
	seen := make(map[string]int, len(o.Roles))
	for i, r := range o.Roles {
		path := fmt.Sprintf("sso.roles[%d]", i)
		if r.Match == "" {
			errs = append(errs, fmt.Errorf("%s.match: required", path))
		} else if prev, dup := seen[r.Match]; dup {
			errs = append(errs, fmt.Errorf(
				"%s.match: %q already matched by sso.roles[%d]; the first match wins, so the second can never apply", path, r.Match, prev))
		} else {
			seen[r.Match] = i
		}
		if r.RPMLimit < 0 {
			errs = append(errs, fmt.Errorf("%s.rpm_limit: must be >= 0, got %d", path, r.RPMLimit))
		}
		if r.TPMLimit < 0 {
			errs = append(errs, fmt.Errorf("%s.tpm_limit: must be >= 0, got %d", path, r.TPMLimit))
		}
		if r.MaxBudget < 0 {
			errs = append(errs, fmt.Errorf("%s.max_budget: must be >= 0, got %v", path, r.MaxBudget))
		}
		// The same reasoning as /key/generate: a window with no cap limits
		// nothing while looking as though it does.
		if r.BudgetDuration > 0 && r.MaxBudget == 0 {
			errs = append(errs, fmt.Errorf(
				"%s.budget_duration: requires max_budget; a window with no cap limits nothing", path))
		}
		for _, m := range r.Models {
			if m == "*" || strings.HasSuffix(m, "*") {
				continue
			}
			if _, ok := groups[m]; !ok {
				errs = append(errs, fmt.Errorf(
					"%s.models: no deployment serves %q, so this role could call nothing", path, m))
			}
		}
	}

	// Without a model name the client cannot be configured at all, and the
	// failure would surface as Claude Code asking for a model the gateway does
	// not route rather than as anything naming SSO.
	if o.Model == "" {
		errs = append(errs, errors.New(
			"sso.model: required when more than one model group is configured; it is handed to clients as ANTHROPIC_MODEL, which Claude Code needs because it skips model discovery when its only credential is a custom header"))
	} else if _, ok := groups[o.Model]; !ok {
		errs = append(errs, fmt.Errorf("sso.model: no deployment serves %q", o.Model))
	}

	if o.BaseURL == "" {
		errs = append(errs, errors.New(
			"sso.base_url: required when sso.redirect_url is not an absolute URL; it is handed to clients as ANTHROPIC_BASE_URL"))
	}

	errs = append(errs, validateJWTAuth(o)...)
	return errs
}

// validateJWTAuth checks the per-request token block.
//
// It is separate because everything here is inert until jwt_auth.enabled, and
// an operator who has not turned it on should not be told about fields they
// never wrote.
func validateJWTAuth(o SSOConfig) []error {
	j := o.JWTAuth
	if !j.Enabled {
		// A block written out but not enabled is the same oversight the issuer
		// guard catches: nothing is served, and nothing says so.
		if len(j.Audiences) > 0 || j.CacheTTL != nil {
			return []error{errors.New(
				"sso.jwt_auth: configured but not enabled, so the provider's tokens are not accepted; set sso.jwt_auth.enabled")}
		}
		return nil
	}

	var errs []error
	// Defaults fill this from client_id, so an empty list here means the client
	// id was empty too — which is already reported. Guarding anyway, because an
	// empty allowlist accepts nothing and would present as every token being
	// refused for no stated reason.
	if len(j.Audiences) == 0 {
		errs = append(errs, errors.New(
			"sso.jwt_auth.audiences: empty, so no token can be accepted; it defaults to sso.client_id"))
	}
	for i, a := range j.Audiences {
		switch {
		case strings.TrimSpace(a) == "":
			errs = append(errs, fmt.Errorf("sso.jwt_auth.audiences[%d]: must not be empty", i))
		case a == "*":
			// There is no wildcard, and silently treating one as a literal
			// audience would leave an operator believing they had opened this up
			// while every token was refused.
			errs = append(errs, errors.New(
				`sso.jwt_auth.audiences: "*" is not a wildcard; list each acceptable "aud" value, because a token minted for another client of the same provider says nothing about this gateway`))
		}
	}
	if j.CacheTTL != nil && *j.CacheTTL < 0 {
		errs = append(errs, fmt.Errorf(
			"sso.jwt_auth.cache_ttl: must not be negative, got %s", *j.CacheTTL))
	}
	return errs
}

// isLoopbackHost reports whether a hostname names this machine. It is the same
// question the SSO callback asks of a client's redirect URI, and the answer has
// to be exact in both places: a host that merely looks loopback, such as
// "127.0.0.1.example.com", is somebody else's.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// validateRBAC checks the entitlement hierarchy and every reference into it.
//
// The structural faults — a missing id, a separator inside a segment, a
// duplicate path — are already refused by resolveScopes, which has to reject
// them to build a map at all. What is left here is everything about the
// hierarchy's *contents*, and it is reported alongside the rest of the
// configuration rather than at the first fault.
func validateRBAC(c *Config) []error {
	var errs []error

	for _, id := range c.ScopeIDs() {
		s := c.scopes[id]
		where := fmt.Sprintf("rbac scope %q", id)

		if s.BudgetDuration > 0 && s.MaxBudget == 0 {
			// The same rule a key is held to: a window with nothing to cap is
			// far likelier to be a budget the operator believes they set than a
			// deliberate no-op.
			errs = append(errs, fmt.Errorf(
				"%s: budget_duration requires max_budget; a window with no cap limits nothing", where))
		}
		if s.MaxBudget < 0 {
			errs = append(errs, fmt.Errorf("%s: max_budget must not be negative, got %v", where, s.MaxBudget))
		}
		if s.RPMLimit < 0 || s.TPMLimit < 0 {
			errs = append(errs, fmt.Errorf("%s: rpm_limit and tpm_limit must not be negative", where))
		}

		// A model a parent withholds can never be reached from here, so listing
		// it is a statement the hierarchy contradicts. Refusing it is the point
		// of an intersection: an operator who writes it believes they granted
		// something, and nothing else would tell them otherwise.
		if parent := s.Parent; parent != nil {
			for _, m := range s.Models {
				if strings.HasSuffix(m, "*") {
					// A pattern is checked against the parent's patterns rather
					// than expanded: "claude-*" under a parent allowing
					// "claude-sonnet" is narrower for some names and wider for
					// others, and deciding which needs the model list, not the
					// hierarchy. The group check below covers the concrete case.
					continue
				}
				if !parent.AllowsModel(m) {
					errs = append(errs, fmt.Errorf(
						"%s: models lists %q, which %s %q does not allow; the chain is an intersection, so a child cannot widen its parent",
						where, m, parent.Kind, parent.ID))
				}
			}
		}

		// A concrete name that matches no configured group is a typo that would
		// otherwise present as a caller being refused a model nobody can call.
		groups := c.Groups()
		for _, m := range s.Models {
			if strings.HasSuffix(m, "*") || m == "*" {
				continue
			}
			if _, ok := groups[m]; !ok {
				errs = append(errs, fmt.Errorf("%s: models lists %q, which is not a configured model_name", where, m))
			}
		}
	}

	// Every reference into the hierarchy is resolved here rather than at first
	// use, so a mistyped scope fails at load rather than on the request that
	// happens to present the key.
	known := c.ScopeIDs()
	check := func(where, id string) {
		if id == "" {
			return
		}
		if c.Scope(id) == nil {
			msg := fmt.Sprintf("%s: scope %q is not declared under rbac", where, id)
			if len(known) > 0 {
				msg += " (declared: " + strings.Join(known, ", ") + ")"
			} else {
				msg += "; no rbac hierarchy is configured"
			}
			errs = append(errs, errors.New(msg))
		}
	}
	for i, k := range c.VirtualKeys.Keys {
		check(fmt.Sprintf("virtual_keys.keys[%d].scope", i), k.Scope)
	}
	for i, r := range c.SSO.Roles {
		check(fmt.Sprintf("sso.roles[%d].scope", i), r.Scope)
	}
	return errs
}

// validateAudit checks the audit log's configuration.
//
// It runs whether or not the block is enabled, unlike validateUI. A typo in a
// sink name costs nothing to catch now and is discovered at the worst possible
// moment otherwise: the day an operator turns auditing on, which is usually the
// day somebody has asked them to prove it was on.
func validateAudit(c *Config) []error {
	a := c.Audit
	var errs []error
	switch a.SinkKind() {
	case AuditSinkStdout:
		// Reported rather than ignored: an operator who names a file and gets
		// stdout believes they have a chain that survives restarts, and will
		// find out they do not by going to read it.
		if a.Path != "" {
			errs = append(errs, fmt.Errorf(
				"audit.path: %s is set while audit.sink is %q, which writes to stdout; set audit.sink to %q or remove the path",
				strconv.Quote(a.Path), AuditSinkStdout, AuditSinkFile))
		}
	case AuditSinkFile:
		if a.Path == "" {
			errs = append(errs, fmt.Errorf(
				"audit.path: required when audit.sink is %q, since a file sink has nowhere to write without one",
				AuditSinkFile))
		}
	case AuditSinkPostgres:
		if a.DSN == "" {
			errs = append(errs, fmt.Errorf(
				"audit.dsn: required when audit.sink is %q (unset or empty ${ENV} reference?)",
				AuditSinkPostgres))
		}
		// The mirror of the path check above, and the more consequential
		// direction: an operator switching a file chain to a shared one edits
		// the line under the sink, and a leftover path names a file the gateway
		// no longer writes while looking exactly like the place to go and read
		// the records.
		if a.Path != "" {
			errs = append(errs, fmt.Errorf(
				"audit.path: %s is set while audit.sink is %q, which keeps no file; remove the path or use sink: %q",
				strconv.Quote(a.Path), AuditSinkPostgres, AuditSinkFile))
		}
	default:
		errs = append(errs, fmt.Errorf("audit.sink: must be %q, %q or %q, got %q",
			AuditSinkStdout, AuditSinkFile, AuditSinkPostgres, a.Sink))
	}
	// A dsn on any other sink is refused rather than ignored, for the reason
	// the key store refuses one: the two ways to arrive here are an operator
	// who set the connection string and forgot the sink, and one who moved back
	// off Postgres and left the dsn behind. The first records nowhere it
	// expects; the second leaves a live database credential in a file with no
	// use for it.
	if a.SinkKind() != AuditSinkPostgres && a.DSN != "" {
		errs = append(errs, fmt.Errorf(
			"audit.dsn: set while audit.sink is %q, which never connects to a database; use sink: %q or remove the dsn",
			a.SinkKind(), AuditSinkPostgres))
	}
	if a.Timeout < 0 {
		errs = append(errs, fmt.Errorf("audit.timeout: must not be negative, got %s", a.Timeout))
	}
	if a.MaxConns < 0 {
		errs = append(errs, fmt.Errorf("audit.max_conns: must be >= 0, got %d", a.MaxConns))
	}
	return errs
}

// validateUI checks the admin UI's configuration.
//
// The only hard requirement is a master key. The UI signs an operator in by
// checking one against the configured master key and handing back a session
// cookie, so a UI enabled without one is a sign-in page nobody can pass —
// which is worth refusing at load rather than discovering in a browser.
func validateUI(c *Config) []error {
	if !c.UI.Enabled {
		return nil
	}
	var errs []error
	if c.UI.RequestLog() < 0 {
		errs = append(errs, fmt.Errorf("ui.request_log_size: must be >= 0, got %d", c.UI.RequestLog()))
	}
	if c.VirtualKeys.MasterKey == "" {
		errs = append(errs, errors.New(
			"ui.enabled: requires virtual_keys.master_key, since signing in to the UI means presenting it"))
	}
	return errs
}
