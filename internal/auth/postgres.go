package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrations are the schema statements, in order. Index+1 is the version each
// one records; a statement that has been released is never edited, only
// followed by another.
//
// Operators here have no migration tool — the gateway is one binary and a YAML
// file — so it brings its own. That makes the ordering rule load-bearing rather
// than a convention: a fleet is upgraded one replica at a time, so an old
// binary and a new one run against the same table for as long as the rollout
// takes. Every future statement must therefore be additive (a nullable or
// defaulted column, an index), so the previous version keeps working against
// it, and must be idempotent, so an instance that dies mid-migration leaves
// nothing for the next one to trip over.
var migrations = []string{
	// Column choices worth stating:
	//
	//   hash               the primary key, and the only representation of a
	//                      credential this table holds. The plaintext key
	//                      exists once, in the response that issued it, and is
	//                      never written here — exactly as the file store
	//                      behaves.
	//   models             text[] rather than a delimited string, so a model
	//                      name containing the delimiter cannot be a parsing
	//                      bug, and so an operator can query the table.
	//   budget_duration_ns bigint nanoseconds, which is what time.Duration is.
	//                      interval would be the idiomatic type and the wrong
	//                      one: it rounds to microseconds and carries months
	//                      and days, whose length depends on when they are
	//                      measured from — a budget window that is not a fixed
	//                      quantity of time.
	//   created_at,        timestamptz. Postgres stores microseconds, so a
	//   expires_at         timestamp round-trips truncated rather than exact.
	//                      That is the one field of core.Key this store does
	//                      not return bit-identical, and it is the right trade:
	//                      the alternative is another bigint nobody can read in
	//                      psql, to preserve a resolution no expiry check uses.
	//
	// Text columns are NOT NULL DEFAULT '' rather than nullable: core.Key has
	// no nil string, so a NULL here could only mean a row this gateway did not
	// write. expires_at is the exception because *time.Time genuinely has two
	// states — "expires then" and "does not expire".
	`CREATE TABLE IF NOT EXISTS virtual_keys (
		hash               text PRIMARY KEY,
		alias              text NOT NULL DEFAULT '',
		models             text[] NOT NULL DEFAULT '{}',
		rpm_limit          integer NOT NULL DEFAULT 0,
		tpm_limit          integer NOT NULL DEFAULT 0,
		allow_passthrough  boolean NOT NULL DEFAULT false,
		blocked            boolean NOT NULL DEFAULT false,
		created_at         timestamptz NOT NULL,
		expires_at         timestamptz,
		max_budget         double precision NOT NULL DEFAULT 0,
		budget_duration_ns bigint NOT NULL DEFAULT 0,
		subject            text NOT NULL DEFAULT '',
		device             text NOT NULL DEFAULT '',
		scope              text NOT NULL DEFAULT ''
	)`,
	// Offboarding is the one lookup that is not by hash: revoking a person
	// means finding every key issued to their subject, across the laptops they
	// signed in from. It goes through List today, so this index buys nothing
	// yet; it exists because the column it covers is the one an operator
	// reaches for by hand, in psql, on the day someone leaves.
	`CREATE INDEX IF NOT EXISTS virtual_keys_subject_idx ON virtual_keys (subject) WHERE subject <> ''`,
}

// schemaTable records which migrations have run. It is not strictly needed
// while every statement above is idempotent, and exists for the migration that
// will not be — a backfill, a rewrite of a column's meaning — plus the plain
// operational value of being able to ask a database which schema it is on.
const schemaTable = `CREATE TABLE IF NOT EXISTS virtual_keys_schema (
	version    integer PRIMARY KEY,
	applied_at timestamptz NOT NULL
)`

