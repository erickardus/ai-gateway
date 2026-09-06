package spend

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/jackc/pgx/v5"
)

// These tests run against a real Postgres and skip themselves when there is
// none, for the reason the key store's and the audit sink's do: what is under
// test is behaviour the database owns — COPY into a batch, the rollup upsert's
// arithmetic under concurrency, aggregation over an unnested array, a delete
// that must leave one table alone — and a fake would be a second implementation
// of exactly the parts that could be wrong.
//
// Locally:
//
//	docker run --rm -d -e POSTGRES_PASSWORD=postgres -p 5432:5432 postgres:16
const defaultTestDSN = "postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable"

var (
	probeOnce sync.Once
	probedDSN string
	probeErr  error
)

func testDSN(t *testing.T) string {
	t.Helper()
	probeOnce.Do(func() {
		dsn := os.Getenv("GATEWAY_TEST_POSTGRES_DSN")
		if dsn == "" {
			dsn = defaultTestDSN
		}
		probedDSN = dsn

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			probeErr = err
			return
		}
		probeErr = conn.Ping(ctx)
		_ = conn.Close(ctx)
	})
	if probeErr != nil {
		t.Skipf("no postgres for the spend history tests (%v); set GATEWAY_TEST_POSTGRES_DSN or run: docker run --rm -d -e POSTGRES_PASSWORD=postgres -p 5432:5432 postgres:16", probeErr)
	}
	return probedDSN
}

