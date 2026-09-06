package rstate

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/router"
	"github.com/erickardus/ai-gateway/internal/spend"
)

var redisAddr string

// TestMain starts a real redis-server rather than a mock. The behaviour under
// test is largely Lua atomicity and expiry semantics, which is exactly what a
// mock reimplements approximately and where a divergence would hide a bug.
func TestMain(m *testing.M) {
	bin, err := exec.LookPath("redis-server")
	if err != nil {
		fmt.Fprintln(os.Stderr, "redis-server not found; skipping rstate integration tests")
		os.Exit(0)
	}

	port := "6399"
	cmd := exec.Command(bin, "--port", port, "--save", "", "--appendonly", "no")
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "could not start redis-server: %v\n", err)
		os.Exit(0)
	}
	redisAddr = "127.0.0.1:" + port

	// Wait for it to accept connections.
	ready := false
	for range 100 {
		s := newStore(nil, "probe")
		if s.Ping(context.Background()) == nil {
			s.Close()
			ready = true
			break
		}
		s.Close()
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		cmd.Process.Kill()
		fmt.Fprintln(os.Stderr, "redis-server did not become ready")
		os.Exit(0)
	}

	code := m.Run()
	cmd.Process.Kill()
	cmd.Wait()
	os.Exit(code)
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func newStore(t *testing.T, prefix string) *Store {
	s := New(Options{Addr: redisAddr, KeyPrefix: prefix, Timeout: 2 * time.Second}, router.NewMemState(), discard())
	if t != nil {
		t.Cleanup(func() { s.Close() })
	}
	return s
}

// uniquePrefix keeps tests from colliding in the shared server.
func uniquePrefix(t *testing.T) string {
	return fmt.Sprintf("test:%s:%d", t.Name(), time.Now().UnixNano())
}

// The whole point: two instances must share one budget of requests.
func TestRateLimitSharedAcrossInstances(t *testing.T) {
	prefix := uniquePrefix(t)
	a, b := newStore(t, prefix), newStore(t, prefix)
	ctx := context.Background()

	const rpm = 10
	admitted := 0
	for i := range 20 {
		store := a
		if i%2 == 1 {
			store = b
		}
		ok, err := store.Reserve(ctx, "dep-1", rpm, 0)
		if err != nil {
			t.Fatalf("Reserve: %v", err)
		}
		if ok {
			admitted++
		}
	}
	if admitted != rpm {
		t.Errorf("admitted %d of 20 across two instances, want exactly %d: the limit is not shared", admitted, rpm)
	}
}

// Allow must not consume budget, even across instances.
func TestAllowDoesNotConsumeAcrossInstances(t *testing.T) {
	prefix := uniquePrefix(t)
	a, b := newStore(t, prefix), newStore(t, prefix)
	ctx := context.Background()

	for range 50 {
		if ok, err := a.Allow(ctx, "dep-1", 5, 0); err != nil || !ok {
			t.Fatalf("Allow = %v, %v", ok, err)
		}
	}
	admitted := 0
	for range 10 {
		if ok, _ := b.Reserve(ctx, "dep-1", 5, 0); ok {
			admitted++
		}
	}
	if admitted != 5 {
		t.Errorf("admitted %d, want 5: Allow consumed budget", admitted)
	}
}

// A cooldown set by one instance must eject the deployment everywhere.
func TestCooldownSharedAcrossInstances(t *testing.T) {
	prefix := uniquePrefix(t)
	a, b := newStore(t, prefix), newStore(t, prefix)
	ctx, now := context.Background(), time.Now()

	const allowed = 2
	for range allowed + 1 {
		if err := a.RecordFailure(ctx, "dep-1", now, allowed, 5*time.Second); err != nil {
			t.Fatalf("RecordFailure: %v", err)
		}
	}

	cooling, err := b.InCooldown(ctx, "dep-1", now)
	if err != nil {
		t.Fatalf("InCooldown: %v", err)
	}
	if !cooling {
		t.Error("the second instance does not see the ejection")
	}
}

// The counter must carry a TTL, or a crash between INCR and EXPIRE would leave
// a deployment permanently at its limit.
func TestRateLimitKeyExpires(t *testing.T) {
	prefix := uniquePrefix(t)
	s := newStore(t, prefix)
	ctx := context.Background()

	if _, err := s.Reserve(ctx, "dep-ttl", 5, 0); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	ttl, err := s.Client().TTL(ctx, s.windowKey("rpm", "dep-ttl")).Result()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if ttl <= 0 || ttl > windowSeconds*time.Second {
		t.Errorf("TTL = %v, want a positive value no greater than the window", ttl)
	}
}