// migrationLockID namespaces the advisory lock migrations are applied under.
// Several replicas starting at once would otherwise race on CREATE TABLE IF NOT
// EXISTS, which is not as safe as it reads: concurrent creations of the same
// table deadlock or raise a duplicate-object error rather than one winning
// quietly. The lock is taken for the transaction, so it is released by the
// commit whether or not the migration succeeded, and a crashed instance's lock
// dies with its connection.
//
// The value is arbitrary but must be stable: it is chosen from this table's
// name so a second application sharing the database picks a different one.
const migrationLockID int64 = 0x7669726b65797300 // "virkeys\0"

// migrateTimeout bounds schema setup. It is far longer than the per-query
// timeout because it is not a per-query operation: it may wait behind another
// replica running the same migration, and it happens once, at boot, where a
// spurious failure means a gateway that refuses to start.
const migrateTimeout = 30 * time.Second

// Fallbacks for a store built directly rather than from configuration, which
// supplies both. config.DefaultKeyStoreTimeout and config.DefaultKeyStoreMaxConns
// are where an operator-facing default belongs; these keep a zero-valued
// PostgresOptions from meaning "no timeout" and "one connection".
const (
	fallbackTimeout  = 2 * time.Second
	fallbackMaxConns = 10
)

// keyColumns is the column list every read shares, in the order scanKey expects.
const keyColumns = `hash, alias, models, rpm_limit, tpm_limit, allow_passthrough,
	blocked, created_at, expires_at, max_budget, budget_duration_ns, subject, device, scope`

// PostgresOptions configures the store.
type PostgresOptions struct {
	// DSN is a libpq connection string or URL. It carries a password, so it
	// comes from the environment rather than from the config file.
	DSN string
	// Timeout bounds each query. Unlike rstate's, it is not sized to be
	// negligible on the request path — there is nothing to degrade to, so a
	// query that would be abandoned is a request that cannot be authenticated.
	// It is sized instead to ride out a reconnect or a GC pause while still
	// failing long before the router's own timeout.
	Timeout time.Duration
	// MaxConns caps the pool. A key lookup is a primary-key read that returns
	// in about a millisecond, so this bounds concurrent authentication, not
	// throughput.
	MaxConns int32
}

// PostgresStore persists virtual keys in Postgres, so that every replica of the
// gateway authenticates against one set of keys.
//
// It exists because the file store is single-node in a way that is easy to miss:
// two instances pointed at one path do not share it, they take turns
// overwriting it, and a key issued on instance A is deleted by the next write
// from instance B. Anything that mints keys at runtime — /key/generate, an SSO
// login, a renewal — is therefore unusable behind a load balancer without this.
//
// Connection failure is handled the opposite way to rstate. Redis going away
// degrades the gateway to per-instance limits, which is worse service but still
// service; the key store going away means no request can be authenticated at
// all, and there is no weaker answer to fall back to. Serving from a stale
// in-memory copy would be worse than failing: it would keep honouring keys an
// operator had just revoked, which is precisely the request that must not
// survive an outage. So the posture is:
//
//   - unreachable at boot: the gateway refuses to start, loudly, rather than
//     coming up as an endpoint that 401s every caller and looks like a
//     credential problem to everyone holding a valid key;
//   - unreachable later: requests fail and /health/readiness reports the
//     instance unready, so a load balancer takes it out of rotation and puts
//     it back when the database returns.
type PostgresStore struct {
	pool    *pgxpool.Pool
	timeout time.Duration
	log     *slog.Logger
}

