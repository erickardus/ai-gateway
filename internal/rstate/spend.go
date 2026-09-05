package rstate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/erickardus/ai-gateway/internal/spend"
	"github.com/redis/go-redis/v9"
)

// recordScript accumulates one request's usage into a subject's hash and sets
// the window's expiry on first write.
//
// It is a script for the same reason the rate-limit one is: several fields must
// move together, and the expiry must be established atomically with the first
// write or a crash in between leaves a budget that never resets.
const recordScript = `
local key = KEYS[1]
local ttl = tonumber(ARGV[1])

local created = redis.call('HSETNX', key, 'window_start', ARGV[2])
redis.call('HINCRBY', key, 'requests', ARGV[3])
redis.call('HINCRBY', key, 'input_tokens', ARGV[4])
redis.call('HINCRBY', key, 'output_tokens', ARGV[5])
redis.call('HINCRBY', key, 'cache_read_tokens', ARGV[6])
redis.call('HINCRBY', key, 'cache_write_tokens', ARGV[7])
redis.call('HINCRBY', key, 'billable_requests', ARGV[8])
redis.call('HINCRBYFLOAT', key, 'cost', ARGV[9])
if created == 1 and ttl > 0 then
  redis.call('EXPIRE', key, ttl)
end
return 1
`

// Ledger implements spend.Store against Redis so budgets hold across instances.
//
// A per-process ledger would let a key spend its whole allowance once per
// replica, which is the failure this exists to prevent. When Redis is
// unreachable it degrades to the local ledger — the same trade as routing state,
// and logged the same way.
type Ledger struct {
	client   *redis.Client
	local    *spend.Ledger
	prefix   string
	log      *slog.Logger
	timeout  time.Duration
	record   *redis.Script
	degraded *Store
}

// NewLedger builds a Redis-backed ledger sharing the store's connection.
func NewLedger(store *Store, local *spend.Ledger, prefix string, log *slog.Logger, timeout time.Duration) *Ledger {
	if timeout <= 0 {
		timeout = 250 * time.Millisecond
	}
	return &Ledger{
		client: store.Client(), local: local, prefix: prefix,
		log: log, timeout: timeout,
		record: redis.NewScript(recordScript), degraded: store,
	}
}

func (l *Ledger) key(kind, subject string) string {
	return l.prefix + ":spend:" + kind + ":" + subject
}

func (l *Ledger) ctx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, l.timeout)
}

// Record implements spend.Store.
func (l *Ledger) Record(ctx context.Context, e spend.Entry) error {
	// The local ledger is kept current regardless, so a later degradation has
	// something to fall back to and the /spend endpoints still answer.
	if err := l.local.Record(ctx, e); err != nil {
		return err
	}

	billable := 0
	if e.Billable {
		billable = 1
	}
	rctx, cancel := l.ctx(ctx)
	defer cancel()

	for kind, subject := range map[string]string{"key": e.KeyHash, "deployment": e.DeploymentID} {
		if subject == "" {
			continue
		}
		err := l.record.Run(rctx, l.client, []string{l.key(kind, subject)},
			0, // no TTL by default; the budget window governs expiry via KeySpend
			time.Now().Unix(),
			1, e.Usage.InputTokens, e.Usage.OutputTokens,
			e.Usage.CacheReadTokens, e.Usage.CacheWriteTokens,
			billable, strconv.FormatFloat(e.Cost, 'f', -1, 64),
		).Err()
		if l.degraded.degrade(err, "spend_record") {
			return nil // already recorded locally
		}
	}
	if e.KeyAlias != "" && e.KeyHash != "" {
		_ = l.client.HSet(rctx, l.key("key", e.KeyHash), "alias", e.KeyAlias).Err()
	}
	l.degraded.recovered()
	return nil
}

