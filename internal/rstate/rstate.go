// Package rstate shares routing state across gateway instances via Redis.
//
// Not all state belongs here, and the split is deliberate.
//
// Rate limits, cooldowns and spend MUST be global: a limit enforced per process
// is silently multiplied by the replica count, and a budget enforced per process
// lets a key spend its allowance once per instance. Those live in Redis.
//
// Latency and in-flight counts stay local. Latency measures this instance's own
// network path to an upstream, so blending measurements taken from different
// network positions would make the signal worse, not better. In-flight is a view
// of load this instance is actually carrying, and a global counter would leak
// permanently whenever an instance died mid-request.
package rstate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/erickardus/ai-gateway/internal/router"
	"github.com/redis/go-redis/v9"
)

// reserveScript performs the check and the increment together, and sets the
// window's expiry on first use.
//
// It has to be a script. INCR followed by EXPIRE is two round trips, and an
// instance that dies between them leaves a counter with no TTL — a deployment
// permanently at its limit, recoverable only by hand.
const reserveScript = `
local rpmKey, tpmKey = KEYS[1], KEYS[2]
local rpm, tpm, ttl = tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3])

if rpm > 0 then
  local used = tonumber(redis.call('GET', rpmKey) or '0')
  if used >= rpm then return 0 end
end
if tpm > 0 then
  local used = tonumber(redis.call('GET', tpmKey) or '0')
  if used >= tpm then return 0 end
end

local n = redis.call('INCR', rpmKey)
if n == 1 then redis.call('EXPIRE', rpmKey, ttl) end
return 1
`

// reserveAllScript reserves across several subjects at once, all or nothing.
//
// It exists for the entitlement chain: a request is charged against its key and
// every scope above it, and doing that as a sequence of reserveScript calls is
// both a round trip per level and non-atomic — a request refused by an outer
// scope would keep the increment it had already made to an inner one, so a
// key's own window would run ahead of the requests it actually served.
//
// KEYS holds an rpm/tpm pair per subject; ARGV an rpm/tpm pair plus the shared
// TTL last. Every limit is checked before any counter moves, so the script
// either admits the request everywhere or nowhere. It returns the 1-based index
// of the subject that refused, or 0 when all fit — 0 rather than -1 because a
// Lua number returned to Redis is what the client reads back as an integer, and
// the sign costs a branch on both sides.
const reserveAllScript = `
local ttl = tonumber(ARGV[#ARGV])
local n = #KEYS / 2

for i = 1, n do
  local rpm = tonumber(ARGV[i * 2 - 1])
  local tpm = tonumber(ARGV[i * 2])
  if rpm > 0 then
    local used = tonumber(redis.call('GET', KEYS[i * 2 - 1]) or '0')
    if used >= rpm then return i end
  end
  if tpm > 0 then
    local used = tonumber(redis.call('GET', KEYS[i * 2]) or '0')
    if used >= tpm then return i end
  end
end

for i = 1, n do
  local c = redis.call('INCR', KEYS[i * 2 - 1])
  if c == 1 then redis.call('EXPIRE', KEYS[i * 2 - 1], ttl) end
end
return 0
`

// allowScript is the non-consuming check used while filtering candidates.
const allowScript = `
local rpmKey, tpmKey = KEYS[1], KEYS[2]
local rpm, tpm = tonumber(ARGV[1]), tonumber(ARGV[2])
if rpm > 0 and tonumber(redis.call('GET', rpmKey) or '0') >= rpm then return 0 end
if tpm > 0 and tonumber(redis.call('GET', tpmKey) or '0') >= tpm then return 0 end
return 1
`

// addTokensScript increments the token counter and sets its expiry on first use,
// for the same reason reserveScript is a script.
const addTokensScript = `
local key, tokens, ttl = KEYS[1], tonumber(ARGV[1]), tonumber(ARGV[2])
local n = redis.call('INCRBY', key, tokens)
if n == tokens then redis.call('EXPIRE', key, ttl) end
return n
`

// failureScript counts a failure and ejects the deployment once it exceeds the
// allowance, all in one atomic step.
const failureScript = `
local failKey, coolKey = KEYS[1], KEYS[2]
local allowed, period = tonumber(ARGV[1]), tonumber(ARGV[2])

local n = redis.call('INCR', failKey)
if n == 1 then redis.call('EXPIRE', failKey, period) end
if n > allowed then
  redis.call('SET', coolKey, '1', 'EX', period)
  redis.call('DEL', failKey)
  return 1
end
return 0
`

// windowSeconds is the rate-limit window, matching the local limiter's.
const windowSeconds = 60

// Options configures the store.
type Options struct {
	Addr      string
	Username  string
	Password  string
	DB        int
	KeyPrefix string
	// Timeout bounds each Redis call. It is deliberately short: a slow Redis
	// must degrade the gateway to local state, not add its latency to every
	// request.
	Timeout time.Duration
}