// NewPostgresStore connects, applies the schema and verifies the database is
// usable before returning. It connects eagerly on purpose: a store that cannot
// be reached is a gateway that cannot authenticate, and the moment to discover
// that is at startup, in front of the operator deploying it, rather than on the
// first request after a rollout has replaced every healthy replica.
func NewPostgresStore(ctx context.Context, opts PostgresOptions, log *slog.Logger) (*PostgresStore, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = fallbackTimeout
	}
	if opts.MaxConns <= 0 {
		opts.MaxConns = fallbackMaxConns
	}

	cfg, err := pgxpool.ParseConfig(opts.DSN)
	if err != nil {
		// The DSN carries a password, so the driver's message is not repeated:
		// it quotes the string it failed to parse.
		return nil, errors.New("key store: the postgres dsn could not be parsed")
	}
	cfg.MaxConns = opts.MaxConns
	cfg.ConnConfig.ConnectTimeout = opts.Timeout

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("key store: connect to postgres: %w", err)
	}

	s := &PostgresStore{pool: pool, timeout: opts.Timeout, log: log}
	if err := s.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("key store: postgres unreachable at startup: %w", err)
	}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if log != nil {
		log.Info("key store backed by postgres; keys are shared across instances",
			"host", cfg.ConnConfig.Host, "database", cfg.ConnConfig.Database,
			"schema_version", len(migrations))
	}
	return s, nil
}

// migrate brings the schema up to date, under an advisory lock so that replicas
// starting together apply it once between them.
func (s *PostgresStore) migrate(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, migrateTimeout)
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("key store: begin schema transaction: %w", err)
	}
	// Rollback after a successful commit is a no-op, so this needs no branch.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("key store: take schema lock: %w", err)
	}
	if _, err := tx.Exec(ctx, schemaTable); err != nil {
		return fmt.Errorf("key store: create schema table: %w", err)
	}

	var applied int
	if err := tx.QueryRow(ctx, "SELECT coalesce(max(version), 0) FROM virtual_keys_schema").Scan(&applied); err != nil {
		return fmt.Errorf("key store: read schema version: %w", err)
	}
	for i, stmt := range migrations {
		version := i + 1
		if version <= applied {
			continue
		}
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("key store: apply schema version %d: %w", version, err)
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO virtual_keys_schema (version, applied_at) VALUES ($1, now())", version); err != nil {
			return fmt.Errorf("key store: record schema version %d: %w", version, err)
		}
		if s.log != nil {
			s.log.Info("key store schema applied", "version", version)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("key store: commit schema: %w", err)
	}
	return nil
}

// Ping reports whether the database is reachable. /health/readiness reaches the
// same conclusion through List today; this is the cheap version of the same
// question, for a caller that only wants the answer.
func (s *PostgresStore) Ping(parent context.Context) error {
	ctx, cancel := s.ctx(parent)
	defer cancel()
	return s.pool.Ping(ctx)
}

// Close releases the pool. It returns an error only to satisfy io.Closer, which
// is how the gateway closes whichever store it built without knowing its type.
func (s *PostgresStore) Close() error {
	s.pool.Close()
	return nil
}

func (s *PostgresStore) ctx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, s.timeout)
}

// Get implements KeyStore.
func (s *PostgresStore) Get(parent context.Context, hash string) (*core.Key, error) {
	ctx, cancel := s.ctx(parent)
	defer cancel()

	row := s.pool.QueryRow(ctx, "SELECT "+keyColumns+" FROM virtual_keys WHERE hash = $1", hash)
	key, err := scanKey(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, core.ErrKeyInvalid
	}
	if err != nil {
		// The hash is not the credential, but it is the lookup value for one,
		// and this error reaches a log line rather than the caller. Naming the
		// operation is enough to place the failure.
		return nil, fmt.Errorf("key store: get key: %w", err)
	}
	return key, nil
}