// Reserve must be atomic: concurrent callers cannot both take the last slot.
func TestReserveIsAtomicUnderConcurrency(t *testing.T) {
	prefix := uniquePrefix(t)
	ctx := context.Background()
	const limit = 25
	stores := []*Store{newStore(t, prefix), newStore(t, prefix), newStore(t, prefix)}

	results := make(chan bool, 300)
	for i := range 300 {
		go func(i int) {
			ok, _ := stores[i%len(stores)].Reserve(ctx, "dep-race", limit, 0)
			results <- ok
		}(i)
	}
	admitted := 0
	for range 300 {
		if <-results {
			admitted++
		}
	}
	if admitted != limit {
		t.Errorf("admitted %d of 300 concurrent requests, want exactly %d", admitted, limit)
	}
}

// Budgets must hold across instances, or a key spends its allowance once per
// replica — the failure this whole package exists to prevent.
func TestBudgetSharedAcrossInstances(t *testing.T) {
	prefix := uniquePrefix(t)
	ctx := context.Background()
	sa, sb := newStore(t, prefix), newStore(t, prefix)
	la := NewLedger(sa, spend.New(), prefix, discard(), 2*time.Second)
	lb := NewLedger(sb, spend.New(), prefix, discard(), 2*time.Second)

	entry := spend.Entry{
		KeyHash: "k1", KeyAlias: "dev", DeploymentID: "dep-1",
		Usage: core.Usage{InputTokens: 100, OutputTokens: 50}, Cost: 1.25, Billable: true,
	}
	for range 4 {
		if err := la.Record(ctx, entry); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	spent, err := lb.KeySpend(ctx, "k1", time.Hour)
	if err != nil {
		t.Fatalf("KeySpend: %v", err)
	}
	if spent < 4.99 || spent > 5.01 {
		t.Errorf("second instance sees %v spent, want ~5.00", spent)
	}
}

func TestSpendSummariesFromRedis(t *testing.T) {
	prefix := uniquePrefix(t)
	ctx := context.Background()
	s := newStore(t, prefix)
	l := NewLedger(s, spend.New(), prefix, discard(), 2*time.Second)

	l.Record(ctx, spend.Entry{
		KeyHash: "k1", KeyAlias: "dev-laptop", DeploymentID: "dep-1",
		Usage:        core.Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 100},
		Cost:         0.5,
		CacheSavings: 0.27,
		Billable:     true,
	})
	// Passthrough traffic: usage but no cost.
	l.Record(ctx, spend.Entry{
		KeyHash: "k1", DeploymentID: "dep-2",
		Usage: core.Usage{InputTokens: 7}, Cost: 0, Billable: false,
	})

	keys, err := l.Keys(ctx)
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d key summaries, want 1", len(keys))
	}
	got := keys[0]
	if got.Requests != 2 || got.BillableRequests != 1 {
		t.Errorf("requests = %d/%d, want 2/1", got.Requests, got.BillableRequests)
	}
	if got.InputTokens != 17 || got.CacheReadTokens != 100 {
		t.Errorf("tokens = %d input, %d cache read; want 17 and 100", got.InputTokens, got.CacheReadTokens)
	}
	if got.CacheSavings < 0.269 || got.CacheSavings > 0.271 {
		t.Errorf("cache_savings = %v, want ~0.27", got.CacheSavings)
	}
	if got.Alias != "dev-laptop" {
		t.Errorf("alias = %q", got.Alias)
	}

	deps, err := l.Deployments(ctx)
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	if len(deps) != 2 {
		t.Errorf("got %d deployment summaries, want 2", len(deps))
	}
}

func TestForgetRemovesRedisRecord(t *testing.T) {
	prefix := uniquePrefix(t)
	ctx := context.Background()
	s := newStore(t, prefix)
	l := NewLedger(s, spend.New(), prefix, discard(), 2*time.Second)

	l.Record(ctx, spend.Entry{KeyHash: "doomed", DeploymentID: "d", Cost: 3, Billable: true})
	l.Forget("doomed")
	if spent, _ := l.KeySpend(ctx, "doomed", 0); spent != 0 {
		t.Errorf("spend = %v, want 0 after Forget", spent)
	}
}