// Store implements router.StateStore against Redis, falling back to a local
// store when Redis is unreachable.
//
// The fallback is degradation, not failure. A gateway that stops serving because
// Redis blipped is worse than one whose limits are briefly enforced per
// instance. But degradation must be visible, so every transition is logged and
// counted rather than passing silently.
type Store struct {
	client  *redis.Client
	local   *router.MemState
	prefix  string
	log     *slog.Logger
	timeout time.Duration

	scripts struct {
		reserve, reserveAll, allow, addTokens, failure *redis.Script
	}

	// degraded is set while Redis is unreachable, so the transition back is
	// logged once rather than on every request.
	degraded atomic.Bool
	// degradations counts how many times the store fell back, for metrics.
	degradations atomic.Uint64
}

// New builds a Redis-backed store. It does not connect eagerly: an unavailable
// Redis at startup degrades to local state rather than preventing the gateway
// from serving.
func New(opts Options, local *router.MemState, log *slog.Logger) *Store {
	if opts.Timeout <= 0 {
		opts.Timeout = 250 * time.Millisecond
	}
	s := &Store{
		client: redis.NewClient(&redis.Options{
			Addr:         opts.Addr,
			Username:     opts.Username,
			Password:     opts.Password,
			DB:           opts.DB,
			DialTimeout:  opts.Timeout,
			ReadTimeout:  opts.Timeout,
			WriteTimeout: opts.Timeout,
		}),
		local:   local,
		prefix:  opts.KeyPrefix,
		log:     log,
		timeout: opts.Timeout,
	}
	s.scripts.reserve = redis.NewScript(reserveScript)
	s.scripts.reserveAll = redis.NewScript(reserveAllScript)
	s.scripts.allow = redis.NewScript(allowScript)
	s.scripts.addTokens = redis.NewScript(addTokensScript)
	s.scripts.failure = redis.NewScript(failureScript)
	return s
}

// Client exposes the underlying connection so the spend ledger can share it.
func (s *Store) Client() *redis.Client { return s.client }

// Close releases the connection pool.
func (s *Store) Close() error { return s.client.Close() }

// Degradations reports how many times the store fell back to local state.
func (s *Store) Degradations() uint64 { return s.degradations.Load() }

// Ping reports whether Redis is reachable, for the readiness probe.
func (s *Store) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return s.client.Ping(ctx).Err()
}

func (s *Store) key(parts ...string) string {
	out := s.prefix
	for _, p := range parts {
		out += ":" + p
	}
	return out
}

// windowKey returns a key scoped to the current minute, so counters expire
// naturally instead of needing to be reset.
func (s *Store) windowKey(kind, id string) string {
	bucket := time.Now().Unix() / windowSeconds
	return s.key(kind, id, fmt.Sprint(bucket))
}

// degrade records a Redis failure and reports whether to fall back. A context
// cancelled by the caller is not a Redis problem and is not counted.
func (s *Store) degrade(err error, op string) bool {
	if err == nil || errors.Is(err, redis.Nil) {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	s.degradations.Add(1)
	if s.degraded.CompareAndSwap(false, true) {
		s.log.Error("redis unavailable, falling back to per-instance state; limits and budgets are enforced per replica until it recovers",
			"op", op, "error", err)
	}
	return true
}

// recovered notes a successful call after a period of degradation.
func (s *Store) recovered() {
	if s.degraded.CompareAndSwap(true, false) {
		s.log.Info("redis reachable again; state is shared across instances once more")
	}
}

func (s *Store) ctx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, s.timeout)
}

// Allow implements router.StateStore.
func (s *Store) Allow(ctx context.Context, id string, rpm, tpm int) (bool, error) {
	if rpm <= 0 && tpm <= 0 {
		return true, nil
	}
	rctx, cancel := s.ctx(ctx)
	defer cancel()

	res, err := s.scripts.allow.Run(rctx, s.client,
		[]string{s.windowKey("rpm", id), s.windowKey("tpm", id)},
		rpm, tpm).Int()
	if s.degrade(err, "allow") {
		return s.local.Allow(ctx, id, rpm, tpm)
	}
	s.recovered()
	return res == 1, nil
}

// Reserve implements router.StateStore.
func (s *Store) Reserve(ctx context.Context, id string, rpm, tpm int) (bool, error) {
	if rpm <= 0 && tpm <= 0 {
		return true, nil
	}
	rctx, cancel := s.ctx(ctx)
	defer cancel()

	res, err := s.scripts.reserve.Run(rctx, s.client,
		[]string{s.windowKey("rpm", id), s.windowKey("tpm", id)},
		rpm, tpm, windowSeconds).Int()
	if s.degrade(err, "reserve") {
		return s.local.Reserve(ctx, id, rpm, tpm)
	}
	s.recovered()
	return res == 1, nil
}