// Put implements KeyStore. The row is replaced rather than merged, matching
// MemStore: a caller editing one field reads the key, changes it and writes the
// whole thing back, so a merge here would make a cleared field impossible to
// express.
func (s *PostgresStore) Put(parent context.Context, key *core.Key) error {
	if key == nil || key.Hash == "" {
		return fmt.Errorf("put key: hash is required")
	}
	ctx, cancel := s.ctx(parent)
	defer cancel()

	createdAt := key.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	models := key.Models
	if models == nil {
		// text[] has no nil, and a NULL column would mean "no restriction"
		// twice over. The empty array is the one representation.
		models = []string{}
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO virtual_keys (`+keyColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (hash) DO UPDATE SET
			alias = EXCLUDED.alias,
			models = EXCLUDED.models,
			rpm_limit = EXCLUDED.rpm_limit,
			tpm_limit = EXCLUDED.tpm_limit,
			allow_passthrough = EXCLUDED.allow_passthrough,
			blocked = EXCLUDED.blocked,
			created_at = EXCLUDED.created_at,
			expires_at = EXCLUDED.expires_at,
			max_budget = EXCLUDED.max_budget,
			budget_duration_ns = EXCLUDED.budget_duration_ns,
			subject = EXCLUDED.subject,
			device = EXCLUDED.device,
			scope = EXCLUDED.scope`,
		key.Hash, key.Alias, models, key.RPMLimit, key.TPMLimit, key.AllowPassthrough,
		key.Blocked, createdAt, key.ExpiresAt, key.MaxBudget, int64(key.BudgetDuration),
		key.Subject, key.Device, key.Scope)
	if err != nil {
		return fmt.Errorf("key store: put key: %w", err)
	}
	return nil
}

// Delete implements KeyStore. Deleting an absent key affects no rows and is not
// an error, so revoking twice — from two operators, or from a retried request —
// reads the same as revoking once.
func (s *PostgresStore) Delete(parent context.Context, hash string) error {
	ctx, cancel := s.ctx(parent)
	defer cancel()

	if _, err := s.pool.Exec(ctx, "DELETE FROM virtual_keys WHERE hash = $1", hash); err != nil {
		return fmt.Errorf("key store: delete key: %w", err)
	}
	return nil
}

// List implements KeyStore. The order is fixed rather than incidental: this
// feeds the console's key table, which would otherwise reshuffle on every poll,
// and a stable order costs nothing on a table this size.
func (s *PostgresStore) List(parent context.Context) ([]*core.Key, error) {
	ctx, cancel := s.ctx(parent)
	defer cancel()

	rows, err := s.pool.Query(ctx, "SELECT "+keyColumns+" FROM virtual_keys ORDER BY created_at, hash")
	if err != nil {
		return nil, fmt.Errorf("key store: list keys: %w", err)
	}
	defer rows.Close()

	out := make([]*core.Key, 0)
	for rows.Next() {
		key, err := scanKey(rows)
		if err != nil {
			return nil, fmt.Errorf("key store: list keys: %w", err)
		}
		out = append(out, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("key store: list keys: %w", err)
	}
	return out, nil
}

// scanKey reads one row into a core.Key. Both Get and List go through it so a
// column added to one cannot be forgotten in the other.
func scanKey(row pgx.Row) (*core.Key, error) {
	var (
		k        core.Key
		models   []string
		duration int64
	)
	if err := row.Scan(&k.Hash, &k.Alias, &models, &k.RPMLimit, &k.TPMLimit,
		&k.AllowPassthrough, &k.Blocked, &k.CreatedAt, &k.ExpiresAt, &k.MaxBudget,
		&duration, &k.Subject, &k.Device, &k.Scope); err != nil {
		return nil, err
	}
	// An empty array comes back as an empty slice, and a key stored with no
	// model restriction had nil. The difference is invisible to every caller —
	// both mean "any model" — but it is visible to a test comparing a key with
	// the one it stored, and to the JSON the console reads, where `omitempty`
	// erases an empty slice and not an empty non-nil one.
	if len(models) > 0 {
		k.Models = models
	}
	k.BudgetDuration = time.Duration(duration)
	// pgx returns timestamptz in the session's zone, which is the server's
	// rather than anything this process chose. Every other producer of a
	// core.Key uses UTC, and a key that came back in a different location would
	// compare unequal to the one that was stored while representing the same
	// instant.
	k.CreatedAt = k.CreatedAt.UTC()
	if k.ExpiresAt != nil {
		utc := k.ExpiresAt.UTC()
		k.ExpiresAt = &utc
	}
	return &k, nil
}

var _ KeyStore = (*PostgresStore)(nil)