// An unreachable Redis must degrade to local state rather than fail requests —
// and the degradation must be visible, not silent.
func TestDegradesToLocalWhenRedisUnavailable(t *testing.T) {
	local := router.NewMemState()
	s := New(Options{
		Addr:      "127.0.0.1:1", // nothing listening
		KeyPrefix: "test", Timeout: 100 * time.Millisecond,
	}, local, discard())
	defer s.Close()
	ctx := context.Background()

	// Requests still flow, enforced locally.
	admitted := 0
	for range 10 {
		ok, err := s.Reserve(ctx, "dep-1", 4, 0)
		if err != nil {
			t.Fatalf("Reserve returned an error instead of degrading: %v", err)
		}
		if ok {
			admitted++
		}
	}
	if admitted != 4 {
		t.Errorf("admitted %d, want 4 from the local fallback", admitted)
	}
	if s.Degradations() == 0 {
		t.Error("degradation was not counted; a silent fallback is the trap this guards against")
	}

	// Cooldowns and tokens degrade too, rather than erroring.
	if err := s.RecordFailure(ctx, "dep-1", time.Now(), 1, time.Second); err != nil {
		t.Errorf("RecordFailure: %v", err)
	}
	if _, err := s.InCooldown(ctx, "dep-1", time.Now()); err != nil {
		t.Errorf("InCooldown: %v", err)
	}
	if err := s.AddTokens(ctx, "dep-1", 10); err != nil {
		t.Errorf("AddTokens: %v", err)
	}
}

// Latency and in-flight are deliberately local; confirm they do not reach Redis.
func TestLatencyAndInFlightStayLocal(t *testing.T) {
	prefix := uniquePrefix(t)
	a, b := newStore(t, prefix), newStore(t, prefix)
	ctx := context.Background()

	a.BeginRequest(ctx, "dep-1")
	a.EndRequest(ctx, "dep-1", 50*time.Millisecond, true)
	a.BeginRequest(ctx, "dep-1")

	if n, _ := a.InFlight(ctx, "dep-1"); n != 1 {
		t.Errorf("instance A in-flight = %d, want 1", n)
	}
	if n, _ := b.InFlight(ctx, "dep-1"); n != 0 {
		t.Errorf("instance B in-flight = %d, want 0: in-flight is per-instance by design", n)
	}
	if _, seen, _ := b.MeanLatency(ctx, "dep-1"); seen {
		t.Error("instance B sees A's latency samples; latency is per-instance by design")
	}
}

// A prompt-prefix pin has to be shared. Behind a load balancer the next turn of
// a conversation lands on a different replica, and a per-instance pin would send
// it to a different upstream — the exact thing the pin exists to prevent.
func TestAffinitySharedAcrossInstances(t *testing.T) {
	prefix := uniquePrefix(t)
	a, b := newStore(t, prefix), newStore(t, prefix)
	ctx := context.Background()

	if _, ok, err := b.Affinity(ctx, "fp-1"); err != nil || ok {
		t.Fatalf("unset pin: ok = %v, err = %v; want a clean miss", ok, err)
	}
	if err := a.SetAffinity(ctx, "fp-1", "dep-1", time.Minute); err != nil {
		t.Fatalf("SetAffinity: %v", err)
	}

	got, ok, err := b.Affinity(ctx, "fp-1")
	if err != nil {
		t.Fatalf("Affinity: %v", err)
	}
	if !ok || got != "dep-1" {
		t.Errorf("the other instance read %q, %v; want dep-1, true", got, ok)
	}

	// A pin is refreshed on every success, so it follows a failover rather than
	// holding a conversation on a deployment that stopped serving it.
	if err := b.SetAffinity(ctx, "fp-1", "dep-2", time.Minute); err != nil {
		t.Fatalf("SetAffinity: %v", err)
	}
	if got, _, _ := a.Affinity(ctx, "fp-1"); got != "dep-2" {
		t.Errorf("pin = %q after re-pinning, want dep-2", got)
	}
}

func TestAffinityExpiresInRedis(t *testing.T) {
	s := newStore(t, uniquePrefix(t))
	ctx := context.Background()

	if err := s.SetAffinity(ctx, "fp-ttl", "dep-1", time.Second); err != nil {
		t.Fatalf("SetAffinity: %v", err)
	}
	ttl, err := s.Client().TTL(ctx, s.key("affinity", "fp-ttl")).Result()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if ttl <= 0 || ttl > time.Second {
		t.Errorf("ttl = %v, want a positive value no greater than 1s — a pin with no expiry would outlive the cache it points at", ttl)
	}
}