// AddTokens implements router.StateStore.
func (s *Store) AddTokens(ctx context.Context, id string, tokens int) error {
	if tokens <= 0 {
		return nil
	}
	rctx, cancel := s.ctx(ctx)
	defer cancel()

	err := s.scripts.addTokens.Run(rctx, s.client,
		[]string{s.windowKey("tpm", id)}, tokens, windowSeconds).Err()
	if s.degrade(err, "add_tokens") {
		return s.local.AddTokens(ctx, id, tokens)
	}
	s.recovered()
	return nil
}

// TokensUsed implements router.StateStore.
func (s *Store) TokensUsed(ctx context.Context, id string) (int, error) {
	rctx, cancel := s.ctx(ctx)
	defer cancel()

	n, err := s.client.Get(rctx, s.windowKey("tpm", id)).Int()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if s.degrade(err, "tokens_used") {
		return s.local.TokensUsed(ctx, id)
	}
	s.recovered()
	return n, nil
}

// Affinity implements router.StateStore. Prompt-prefix pins are shared, unlike
// latency and in-flight: the point of a pin is that every replica sends a
// conversation to the deployment holding its warm prompt cache, and a
// per-instance pin behind a load balancer would send the same conversation to a
// different upstream on every turn.
//
// Falling back to the local store while Redis is down usually means no pin at
// all, since pins are only written to Redis while it is healthy. That degrades
// to ordinary load balancing — more cache writes, no incorrect routing — which
// is the right way to lose this particular feature.
func (s *Store) Affinity(ctx context.Context, fingerprint string) (string, bool, error) {
	rctx, cancel := s.ctx(ctx)
	defer cancel()

	id, err := s.client.Get(rctx, s.key("affinity", fingerprint)).Result()
	if errors.Is(err, redis.Nil) {
		s.recovered()
		return "", false, nil
	}
	if s.degrade(err, "affinity") {
		return s.local.Affinity(ctx, fingerprint)
	}
	s.recovered()
	return id, id != "", nil
}

// SetAffinity implements router.StateStore. The TTL is refreshed on every
// success, so a pin lives as long as the conversation is active and lapses
// once it stops — which is how the upstream's own cache entry behaves.
func (s *Store) SetAffinity(ctx context.Context, fingerprint, id string, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	rctx, cancel := s.ctx(ctx)
	defer cancel()

	err := s.client.Set(rctx, s.key("affinity", fingerprint), id, ttl).Err()
	if s.degrade(err, "set_affinity") {
		return s.local.SetAffinity(ctx, fingerprint, id, ttl)
	}
	s.recovered()
	return nil
}

// InCooldown implements router.StateStore.
func (s *Store) InCooldown(ctx context.Context, id string, now time.Time) (bool, error) {
	rctx, cancel := s.ctx(ctx)
	defer cancel()

	n, err := s.client.Exists(rctx, s.key("cooldown", id)).Result()
	if s.degrade(err, "in_cooldown") {
		return s.local.InCooldown(ctx, id, now)
	}
	s.recovered()
	return n > 0, nil
}

// RecordFailure implements router.StateStore.
func (s *Store) RecordFailure(ctx context.Context, id string, now time.Time, allowedFails int, period time.Duration) error {
	rctx, cancel := s.ctx(ctx)
	defer cancel()

	ejected, err := s.scripts.failure.Run(rctx, s.client,
		[]string{s.key("fails", id), s.key("cooldown", id)},
		allowedFails, int(period.Seconds())).Int()
	if s.degrade(err, "record_failure") {
		return s.local.RecordFailure(ctx, id, now, allowedFails, period)
	}
	s.recovered()
	if ejected == 1 {
		s.log.Warn("deployment ejected across all instances", "deployment", id, "period", period)
	}
	return nil
}

// BeginRequest implements router.StateStore. In-flight is tracked locally: it
// describes the load this instance is carrying, and a shared counter would leak
// permanently if an instance died mid-request.
func (s *Store) BeginRequest(ctx context.Context, id string) error {
	return s.local.BeginRequest(ctx, id)
}

// EndRequest implements router.StateStore.
func (s *Store) EndRequest(ctx context.Context, id string, took time.Duration, ok bool) error {
	return s.local.EndRequest(ctx, id, took, ok)
}

// InFlight implements router.StateStore.
func (s *Store) InFlight(ctx context.Context, id string) (int, error) {
	return s.local.InFlight(ctx, id)
}

// MeanLatency implements router.StateStore. Latency stays local because it
// measures this instance's own path to the upstream.
func (s *Store) MeanLatency(ctx context.Context, id string) (time.Duration, bool, error) {
	return s.local.MeanLatency(ctx, id)
}

// Prepare pre-creates local state for known deployments.
func (s *Store) Prepare(ids []string) { s.local.Prepare(ids) }

var _ router.StateStore = (*Store)(nil)
