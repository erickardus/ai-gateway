package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgMigrations are the schema statements, in order, one statement each. Index+1
// is the version a statement records; a released statement is never edited,
// only followed by another.
//
// The append-only rule is the same one auth.PostgresStore states and for the
// same reason — a rolling upgrade runs the old binary and the new one against
// this table for as long as it takes — with one addition peculiar to this
// table: a record's hash covers its JSON, so anything that rewrites stored
// records invalidates every chain ever written. A migration here may add
// columns beside the record. It may never touch one.
var pgMigrations = []string{
	// Column choices worth stating:
	//
	//   record  the sealed JSON line, byte for byte as the file sink would have
	//           written it. It is the record; everything else in the row is an
	//           index into it. text rather than jsonb, and that is load-bearing
	//           rather than lazy: jsonb normalizes whitespace and reorders keys,
	//           so a record stored as jsonb would come back as different bytes
	//           and hash to something else. Query it with record::jsonb, which
	//           costs a parse per row on a table nobody reads at speed.
	//   seq     the primary key, so the database refuses a duplicate sequence
	//           even if this process somehow offered one, and so ordering is an
	//           index scan.
	//   at      duplicated out of the record for the one query an auditor
	//           actually starts with — a date range — and indexed below.
	`CREATE TABLE IF NOT EXISTS gateway_audit (
		seq    bigint PRIMARY KEY,
		at     timestamptz NOT NULL,
		record text NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS gateway_audit_at_idx ON gateway_audit (at)`,
	// Append-only, enforced by the database rather than by convention.
	//
	// It is not a substitute for the hash chain and does not pretend to be: a
	// superuser can drop this trigger, and the chain is what detects it if they
	// do. What it stops is everything below that bar — a migration tool with a
	// DELETE in it, a retention script pointed at the wrong table, an operator
	// tidying up a row they think is noise. Those are how audit logs actually
	// lose records, far more often than anybody attacks one.
	//
	// Pair it with a grant: the gateway's own role needs INSERT and SELECT and
	// nothing else. See docs/audit.md.
	`CREATE OR REPLACE FUNCTION gateway_audit_append_only() RETURNS trigger AS $$
	BEGIN
		RAISE EXCEPTION 'gateway_audit is append-only: % is not permitted on it', TG_OP
			USING HINT = 'archive the chain instead; see docs/audit.md';
	END;
	$$ LANGUAGE plpgsql`,
	// Dropped first so the pair is idempotent on every Postgres version, rather
	// than only on those with CREATE OR REPLACE TRIGGER.
	`DROP TRIGGER IF EXISTS gateway_audit_no_change ON gateway_audit`,
	`CREATE TRIGGER gateway_audit_no_change
		BEFORE UPDATE OR DELETE ON gateway_audit
		FOR EACH ROW EXECUTE FUNCTION gateway_audit_append_only()`,
	// TRUNCATE is not an UPDATE or a DELETE and would otherwise walk straight
	// past the trigger above, taking the whole chain with it.
	`DROP TRIGGER IF EXISTS gateway_audit_no_truncate ON gateway_audit`,
	`CREATE TRIGGER gateway_audit_no_truncate
		BEFORE TRUNCATE ON gateway_audit
		FOR EACH STATEMENT EXECUTE FUNCTION gateway_audit_append_only()`,
}

// pgSchemaTable records which migrations have run, for the same reasons the key
// store keeps one: the migration that is not idempotent has not been written
// yet, and an operator should be able to ask a database which schema it is on.
const pgSchemaTable = `CREATE TABLE IF NOT EXISTS gateway_audit_schema (
	version    integer PRIMARY KEY,
	applied_at timestamptz NOT NULL
)`

const (
	// pgMigrationLockID namespaces the advisory lock the schema is applied
	// under, so replicas starting together apply it once between them.
	pgMigrationLockID int64 = 0x6777617564697400 // "gwaudit\0"
	// pgAppendLockID serializes appends across the whole fleet.
	//
	// This is what makes one chain out of many writers. Each append takes the
	// lock for its transaction, reads the tip, seals a record onto it and
	// commits, so two replicas cannot both claim the same sequence number and
	// no reader ever sees a gap.
	//
	// Row locks were tried in the design and are wrong here: SELECT ... FOR
	// UPDATE on the tail row blocks the second writer, then hands it back the
	// row it locked rather than the newer one the first writer inserted, so it
	// computes a sequence that is already taken. The advisory lock has no such
	// hazard, and costs one round trip on a path that carries a handful of
	// requests a minute at its very busiest.
	pgAppendLockID int64 = 0x6777617564697401 // "gwaudit\1"
)

