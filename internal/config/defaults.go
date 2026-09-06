package config

import (
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
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

	// DefaultUISessionTTL is one working day, so an operator signs in once in
	// the morning rather than every hour, and a browser left open overnight is
	// signed out by the time nobody is watching it.
	DefaultUISessionTTL = 12 * time.Hour

	// DefaultUIRequestLogSize is a few minutes of history on a busy gateway and
	// a few days on a quiet one, for roughly a megabyte of memory. It is sized
	// for reading rather than for archiving: anything that needs to outlive the
	// process belongs in the access log.
	DefaultUIRequestLogSize = 1000

	// DefaultOTLPInterval matches the OpenTelemetry SDK's own default export
	// interval, so a collector sees this gateway arrive at the same cadence as
	// everything else pointed at it.
	DefaultOTLPInterval = 60 * time.Second
	// DefaultOTLPTimeout bounds one export. Well under the interval, so a slow
	// collector cannot leave exports overlapping.
	DefaultOTLPTimeout = 10 * time.Second
	// DefaultOTLPProtocol is what every collector accepts.
	DefaultOTLPProtocol = "http/protobuf"
	// DefaultSSOKeyDuration is how long a key issued by an SSO login lives.
	// Long enough that renewal is rare, short enough that a key left behind on
	// a machine nobody logs into again stops working.
	DefaultSSOKeyDuration = 720 * time.Hour
	// DefaultSSORenewWithin is how far ahead of expiry a client renews. A
	// quarter of the key's life, so a developer who runs Claude Code even once
	// a week never sees an expired key.
	DefaultSSORenewWithin = 168 * time.Hour
	// DefaultSSORoleClaim is the claim matched against the configured roles.
	// Every major provider can emit group membership under this name.
	DefaultSSORoleClaim = "groups"

	// DefaultServiceName is the service.name resource attribute, the key almost
	// every OTLP backend groups by.
	DefaultServiceName = "ai-gateway"

	// DefaultRedisKeyPrefix namespaces shared state.
	DefaultRedisKeyPrefix = "ai-gateway"
	// DefaultRedisTimeout bounds each Redis call, short enough that an
	// unhealthy Redis degrades the gateway rather than slowing it.
	DefaultRedisTimeout = 250 * time.Millisecond

	// DefaultKeyStoreTimeout bounds each query against a Postgres key store. It
	// is an order of magnitude longer than DefaultRedisTimeout because the two
	// failures are not alike: an abandoned Redis call falls back to local
	// state, while an abandoned key lookup refuses a request from a caller
	// holding a valid key. Long enough to ride out a reconnect or a stall,
	// short enough to fail well inside router.timeout.
	DefaultKeyStoreTimeout = 2 * time.Second
	// DefaultKeyStoreMaxConns sizes the Postgres pool. A key lookup is a
	// primary-key read on a table with as many rows as the fleet has keys, so
	// ten connections carry far more authentication than one gateway process
	// can produce, and a small pool is what keeps a fleet of replicas from
	// exhausting a database's own connection limit between them.
	DefaultKeyStoreMaxConns int32 = 10

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
	// DefaultAffinityMaxInFlightLead is how far ahead of its idlest peer a
	// pinned deployment may run before pins stop being honoured for it.
	//
	// It is deliberately loose. A handful of concurrent Claude Code sessions
	// never reaches it, which is the point: those should stay pinned. Traffic
	// that has genuinely collapsed onto one deployment passes it almost at
	// once, and each request that yields lowers the lead, so the group settles
	// with most requests still hitting a warm cache.
	DefaultAffinityMaxInFlightLead = 4
	// DefaultInjectMinBytes is roughly the smallest prefix Anthropic will cache
	// — its minimum is 1024 tokens on the models this gateway fronts, and JSON
	// prompt text runs about four bytes to the token. Below it a breakpoint is
	// ignored upstream, so placing one only adds bytes to the request.
	DefaultInjectMinBytes = 4096

	// Audit sinks. AuditSinkStdout is the default because it needs no path and
	// introduces no failure the process's own logging does not already have;
	// AuditSinkFile is the one that continues a hash chain across a restart.
	AuditSinkStdout   = "stdout"
	AuditSinkFile     = "file"
	AuditSinkPostgres = "postgres"

	// DefaultAuditTimeout bounds each append to a Postgres audit chain. It is
	// longer than DefaultKeyStoreTimeout because the two sit in different
	// places: a key lookup happens on every inference request and must fail
	// well inside the router's own timeout, while an append happens when an
	// operator mints a key and is worth waiting for.
	DefaultAuditTimeout = 5 * time.Second
	// DefaultAuditMaxConns sizes the Postgres audit pool. Appends serialize on
	// a fleet-wide advisory lock, so more connections buy no throughput; this
	// is sized to hold a spare or two for the verification query and no more,
	// since every replica opens its own.
	DefaultAuditMaxConns int32 = 4

	// DefaultJWTAuthCacheTTL is how long a verified token is reused. It is short
	// because the point of verifying per request is that revocation arrives
	// quickly, and a long cache would hand back the lifetime a virtual key
	// already offers with none of its simplicity.
	DefaultJWTAuthCacheTTL = 60 * time.Second
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

// DefaultSSOScopes are requested when sso.scopes is unset. offline_access buys
// the refresh token that lets renewal re-check the identity against the
// provider, which is what makes disabling an account stop its renewals.
var DefaultSSOScopes = []string{"openid", "email", "profile", "groups", "offline_access"}

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
	if v.Store.Timeout == 0 {
		v.Store.Timeout = DefaultKeyStoreTimeout
	}
	if v.Store.MaxConns == 0 {
		v.Store.MaxConns = DefaultKeyStoreMaxConns
	}

	if c.Audit.Timeout == 0 {
		c.Audit.Timeout = DefaultAuditTimeout
	}
	if c.Audit.MaxConns == 0 {
		c.Audit.MaxConns = DefaultAuditMaxConns
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
	applyOTLPDefaults(&c.Observability.OTLP)
	applySSODefaults(c)

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

	if c.UI.SessionTTL == 0 {
		c.UI.SessionTTL = DefaultUISessionTTL
	}
}

// applyOTLPDefaults fills the exporter's blanks, reading the standard
// OpenTelemetry environment variables where the config file is silent.
//
// Honouring OTEL_* matters more than it looks. Those variables are how a
// collector sidecar, an operator's injected environment, or a hosted vendor's
// setup instructions configure every other component in a fleet; a gateway that
// read only its own YAML would be the one process needing to be told separately,
// and would silently export nothing in an environment where everything else
// worked. The config file still wins wherever it speaks, because it is the more
// specific statement.
func applyOTLPDefaults(o *OTLPConfig) {
	if o.Endpoint == "" {
		// The metrics-specific variable is the more specific of the two and
		// wins, which is the precedence the OTLP specification defines. Note it
		// is a full signal URL rather than a base, but endpointURL leaves a
		// path it is given alone, so both spellings land in the right place.
		o.Endpoint = firstNonEmpty(
			os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"),
			os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		)
	}
	if !o.Enabled() {
		return
	}
	if o.Protocol == "" {
		o.Protocol = firstNonEmpty(
			os.Getenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"),
			os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"),
			DefaultOTLPProtocol,
		)
	}
	if o.Interval == 0 {
		o.Interval = DefaultOTLPInterval
	}
	if o.Timeout == 0 {
		o.Timeout = DefaultOTLPTimeout
	}
	if o.ServiceName == "" {
		o.ServiceName = firstNonEmpty(os.Getenv("OTEL_SERVICE_NAME"), DefaultServiceName)
	}

	// Headers from the environment are the base; anything the config file names
	// is layered over them, so a file can override one header without having to
	// restate the rest.
	env := parseOTLPHeaders(firstNonEmpty(
		os.Getenv("OTEL_EXPORTER_OTLP_METRICS_HEADERS"),
		os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"),
	))
	if len(env) > 0 {
		merged := make(map[string]string, len(env)+len(o.Headers))
		maps.Copy(merged, env)
		maps.Copy(merged, o.Headers)
		o.Headers = merged
	}

	if attrs := parseOTLPAttributes(os.Getenv("OTEL_RESOURCE_ATTRIBUTES")); len(attrs) > 0 {
		merged := make(map[string]string, len(attrs)+len(o.ResourceAttributes))
		maps.Copy(merged, attrs)
		maps.Copy(merged, o.ResourceAttributes)
		o.ResourceAttributes = merged
	}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// parseOTLPHeaders reads the W3C Baggage form the OTLP specification uses for
// OTEL_EXPORTER_OTLP_HEADERS: comma-separated key=value pairs, percent-encoded.
//
// A malformed pair is skipped rather than failing the load. The variable is
// usually injected by something else — a sidecar, a platform — and refusing to
// start over one unreadable header would take the gateway down for a
// telemetry-only problem.
func parseOTLPHeaders(raw string) map[string]string {
	return parseOTLPPairs(raw, true)
}

// parseOTLPAttributes reads OTEL_RESOURCE_ATTRIBUTES, which uses the same form.
func parseOTLPAttributes(raw string) map[string]string {
	return parseOTLPPairs(raw, true)
}

func parseOTLPPairs(raw string, unescape bool) map[string]string {
	if raw == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		k, v, ok := strings.Cut(pair, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" {
			continue
		}
		if unescape {
			if dk, err := url.QueryUnescape(k); err == nil {
				k = dk
			}
			if dv, err := url.QueryUnescape(v); err == nil {
				v = dv
			}
		}
		out[k] = v
	}
	return out
}

// applySSODefaults fills the SSO block. It is a no-op when SSO is not
// configured, so an unconfigured gateway does not acquire a half-populated
// block that validation would then have to reason about.
func applySSODefaults(c *Config) {
	s := &c.SSO
	if !s.Enabled() {
		return
	}
	if len(s.Scopes) == 0 {
		s.Scopes = slices.Clone(DefaultSSOScopes)
	}
	if s.KeyDuration == 0 {
		s.KeyDuration = DefaultSSOKeyDuration
	}
	if s.RenewWithin == 0 {
		s.RenewWithin = DefaultSSORenewWithin
	}
	if s.RoleClaim == "" {
		s.RoleClaim = DefaultSSORoleClaim
	}
	if s.BaseURL == "" {
		if u, err := url.Parse(s.RedirectURL); err == nil && u.Scheme != "" && u.Host != "" {
			s.BaseURL = u.Scheme + "://" + u.Host
		}
	}
	// One model group is unambiguous, so asking the operator to name it again
	// would only be a second place to keep in step. Several are ambiguous, and
	// validation says so rather than picking one.
	if s.Model == "" {
		if groups := c.Groups(); len(groups) == 1 {
			for name := range groups {
				s.Model = name
			}
		}
	}

	if s.JWTAuth.Enabled {
		if len(s.JWTAuth.Audiences) == 0 {
			// The client id is what an ID token carries, and it is the value the
			// login path already checks, so a gateway that turns this on without
			// naming an audience accepts exactly the tokens it was already
			// prepared to verify — rather than accepting any audience, which is
			// the one default that would be a hole.
			s.JWTAuth.Audiences = []string{s.ClientID}
		}
		if s.JWTAuth.CacheTTL == nil {
			d := DefaultJWTAuthCacheTTL
			s.JWTAuth.CacheTTL = &d
		}
	}
}
