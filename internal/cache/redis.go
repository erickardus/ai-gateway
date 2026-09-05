package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis stores entries in Redis so a cache hit on one instance serves every
// instance. It falls back to a local cache when Redis is unreachable, matching
// how the rest of the gateway degrades: a cache is an optimization, and losing
// it must never turn into losing the request.
type Redis struct {
	client  *redis.Client
	local   *Memory
	prefix  string
	log     *slog.Logger
	timeout time.Duration
}

// NewRedis builds a Redis-backed cache with a local fallback.
func NewRedis(client *redis.Client, local *Memory, prefix string, log *slog.Logger, timeout time.Duration) *Redis {
	if timeout <= 0 {
		timeout = 250 * time.Millisecond
	}
	return &Redis{client: client, local: local, prefix: prefix, log: log, timeout: timeout}
}

func (r *Redis) redisKey(key string) string { return r.prefix + ":cache:" + key }

// Get implements Cache.
func (r *Redis) Get(ctx context.Context, key string) (*Entry, bool, error) {
	rctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	raw, err := r.client.Get(rctx, r.redisKey(key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		r.log.Debug("cache read failed, falling back to local", "error", err)
		return r.local.Get(ctx, key)
	}

	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		// A corrupt entry is a miss, not a failure: drop it and let the request
		// proceed to the upstream.
		r.log.Warn("discarding corrupt cache entry", "error", err)
		_ = r.client.Del(rctx, r.redisKey(key)).Err()
		return nil, false, nil
	}
	return &e, true, nil
}

// Put implements Cache.
func (r *Redis) Put(ctx context.Context, key string, e *Entry, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	// Keep the local copy current too, so a later degradation still has a warm
	// cache to serve from.
	_ = r.local.Put(ctx, key, e, ttl)

	raw, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode cache entry: %w", err)
	}
	rctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	if err := r.client.Set(rctx, r.redisKey(key), raw, ttl).Err(); err != nil {
		// A cache write failing is not a request failure.
		r.log.Debug("cache write failed", "error", err)
	}
	return nil
}

// Purge implements Cache.
func (r *Redis) Purge(ctx context.Context) error {
	_ = r.local.Purge(ctx)

	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var cursor uint64
	pattern := r.prefix + ":cache:*"
	for {
		keys, next, err := r.client.Scan(rctx, cursor, pattern, 500).Result()
		if err != nil {
			return fmt.Errorf("scan cache keys: %w", err)
		}
		if len(keys) > 0 {
			if err := r.client.Del(rctx, keys...).Err(); err != nil {
				return fmt.Errorf("delete cache keys: %w", err)
			}
		}
		if next == 0 {
			return nil
		}
		cursor = next
	}
}