const (
	// pgMigrateTimeout bounds schema setup, which may wait behind another
	// replica running the same migration.
	pgMigrateTimeout = 30 * time.Second
	// pgFallbackTimeout and pgFallbackMaxConns keep a zero-valued
	// PostgresOptions from meaning "no timeout" and "one connection". The
	// operator-facing defaults live in config.
	pgFallbackTimeout  = 5 * time.Second
	pgFallbackMaxConns = 4
	// pgBootVerifyRecords is how much of the chain is verified at startup.
	//
	// The file sink verifies all of it, which it can afford: the file is local
	// and holds one host's records. A shared chain holds the fleet's, and the
	// volume is not what the package comment's "thousands, not millions"
	// assumed — SSO renewals are audited, so a few hundred developers produce
	// a few hundred thousand records a year. Reading all of them across a
	// network on every pod start would make booting cost more the longer the
	// gateway has been trusted, which is a bad trade for a check that a person
	// running `gateway -verify-audit` makes properly.
	//
	// What this window does catch is the tampering that matters at boot:
	// something that happened recently, to the part of the chain this instance
	// is about to append to. Every append then re-checks the tip, so the newest
	// link is verified continuously rather than once.
	pgBootVerifyRecords = 1000
)

// PostgresOptions configures the sink.
type PostgresOptions struct {
	// DSN is a libpq connection string or URL. It carries a password, so it
	// comes from the environment rather than from the config file.
	DSN string
	// Timeout bounds each append. It is generous next to the key store's
	// because nothing here is on the inference path: the caller is an operator
	// minting a key, and a slow record is better than a refused action.
	Timeout time.Duration
	// MaxConns caps the pool. Appends serialize on an advisory lock anyway, so
	// this bounds connections held rather than throughput; it is small so that
	// a fleet of replicas does not spend a database's connection limit on a
	// table it writes to a few times an hour.
	MaxConns int32
	// Instance names this gateway in the records it writes, and is stamped into
	// every record's detail. Defaults to the hostname.
	//
	// It matters only here. A file chain is one host's by construction, and a
	// stdout chain is labelled by whatever collects it; a shared chain is the
	// one place where "which replica did this" is a real question and nothing
	// else in the record answers it.
	Instance string
}

// PostgresSink appends records to one chain shared by every instance of the
// gateway.
//
// It exists because the file sink is per host, which on a fleet is worse than
// it sounds. Several replicas keep several unrelated chains, each starting at
// sequence 1, with no ordering between them and no way to tell a chain that was
// deleted from one that never existed — and on an orchestrator, each of those
// chains lives on a container filesystem that is discarded when the pod is
// rescheduled. The evidence a compliance process asks for is then spread across
// hosts that no longer exist.
//
// One chain in a database fixes all of that at the cost of one advisory lock
// per administrative action, and administrative actions are rare. Durability is
// the commit: Postgres has already fsynced the write-ahead log by the time
// Commit returns, which is the same guarantee FileSink buys with its own fsync
// and the reason a record is safe to act on once this returns.
type PostgresSink struct {
	state
	pool     *pgxpool.Pool
	timeout  time.Duration
	instance string
	log      *slog.Logger
	now      func() time.Time
}

// OpenPostgres connects, applies the schema, verifies the tip of the chain and
// returns a sink positioned to continue it.
//
// The two failures are answered differently, which is the whole posture of this
// package in one function. A database that cannot be reached or migrated
// returns an error, and the gateway refuses to start: that is transient, a
// restart is how an orchestrator retries it, and coming up regardless would
// mean administering a gateway with nothing recording it. A chain that does not
// verify returns a *sealed* sink and no error: that is not transient, no restart
// will fix it, and taking every replica down until a person archives the chain
// would stop inference to protect a record that is already broken.
func OpenPostgres(ctx context.Context, opts PostgresOptions, log *slog.Logger) (*PostgresSink, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = pgFallbackTimeout
	}
	if opts.MaxConns <= 0 {
		opts.MaxConns = pgFallbackMaxConns
	}

	cfg, err := pgxpool.ParseConfig(opts.DSN)
	if err != nil {
		// The DSN carries a password and the driver's message quotes the string
		// it failed to parse, so it is not repeated.
		return nil, errors.New("audit log: the postgres dsn could not be parsed")
	}
	cfg.MaxConns = opts.MaxConns
	cfg.ConnConfig.ConnectTimeout = opts.Timeout

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("audit log: connect to postgres: %w", err)
	}

	s := &PostgresSink{
		pool:     pool,
		timeout:  opts.Timeout,
		instance: opts.Instance,
		log:      log,
		now:      time.Now,
	}
	if s.instance == "" {
		s.instance = hostInstance()
	}

	if err := s.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("audit log: postgres unreachable at startup: %w", err)
	}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}

	summary, err := s.checkTail(ctx)
	var broken *BreakError
	switch {
	case err == nil:
		if log != nil {
			log.Info("audit log backed by postgres; one chain is shared by every instance",
				"host", cfg.ConnConfig.Host, "database", cfg.ConnConfig.Database,
				"instance", s.instance, "last_seq", summary.LastSeq,
				"verified_records", summary.Records, "schema_version", len(pgMigrations))
		}
	case errors.As(err, &broken):
		s.seal(err.Error())
	default:
		pool.Close()
		return nil, err
	}
	return s, nil
}

