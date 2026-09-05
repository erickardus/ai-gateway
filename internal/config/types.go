// Package config defines the gateway's configuration model and loads it from
// YAML. Loading applies defaults and then validates, so a Config returned by
// Load is safe for the rest of the gateway to consume without re-checking.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/erickardus/ai-gateway/internal/cache"
	"github.com/erickardus/ai-gateway/internal/core"
)

// Config is the root of gateway.yaml.
type Config struct {
	Server        ServerConfig        `yaml:"server"`
	ModelList     []Deployment        `yaml:"model_list"`
	Router        RouterConfig        `yaml:"router"`
	VirtualKeys   VirtualKeysConfig   `yaml:"virtual_keys"`
	Observability ObservabilityConfig `yaml:"observability"`
	Redis         RedisConfig         `yaml:"redis"`
	Cache         CacheConfig         `yaml:"cache"`
}

// CacheConfig controls response caching.
type CacheConfig struct {
	Enabled bool          `yaml:"enabled"`
	TTL     time.Duration `yaml:"ttl"`
	// Scope decides who may see a cached response. "key" — the default — keeps
	// each virtual key to its own responses. "shared" lets every caller reuse
	// any cached response, which saves far more but serves one tenant's
	// completion to another, so it is only appropriate when all callers are
	// equally trusted.
	Scope cache.Scope `yaml:"scope"`
	// MaxEntries bounds the in-process cache.
	MaxEntries int `yaml:"max_entries"`
	// MaxEntryBytes refuses to cache responses larger than this, so one
	// oversized completion cannot evict everything else.
	MaxEntryBytes int64 `yaml:"max_entry_bytes"`
	// Shared stores entries in Redis, so a hit on one instance serves them all.
	// Requires redis.addr.
	Shared bool `yaml:"shared"`
}

// RedisConfig shares state across gateway instances.
//
// Without it every replica enforces its own limits, so a rate limit is silently
// multiplied by the replica count and a key can spend its whole budget once per
// instance. Latency and in-flight stay per-instance even when this is set, since
// both describe one instance's own view.
type RedisConfig struct {
	// Addr enables sharing when set, e.g. "localhost:6379".
	Addr     string `yaml:"addr"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
	// KeyPrefix namespaces this gateway's keys, so several deployments can
	// share one Redis without colliding.
	KeyPrefix string `yaml:"key_prefix"`
	// Timeout bounds each Redis call. It is short by design: a slow Redis must
	// degrade the gateway to local state rather than add its latency to every
	// request.
	Timeout time.Duration `yaml:"timeout"`
}

// Enabled reports whether state sharing is configured.
func (r RedisConfig) Enabled() bool { return r.Addr != "" }

// ServerConfig controls the HTTP listener.
type ServerConfig struct {
	Addr              string        `yaml:"addr"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	ShutdownGrace     time.Duration `yaml:"shutdown_grace"`
	MaxBodyBytes      int64         `yaml:"max_body_bytes"`
}

// Deployment is one concrete upstream backing a public model name. Several
// deployments may share a ModelName; together they form a model group that the
// router load-balances across.
type Deployment struct {
	ModelName string           `yaml:"model_name"`
	Params    DeploymentParams `yaml:"params"`
	// Weight is a pointer so an explicit 0, meaning "drain this deployment",
	// is distinguishable from an omitted field.
	Weight *int `yaml:"weight"`
	RPM    int  `yaml:"rpm"`
	TPM    int  `yaml:"tpm"`
	// Cost prices this deployment's tokens. It is meaningless on a passthrough
	// deployment, where the upstream bills the caller's own subscription rather
	// than the operator, and is rejected there.
	Cost core.Pricing `yaml:"cost"`

	// id is derived at load time and is stable for a given config.
	id string
}

// Share returns the deployment's selection weight, or 1 when unset.
func (d *Deployment) Share() int {
	if d.Weight == nil {
		return 1
	}
	return *d.Weight
}

// ID returns the deployment's stable identifier. Routing state, cooldowns and
// metrics all key on it.
func (d *Deployment) ID() string { return d.id }

// setID derives a stable ID from the public name and the upstream it points at.
// The ordinal is deliberately excluded: including it would make every entry
// unique by construction and so make genuine duplicates undetectable, and it
// would renumber deployments when an unrelated one is inserted above them.
func (d *Deployment) setID() {
	sum := sha256.Sum256([]byte(d.Params.APIBase + "|" + d.Params.Model))
	d.id = fmt.Sprintf("%s#%s", d.ModelName, hex.EncodeToString(sum[:6]))
}

// DeploymentParams describes how to reach one upstream.
type DeploymentParams struct {
	Format  core.Format `yaml:"format"`
	APIBase string      `yaml:"api_base"`
	// Model is the upstream's own model identifier. When it differs from the
	// enclosing ModelName the gateway rewrites exactly that one JSON value;
	// when they match the body is forwarded with no modification at all.
	Model string `yaml:"model"`

	AuthMode core.AuthMode `yaml:"auth_mode"`
	// AuthHeader names the header carrying the credential in api_key mode,
	// typically "x-api-key" for Anthropic or "authorization" for OpenAI.
	AuthHeader string `yaml:"auth_header"`
	// AuthScheme is an optional prefix such as "Bearer" for AuthHeader.
	AuthScheme string `yaml:"auth_scheme"`
	APIKey     string `yaml:"api_key"`
}