// Losing a pin costs a cache write, not a request.
func TestAffinityDegradesWithoutRedis(t *testing.T) {
	s := New(Options{
		Addr:      "127.0.0.1:1", // nothing listening
		KeyPrefix: "test", Timeout: 100 * time.Millisecond,
	}, router.NewMemState(), discard())
	defer s.Close()
	ctx := context.Background()

	if err := s.SetAffinity(ctx, "fp", "dep-1", time.Minute); err != nil {
		t.Fatalf("SetAffinity returned an error instead of degrading: %v", err)
	}
	if _, _, err := s.Affinity(ctx, "fp"); err != nil {
		t.Fatalf("Affinity returned an error instead of degrading: %v", err)
	}
}

// The Redis ledger stores its totals field by field in a Lua script, so a field
// added to spend.Totals is only reported once it is added in three places. Miss
// one and the endpoint answers 0 — which reads as "nothing happened" rather than
// as missing data, beside sibling figures that are correct.
//
// So this compares the two implementations rather than checking known fields:
// one entry through each, every numeric total expected to agree. A new field
// that only lands in the local ledger fails here.
func TestRedisLedgerMatchesTheLocalLedgerFieldForField(t *testing.T) {
	prefix := uniquePrefix(t)
	ctx := context.Background()
	s := newStore(t, prefix)

	local := spend.New()
	shared := NewLedger(s, spend.New(), prefix, discard(), 2*time.Second)

	// Every field that feeds Totals is non-zero, so a dropped one is visible as
	// a difference rather than as two matching zeros.
	entry := spend.Entry{
		KeyHash: "k1", KeyAlias: "dev", DeploymentID: "dep-1",
		Usage: core.Usage{
			InputTokens: 11, OutputTokens: 22,
			CacheReadTokens: 33, CacheWriteTokens: 44,
		},
		Cost:         1.5,
		CacheSavings: 2.75,
		Billable:     true,
	}
	if err := local.Record(ctx, entry); err != nil {
		t.Fatalf("local Record: %v", err)
	}
	if err := shared.Record(ctx, entry); err != nil {
		t.Fatalf("shared Record: %v", err)
	}

	localRows, err := local.Keys(ctx)
	if err != nil {
		t.Fatalf("local Keys: %v", err)
	}
	sharedRows, err := shared.Keys(ctx)
	if err != nil {
		t.Fatalf("shared Keys: %v", err)
	}
	if len(localRows) != 1 || len(sharedRows) != 1 {
		t.Fatalf("got %d local and %d shared summaries, want 1 of each", len(localRows), len(sharedRows))
	}

	wantTotals := reflect.ValueOf(localRows[0].Totals)
	gotTotals := reflect.ValueOf(sharedRows[0].Totals)
	fields := wantTotals.Type()

	for i := range fields.NumField() {
		name := fields.Field(i).Name
		if name == "WindowStart" {
			continue // set from the clock, not from the entry
		}
		want, got := wantTotals.Field(i), gotTotals.Field(i)
		switch want.Kind() {
		case reflect.Int:
			if got.Int() != want.Int() {
				t.Errorf("%s = %d via Redis, %d locally", name, got.Int(), want.Int())
			}
		case reflect.Float64:
			if diff := got.Float() - want.Float(); diff > 1e-9 || diff < -1e-9 {
				t.Errorf("%s = %v via Redis, %v locally", name, got.Float(), want.Float())
			}
		default:
			t.Errorf("%s has kind %s, which this comparison does not cover — extend it", name, want.Kind())
		}
	}
}

