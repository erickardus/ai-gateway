package config

import (
	"slices"
	"time"

	"github.com/erickardus/ai-gateway/internal/cache"

	"github.com/erickardus/ai-gateway/internal/core"
)

// Default values. Several deliberately differ from LiteLLM: its effective
// cooldown of 5s is too twitchy for real upstreams, and its rpm/tpm fields are
// weights that go unenforced, whereas here they are real limits.
const (
	DefaultAddr              = "0.0.0.0:4000"
	DefaultReadHeaderTimeout = 30 * time.Second
	DefaultIdleTimeout       = 120 * time.Second
	DefaultShutdownGrace     = 30 * time.Second
	DefaultMaxBodyBytes      = 32 << 20

	DefaultStrategy        = StrategyWeightedShuffle
	DefaultNumRetries      = 2
	DefaultTimeout         = 600 * time.Second
	DefaultStreamTimeout   = 60 * time.Second
	DefaultAllowedFails    = 3
	DefaultCooldownPeriod  = 30 * time.Second
	DefaultBackoffInitial  = 500 * time.Millisecond
	DefaultBackoffMax      = 8 * time.Second
	DefaultBackoffJitter   = 0.75
	DefaultMaxFallbackHops = 5

	// DefaultSpendFlushInterval paces persistence of the spend ledger.
	DefaultSpendFlushInterval = 30 * time.Second

	// DefaultRedisKeyPrefix namespaces shared state.
	DefaultRedisKeyPrefix = "ai-gateway"
	// DefaultRedisTimeout bounds each Redis call, short enough that an
	// unhealthy Redis degrades the gateway rather than slowing it.
	DefaultRedisTimeout = 250 * time.Millisecond

	// DefaultCacheTTL is short: an LLM response is only interchangeable with a
	// fresh one for so long, and a long TTL turns a cache into stale answers.
	DefaultCacheTTL = 5 * time.Minute
	// DefaultCacheMaxEntries bounds the in-process cache.
	DefaultCacheMaxEntries = 1000
	// DefaultCacheMaxEntryBytes refuses outsized responses.
	DefaultCacheMaxEntryBytes = 1 << 20

	// DefaultAffinityTTL matches the lifetime of an Anthropic ephemeral prompt
	// cache entry. A pin that outlived the cache it points at would keep
	// concentrating a conversation on one deployment after the reason for doing
	// so had expired.
	DefaultAffinityTTL = 5 * time.Minute
	// DefaultInjectMinBytes is roughly the smallest prefix Anthropic will cache
	// — its minimum is 1024 tokens on the models this gateway fronts, and JSON
	// prompt text runs about four bytes to the token. Below it a breakpoint is
	// ignored upstream, so placing one only adds bytes to the request.
	DefaultInjectMinBytes = 4096
)

// Strategy names accepted by router.strategy.
const (
	StrategyWeightedShuffle = "weighted-shuffle"
	StrategyLeastBusy       = "least-busy"
	StrategyUsageBased      = "usage-based"
	StrategyLatencyBased    = "latency-based"
)

// KnownStrategies lists every valid strategy name.
var KnownStrategies = []string{
	StrategyWeightedShuffle,
	StrategyLeastBusy,
	StrategyUsageBased,
	StrategyLatencyBased,
}

func (c *Config) applyDefaults() {
	s := &c.Server
	if s.Addr == "" {
		s.Addr = DefaultAddr
	}
	if s.ReadHeaderTimeout == 0 {
		s.ReadHeaderTimeout = DefaultReadHeaderTimeout
	}
	if s.IdleTimeout == 0 {
		s.IdleTimeout = DefaultIdleTimeout
	}
	if s.ShutdownGrace == 0 {
		s.ShutdownGrace = DefaultShutdownGrace
	}
	if s.MaxBodyBytes == 0 {
		s.MaxBodyBytes = DefaultMaxBodyBytes
	}

	r := &c.Router
	if r.Strategy == "" {
		r.Strategy = DefaultStrategy
	}
	if r.Timeout == 0 {
		r.Timeout = DefaultTimeout
	}
	if r.StreamTimeout == 0 {
		r.StreamTimeout = DefaultStreamTimeout
	}
	if r.Cooldown.Period == 0 {
		r.Cooldown.Period = DefaultCooldownPeriod
	}
	if r.Backoff.Initial == 0 {
		r.Backoff.Initial = DefaultBackoffInitial
	}
	if r.Backoff.Max == 0 {
		r.Backoff.Max = DefaultBackoffMax
	}
	if r.MaxFallbackHops == 0 {
		r.MaxFallbackHops = DefaultMaxFallbackHops
	}

	v := &c.VirtualKeys
	if len(v.HeaderNames) == 0 {
		v.HeaderNames = slices.Clone(core.DefaultKeyHeaderNames)
	}
	if v.Store.Kind == "" {
		v.Store.Kind = "memory"
	}

	for i := range c.ModelList {
		d := &c.ModelList[i]
		if d.Params.Format == "" {
			d.Params.Format = "anthropic"
		}
		if d.Params.AuthMode == "" {
			d.Params.AuthMode = "api_key"
		}
		if d.Params.Model == "" {
			d.Params.Model = d.ModelName
		}
		if d.Params.AuthHeader == "" {
			if d.Params.Format == "anthropic" {
				d.Params.AuthHeader = "x-api-key"
			} else {
				d.Params.AuthHeader = "authorization"
				d.Params.AuthScheme = "Bearer"
			}
		}
	}

	if c.Observability.LogLevel == "" {
		c.Observability.LogLevel = "info"
	}
	if c.Observability.LogFormat == "" {
		c.Observability.LogFormat = "text"
	}
	if c.Observability.SpendFlushInterval == 0 {
		c.Observability.SpendFlushInterval = DefaultSpendFlushInterval
	}

	if c.Redis.KeyPrefix == "" {
		c.Redis.KeyPrefix = DefaultRedisKeyPrefix
	}
	if c.Redis.Timeout == 0 {
		c.Redis.Timeout = DefaultRedisTimeout
	}

	if c.Cache.TTL == 0 {
		c.Cache.TTL = DefaultCacheTTL
	}
	if c.Cache.Scope == "" {
		// Per-key by default: a shared cache is a deliberate choice, not
		// something to arrive at by omission.
		c.Cache.Scope = cache.ScopeKey
	}
	if c.Cache.MaxEntries == 0 {
		c.Cache.MaxEntries = DefaultCacheMaxEntries
	}
	if c.Cache.MaxEntryBytes == 0 {
		c.Cache.MaxEntryBytes = DefaultCacheMaxEntryBytes
	}

	if c.PromptCache.AffinityTTL == 0 {
		c.PromptCache.AffinityTTL = DefaultAffinityTTL
	}
	if c.PromptCache.InjectMinBytes == 0 {
		c.PromptCache.InjectMinBytes = DefaultInjectMinBytes
	}
}
