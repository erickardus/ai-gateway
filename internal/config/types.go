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
	PromptCache   PromptCacheConfig   `yaml:"prompt_cache"`
	SSO           SSOConfig           `yaml:"sso"`
	RBAC          RBACConfig          `yaml:"rbac"`

	// scopes is the flattened hierarchy, resolved once at load. Keyed by fully
	// qualified id, with Parent pointers already wired.
	scopes map[string]*core.Scope
}

// PromptCacheConfig controls how the gateway treats the provider's own prompt
// cache. It is unrelated to CacheConfig, which caches whole responses here; this
// one is about the cache Anthropic keeps of a request's leading tokens.
type PromptCacheConfig struct {
	// Affinity pins requests sharing a cacheable prefix to the deployment that
	// last served one, so a conversation keeps hitting the prompt cache it
	// warmed instead of paying a cache write on every hop. On by default: with
	// prompt caching in play, spreading a conversation across deployments costs
	// more than it balances.
	//
	// It is a preference, never a constraint. A pinned deployment that is
	// cooling down, at its rate limit or already failed this request is passed
	// over exactly as if there were no pin.
	Affinity *bool `yaml:"affinity"`
	// AffinityTTL is how long a pin survives without use. It defaults to the
	// lifetime of an ephemeral prompt cache entry, since a pin outliving the
	// cache it points at only concentrates load.
	AffinityTTL time.Duration `yaml:"affinity_ttl"`
	// AffinityMaxInFlightLead bounds how far a pin may concentrate load. A pin
	// is passed over once the pinned deployment is carrying this many more
	// in-flight requests than the least busy deployment that could serve the
	// request instead.
	//
	// It exists because a fingerprint is a prefix, not a conversation: traffic
	// that shares a system prompt, its tools and its opening turns — a
	// templated single-turn caller, say — shares one pin, and without a bound
	// every request of it lands on one deployment while the rest of the group
	// sits idle.
	//
	// The bound is a load comparison rather than a share quota, because
	// concentration is only a problem when there is contention. One busy
	// conversation pinned to one deployment while its peers are idle is the
	// feature working; diverting it would buy a cache write and nothing else.
	//
	// A pointer, so an explicit 0 — yield as soon as any alternative is less
	// loaded — is not mistaken for an omitted field.
	AffinityMaxInFlightLead *int `yaml:"affinity_max_in_flight_lead"`

	// Inject marks the stable prefix of an Anthropic request — the tools and
	// the system prompt — as cacheable when the caller has not marked anything
	// itself.
	//
	// Off by default, and refused outright while any passthrough deployment is
	// configured: injection edits the request body, and the passthrough path
	// exists precisely to forward a body unchanged.
	Inject bool `yaml:"inject"`
	// InjectMinBytes suppresses injection for prefixes too small for a provider
	// to cache. Anthropic ignores a breakpoint below its own minimum rather
	// than rejecting it, so this is an economy rather than a correctness rule.
	InjectMinBytes int `yaml:"inject_min_bytes"`
}

// AffinityEnabled reports whether prefix affinity is on, defaulting to true.
func (p PromptCacheConfig) AffinityEnabled() bool {
	return p.Affinity == nil || *p.Affinity
}

// MaxInFlightLead returns the configured in-flight lead a pin may hold before it
// is passed over, or the default when unset.
func (p PromptCacheConfig) MaxInFlightLead() int {
	if p.AffinityMaxInFlightLead == nil {
		return DefaultAffinityMaxInFlightLead
	}
	return *p.AffinityMaxInFlightLead
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

	// SupportsCacheControl declares that this upstream reads Anthropic's
	// cache_control marker even though it speaks the OpenAI wire format.
	//
	// Most of that ecosystem does not: OpenAI, Kimi, GLM and DeepSeek cache
	// automatically, and a marker is at best ignored. Alibaba's Qwen is the
	// exception — it has an explicit cache entered by marking a content block,
	// with a higher hit ratio than the implicit one it falls back to.
	//
	// It is opt-in per deployment because it is a trade rather than a free win:
	// the explicit cache charges for writes where the implicit one does not, so
	// turning it on for traffic that does not reuse its prefix costs money. It
	// is also unverifiable from here — the gateway cannot ask an arbitrary
	// OpenAI-compatible base URL what it accepts — so an operator naming the
	// upstream is the only sound source of the answer.
	//
	// Meaningless on an anthropic deployment, which always reads the marker, and
	// on a passthrough one, whose body is never annotated.
	SupportsCacheControl bool `yaml:"supports_cache_control"`

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
	// Scope is the fully qualified id of a project, team or organisation
	// declared under `rbac`. The scope's limits bind this key in addition to
	// its own, and the scope's budget is a pool this key draws from rather
	// than a second allowance of its own.
	Scope string `yaml:"scope"`
}