// KeySpend implements spend.Store.
//
// The window is enforced by comparing the stored start against now and clearing
// the record when it has elapsed, rather than by a Redis TTL: the budget window
// starts at a key's first request, and a TTL set at that moment would expire
// mid-window if the key went briefly idle.
func (l *Ledger) KeySpend(ctx context.Context, keyHash string, window time.Duration) (float64, error) {
	rctx, cancel := l.ctx(ctx)
	defer cancel()
	key := l.key("key", keyHash)

	vals, err := l.client.HMGet(rctx, key, "cost", "window_start").Result()
	if l.degraded.degrade(err, "key_spend") {
		return l.local.KeySpend(ctx, keyHash, window)
	}
	l.degraded.recovered()

	cost := parseFloat(vals[0])
	if window > 0 {
		started := int64(parseFloat(vals[1]))
		if started > 0 && time.Since(time.Unix(started, 0)) >= window {
			// The window elapsed: clear the record so the reported spend
			// matches what is enforced from here on.
			if err := l.client.Del(rctx, key).Err(); err != nil {
				l.log.Warn("reset spend window", "error", err)
			}
			return 0, nil
		}
	}
	return cost, nil
}

// Keys implements spend.Store.
func (l *Ledger) Keys(ctx context.Context) ([]spend.Summary, error) {
	return l.summaries(ctx, "key", func() ([]spend.Summary, error) { return l.local.Keys(ctx) })
}

// Deployments implements spend.Store.
func (l *Ledger) Deployments(ctx context.Context) ([]spend.Summary, error) {
	return l.summaries(ctx, "deployment", func() ([]spend.Summary, error) { return l.local.Deployments(ctx) })
}

// summaries reads every subject of a kind. It scans rather than keeping an
// index: these endpoints are for operators, not the request path, and an index
// would be one more thing to keep consistent.
func (l *Ledger) summaries(ctx context.Context, kind string, fallback func() ([]spend.Summary, error)) ([]spend.Summary, error) {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	pattern := l.prefix + ":spend:" + kind + ":*"
	var out []spend.Summary
	iter := l.client.Scan(rctx, 0, pattern, 100).Iterator()
	for iter.Next(rctx) {
		key := iter.Val()
		fields, err := l.client.HGetAll(rctx, key).Result()
		if err != nil {
			continue
		}
		out = append(out, summaryFrom(key[len(pattern)-1:], fields))
	}
	if err := iter.Err(); err != nil {
		if l.degraded.degrade(err, "spend_scan") {
			return fallback()
		}
		return nil, fmt.Errorf("scan spend records: %w", err)
	}
	l.degraded.recovered()
	return out, nil
}

func summaryFrom(subject string, fields map[string]string) spend.Summary {
	s := spend.Summary{Subject: subject, Alias: fields["alias"]}
	s.Requests = parseInt(fields["requests"])
	s.InputTokens = parseInt(fields["input_tokens"])
	s.OutputTokens = parseInt(fields["output_tokens"])
	s.CacheReadTokens = parseInt(fields["cache_read_tokens"])
	s.CacheWriteTokens = parseInt(fields["cache_write_tokens"])
	s.BillableRequests = parseInt(fields["billable_requests"])
	if v, err := strconv.ParseFloat(fields["cost"], 64); err == nil {
		s.Cost = v
	}
	if started := parseInt(fields["window_start"]); started > 0 {
		s.WindowStart = time.Unix(int64(started), 0).UTC()
	}
	return s
}

// Forget drops a revoked key's record from both stores.
func (l *Ledger) Forget(keyHash string) {
	l.local.Forget(keyHash)
	ctx, cancel := context.WithTimeout(context.Background(), l.timeout)
	defer cancel()
	if err := l.client.Del(ctx, l.key("key", keyHash)).Err(); err != nil {
		l.log.Warn("drop spend record for revoked key", "error", err)
	}
}

// Flush persists the local mirror, so a restart with Redis down still has a
// recent picture.
func (l *Ledger) Flush() error { return l.local.Flush() }

func parseInt(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

func parseFloat(v any) float64 {
	s, ok := v.(string)
	if !ok {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

var (
	_ spend.Store = (*Ledger)(nil)
	_             = errors.Is
)