// migrate brings the schema up to date, under an advisory lock so that replicas
// starting together apply it once between them.
func (s *PostgresSink) migrate(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, pgMigrateTimeout)
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("audit log: begin schema transaction: %w", err)
	}
	// Rollback after a successful commit is a no-op, so this needs no branch.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", pgMigrationLockID); err != nil {
		return fmt.Errorf("audit log: take schema lock: %w", err)
	}
	if _, err := tx.Exec(ctx, pgSchemaTable); err != nil {
		return fmt.Errorf("audit log: create schema table: %w", err)
	}

	var applied int
	if err := tx.QueryRow(ctx, "SELECT coalesce(max(version), 0) FROM gateway_audit_schema").Scan(&applied); err != nil {
		return fmt.Errorf("audit log: read schema version: %w", err)
	}
	for i, stmt := range pgMigrations {
		version := i + 1
		if version <= applied {
			continue
		}
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("audit log: apply schema version %d: %w", version, err)
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO gateway_audit_schema (version, applied_at) VALUES ($1, now())", version); err != nil {
			return fmt.Errorf("audit log: record schema version %d: %w", version, err)
		}
		if s.log != nil {
			s.log.Info("audit log schema applied", "version", version)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("audit log: commit schema: %w", err)
	}
	return nil
}

// Ping reports whether the database is reachable.
func (s *PostgresSink) Ping(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, s.timeout)
	defer cancel()
	return s.pool.Ping(ctx)
}

// Close releases the pool.
func (s *PostgresSink) Close() error {
	return s.shut(func() error {
		s.pool.Close()
		return nil
	})
}