// SSOConfig provisions virtual keys from an OpenID Connect identity provider.
//
// It exists because the alternative is an operator running /key/generate,
// copying a plaintext key out of the response and sending it to a developer to
// paste into their Claude Code settings. That is manual at both ends, puts a
// long-lived credential through a chat window, and ties the key to nothing, so
// offboarding depends on someone remembering which hash belonged to whom.
//
// The key an SSO login issues is an ordinary virtual key. It travels in the same
// custom header as every other one, which is the whole reason this can exist at
// all: Claude Code's only credential channel that does not displace a claude.ai
// subscription is ANTHROPIC_CUSTOM_HEADERS. Every mechanism it offers for
// fetching a credential dynamically — apiKeyHelper above all — writes
// Authorization and x-api-key instead, and would bill the developer per token.
type SSOConfig struct {
	// Issuer is the provider's base URL, from which the OpenID Connect
	// discovery document is read. Empty disables SSO and its endpoints.
	Issuer string `yaml:"issuer"`
	// ClientID and ClientSecret identify the gateway to the provider. The
	// gateway is the only OIDC client: the CLI holds no provider configuration,
	// so the identity team registers one redirect URI rather than one per tool.
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	// Scopes requested at authorization. offline_access is included by default
	// because without a refresh token the gateway cannot re-check an identity
	// at renewal, and renewal would then outlive the account it belongs to.
	Scopes []string `yaml:"scopes"`
	// RedirectURL is the gateway's own callback, and must be the URI registered
	// at the provider.
	RedirectURL string `yaml:"redirect_url"`
	// KeyDuration is how long an issued key lives.
	KeyDuration time.Duration `yaml:"key_duration"`
	// RenewWithin is how far ahead of expiry the client renews. Renewing early
	// is what keeps renewal invisible: the value a running session already read
	// stays valid, and the rewritten one applies at the next launch.
	RenewWithin time.Duration `yaml:"renew_within"`
	// RoleClaim names the ID-token claim whose values are matched against
	// Roles. It may hold a single string or a list of them.
	RoleClaim string `yaml:"role_claim"`
	// Roles map an identity to what it may do. Entitlements are declared here
	// and never read from the token: a claim that could grant a model or raise
	// a budget would make any mapping mistake at the provider a privilege
	// escalation here.
	Roles []SSORole `yaml:"roles"`
	// BaseURL is the gateway address handed to clients as ANTHROPIC_BASE_URL.
	// It defaults to the origin of RedirectURL, which is already the address
	// the provider redirects a browser to.
	BaseURL string `yaml:"base_url"`
	// Model is handed to clients as ANTHROPIC_MODEL. It is not optional in
	// practice: Claude Code skips model discovery when its only credential
	// arrives in a custom header, which is exactly this arrangement. With one
	// model group configured it defaults to that group.
	Model string `yaml:"model"`
	// JWTAuth accepts the provider's own tokens on inference requests, as an
	// alternative to exchanging one for a gateway key at login.
	JWTAuth JWTAuthConfig `yaml:"jwt_auth"`
}

