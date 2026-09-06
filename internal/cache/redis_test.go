package cache

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/redis/go-redis/v9"
)

// The shared response cache is what makes a hit on one instance a hit on every
// instance. It is also the one place a stored response crosses a process
// boundary, so what it must prove is that an entry survives the round trip
// intact — a body that came back short or a status that came back zero would be
// served to a caller as the real answer.

var cacheRedisAddr string

// TestMain runs a real redis-server, for the reason the rstate tests do: what is
// under test is serialization against an actual server and its expiry
// semantics, which is exactly what a mock reimplements approximately.
func TestMain(m *testing.M) {
	bin, err := exec.LookPath("redis-server")
	if err != nil {
		fmt.Fprintln(os.Stderr, "redis-server not found; skipping shared cache integration tests")
		os.Exit(m.Run())
	}

	const port = "6398"
	cmd := exec.Command(bin, "--port", port, "--save", "", "--appendonly", "no")
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "could not start redis-server: %v\n", err)
		os.Exit(m.Run())
	}
	addr := "127.0.0.1:" + port

	ready := false
	for range 100 {
		c := redis.NewClient(&redis.Options{Addr: addr})
		if c.Ping(context.Background()).Err() == nil {
			c.Close()
			ready = true
			break
		}
		c.Close()
		time.Sleep(50 * time.Millisecond)
	}
	if ready {
		cacheRedisAddr = addr
	}

	code := m.Run()
	cmd.Process.Kill()
	cmd.Wait()
	os.Exit(code)
}