// testSchema gives one test a schema of its own, dropped when it ends, so the
// DDL under test is exactly the DDL the gateway runs, table names and all.
func testSchema(t *testing.T) string {
	t.Helper()
	dsn := testDSN(t)
	ctx := context.Background()

	schema := fmt.Sprintf("spendtest_%d_%d", time.Now().UnixNano(), os.Getpid())
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			return
		}
		defer cleanup.Close(context.Background())
		_, _ = cleanup.Exec(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`)
	})
	return withSearchPath(dsn, schema)
}

func withSearchPath(dsn, schema string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		return dsn + sep + "search_path=" + schema
	}
	return dsn + " search_path=" + schema
}

func openTestHistory(t *testing.T, dsn string, opts PostgresOptions) *PostgresHistory {
	t.Helper()
	if opts.DSN == "" {
		opts.DSN = dsn
	}
	if opts.FlushInterval == 0 {
		// Short, because every test here waits for a flush.
		opts.FlushInterval = 25 * time.Millisecond
	}
	h, err := OpenPostgres(context.Background(), opts, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// waitWritten blocks until the writer has committed n rows, so the tests
// observe the real batching path rather than a synchronous write nothing uses.
func waitWritten(t *testing.T, h *PostgresHistory, n int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		written, dropped, _ := h.Stats()
		if dropped > 0 {
			t.Fatalf("the writer dropped %d rows", dropped)
		}
		if written >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	written, _, pending := h.Stats()
	t.Fatalf("only %d of %d rows were written (%d still queued)", written, n, pending)
}

func entryAt(at time.Time, cost float64, scope string) Entry {
	return Entry{
		At:           at,
		RequestID:    fmt.Sprintf("req-%d", at.UnixNano()),
		KeyHash:      "hash-a",
		KeyAlias:     "laptop",
		ModelGroup:   "anthropic-claude",
		DeploymentID: "anthropic-claude/primary",
		Usage:        core.Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 10},
		Cost:         cost,
		Billable:     true,
		CacheSavings: cost / 10,
		Scopes:       []string{scope, "acme"},
		ScopeAliases: []string{"Payments", "Acme"},
	}
}

// The question the whole feature exists for: what did one team spend over a
// range that has already left every budget window.
func TestHistoryAnswersAMonth(t *testing.T) {
	dsn := testSchema(t)
	h := openTestHistory(t, dsn, PostgresOptions{})

	base := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	for i := range 5 {
		if err := h.Append(context.Background(), entryAt(base.AddDate(0, 0, i), 2, "acme/payments")); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	waitWritten(t, h, 5)

	buckets, err := h.Series(context.Background(), Query{
		Kind: KindScope, Subject: "acme/payments", Interval: IntervalDay,
		From: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(buckets) != 5 {
		t.Fatalf("buckets = %d, want one per day of traffic (5)", len(buckets))
	}

	var total float64
	for _, b := range buckets {
		total += b.Cost
		if b.Alias != "Payments" {
			t.Errorf("bucket alias = %q, want Payments", b.Alias)
		}
		if b.Start.Hour() != 0 || b.Start.Location() != time.UTC {
			t.Errorf("bucket start = %s, want a UTC midnight", b.Start)
		}
	}
	if total != 10 {
		t.Errorf("month total = %v, want 10", total)
	}

	// And the range is honoured at both ends: a neighbouring month must not
	// pick these up, or consecutive months would double count.
	july, err := h.Series(context.Background(), Query{
		Kind: KindScope, Subject: "acme/payments", Interval: IntervalDay,
		From: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(july) != 0 {
		t.Errorf("July returned %d buckets of August's traffic", len(july))
	}
}

// A range ending now must include the day in progress. It is the range every
// default report asks for, and a bound taken from To's own date silently drops
// today from all of them.
func TestSeriesIncludesTheDayInProgress(t *testing.T) {
	dsn := testSchema(t)
	h := openTestHistory(t, dsn, PostgresOptions{})

	now := time.Now().UTC()
	if err := h.Append(context.Background(), entryAt(now, 7, "acme/payments")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	waitWritten(t, h, 1)

	buckets, err := h.Series(context.Background(), Query{
		Kind: KindScope, Subject: "acme/payments", Interval: IntervalDay,
		From: now.AddDate(0, 0, -30), To: now,
	})
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Cost != 7 {
		t.Fatalf("buckets = %+v, want today's 7 — a report ending now dropped the day in progress", buckets)
	}

	// And the exclusive end still excludes: a range ending at midnight covers
	// the day before it and not the day it names, or consecutive months would
	// each claim the boundary.
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	before, err := h.Series(context.Background(), Query{
		Kind: KindScope, Subject: "acme/payments", Interval: IntervalDay,
		From: midnight.AddDate(0, 0, -7), To: midnight,
	})
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(before) != 0 {
		t.Errorf("a range ending at midnight returned %d buckets of today's traffic", len(before))
	}
}

// The rollup is maintained on write, so it must agree with the rows it
// summarizes. If it ever does not, every report is wrong and nothing says so.
func TestRollupAgreesWithTheRows(t *testing.T) {
	dsn := testSchema(t)
	h := openTestHistory(t, dsn, PostgresOptions{BatchSize: 7})

	base := time.Date(2026, 8, 10, 8, 0, 0, 0, time.UTC)
	const n = 50
	for i := range n {
		e := entryAt(base.Add(time.Duration(i)*time.Minute), 0.25, "acme/payments")
		if err := h.Append(context.Background(), e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	waitWritten(t, h, n)

	var (
		rowCost, rollupCost float64
		rowCount            int64
	)
	ctx := context.Background()
	if err := h.pool.QueryRow(ctx,
		"SELECT coalesce(sum(cost), 0), count(*) FROM spend_requests").Scan(&rowCost, &rowCount); err != nil {
		t.Fatalf("sum rows: %v", err)
	}
	if err := h.pool.QueryRow(ctx,
		"SELECT coalesce(sum(cost), 0) FROM spend_daily WHERE subject_kind = 'key'").Scan(&rollupCost); err != nil {
		t.Fatalf("sum rollup: %v", err)
	}
	if rowCount != n {
		t.Errorf("rows = %d, want %d", rowCount, n)
	}
	if rowCost != rollupCost {
		t.Errorf("rollup total %v does not match the rows' %v", rollupCost, rowCost)
	}
}

// Two gateways writing the same day's rollup must add to it rather than
// overwrite it, and must not deadlock doing so.
func TestConcurrentInstancesAccumulateOneRollup(t *testing.T) {
	dsn := testSchema(t)

	const instances, each = 4, 25
	writers := make([]*PostgresHistory, instances)
	for i := range writers {
		writers[i] = openTestHistory(t, dsn, PostgresOptions{BatchSize: 5})
	}

	base := time.Date(2026, 8, 20, 6, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for i, h := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range each {
				at := base.Add(time.Duration(i*each+n) * time.Second)
				_ = h.Append(context.Background(), entryAt(at, 1, "acme/payments"))
			}
		}()
	}
	wg.Wait()
	for _, h := range writers {
		waitWritten(t, h, each)
	}

	var cost float64
	var requests int64
	if err := writers[0].pool.QueryRow(context.Background(),
		`SELECT cost, requests FROM spend_daily
		 WHERE subject_kind = 'scope' AND subject = 'acme/payments' AND day = DATE '2026-08-20'`).
		Scan(&cost, &requests); err != nil {
		t.Fatalf("read rollup: %v", err)
	}
	if want := float64(instances * each); cost != want {
		t.Errorf("pooled cost = %v, want %v — a writer overwrote another's contribution", cost, want)
	}
	if requests != instances*each {
		t.Errorf("pooled requests = %d, want %d", requests, instances*each)
	}
}

// A scope report at the organisation level must find the traffic its teams
// produced, which is what makes the whole chain worth storing per row.
func TestScopeReportRollsUpTheChain(t *testing.T) {
	dsn := testSchema(t)
	h := openTestHistory(t, dsn, PostgresOptions{})

	at := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	_ = h.Append(context.Background(), entryAt(at, 3, "acme/payments"))
	_ = h.Append(context.Background(), entryAt(at, 5, "acme/trading"))
	waitWritten(t, h, 2)

	q := Query{
		Kind: KindScope, Subject: "acme", Interval: IntervalDay,
		From: at.AddDate(0, 0, -1), To: at.AddDate(0, 0, 1),
	}
	buckets, err := h.Series(context.Background(), q)
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Cost != 8 {
		t.Fatalf("organisation total = %+v, want one bucket of 8", buckets)
	}

	// The same question by hour reads the per-request rows through a different
	// path, and must reach the same number.
	q.Interval = IntervalHour
	hourly, err := h.Series(context.Background(), q)
	if err != nil {
		t.Fatalf("Series hourly: %v", err)
	}
	var total float64
	for _, b := range hourly {
		total += b.Cost
	}
	if total != 8 {
		t.Errorf("hourly total = %v, want the 8 the day rollup reports", total)
	}
}

// Export is the artefact chargeback actually uses, so it must stream every row
// a range selects and nothing outside it.
func TestEachRowStreamsTheRange(t *testing.T) {
	dsn := testSchema(t)
	h := openTestHistory(t, dsn, PostgresOptions{})

	base := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	for i := range 6 {
		_ = h.Append(context.Background(), entryAt(base.AddDate(0, 0, i), 1, "acme/payments"))
	}
	waitWritten(t, h, 6)

	var seen []Row
	err := h.EachRow(context.Background(), Query{
		Kind: KindScope, Subject: "acme/payments",
		From: base.AddDate(0, 0, 1), To: base.AddDate(0, 0, 4),
	}, func(r Row) error {
		seen = append(seen, r)
		return nil
	})
	if err != nil {
		t.Fatalf("EachRow: %v", err)
	}
	if len(seen) != 3 {
		t.Fatalf("rows = %d, want the 3 inside the range", len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i].At.Before(seen[i-1].At) {
			t.Error("rows are not in time order, which an export is read in")
		}
	}
	if seen[0].KeyAlias != "laptop" || seen[0].Scope != "acme/payments" {
		t.Errorf("row = %+v, want the alias and scope it was charged under", seen[0])
	}
	if seen[0].Usage.InputTokens != 100 {
		t.Errorf("usage did not round-trip: %+v", seen[0].Usage)
	}
}

// Retention is what makes the raw table bounded. It must leave the rollups
// alone, or pruning would delete the history it exists to preserve.
func TestPruneKeepsTheRollups(t *testing.T) {
	dsn := testSchema(t)
	h := openTestHistory(t, dsn, PostgresOptions{Retention: 24 * time.Hour})

	old := time.Now().UTC().AddDate(0, 0, -30)
	recent := time.Now().UTC().Add(-time.Hour)
	_ = h.Append(context.Background(), entryAt(old, 4, "acme/payments"))
	_ = h.Append(context.Background(), entryAt(recent, 6, "acme/payments"))
	waitWritten(t, h, 2)

	h.prune()

	ctx := context.Background()
	var rows int64
	if err := h.pool.QueryRow(ctx, "SELECT count(*) FROM spend_requests").Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("rows after pruning = %d, want only the recent one", rows)
	}

	var cost float64
	if err := h.pool.QueryRow(ctx,
		"SELECT coalesce(sum(cost), 0) FROM spend_daily WHERE subject_kind = 'scope' AND subject = 'acme/payments'").
		Scan(&cost); err != nil {
		t.Fatalf("sum rollup: %v", err)
	}
	if cost != 10 {
		t.Errorf("rollup total after pruning = %v, want the full 10 — pruning took the history with the rows", cost)
	}
}

// A full buffer drops rather than blocks, because the alternative is an
// inference request waiting on the database that answers monthly questions.
func TestAppendDropsRatherThanBlocking(t *testing.T) {
	dsn := testSchema(t)
	// One slot, and a flush interval long enough that nothing drains it during
	// the test.
	h := openTestHistory(t, dsn, PostgresOptions{
		BufferSize: 1, BatchSize: 1, FlushInterval: time.Hour,
	})

	at := time.Now().UTC()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			_ = h.Append(context.Background(), entryAt(at, 1, "acme/payments"))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Append blocked on a full buffer; an inference request would have waited on it")
	}

	_, dropped, _ := h.Stats()
	if dropped == 0 {
		t.Error("nothing was counted as dropped, so a full buffer is losing rows silently")
	}
}

// Closing must commit what is still buffered: a graceful shutdown that threw
// away the last few seconds of traffic would lose money from every report, on
// every deploy.
func TestCloseFlushesWhatIsBuffered(t *testing.T) {
	dsn := testSchema(t)
	h, err := OpenPostgres(context.Background(), PostgresOptions{
		DSN: dsn, BatchSize: 1000, FlushInterval: time.Hour,
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	at := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	for i := range 10 {
		_ = h.Append(context.Background(), entryAt(at.Add(time.Duration(i)*time.Second), 1, "acme/payments"))
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())

	var rows int64
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM spend_requests").Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 10 {
		t.Errorf("rows after Close = %d, want the 10 that were buffered", rows)
	}
}

// A gateway pointed at a database that is not there must not start: it would
// serve traffic whose cost nothing was recording, which is discovered a month
// later when the report is asked for.
func TestOpenPostgresFailsWhenUnreachable(t *testing.T) {
	_, err := OpenPostgres(context.Background(), PostgresOptions{
		DSN: "postgres://postgres:postgres@127.0.0.1:1/postgres?sslmode=disable&connect_timeout=1",
	}, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("OpenPostgres succeeded against a database that does not answer")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("error = %v, want it to say the database was unreachable", err)
	}
}

func TestPostgresTargetOmitsTheCredential(t *testing.T) {
	got := PostgresTarget("postgres://gateway:hunter2@db.internal:5432/spend?sslmode=require")
	if strings.Contains(got, "hunter2") {
		t.Fatalf("target = %q, want no password in it", got)
	}
	if !strings.Contains(got, "db.internal") || !strings.Contains(got, "spend") {
		t.Errorf("target = %q, want it to name the host and database", got)
	}
}