// JWTAuthConfig accepts an identity provider's own token on every request,
// instead of the virtual key an SSO login exchanges one for.
//
// The two arrangements answer the same question at different times, and the
// trade between them is revocation against moving parts. A virtual key is
// verified once at login and then trusted for its lifetime, so disabling
// someone at the provider does not reach the gateway until their key expires;
// their token, by contrast, is checked on every request, so a suspended account
// stops working as soon as its current token does — typically an hour. What it
// costs is that every caller must hold a live token and refresh it, which is
// ordinary for a service but is precisely what Claude Code cannot do while
// keeping a subscription login, since the only header it will carry a gateway
// credential in is a static one.
//
// So this is for the callers a key does not suit — CI, a service, a script run
// under a workload identity — and it sits beside the key path rather than
// replacing it. Both resolve to the same entitlements through the same roles,
// and both account to the same subject, so a person moving between them draws
// on one budget.
type JWTAuthConfig struct {
	// Enabled turns on token authentication. Off by default: accepting a
	// provider's tokens is a second way in, and one an operator should choose.
	Enabled bool `yaml:"enabled"`
	// Audiences are the "aud" values a token may carry. It defaults to the
	// gateway's own client id, which is what an ID token carries; an access
	// token issued for an API usually names that API instead, so a deployment
	// presenting access tokens lists its API identifier here.
	//
	// It is an allowlist with no wildcard for the reason the login path checks
	// the same claim: a token minted for another client of the same provider is
	// a valid token that says nothing about this gateway, and accepting one
	// would let any other application in the organisation authenticate here.
	Audiences []string `yaml:"audiences"`
	// CacheTTL is how long a verified token's result is reused before it is
	// verified again.
	//
	// Verification is a signature check against a cached key set, so this is an
	// optimization rather than a necessity — but it is on the path of every
	// request, and a caller sending a hundred a second would otherwise pay a
	// hundred RSA verifications a second for an answer that cannot change. It
	// never extends a token: an entry is dropped at the token's own expiry
	// whenever that comes first.
	//
	// A pointer, so an explicit 0 — verify every request, cache nothing — is
	// not mistaken for an omitted field. That is a configuration an operator
	// might genuinely want, and it is the one this feature exists to make
	// possible: the whole reason to check a token per request is that the
	// answer can change, and an operator entitled to decide the answer changes
	// faster than a minute should be able to say so.
	CacheTTL *time.Duration `yaml:"cache_ttl"`
}

// TTL returns how long a verified token is reused, or the default when unset.
func (j JWTAuthConfig) TTL() time.Duration {
	if j.CacheTTL == nil {
		return DefaultJWTAuthCacheTTL
	}
	return *j.CacheTTL
}

// Enabled reports whether SSO is configured. The endpoints are not registered
// when it is not, so an unconfigured gateway serves no OIDC surface at all.
func (s SSOConfig) Enabled() bool { return s.Issuer != "" }

// SSORole is what an identity gets. The fields mirror KeySpec, so the
// entitlements an SSO key carries are described the same way as those of a key
// declared in configuration.
type SSORole struct {
	// Match is compared against the values of RoleClaim. The first role that
	// matches wins, and "*" matches anything, so ordering is meaningful and a
	// catch-all belongs last.
	Match            string        `yaml:"match"`
	Models           []string      `yaml:"models"`
	RPMLimit         int           `yaml:"rpm_limit"`
	TPMLimit         int           `yaml:"tpm_limit"`
	AllowPassthrough bool          `yaml:"allow_passthrough"`
	MaxBudget        float64       `yaml:"max_budget"`
	BudgetDuration   time.Duration `yaml:"budget_duration"`
	// Scope places everyone matching this role inside a project, team or
	// organisation declared under `rbac`.
	//
	// It is what makes a role a pool rather than a template. Without it every
	// identity the role matches receives its own copy of MaxBudget, so a cap
	// written once is multiplied by the number of people it applies to; with
	// it, they share the scope's budget and the role's own MaxBudget becomes a
	// per-person cap *within* that pool.
	Scope string `yaml:"scope"`
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

	// StreamUsage asks an OpenAI-compatible upstream to report token usage on a
	// streamed reply, by adding stream_options.include_usage to requests that
	// did not set stream_options themselves.
	//
	// On by default, because without it such a reply carries no usage at all:
	// the request is recorded as zero input, zero output and zero cached
	// tokens, so it costs nothing, counts nothing against a budget or a rate
	// limit, and reports no prompt-cache savings. That is the failure this
	// gateway exists to make visible, and it is silent in every other respect.
	//
	// The cost of asking is one extra chunk at the end of the stream, carrying
	// the usage and an empty choices array. It is part of the OpenAI protocol
	// and the official clients expect it, but a hand-written client that indexes
	// choices[0] on every chunk will not, and a strict OpenAI-compatible server
	// may reject the field outright. Either is a reason to turn this off, at the
	// price of an unpriced deployment.
	//
	// It does not apply to the Anthropic format, which reports usage on
	// message_start whether or not it was asked to. A pointer, so an explicit
	// false is not mistaken for an omitted field.
	StreamUsage *bool `yaml:"stream_usage"`

	// OTLP pushes the same metrics /metrics serves to an OpenTelemetry
	// collector.
	OTLP OTLPConfig `yaml:"otlp"`
}