// RouterConfig controls deployment selection, retries and fallbacks.
type RouterConfig struct {
	Strategy string `yaml:"strategy"`
	// NumRetries is how many attempts follow the first. Like AllowedFails it is
	// a pointer, so an explicit 0 meaning "never retry" is not mistaken for an
	// omitted field and silently replaced by the default.
	NumRetries    *int          `yaml:"num_retries"`
	Timeout       time.Duration `yaml:"timeout"`
	StreamTimeout time.Duration `yaml:"stream_timeout"`

	Cooldown CooldownConfig `yaml:"cooldown"`
	Backoff  BackoffConfig  `yaml:"backoff"`

	MaxFallbackHops        int            `yaml:"max_fallback_hops"`
	Fallbacks              []FallbackRule `yaml:"fallbacks"`
	ContextWindowFallbacks []FallbackRule `yaml:"context_window_fallbacks"`
	ContentPolicyFallbacks []FallbackRule `yaml:"content_policy_fallbacks"`

	// LowestLatencyBuffer widens the latency-based strategy's candidate set to
	// every deployment within this fraction of the best observed latency.
	LowestLatencyBuffer float64 `yaml:"lowest_latency_buffer"`
}

// CooldownConfig controls ejection of failing deployments.
type CooldownConfig struct {
	// AllowedFails is how many failures within Period are tolerated before a
	// deployment is ejected. It is a pointer so that an explicit 0, meaning
	// "eject on the first failure", is distinguishable from an omitted field.
	// LiteLLM conflates the two, which silently turns some configured values
	// into no-ops.
	AllowedFails *int          `yaml:"allowed_fails"`
	Period       time.Duration `yaml:"period"`
}

// Retries returns the configured retry count, or the default when unset.
func (r RouterConfig) Retries() int {
	if r.NumRetries == nil {
		return DefaultNumRetries
	}
	return *r.NumRetries
}

// Fails returns the configured failure allowance, or the default when unset.
func (c CooldownConfig) Fails() int {
	if c.AllowedFails == nil {
		return DefaultAllowedFails
	}
	return *c.AllowedFails
}

// BackoffConfig controls retry pacing. Backoff only applies when no other
// healthy deployment is available; otherwise the router retries immediately
// against a different one.
type BackoffConfig struct {
	Initial time.Duration `yaml:"initial"`
	Max     time.Duration `yaml:"max"`
	// Jitter is a pointer so an explicit 0, meaning deterministic backoff, is
	// distinguishable from an omitted field.
	Jitter *float64 `yaml:"jitter"`
}

// JitterFactor returns the configured jitter, or the default when unset.
func (b BackoffConfig) JitterFactor() float64 {
	if b.Jitter == nil {
		return DefaultBackoffJitter
	}
	return *b.Jitter
}

// FallbackRule redirects a failing model group to other groups, in order.
type FallbackRule struct {
	From string   `yaml:"from"`
	To   []string `yaml:"to"`
}

// VirtualKeysConfig controls caller authentication.
type VirtualKeysConfig struct {
	// MasterKey authenticates the /key/* management endpoints. When empty those
	// endpoints are disabled.
	MasterKey string      `yaml:"master_key"`
	Store     StoreConfig `yaml:"store"`
	// HeaderNames lists the custom headers that may carry a virtual key, in
	// precedence order. These must not collide with a header Claude Code sets
	// itself: the whole design depends on the key travelling beside the
	// caller's own credential rather than replacing it.
	HeaderNames []string `yaml:"header_names"`
	// AllowedUpstreamHosts bounds where a passthrough deployment may relay a
	// caller credential. A host absent from this list is refused.
	AllowedUpstreamHosts []string  `yaml:"allowed_upstream_hosts"`
	Keys                 []KeySpec `yaml:"keys"`
}

// StoreConfig selects the key persistence backend.
type StoreConfig struct {
	// Kind is "memory" or "file".
	Kind string `yaml:"kind"`
	Path string `yaml:"path"`
}

// KeySpec declares a virtual key in configuration. The plaintext is hashed at
// load time and never retained.
type KeySpec struct {
	Key              string     `yaml:"key"`
	Alias            string     `yaml:"alias"`
	Models           []string   `yaml:"models"`
	RPMLimit         int        `yaml:"rpm_limit"`
	TPMLimit         int        `yaml:"tpm_limit"`
	AllowPassthrough bool       `yaml:"allow_passthrough"`
	Blocked          bool       `yaml:"blocked"`
	ExpiresAt        *time.Time `yaml:"expires_at"`
	// MaxBudget caps what this key may spend within BudgetDuration. Zero means
	// unlimited. Only billable traffic counts toward it.
	MaxBudget float64 `yaml:"max_budget"`
	// BudgetDuration is the window MaxBudget applies over. Zero means the key's
	// whole lifetime.
	BudgetDuration time.Duration `yaml:"budget_duration"`
}

// ObservabilityConfig controls logging, metrics and spend persistence.
type ObservabilityConfig struct {
	LogLevel  string `yaml:"log_level"`
	LogFormat string `yaml:"log_format"`
	// Metrics enables GET /metrics in the Prometheus text format.
	Metrics bool `yaml:"metrics"`
	// SpendStorePath persists the spend ledger so budgets survive a restart.
	// Empty keeps it in memory only.
	SpendStorePath string `yaml:"spend_store_path"`
	// SpendFlushInterval is how often the ledger is written to disk.
	SpendFlushInterval time.Duration `yaml:"spend_flush_interval"`
}

// Groups returns deployments indexed by public model name, preserving
// declaration order within each group.
func (c *Config) Groups() map[string][]*Deployment {
	out := make(map[string][]*Deployment)
	for i := range c.ModelList {
		d := &c.ModelList[i]
		out[d.ModelName] = append(out[d.ModelName], d)
	}
	return out
}