// Record seals an event onto the shared chain and returns once it is committed.
//
// The whole operation is one transaction holding the fleet's append lock: take
// the lock, read the tip and check it still verifies, seal a record onto it,
// insert, commit. Nothing between those steps can interleave with another
// instance, which is what makes the sequence numbers a single order rather than
// each replica's opinion of one.
func (s *PostgresSink) Record(parent context.Context, e Event) (Record, error) {
	if err := s.admit(); err != nil {
		return Record{}, err
	}
	e.Detail = withInstance(e.Detail, s.instance)

	ctx, cancel := context.WithTimeout(parent, s.timeout)
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Record{}, fmt.Errorf("audit log: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", pgAppendLockID); err != nil {
		return Record{}, fmt.Errorf("audit log: take the append lock: %w", err)
	}

	seq, prev, err := s.tip(ctx, tx)
	if err != nil {
		// A tip that does not verify is not a failed write to retry; it is a
		// broken chain, and this instance stops appending to it here rather
		// than at the next restart.
		var broken *BreakError
		if errors.As(err, &broken) {
			s.seal(err.Error())
		}
		return Record{}, err
	}

	rec, line, err := sealAt(seq+1, prev, s.now().UTC(), e)
	if err != nil {
		return Record{}, err
	}
	// The stored bytes are the sealed line without its newline: a file needs
	// the separator, a row is already one record.
	if _, err := tx.Exec(ctx,
		"INSERT INTO gateway_audit (seq, at, record) VALUES ($1, $2, $3)",
		int64(rec.Seq), rec.At, string(bytes.TrimSuffix(line, []byte("\n")))); err != nil {
		return Record{}, fmt.Errorf("audit log: insert record: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Record{}, fmt.Errorf("audit log: commit record: %w", err)
	}
	return rec, nil
}

// tip returns the position the next record takes, having checked that the
// record it is about to chain onto still verifies.
//
// Two rows rather than one, because one row can only be checked against its own
// hash: the second gives the link between them, so a record deleted or replaced
// at the end of the chain is caught by the next append rather than waiting for
// somebody to run a verification. The cost is reading two short rows inside a
// transaction that is open anyway.
func (s *PostgresSink) tip(ctx context.Context, tx pgx.Tx) (uint64, string, error) {
	raws, err := scanRecords(ctx, tx, `
		SELECT record FROM (
			SELECT seq, record FROM gateway_audit ORDER BY seq DESC LIMIT 2
		) newest ORDER BY seq`)
	if err != nil {
		return 0, "", fmt.Errorf("audit log: read the chain tip: %w", err)
	}
	if len(raws) == 0 {
		return 0, "", nil
	}
	v := &verifier{origin: false}
	for i, raw := range raws {
		if err := v.push(i+1, raw); err != nil {
			return 0, "", err
		}
	}
	return v.summary.LastSeq, v.summary.LastHash, nil
}

// checkTail verifies the newest records at startup. See pgBootVerifyRecords for
// why it is a window rather than the whole chain.
func (s *PostgresSink) checkTail(parent context.Context) (Summary, error) {
	ctx, cancel := context.WithTimeout(parent, s.timeout)
	defer cancel()

	raws, err := scanRecords(ctx, s.pool, `
		SELECT record FROM (
			SELECT seq, record FROM gateway_audit ORDER BY seq DESC LIMIT $1
		) newest ORDER BY seq`, pgBootVerifyRecords)
	if err != nil {
		return Summary{}, fmt.Errorf("audit log: read the newest records: %w", err)
	}
	v := &verifier{origin: false}
	for i, raw := range raws {
		if err := v.push(i+1, raw); err != nil {
			return v.summary, err
		}
	}
	return v.summary, nil
}

// Verify walks the whole chain in the database.
//
// Unbounded by design, and not on any startup path: this is what an auditor
// runs, and what a scheduled job runs nightly against the same table the
// gateway is writing to. Reading committed rows while appends continue is safe
// — a row is never modified once written, so the walk sees a prefix of the
// chain and verifies it as one.
func (s *PostgresSink) Verify(ctx context.Context) (Summary, error) {
	return verifyRows(ctx, s.pool)
}

// VerifyPostgres verifies a chain in a database from a DSN alone, for
// `gateway -verify-audit`. It applies no schema and writes nothing: verifying
// somebody else's archive must not create anything in it.
func VerifyPostgres(ctx context.Context, dsn string) (Summary, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return Summary{}, errors.New("the postgres dsn could not be parsed")
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return Summary{}, fmt.Errorf("connect to postgres: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return Summary{}, fmt.Errorf("postgres unreachable: %w", err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('gateway_audit') IS NOT NULL").Scan(&exists); err != nil {
		return Summary{}, fmt.Errorf("look for the audit table: %w", err)
	}
	if !exists {
		return Summary{}, errors.New("this database has no gateway_audit table, so no chain has ever been written to it")
	}
	return verifyRows(ctx, pool)
}

// PostgresTarget names a database for an operator-facing line, without its
// credentials. A DSN that cannot be parsed yields a placeholder rather than an
// error: the caller is about to report a failure that says more than this does.
func PostgresTarget(dsn string) string {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return "postgres"
	}
	return fmt.Sprintf("postgres %s/%s", cfg.ConnConfig.Host, cfg.ConnConfig.Database)
}

// verifyRows walks every record in seq order.
func verifyRows(ctx context.Context, pool *pgxpool.Pool) (Summary, error) {
	rows, err := pool.Query(ctx, "SELECT record FROM gateway_audit ORDER BY seq")
	if err != nil {
		return Summary{}, fmt.Errorf("read the audit chain: %w", err)
	}
	defer rows.Close()

	v := &verifier{origin: true}
	pos := 0
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return v.summary, fmt.Errorf("read the audit chain: %w", err)
		}
		pos++
		if err := v.push(pos, []byte(raw)); err != nil {
			return v.summary, err
		}
	}
	if err := rows.Err(); err != nil {
		return v.summary, fmt.Errorf("read the audit chain: %w", err)
	}
	return v.summary, v.done()
}

// querier is the part of pgx a read needs, so the same helper serves a pool and
// a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func scanRecords(ctx context.Context, q querier, sql string, args ...any) ([][]byte, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out [][]byte
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		out = append(out, []byte(raw))
	}
	return out, rows.Err()
}

// withInstance stamps the writing instance into a record's detail.
//
// The caller's map is copied rather than written to. Handlers build an event
// once and may record it twice — the write-ahead record and the failure record
// that follows it — and a sink that mutated what it was handed would be
// reaching back into its caller's next call.
func withInstance(d map[string]string, instance string) map[string]string {
	if instance == "" {
		return d
	}
	out := make(map[string]string, len(d)+1)
	for k, v := range d {
		out[k] = v
	}
	out["instance"] = instance
	return out
}

// hostInstance names this process by its hostname, which on an orchestrator is
// the pod name — the identifier every other tool an operator will reach for is
// already keyed by.
func hostInstance() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}

var _ Sink = (*PostgresSink)(nil)