// OTLPConfig configures the OpenTelemetry metrics exporter.
//
// It is a push where Prometheus is a pull, and that difference is the reason to
// use it: a gateway that cannot be scraped — behind NAT, in a serverless
// runtime, one instance of an autoscaled group whose short-lived members are
// gone before the next scrape — has no way to publish a pull-based metric at
// all. The two are not alternatives, and both can be on: they render the same
// snapshot, so they cannot disagree.
type OTLPConfig struct {
	// Endpoint is the collector's base URL, e.g. http://localhost:4318. The
	// exporter appends /v1/metrics unless the endpoint already names a path.
	//
	// Empty disables the exporter, unless OTEL_EXPORTER_OTLP_METRICS_ENDPOINT
	// or OTEL_EXPORTER_OTLP_ENDPOINT is set: those are where the whole
	// OpenTelemetry ecosystem expects to find this, and a gateway that ignored
	// them would be the one component in a fleet needing its own configuration.
	Endpoint string `yaml:"endpoint"`

	// Protocol is "http/protobuf" (the default) or "http/json".
	//
	// Protobuf is what every collector accepts. JSON is the same payload in a
	// form you can read, which is worth having the first time a collector
	// rejects an export — but only some endpoints accept it.
	Protocol string `yaml:"protocol"`

	// Headers are sent on every export, typically an API key for a hosted
	// collector. Merged over anything parsed from OTEL_EXPORTER_OTLP_HEADERS,
	// with these winning.
	Headers map[string]string `yaml:"headers"`

	// Interval is how often metrics are pushed.
	//
	// Cumulative counters make this a resolution choice rather than a
	// correctness one: a missed export is made good by the next, since each
	// carries a running total rather than a delta since the last.
	Interval time.Duration `yaml:"interval"`

	// Timeout bounds one export attempt, and the flush on shutdown.
	Timeout time.Duration `yaml:"timeout"`

	// Compress gzips the payload. On by default: metric bodies are highly
	// repetitive text and compress by roughly an order of magnitude, which
	// matters on a per-GB ingest bill.
	Compress *bool `yaml:"compress"`

	// ServiceName becomes the service.name resource attribute, which is the
	// primary key almost every OTLP backend groups by. Defaults to
	// OTEL_SERVICE_NAME, then "ai-gateway".
	ServiceName string `yaml:"service_name"`

	// ResourceAttributes are added to every export — deployment.environment,
	// service.instance.id, a region. They describe the process rather than the
	// measurement, so they are sent once per export rather than repeated on
	// every data point.
	ResourceAttributes map[string]string `yaml:"resource_attributes"`
}

// Enabled reports whether the exporter should run.
func (o OTLPConfig) Enabled() bool { return o.Endpoint != "" }

// CompressEnabled reports whether to gzip, defaulting to true.
func (o OTLPConfig) CompressEnabled() bool { return o.Compress == nil || *o.Compress }

// StreamUsageEnabled reports whether streamed OpenAI-compatible requests should
// ask for usage.
func (o ObservabilityConfig) StreamUsageEnabled() bool {
	return o.StreamUsage == nil || *o.StreamUsage
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