// TestNegativeSavingsSurviveRedis is the multi-instance half of a figure that
// can now be either sign.
//
// Savings are net of the prompt cache's write premium, so a workload writing
// caches nobody reads reports a negative one. Redis accumulates it with
// HINCRBYFLOAT and reads it back as a string, and both a formatter that dropped
// the sign and a parser that gave up on a leading minus would turn a reported
// loss into a reported nothing — which is exactly the direction an operator
// needs to see.
func TestNegativeSavingsSurviveRedis(t *testing.T) {
	prefix := uniquePrefix(t)
	ctx := context.Background()
	s := newStore(t, prefix)
	l := NewLedger(s, spend.New(), prefix, discard(), 2*time.Second)

	// A write nobody read, three times over, then one turn that read it back.
	for _, savings := range []float64{-0.75, -0.75, -0.75, 1.00} {
		if err := l.Record(ctx, spend.Entry{
			KeyHash: "k1", DeploymentID: "dep-1",
			Usage:        core.Usage{InputTokens: 10, OutputTokens: 5, CacheWriteTokens: 200},
			Cost:         0.25,
			CacheSavings: savings,
			Billable:     true,
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	rows, err := l.Keys(ctx)
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if got := rows[0].CacheSavings; got > -1.2499 || got < -1.2501 {
		t.Errorf("cache savings = %v via Redis, want -1.25", got)
	}
	if rows[0].Cost < 0.99 || rows[0].Cost > 1.01 {
		t.Errorf("cost = %v, want ~1.00: only the savings figure may go negative", rows[0].Cost)
	}
}

// Token usage is what the usage-based strategy balances on, so it has to be the
// fleet's view rather than one replica's: an instance that only saw its own
// traffic would send every request to whichever deployment it personally had
// used least.
func TestTokensUsedSharedAcrossInstances(t *testing.T) {
	prefix := uniquePrefix(t)
	ctx := context.Background()
	a, b := newStore(t, prefix), newStore(t, prefix)

	if err := a.AddTokens(ctx, "dep-1", 900); err != nil {
		t.Fatalf("AddTokens: %v", err)
	}
	if err := b.AddTokens(ctx, "dep-1", 100); err != nil {
		t.Fatalf("AddTokens: %v", err)
	}

	got, err := b.TokensUsed(ctx, "dep-1")
	if err != nil {
		t.Fatalf("TokensUsed: %v", err)
	}
	if got != 1000 {
		t.Errorf("TokensUsed = %d, want 1000: the second instance is not seeing the first's traffic", got)
	}
	if idle, err := a.TokensUsed(ctx, "dep-2"); err != nil || idle != 0 {
		t.Errorf("TokensUsed on an unused deployment = %d, %v; want 0, nil", idle, err)
	}
}

// Flushing the shared ledger persists the local mirror, which is what a restart
// with Redis down falls back to.
func TestSharedLedgerFlushPersistsTheLocalMirror(t *testing.T) {
	prefix := uniquePrefix(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "spend.json")

	local, err := spend.NewFileLedger(path)
	if err != nil {
		t.Fatalf("NewFileLedger: %v", err)
	}
	l := NewLedger(newStore(t, prefix), local, prefix, discard(), 2*time.Second)
	if err := l.Record(ctx, spend.Entry{
		KeyHash: "k1", DeploymentID: "dep-1",
		Usage: core.Usage{InputTokens: 10, OutputTokens: 5}, Cost: 0.5, Billable: true,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := l.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	reopened, err := spend.NewFileLedger(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	spent, err := reopened.KeySpend(ctx, "k1", 0)
	if err != nil {
		t.Fatalf("KeySpend: %v", err)
	}
	if spent < 0.49 || spent > 0.51 {
		t.Errorf("spend after restart = %v, want 0.5", spent)
	}
}

// With Redis unreachable the /spend endpoints still answer, from the local
// mirror every instance keeps. Reporting an error there would take the operator
// their cost view at exactly the moment they most want it.
func TestSpendSummariesFallBackToLocalWhenRedisIsDown(t *testing.T) {
	ctx := context.Background()
	s := New(Options{
		Addr:      "127.0.0.1:1", // nothing listening
		KeyPrefix: "test", Timeout: 100 * time.Millisecond,
	}, router.NewMemState(), discard())
	defer s.Close()

	l := NewLedger(s, spend.New(), "test", discard(), 100*time.Millisecond)
	if err := l.Record(ctx, spend.Entry{
		KeyHash: "k1", KeyAlias: "dev", DeploymentID: "dep-1",
		Usage:        core.Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 40},
		Cost:         0.5,
		CacheSavings: 0.1,
		Billable:     true,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	keys, err := l.Keys(ctx)
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(keys) != 1 || keys[0].Cost != 0.5 {
		t.Fatalf("keys = %+v, want one row costing 0.5 from the local mirror", keys)
	}
	deployments, err := l.Deployments(ctx)
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	if len(deployments) != 1 || deployments[0].Subject != "dep-1" {
		t.Errorf("deployments = %+v, want one row for dep-1", deployments)
	}
	// Budgets are still enforced, from the same mirror.
	spent, err := l.KeySpend(ctx, "k1", time.Hour)
	if err != nil {
		t.Fatalf("KeySpend: %v", err)
	}
	if spent != 0.5 {
		t.Errorf("KeySpend = %v, want 0.5", spent)
	}
}
