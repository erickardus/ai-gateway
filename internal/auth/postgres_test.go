package auth

import (
	"context"
	"errors"
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
// none, for the reason the rstate tests do: what is under test is behaviour the
// database owns — ON CONFLICT replacement, array and timestamptz round-tripping,
// concurrent writers — and a fake would be a second implementation of exactly
// the parts that could be wrong.
//
// They skip per test rather than from a TestMain that exits, which is how
// internal/rstate does it, because that package is nothing but integration
// tests while this one is mostly not: an os.Exit(0) here would take the whole
// auth suite with it and report a pass.
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

// testDSN returns a reachable Postgres, or skips. The probe runs once: a
// database that is absent is absent for every test, and a connection attempt
// per test would pay the timeout each time.
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
		t.Skipf("no postgres for the key store tests (%v); set GATEWAY_TEST_POSTGRES_DSN or run: docker run --rm -d -e POSTGRES_PASSWORD=postgres -p 5432:5432 postgres:16", probeErr)
	}
	return probedDSN
}

// newTestStore builds a store in a schema of its own, dropped when the test
// ends. Isolating by schema rather than by table name keeps the DDL under test
// exactly the DDL the gateway runs, table names and all, and leaves nothing
// behind in a database an operator may be using for something else.
func newTestStore(t *testing.T) *PostgresStore {
	t.Helper()
	dsn := testDSN(t)
	ctx := context.Background()

	schema := fmt.Sprintf("keytest_%d_%d", time.Now().UnixNano(), os.Getpid())
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

	store, err := NewPostgresStore(ctx, PostgresOptions{
		DSN:     withSearchPath(dsn, schema),
		Timeout: 5 * time.Second,
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
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

// sampleKey sets every field of core.Key, so a column left out of the schema or
// out of a scan cannot pass.
func sampleKey(hash string) *core.Key {
	// Postgres keeps microseconds, so the expectation is stated in
	// microseconds rather than the test asserting a resolution the database
	// does not have.
	expires := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Microsecond)
	return &core.Key{
		Hash:             hash,
		Alias:            "every field",
		Models:           []string{"claude-*", "gpt-4o"},
		RPMLimit:         60,
		TPMLimit:         120000,
		AllowPassthrough: true,
		Blocked:          true,
		CreatedAt:        time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond),
		ExpiresAt:        &expires,
		MaxBudget:        12.5,
		BudgetDuration:   720 * time.Hour,
		Subject:          "auth0|abc123",
		Device:           "laptop-2",
		Scope:            "acme/platform/gateway",
	}
}

func assertSameKey(t *testing.T, got, want *core.Key) {
	t.Helper()
	if got.Hash != want.Hash || got.Alias != want.Alias ||
		got.RPMLimit != want.RPMLimit || got.TPMLimit != want.TPMLimit ||
		got.AllowPassthrough != want.AllowPassthrough || got.Blocked != want.Blocked ||
		got.MaxBudget != want.MaxBudget || got.BudgetDuration != want.BudgetDuration ||
		got.Subject != want.Subject || got.Device != want.Device || got.Scope != want.Scope {
		t.Fatalf("key did not round-trip:\n got %+v\nwant %+v", got, want)
	}
	if len(got.Models) != len(want.Models) {
		t.Fatalf("models = %v, want %v", got.Models, want.Models)
	}
	for i := range want.Models {
		if got.Models[i] != want.Models[i] {
			t.Fatalf("models = %v, want %v", got.Models, want.Models)
		}
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("created_at = %v, want %v", got.CreatedAt, want.CreatedAt)
	}
	switch {
	case want.ExpiresAt == nil && got.ExpiresAt != nil:
		t.Errorf("expires_at = %v, want nil", got.ExpiresAt)
	case want.ExpiresAt != nil && got.ExpiresAt == nil:
		t.Errorf("expires_at = nil, want %v", want.ExpiresAt)
	case want.ExpiresAt != nil && !got.ExpiresAt.Equal(*want.ExpiresAt):
		t.Errorf("expires_at = %v, want %v", got.ExpiresAt, want.ExpiresAt)
	}
}

func TestPostgresStoreRoundTripsEveryField(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	want := sampleKey("hash-every-field")
	if err := store.Put(ctx, want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(ctx, want.Hash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	assertSameKey(t, got, want)

	// The subject is what SpendSubject reads, and a key whose spend accrues
	// under the wrong subject is a budget that resets when someone signs in
	// again.
	if got.SpendSubject() != want.SpendSubject() {
		t.Errorf("SpendSubject = %q, want %q", got.SpendSubject(), want.SpendSubject())
	}
}

// A key with nothing but a hash must also round-trip: the zero values are what
// a key minted by /key/generate has, and "no model restriction" must not come
// back as "restricted to nothing".
func TestPostgresStoreRoundTripsAnEmptyKey(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	want := &core.Key{Hash: "hash-bare", CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	if err := store.Put(ctx, want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(ctx, want.Hash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	assertSameKey(t, got, want)
	if got.Models != nil {
		t.Errorf("models = %#v, want nil", got.Models)
	}
	if got.ExpiresAt != nil {
		t.Errorf("expires_at = %v, want nil", got.ExpiresAt)
	}
	if got.Expired(time.Now()) {
		t.Error("a key with no expiry reads as expired")
	}
}

// Put with no CreatedAt fills one in, as MemStore does: created_at is NOT NULL,
// and a caller minting a key should not have to know that.
func TestPostgresStorePutFillsCreatedAt(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	before := time.Now().UTC().Add(-time.Second)
	if err := store.Put(ctx, &core.Key{Hash: "hash-no-created-at"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(ctx, "hash-no-created-at")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CreatedAt.Before(before) || got.CreatedAt.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("created_at = %v, want roughly now", got.CreatedAt)
	}
}

func TestPostgresStoreGetMissingIsErrKeyInvalid(t *testing.T) {
	store := newTestStore(t)

	_, err := store.Get(context.Background(), "hash-that-was-never-stored")
	if !errors.Is(err, core.ErrKeyInvalid) {
		t.Fatalf("Get on a missing hash = %v, want core.ErrKeyInvalid", err)
	}
}

// Put replaces the whole row rather than merging into it, so /key/update can
// clear a field. A merge would make "no expiry" impossible to write back.
func TestPostgresStorePutReplaces(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	first := sampleKey("hash-replaced")
	if err := store.Put(ctx, first); err != nil {
		t.Fatalf("Put: %v", err)
	}
	second := &core.Key{
		Hash:      first.Hash,
		Alias:     "unblocked",
		CreatedAt: first.CreatedAt,
	}
	if err := store.Put(ctx, second); err != nil {
		t.Fatalf("Put replace: %v", err)
	}

	got, err := store.Get(ctx, first.Hash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	assertSameKey(t, got, second)
	if got.Blocked || got.ExpiresAt != nil || got.Models != nil || got.Scope != "" {
		t.Errorf("replacing a key left fields from the old one: %+v", got)
	}

	keys, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("List returned %d keys, want 1: a replace inserted a second row", len(keys))
	}
}

func TestPostgresStoreDelete(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Revoking a key that is not there is how a retried revocation, or a second
	// operator clicking the same button, must read.
	if err := store.Delete(ctx, "hash-never-stored"); err != nil {
		t.Fatalf("Delete of an absent key: %v", err)
	}

	key := sampleKey("hash-to-delete")
	if err := store.Put(ctx, key); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.Delete(ctx, key.Hash); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, key.Hash); !errors.Is(err, core.ErrKeyInvalid) {
		t.Fatalf("Get after Delete = %v, want core.ErrKeyInvalid", err)
	}
	if err := store.Delete(ctx, key.Hash); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestPostgresStoreList(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	empty, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("List on a fresh store returned %d keys", len(empty))
	}

	base := time.Now().UTC().Truncate(time.Microsecond)
	for i := range 5 {
		k := sampleKey(fmt.Sprintf("hash-list-%d", i))
		k.CreatedAt = base.Add(time.Duration(i) * time.Second)
		if err := store.Put(ctx, k); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	keys, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 5 {
		t.Fatalf("List returned %d keys, want 5", len(keys))
	}
	for i, k := range keys {
		if want := fmt.Sprintf("hash-list-%d", i); k.Hash != want {
			t.Errorf("keys[%d].Hash = %q, want %q (List is ordered by created_at)", i, k.Hash, want)
		}
		if k.Alias != "every field" {
			t.Errorf("keys[%d] came back partially scanned: %+v", i, k)
		}
	}
}

// The point of the whole store: only the hash is written. A plaintext key in
// this table would be a credential an operator, a backup or a replica could
// read back.
func TestPostgresStoreWritesNoPlaintext(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	plaintext, hash, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := store.Put(ctx, &core.Key{Hash: hash, Alias: "issued"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var dump string
	if err := store.pool.QueryRow(ctx,
		"SELECT coalesce(string_agg(virtual_keys::text, ' '), '') FROM virtual_keys").Scan(&dump); err != nil {
		t.Fatalf("read the table back: %v", err)
	}
	if strings.Contains(dump, plaintext) {
		t.Fatal("the plaintext key was written to the database")
	}
	if !strings.Contains(dump, hash) {
		t.Fatal("the key hash was not written to the database")
	}
}

// Two instances start against one database on every rollout, and a schema that
// only applies cleanly once would make the second one fail to boot.
func TestPostgresStoreSchemaIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var version int
	if err := store.pool.QueryRow(ctx, "SELECT max(version) FROM virtual_keys_schema").Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != len(migrations) {
		t.Errorf("schema version = %d, want %d", version, len(migrations))
	}

	var rows int
	if err := store.pool.QueryRow(ctx, "SELECT count(*) FROM virtual_keys_schema").Scan(&rows); err != nil {
		t.Fatalf("count schema rows: %v", err)
	}
	if rows != len(migrations) {
		t.Errorf("virtual_keys_schema has %d rows after two migrations, want %d", rows, len(migrations))
	}
}

// Authentication is concurrent by definition: every in-flight request reads
// this store, while /key/generate and an SSO login write to it.
func TestPostgresStoreConcurrentAccess(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	const writers = 8
	// One key value, written by every worker, so the row's contents are the
	// same whichever writer won and the assertion at the end has something
	// stable to compare against. The workers only read it.
	contended := sampleKey("hash-contended")
	if err := store.Put(ctx, contended); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, writers*4)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			own := fmt.Sprintf("hash-worker-%d", w)
			for range 10 {
				if err := store.Put(ctx, sampleKey(own)); err != nil {
					errs <- fmt.Errorf("put own: %w", err)
					return
				}
				// Every worker rewrites the same row as well, so the ON
				// CONFLICT path is exercised under contention rather than
				// only in isolation.
				if err := store.Put(ctx, contended); err != nil {
					errs <- fmt.Errorf("put shared: %w", err)
					return
				}
				if _, err := store.Get(ctx, contended.Hash); err != nil {
					errs <- fmt.Errorf("get shared: %w", err)
					return
				}
				if _, err := store.List(ctx); err != nil {
					errs <- fmt.Errorf("list: %w", err)
					return
				}
				if err := store.Delete(ctx, own); err != nil {
					errs <- fmt.Errorf("delete own: %w", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	got, err := store.Get(ctx, contended.Hash)
	if err != nil {
		t.Fatalf("Get after contention: %v", err)
	}
	assertSameKey(t, got, contended)
}

// A store that cannot be reached must refuse to be built, because a gateway
// that starts without keys 401s every caller holding a valid one.
func TestNewPostgresStoreFailsOnAnUnreachableDatabase(t *testing.T) {
	ctx := context.Background()
	// Port 1 is reserved and never listening.
	_, err := NewPostgresStore(ctx, PostgresOptions{
		DSN:     "postgres://postgres:postgres@127.0.0.1:1/postgres?sslmode=disable",
		Timeout: time.Second,
	}, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("NewPostgresStore succeeded against a database that is not there")
	}
}

// The DSN carries a password, so a parse failure must not quote it back into a
// log line.
func TestNewPostgresStoreDoesNotEchoTheDSN(t *testing.T) {
	dsn := "postgres://user:hunter2@%zz/db"
	_, err := NewPostgresStore(context.Background(), PostgresOptions{DSN: dsn}, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("an unparseable dsn was accepted")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("the dsn's password reached the error message: %v", err)
	}
}
