package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// These tests run against a real Postgres and skip themselves when there is
// none, for the reason the key store's do: what is under test is behaviour the
// database owns — advisory-lock serialization across concurrent writers, the
// append-only triggers, transactional rollback of a record that was not
// committed — and a fake would be a second implementation of exactly the parts
// that could be wrong.
//
// CI runs a postgres service so the skip does not quietly shrink the suite
// there. Locally:
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
		t.Skipf("no postgres for the audit sink tests (%v); set GATEWAY_TEST_POSTGRES_DSN or run: docker run --rm -d -e POSTGRES_PASSWORD=postgres -p 5432:5432 postgres:16", probeErr)
	}
	return probedDSN
}

// testSchema creates a schema of its own for one test, dropped when it ends,
// and returns a DSN pointed at it. Isolating by schema rather than by table
// name keeps the DDL under test exactly the DDL the gateway runs, trigger names
// and all.
func testSchema(t *testing.T) string {
	t.Helper()
	dsn := testDSN(t)
	ctx := context.Background()

	schema := fmt.Sprintf("audittest_%d_%d", time.Now().UnixNano(), os.Getpid())
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

// withSearchPath points a DSN at one schema, in whichever of the two connection
// string forms it was written.
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

func openTestSink(t *testing.T, dsn, instance string) *PostgresSink {
	t.Helper()
	sink, err := OpenPostgres(context.Background(), PostgresOptions{
		DSN: dsn, Timeout: 10 * time.Second, Instance: instance,
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	return sink
}

func record(t *testing.T, sink Sink, n int) Record {
	t.Helper()
	rec, err := sink.Record(context.Background(), event(n))
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	return rec
}

// The property the whole design turns on: two instances writing to one database
// produce one chain, not two, and it verifies.
func TestPostgresSharesOneChainAcrossInstances(t *testing.T) {
	dsn := testSchema(t)
	a := openTestSink(t, dsn, "gateway-a")
	b := openTestSink(t, dsn, "gateway-b")

	first := record(t, a, 1)
	second := record(t, b, 2)
	third := record(t, a, 3)

	if first.Seq != 1 || second.Seq != 2 || third.Seq != 3 {
		t.Fatalf("sequences = %d, %d, %d; want 1, 2, 3 — the second instance did not continue the first's chain",
			first.Seq, second.Seq, third.Seq)
	}
	if second.Prev != first.Hash || third.Prev != second.Hash {
		t.Error("a record does not carry the hash of the one written by the other instance")
	}

	summary, err := a.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if summary.Records != 3 || summary.LastSeq != 3 {
		t.Errorf("summary = %+v, want 3 records ending at sequence 3", summary)
	}

	// And each record says which instance wrote it, which is the one question a
	// shared chain raises that a per-host one does not.
	if got := detailOf(t, a, 1)["instance"]; got != "gateway-a" {
		t.Errorf("record 1 instance = %q, want gateway-a", got)
	}
	if got := detailOf(t, a, 2)["instance"]; got != "gateway-b" {
		t.Errorf("record 2 instance = %q, want gateway-b", got)
	}
}

// Concurrent appends must not interleave into a chain with a gap, a repeat or a
// broken link. This is what the fleet-wide advisory lock is for, and the failure
// it prevents is silent: every record would be individually valid.
func TestPostgresConcurrentAppendsKeepOneOrder(t *testing.T) {
	dsn := testSchema(t)

	const instances, each = 4, 8
	sinks := make([]*PostgresSink, instances)
	for i := range sinks {
		sinks[i] = openTestSink(t, dsn, fmt.Sprintf("gateway-%d", i))
	}

	var wg sync.WaitGroup
	errs := make(chan error, instances*each)
	for i, sink := range sinks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range each {
				if _, err := sink.Record(context.Background(), event(i*each+n)); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Record: %v", err)
	}

	summary, err := sinks[0].Verify(context.Background())
	if err != nil {
		t.Fatalf("the chain %d writers produced does not verify: %v", instances, err)
	}
	if summary.Records != instances*each || summary.LastSeq != uint64(instances*each) {
		t.Errorf("summary = %+v, want %d records ending at sequence %d",
			summary, instances*each, instances*each)
	}
}

// A restart continues the chain rather than starting a second one, which is
// what the file sink does on one host and what nothing does on stdout.
func TestPostgresContinuesTheChainAcrossRestarts(t *testing.T) {
	dsn := testSchema(t)

	first := openTestSink(t, dsn, "gateway-a")
	record(t, first, 1)
	record(t, first, 2)
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := openTestSink(t, dsn, "gateway-a")
	next := record(t, second, 3)
	if next.Seq != 3 {
		t.Errorf("seq after a restart = %d, want 3", next.Seq)
	}
	if _, err := second.Verify(context.Background()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// The database refuses to change or remove a record. This is not what makes the
// log trustworthy — the chain is — but it is what stops the ordinary way audit
// logs lose records: a cleanup script, a migration tool, an operator tidying.
func TestPostgresRefusesToChangeARecord(t *testing.T) {
	dsn := testSchema(t)
	sink := openTestSink(t, dsn, "gateway-a")
	record(t, sink, 1)
	record(t, sink, 2)

	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())

	for _, tc := range []struct{ name, stmt string }{
		{"update", `UPDATE gateway_audit SET record = '{}' WHERE seq = 1`},
		{"delete", `DELETE FROM gateway_audit WHERE seq = 1`},
		{"truncate", `TRUNCATE gateway_audit`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := conn.Exec(context.Background(), tc.stmt); err == nil {
				t.Fatalf("%s succeeded on an append-only table", tc.name)
			} else if !strings.Contains(err.Error(), "append-only") {
				t.Errorf("error = %v, want it to say the table is append-only", err)
			}
		})
	}
}

// The trigger is a floor, not a ceiling: whoever can drop it can still rewrite a
// row, and the chain is what catches them when they do. Here that privilege is
// simulated the way it actually arrives — session_replication_role, which
// disables triggers for the session.
//
// Once the tip no longer verifies, this instance stops appending to the chain
// rather than adding good records to a broken one, and says why.
func TestPostgresSealsWhenTheTipIsRewritten(t *testing.T) {
	dsn := testSchema(t)
	sink := openTestSink(t, dsn, "gateway-a")
	record(t, sink, 1)
	record(t, sink, 2)

	tamper(t, dsn, `UPDATE gateway_audit SET record = jsonb_set(record::jsonb, '{reason}', '"edited"')::text WHERE seq = 2`)

	_, err := sink.Record(context.Background(), event(3))
	var broken *BreakError
	if !errors.As(err, &broken) {
		t.Fatalf("Record onto a rewritten tip returned %v, want a *BreakError", err)
	}
	if reason := sink.Sealed(); reason == "" {
		t.Fatal("the sink kept appending to a chain that no longer verifies")
	}

	// Sealed means sealed: the next attempt does not even reach the database,
	// and it is distinguishable from a write that merely failed.
	_, err = sink.Record(context.Background(), event(4))
	var sealed *SealedError
	if !errors.As(err, &sealed) {
		t.Fatalf("a sealed sink returned %v, want a *SealedError", err)
	}
	if count := countRecords(t, dsn); count != 2 {
		t.Errorf("records = %d, want the 2 that were there before the sink sealed", count)
	}
}

// A record removed from the tail is caught by the next append too, because the
// tip is read as a pair rather than as one row.
func TestPostgresSealsWhenTheTailIsRemoved(t *testing.T) {
	dsn := testSchema(t)
	sink := openTestSink(t, dsn, "gateway-a")
	for n := range 4 {
		record(t, sink, n)
	}

	tamper(t, dsn, `DELETE FROM gateway_audit WHERE seq = 3`)

	_, err := sink.Record(context.Background(), event(9))
	var broken *BreakError
	if !errors.As(err, &broken) {
		t.Fatalf("Record after a removed record returned %v, want a *BreakError", err)
	}
	if !strings.Contains(broken.Reason, "removed") {
		t.Errorf("reason = %q, want it to name the removal", broken.Reason)
	}
}

// A gateway starting onto a chain that no longer verifies must come up sealed
// rather than not come up: inference is not audited, so an edited row is no
// reason to take a fleet offline. The administrative half is refused instead.
func TestOpenPostgresStartsSealedOnABrokenChain(t *testing.T) {
	dsn := testSchema(t)
	first := openTestSink(t, dsn, "gateway-a")
	for n := range 3 {
		record(t, first, n)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	tamper(t, dsn, `UPDATE gateway_audit SET record = jsonb_set(record::jsonb, '{outcome}', '"refused"')::text WHERE seq = 2`)

	sink, err := OpenPostgres(context.Background(), PostgresOptions{
		DSN: dsn, Timeout: 10 * time.Second, Instance: "gateway-b",
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("OpenPostgres refused to start on a broken chain: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })

	if sink.Sealed() == "" {
		t.Fatal("the sink opened onto a broken chain without sealing")
	}
	_, err = sink.Record(context.Background(), event(4))
	var sealed *SealedError
	if !errors.As(err, &sealed) {
		t.Fatalf("Record on a sealed sink returned %v, want a *SealedError", err)
	}
	if count := countRecords(t, dsn); count != 3 {
		t.Errorf("records = %d, want the 3 that were there before startup", count)
	}
}

// The other half of that posture, and the reason it is not simply "never
// refuse to start": a database that cannot be reached is transient, a restart
// is how an orchestrator retries it, and a gateway that came up anyway would be
// administrable with nothing recording it.
func TestOpenPostgresFailsWhenTheDatabaseIsUnreachable(t *testing.T) {
	// Port 1 answers nothing on any platform this builds for.
	_, err := OpenPostgres(context.Background(), PostgresOptions{
		DSN: "postgres://postgres:postgres@127.0.0.1:1/postgres?sslmode=disable&connect_timeout=1",
		// Deliberately short: the point is the refusal, not the wait.
		Timeout: 2 * time.Second,
	}, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("OpenPostgres succeeded against a database that does not answer")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("error = %v, want it to say the database was unreachable", err)
	}
}

// VerifyPostgres is what an auditor runs, so it must work from a DSN alone,
// create nothing, and say something useful about a database that has no chain.
func TestVerifyPostgresFromADSN(t *testing.T) {
	dsn := testSchema(t)

	if _, err := VerifyPostgres(context.Background(), dsn); err == nil {
		t.Error("verifying a database with no audit table succeeded")
	} else if !strings.Contains(err.Error(), "no gateway_audit table") {
		t.Errorf("error = %v, want it to say the table is absent", err)
	}

	sink := openTestSink(t, dsn, "gateway-a")
	for n := range 5 {
		record(t, sink, n)
	}

	summary, err := VerifyPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("VerifyPostgres: %v", err)
	}
	if summary.Records != 5 || summary.FirstSeq != 1 || summary.LastSeq != 5 {
		t.Errorf("summary = %+v, want 5 records over sequences 1..5", summary)
	}

	tamper(t, dsn, `DELETE FROM gateway_audit WHERE seq = 1`)
	if _, err := VerifyPostgres(context.Background(), dsn); err == nil {
		t.Error("a chain missing its first record verified")
	}
}

// The password must not reach an operator-facing line, because this one is
// printed by a command whose output gets pasted into tickets.
func TestPostgresTargetOmitsTheCredential(t *testing.T) {
	got := PostgresTarget("postgres://gateway:hunter2@db.internal:5432/audit?sslmode=require")
	if strings.Contains(got, "hunter2") {
		t.Fatalf("target = %q, want no password in it", got)
	}
	for _, want := range []string{"db.internal", "audit"} {
		if !strings.Contains(got, want) {
			t.Errorf("target = %q, want it to name %q", got, want)
		}
	}
}

// tamper runs a statement that the append-only triggers exist to refuse, with
// those triggers disabled — the privilege a superuser has and the chain is
// there to catch.
func tamper(t *testing.T, dsn, stmt string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "SET session_replication_role = replica"); err != nil {
		t.Skipf("cannot disable triggers to simulate tampering (%v); this test needs a superuser", err)
	}
	tag, err := conn.Exec(ctx, stmt)
	if err != nil {
		t.Fatalf("tamper: %v", err)
	}
	// Asserted because a statement that matches what is already stored changes
	// nothing and would leave the test proving that an untampered chain
	// verifies — which is a different test, and one that passes.
	if tag.RowsAffected() == 0 {
		t.Fatalf("the tampering statement changed no rows: %s", stmt)
	}
}

func countRecords(t *testing.T, dsn string) int {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM gateway_audit").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func detailOf(t *testing.T, sink *PostgresSink, seq uint64) map[string]string {
	t.Helper()
	var raw string
	if err := sink.pool.QueryRow(context.Background(),
		"SELECT record FROM gateway_audit WHERE seq = $1", int64(seq)).Scan(&raw); err != nil {
		t.Fatalf("read record %d: %v", seq, err)
	}
	var rec Record
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatalf("decode record %d: %v", seq, err)
	}
	return rec.Detail
}