func sharedCache(t *testing.T, prefix string) *Redis {
	t.Helper()
	if cacheRedisAddr == "" {
		t.Skip("redis-server unavailable")
	}
	client := redis.NewClient(&redis.Options{Addr: cacheRedisAddr})
	t.Cleanup(func() { client.Close() })
	return NewRedis(client, NewMemory(16), prefix, discardLog(), time.Second)
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func storedEntry() *Entry {
	return &Entry{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   []byte(`{"id":"msg_1","content":[{"type":"text","text":"hello"}]}`),
		Usage: core.Usage{
			InputTokens: 11, OutputTokens: 22,
			CacheReadTokens: 33, CacheWriteTokens: 44, CacheWrite1hTokens: 44,
		},
		Streaming: true,
		StoredAt:  time.Now().UTC().Truncate(time.Second),
	}
}

// Every field of a stored response has to survive Redis, because every one of
// them is replayed to a caller or reported as their usage.
func TestSharedCacheRoundTripsAnEntryWhole(t *testing.T) {
	c := sharedCache(t, uniqueCachePrefix(t))
	ctx := context.Background()
	want := storedEntry()

	if err := c.Put(ctx, "k1", want, time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Read through a second cache object, so the answer comes from Redis rather
	// than from the local copy Put also keeps.
	reader := sharedCache(t, prefixOfCache(c))
	got, hit, err := reader.Get(ctx, "k1")
	if err != nil || !hit {
		t.Fatalf("Get: hit=%v err=%v", hit, err)
	}
	if got.Status != want.Status {
		t.Errorf("status = %d, want %d", got.Status, want.Status)
	}
	if string(got.Body) != string(want.Body) {
		t.Errorf("body = %q, want %q", got.Body, want.Body)
	}
	if got.Header.Get("Content-Type") != "application/json" {
		t.Errorf("header = %v", got.Header)
	}
	if got.Usage != want.Usage {
		t.Errorf("usage = %+v, want %+v", got.Usage, want.Usage)
	}
	if !got.Streaming {
		t.Error("streaming flag lost; the body would be written whole instead of replayed")
	}
	if !got.StoredAt.Equal(want.StoredAt) {
		t.Errorf("stored_at = %v, want %v", got.StoredAt, want.StoredAt)
	}
}

func TestSharedCacheMissIsNotAnError(t *testing.T) {
	c := sharedCache(t, uniqueCachePrefix(t))
	got, hit, err := c.Get(context.Background(), "never-stored")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if hit || got != nil {
		t.Errorf("got %+v, hit=%v; want a clean miss", got, hit)
	}
}

func TestSharedCacheHonoursTTL(t *testing.T) {
	c := sharedCache(t, uniqueCachePrefix(t))
	ctx := context.Background()

	if err := c.Put(ctx, "k1", storedEntry(), 0); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, hit, _ := c.Get(ctx, "k1"); hit {
		t.Error("a zero TTL stored an entry; nothing should be cached without a lifetime")
	}

	if err := c.Put(ctx, "k2", storedEntry(), 50*time.Millisecond); err != nil {
		t.Fatalf("Put: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	// The local copy expires on the same schedule, so a miss here is a miss
	// everywhere rather than a fallback hit.
	if _, hit, _ := c.Get(ctx, "k2"); hit {
		t.Error("an entry outlived its TTL")
	}
}

// Purge is what an operator reaches for after shipping a bad prompt, so it has
// to clear the shared copy rather than only the local one.
func TestSharedCachePurgeClearsRedis(t *testing.T) {
	prefix := uniqueCachePrefix(t)
	c := sharedCache(t, prefix)
	ctx := context.Background()

	for _, key := range []string{"a", "b", "c"} {
		if err := c.Put(ctx, key, storedEntry(), time.Minute); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := c.Purge(ctx); err != nil {
		t.Fatalf("Purge: %v", err)
	}

	reader := sharedCache(t, prefix)
	for _, key := range []string{"a", "b", "c"} {
		if _, hit, _ := reader.Get(ctx, key); hit {
			t.Errorf("%q survived a purge", key)
		}
	}
}

// A neighbouring prefix is a different gateway. Purging one must not empty the
// other, and neither may read the other's entries.
func TestSharedCacheKeepsPrefixesApart(t *testing.T) {
	ctx := context.Background()
	mine := sharedCache(t, uniqueCachePrefix(t)+"-mine")
	theirs := sharedCache(t, uniqueCachePrefix(t)+"-theirs")

	if err := theirs.Put(ctx, "k1", storedEntry(), time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := mine.Purge(ctx); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if _, hit, _ := theirs.Get(ctx, "k1"); !hit {
		t.Error("purging one gateway's cache emptied another's")
	}
}

// A corrupt entry is a miss, never a failed request: the caller gets a fresh
// answer from the upstream and the bad bytes are dropped.
func TestSharedCacheTreatsCorruptionAsAMiss(t *testing.T) {
	prefix := uniqueCachePrefix(t)
	c := sharedCache(t, prefix)
	ctx := context.Background()

	client := redis.NewClient(&redis.Options{Addr: cacheRedisAddr})
	defer client.Close()
	if err := client.Set(ctx, prefix+":cache:k1", "not json at all", time.Minute).Err(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, hit, err := c.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("Get returned an error for a corrupt entry: %v", err)
	}
	if hit || got != nil {
		t.Errorf("corrupt entry served as a hit: %+v", got)
	}
	if n, _ := client.Exists(ctx, prefix+":cache:k1").Result(); n != 0 {
		t.Error("the corrupt entry was left in place to be re-read on every request")
	}
}

// With Redis unreachable the cache still answers from the local copy Put keeps,
// which is the degradation the rest of the gateway makes too: losing a cache
// must never become losing a request.
func TestSharedCacheFallsBackToLocalWhenRedisIsDown(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer client.Close()
	c := NewRedis(client, NewMemory(16), "test", discardLog(), 100*time.Millisecond)
	ctx := context.Background()

	if err := c.Put(ctx, "k1", storedEntry(), time.Minute); err != nil {
		t.Fatalf("Put returned an error with Redis down: %v", err)
	}
	got, hit, err := c.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !hit {
		t.Fatal("no local fallback; a Redis outage would empty every cache at once")
	}
	if string(got.Body) != string(storedEntry().Body) {
		t.Errorf("body = %q from the local fallback", got.Body)
	}
}

func uniqueCachePrefix(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("cachetest:%s:%d", t.Name(), time.Now().UnixNano())
}

// prefixOfCache reads back the prefix a cache was built with, so a test can open
// a second view of the same keyspace.
func prefixOfCache(c *Redis) string { return c.prefix }
