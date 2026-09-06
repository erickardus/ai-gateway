package spend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgMigrations are the schema statements, in order, one statement each.
// Index+1 is the version a statement records; a released statement is never
// edited, only followed by another, because a rolling upgrade runs the old
// binary and the new one against these tables for as long as it takes.
var pgMigrations = []string{
	// One row per completed request. This is the record a chargeback is built
	// from and an export reads; spend_daily below is derived from it.
	//
	// Column choices worth stating:
	//
	//   scope    the innermost project, team or organisation, denormalized out
	//            of scopes so the common filter is an index lookup rather than
	//            an array containment test.
	//   scopes   the whole chain, innermost first. Both are stored because a
	//            request is charged to every level at once, and a report at the
	//            organisation level must find rows whose innermost scope is a
	//            project three levels down.
	//   cost     double precision, matching spend.Totals rather than numeric.
	//            Summing a year of a large fleet's per-request costs in float64
	//            accumulates a relative error around 1e-13, which is several
	//            orders of magnitude below the cent this is reported in — and
	//            the alternative is a type every caller has to convert at both
	//            ends to fix an error nobody can observe.
	//   billable separates "free" from "unpriced", which a bare zero cost
	//            cannot, and is what makes passthrough traffic legible: usage
	//            with no cost, because the caller's own subscription paid.
	`CREATE TABLE IF NOT EXISTS spend_requests (
		id                 bigserial PRIMARY KEY,
		at                 timestamptz NOT NULL,
		request_id         text NOT NULL DEFAULT '',
		key_hash           text NOT NULL DEFAULT '',
		key_alias          text NOT NULL DEFAULT '',
		scope              text NOT NULL DEFAULT '',
		scopes             text[] NOT NULL DEFAULT '{}',
		model_group        text NOT NULL DEFAULT '',
		deployment         text NOT NULL DEFAULT '',
		input_tokens       bigint NOT NULL DEFAULT 0,
		output_tokens      bigint NOT NULL DEFAULT 0,
		cache_read_tokens  bigint NOT NULL DEFAULT 0,
		cache_write_tokens bigint NOT NULL DEFAULT 0,
		cost               double precision NOT NULL DEFAULT 0,
		billable           boolean NOT NULL DEFAULT false,
		cache_savings      double precision NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS spend_requests_at_idx ON spend_requests (at)`,
	`CREATE INDEX IF NOT EXISTS spend_requests_key_at_idx ON spend_requests (key_hash, at)`,
	`CREATE INDEX IF NOT EXISTS spend_requests_deployment_at_idx ON spend_requests (deployment, at)`,
	// GIN, because the scope filter is array containment: a report on an
	// organisation matches every row any team beneath it produced.
	`CREATE INDEX IF NOT EXISTS spend_requests_scopes_idx ON spend_requests USING gin (scopes)`,

	// Day rollups, one row per subject per UTC day.
	//
	// Maintained on write rather than by a scheduled job, in the same
	// transaction as the rows they summarize. A job would be a second thing to
	// run and to alert on, would leave today's figures missing until it ran,
	// and would have to be idempotent against rows it might see twice. Doing it
	// in the write transaction makes double counting impossible instead of
	// merely unlikely: either both halves commit or neither does.
	//
	// They also outlive the rows. Pruning spend_requests to a retention window
	// leaves every historical total intact, which is what makes the retention
	// setting safe to use.
	`CREATE TABLE IF NOT EXISTS spend_daily (
		day                date NOT NULL,
		subject_kind       text NOT NULL,
		subject            text NOT NULL,
		alias              text NOT NULL DEFAULT '',
		requests           bigint NOT NULL DEFAULT 0,
		billable_requests  bigint NOT NULL DEFAULT 0,
		input_tokens       bigint NOT NULL DEFAULT 0,
		output_tokens      bigint NOT NULL DEFAULT 0,
		cache_read_tokens  bigint NOT NULL DEFAULT 0,
		cache_write_tokens bigint NOT NULL DEFAULT 0,
		cost               double precision NOT NULL DEFAULT 0,
		cache_savings      double precision NOT NULL DEFAULT 0,
		PRIMARY KEY (day, subject_kind, subject)
	)`,
	`CREATE INDEX IF NOT EXISTS spend_daily_kind_day_idx ON spend_daily (subject_kind, day)`,
}

const pgSchemaTable = `CREATE TABLE IF NOT EXISTS spend_schema (
	version    integer PRIMARY KEY,
	applied_at timestamptz NOT NULL
)`

// pgMigrationLockID namespaces the advisory lock the schema is applied under,
// so replicas starting together apply it once between them.
const pgMigrationLockID int64 = 0x7370656e64000000 // "spend\0\0\0"

const (
	pgMigrateTimeout = 30 * time.Second
	// pgWriteTimeout bounds one batch write. Generous, because nothing waits on
	// it: the request it describes was served before the entry was queued.
	pgWriteTimeout = 30 * time.Second
	// pgPruneInterval is how often retention is applied. Hourly rather than
	// daily so a fleet that restarts every afternoon still prunes.
	pgPruneInterval = time.Hour

	fallbackBufferSize    = 8192
	fallbackBatchSize     = 500
	fallbackFlushInterval = 2 * time.Second
	fallbackMaxConns      = 4
	// defaultRowLimit caps an export that named no limit.
	defaultRowLimit = 10000
	// maxHeldBatches is how many failed batches are carried forward before the
	// oldest is dropped. It rides out a database restart without losing rows
	// and bounds what a longer outage costs in memory.
	maxHeldBatches = 5
)

// PostgresOptions configures the history store.
type PostgresOptions struct {
	// DSN is a libpq connection string or URL. It carries a password, so it
	// comes from the environment rather than from the config file.
	DSN string
	// BufferSize is how many entries may be queued for writing before further
	// ones are dropped. See Append for why dropping is the right end of that
	// trade.
	BufferSize int
	// BatchSize is how many rows are written per transaction.
	BatchSize int
	// FlushInterval bounds how long an entry waits in the buffer when traffic
	// is too light to fill a batch. It is what decides how stale a chart is.
	FlushInterval time.Duration
	// MaxConns caps the pool. Writes are serialized through one goroutine, so
	// this is sized for the reports rather than for the writes.
	MaxConns int32
	// Retention drops per-request rows older than this. Zero keeps them
	// forever, which is the default because deleting a financial record should
	// be something an operator asked for. Rollups are never pruned.
	Retention time.Duration
}

// PostgresHistory implements History against Postgres.
//
// Writes are buffered and batched: entries go onto a channel, one goroutine
// drains it, and each batch is one transaction that inserts the rows and
// upserts the day rollups they belong to. That shape is chosen by where this
// sits — Server.record runs inline on the inference path, so an INSERT per
// request would put a database round trip between a response being relayed and
// the handler returning.
type PostgresHistory struct {
	pool     *pgxpool.Pool
	log      *slog.Logger
	in       chan Entry
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	batchSize     int
	flushInterval time.Duration
	retention     time.Duration

	// Counters, published as metrics by whoever built this. Written by the one
	// writer goroutine and by Append's callers, so atomics rather than a lock:
	// Append is on the request path and must not contend with a flush.
	queued  atomic.Int64
	written atomic.Int64
	dropped atomic.Int64
}

// OpenPostgres connects, applies the schema and starts the writer.
//
// An unreachable database refuses to start, like the key store and the audit
// log: it is transient, a restart is how an orchestrator retries it, and a
// gateway that came up regardless would serve traffic whose cost nothing was
// recording — which is discovered a month later, when the report is asked for.
func OpenPostgres(ctx context.Context, opts PostgresOptions, log *slog.Logger) (*PostgresHistory, error) {
	if opts.BufferSize <= 0 {
		opts.BufferSize = fallbackBufferSize
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = fallbackBatchSize
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = fallbackFlushInterval
	}
	if opts.MaxConns <= 0 {
		opts.MaxConns = fallbackMaxConns
	}

	cfg, err := pgxpool.ParseConfig(opts.DSN)
	if err != nil {
		// The driver's message quotes the string it failed to parse, and that
		// string carries a password.
		return nil, errors.New("spend history: the postgres dsn could not be parsed")
	}
	cfg.MaxConns = opts.MaxConns

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("spend history: connect to postgres: %w", err)
	}

	if log == nil {
		// Every path here logs, and the one that logs most is the one that
		// runs when something is already wrong.
		log = slog.New(slog.DiscardHandler)
	}
	h := &PostgresHistory{
		pool: pool, log: log,
		in:            make(chan Entry, opts.BufferSize),
		stop:          make(chan struct{}),
		batchSize:     opts.BatchSize,
		flushInterval: opts.FlushInterval,
		retention:     opts.Retention,
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("spend history: postgres unreachable at startup: %w", err)
	}
	if err := h.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}

	h.wg.Add(1)
	go h.run()

	if log != nil {
		log.Info("spend history recorded to postgres",
			"host", cfg.ConnConfig.Host, "database", cfg.ConnConfig.Database,
			"batch_size", opts.BatchSize, "flush_interval", opts.FlushInterval,
			"retention", opts.Retention, "schema_version", len(pgMigrations))
	}
	return h, nil
}

func (h *PostgresHistory) migrate(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, pgMigrateTimeout)
	defer cancel()

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("spend history: begin schema transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", pgMigrationLockID); err != nil {
		return fmt.Errorf("spend history: take schema lock: %w", err)
	}
	if _, err := tx.Exec(ctx, pgSchemaTable); err != nil {
		return fmt.Errorf("spend history: create schema table: %w", err)
	}
	var applied int
	if err := tx.QueryRow(ctx, "SELECT coalesce(max(version), 0) FROM spend_schema").Scan(&applied); err != nil {
		return fmt.Errorf("spend history: read schema version: %w", err)
	}
	for i, stmt := range pgMigrations {
		version := i + 1
		if version <= applied {
			continue
		}
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("spend history: apply schema version %d: %w", version, err)
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO spend_schema (version, applied_at) VALUES ($1, now())", version); err != nil {
			return fmt.Errorf("spend history: record schema version %d: %w", version, err)
		}
		if h.log != nil {
			h.log.Info("spend history schema applied", "version", version)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("spend history: commit schema: %w", err)
	}
	return nil
}

// Append implements History. It never blocks and never returns an error the
// caller should act on.
//
// A full buffer drops the entry and counts it. That is the deliberate end of
// the trade: this row is a report, and the alternative — blocking until the
// database catches up — would make an inference request wait on the system that
// answers monthly questions, turning a reporting outage into a serving one. The
// enforcing Store has already recorded the same request, so no budget is
// escaped by a row dropped here.
//
// gateway_spend_history_dropped_total is how that becomes visible; it is worth
// an alert, because it is the difference between a chargeback that adds up and
// one that quietly does not.
func (h *PostgresHistory) Append(_ context.Context, e Entry) error {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	select {
	case h.in <- e:
		h.queued.Add(1)
	default:
		if n := h.dropped.Add(1); n == 1 || n%1000 == 0 {
			// Logged sparsely: the condition that drops one entry drops
			// thousands, and a line each would bury the reason among the
			// symptoms.
			h.log.Error("spend history buffer is full; per-request rows are being dropped",
				"dropped_total", n, "buffer", cap(h.in),
				"note", "budgets are unaffected — enforcement does not read this table")
		}
	}
	return nil
}

// Stats reports what the writer has done, for the gauges.
func (h *PostgresHistory) Stats() (written, dropped, pending int64) {
	return h.written.Load(), h.dropped.Load(), int64(len(h.in))
}

// run drains the queue into batched transactions until Close.
func (h *PostgresHistory) run() {
	defer h.wg.Done()

	ticker := time.NewTicker(h.flushInterval)
	defer ticker.Stop()

	var prune <-chan time.Time
	if h.retention > 0 {
		pruneTicker := time.NewTicker(pgPruneInterval)
		defer pruneTicker.Stop()
		prune = pruneTicker.C
		// Once at startup too, so a gateway that is restarted more often than
		// the prune interval still prunes.
		h.prune()
	}

	batch := make([]Entry, 0, h.batchSize)
	// held carries batches a failed write could not commit, oldest first, so a
	// database restart costs latency rather than rows.
	var held [][]Entry

	flush := func() {
		if len(batch) == 0 && len(held) == 0 {
			return
		}
		pending := append(held, batch)
		held = nil
		batch = make([]Entry, 0, h.batchSize)
		for i, b := range pending {
			if err := h.write(b); err != nil {
				// Everything from the first failure onward is held, so the
				// order rows reach the table matches the order they were
				// served in.
				held = h.holdFailed(err, append(held, pending[i:]...))
				return
			}
			h.written.Add(int64(len(b)))
		}
	}

	for {
		select {
		case e := <-h.in:
			batch = append(batch, e)
			if len(batch) >= h.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-prune:
			h.prune()
		case <-h.stop:
			// Drain what is queued before going, so a graceful shutdown does
			// not lose the last few seconds of traffic.
			for {
				select {
				case e := <-h.in:
					batch = append(batch, e)
					if len(batch) >= h.batchSize {
						flush()
					}
					continue
				default:
				}
				break
			}
			flush()
			if n := rows(held); n > 0 {
				h.log.Error("spend history could not be written before shutdown; rows are lost",
					"rows", n)
				h.dropped.Add(int64(n))
			}
			return
		}
	}
}

// holdFailed bounds what an outage costs in memory, dropping the oldest batches
// once too many are waiting, and returns what is still held.
//
// It returns rather than trimming in place because a slice truncated inside a
// callee is truncated only there: the caller would keep the longer header, and
// the bound would count drops without ever applying one.
func (h *PostgresHistory) holdFailed(cause error, held [][]Entry) [][]Entry {
	h.log.Warn("spend history write failed; holding rows for the next flush",
		"error", cause, "held_batches", len(held), "held_rows", rows(held))
	for len(held) > maxHeldBatches {
		dropped := len(held[0])
		held = held[1:]
		h.dropped.Add(int64(dropped))
		h.log.Error("spend history has been unwritable long enough to drop rows",
			"rows", dropped, "dropped_total", h.dropped.Load())
	}
	return held
}

func rows(batches [][]Entry) int {
	n := 0
	for _, b := range batches {
		n += len(b)
	}
	return n
}

// requestColumns is the insert column list, in the order copyRows yields.
var requestColumns = []string{
	"at", "request_id", "key_hash", "key_alias", "scope", "scopes",
	"model_group", "deployment", "input_tokens", "output_tokens",
	"cache_read_tokens", "cache_write_tokens", "cost", "billable", "cache_savings",
}

// write commits one batch: the rows, and the day rollups they belong to, in one
// transaction so the two can never disagree.
func (h *PostgresHistory) write(batch []Entry) error {
	if len(batch) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), pgWriteTimeout)
	defer cancel()

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"spend_requests"}, requestColumns,
		pgx.CopyFromSlice(len(batch), func(i int) ([]any, error) {
			e := batch[i]
			scope := ""
			if len(e.Scopes) > 0 {
				scope = e.Scopes[0]
			}
			// text[] has no nil, and the column is NOT NULL: a key outside any
			// scope has an empty chain, not an absent one.
			scopes := e.Scopes
			if scopes == nil {
				scopes = []string{}
			}
			return []any{
				e.At, e.RequestID, e.KeyHash, e.KeyAlias, scope, scopes,
				e.ModelGroup, e.DeploymentID,
				int64(e.Usage.InputTokens), int64(e.Usage.OutputTokens),
				int64(e.Usage.CacheReadTokens), int64(e.Usage.CacheWriteTokens),
				e.Cost, e.Billable, e.CacheSavings,
			}, nil
		})); err != nil {
		return fmt.Errorf("copy rows: %w", err)
	}

	for _, r := range rollup(batch) {
		if _, err := tx.Exec(ctx, upsertDaily,
			r.day, string(r.kind), r.subject, r.alias,
			r.Requests, r.BillableRequests, r.InputTokens, r.OutputTokens,
			r.CacheReadTokens, r.CacheWriteTokens, r.Cost, r.CacheSavings); err != nil {
			return fmt.Errorf("roll up %s %s: %w", r.kind, r.subject, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// upsertDaily adds one batch's contribution to a day's totals.
//
// The alias is only replaced when the new one says something: a key renamed to
// nothing should keep the label the report already had, and an entry recorded
// before an alias was set should not blank it.
const upsertDaily = `
	INSERT INTO spend_daily (day, subject_kind, subject, alias, requests, billable_requests,
		input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost, cache_savings)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	ON CONFLICT (day, subject_kind, subject) DO UPDATE SET
		alias = CASE WHEN EXCLUDED.alias <> '' THEN EXCLUDED.alias ELSE spend_daily.alias END,
		requests = spend_daily.requests + EXCLUDED.requests,
		billable_requests = spend_daily.billable_requests + EXCLUDED.billable_requests,
		input_tokens = spend_daily.input_tokens + EXCLUDED.input_tokens,
		output_tokens = spend_daily.output_tokens + EXCLUDED.output_tokens,
		cache_read_tokens = spend_daily.cache_read_tokens + EXCLUDED.cache_read_tokens,
		cache_write_tokens = spend_daily.cache_write_tokens + EXCLUDED.cache_write_tokens,
		cost = spend_daily.cost + EXCLUDED.cost,
		cache_savings = spend_daily.cache_savings + EXCLUDED.cache_savings`

// dailyKey addresses one rollup row.
type dailyKey struct {
	day     time.Time
	kind    Kind
	subject string
}

type dailyRow struct {
	dailyKey
	alias string
	Totals
}

// rollup folds a batch into one row per subject per day, so a batch of 500
// requests becomes a handful of upserts rather than 500 of them.
//
// A request contributes to its key, to every scope above it, to its deployment
// and to its model group — the same deliberate duplication the ledger makes,
// and for the same reason: these are independent questions, and a team's total
// cannot be derived by summing its members' rows because membership changes
// underneath historical spend. Summing across kinds therefore counts every
// request several times, which is why every report names one kind.
func rollup(batch []Entry) []dailyRow {
	index := make(map[dailyKey]*dailyRow, len(batch))
	add := func(day time.Time, kind Kind, subject, alias string, e Entry) {
		if subject == "" {
			return
		}
		k := dailyKey{day: day, kind: kind, subject: subject}
		row, ok := index[k]
		if !ok {
			row = &dailyRow{dailyKey: k}
			index[k] = row
		}
		if alias != "" {
			row.alias = alias
		}
		row.Totals.add(e)
	}

	for _, e := range batch {
		day := e.At.UTC().Truncate(24 * time.Hour)
		add(day, KindKey, e.KeyHash, e.KeyAlias, e)
		add(day, KindDeployment, e.DeploymentID, "", e)
		add(day, KindModel, e.ModelGroup, "", e)
		for i, subject := range e.Scopes {
			alias := ""
			if i < len(e.ScopeAliases) {
				alias = e.ScopeAliases[i]
			}
			add(day, KindScope, subject, alias, e)
		}
	}

	out := make([]dailyRow, 0, len(index))
	for _, row := range index {
		out = append(out, *row)
	}
	// Ordered so that two instances upserting the same day's rows take the
	// primary keys in the same sequence, which is what keeps concurrent
	// transactions from deadlocking on each other.
	sortDailyRows(out)
	return out
}

// prune applies the retention window to the per-request rows. Rollups are left
// alone: they are the reason pruning is safe.
func (h *PostgresHistory) prune() {
	if h.retention <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pgWriteTimeout)
	defer cancel()

	cutoff := time.Now().UTC().Add(-h.retention)
	tag, err := h.pool.Exec(ctx, "DELETE FROM spend_requests WHERE at < $1", cutoff)
	if err != nil {
		h.log.Warn("prune spend history", "error", err, "cutoff", cutoff)
		return
	}
	if n := tag.RowsAffected(); n > 0 {
		h.log.Info("pruned spend history rows past their retention",
			"rows", n, "cutoff", cutoff, "note", "day rollups are kept")
	}
}

// Close stops the writer, flushing what is buffered, and releases the pool.
func (h *PostgresHistory) Close() error {
	h.stopOnce.Do(func() { close(h.stop) })
	h.wg.Wait()
	h.pool.Close()
	return nil
}

var _ History = (*PostgresHistory)(nil)

// Series implements History.
func (h *PostgresHistory) Series(ctx context.Context, q Query) ([]Bucket, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	if q.Interval == IntervalHour {
		return h.hourly(ctx, q)
	}
	return h.daily(ctx, q)
}

// daily reads the rollup table, which is what makes a year-long report a scan
// of days rather than of requests.
//
// The upper bound is the day containing the last instant of the range rather
// than the day To falls on, because To is an exclusive instant and a day is
// not. Comparing against To's own date drops the day in progress whenever the
// range ends at "now" — which is what every default report asks for, so the
// symptom is a chart that is always missing today.
func (h *PostgresHistory) daily(ctx context.Context, q Query) ([]Bucket, error) {
	sql := `SELECT day, subject, alias, requests, billable_requests, input_tokens,
			output_tokens, cache_read_tokens, cache_write_tokens, cost, cache_savings
		FROM spend_daily
		WHERE subject_kind = $1
		  AND day >= ($2 AT TIME ZONE 'UTC')::date
		  AND day <= (($3::timestamptz - interval '1 microsecond') AT TIME ZONE 'UTC')::date
		  AND ($4 = '' OR subject = $4)
		ORDER BY day, subject`
	if q.Limit > 0 {
		sql += fmt.Sprintf(" LIMIT %d", q.Limit)
	}
	rows, err := h.pool.Query(ctx, sql, string(q.Kind), q.From, q.To, q.Subject)
	if err != nil {
		return nil, fmt.Errorf("read spend history: %w", err)
	}
	defer rows.Close()

	out := make([]Bucket, 0)
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Start, &b.Subject, &b.Alias, &b.Requests, &b.BillableRequests,
			&b.InputTokens, &b.OutputTokens, &b.CacheReadTokens, &b.CacheWriteTokens,
			&b.Cost, &b.CacheSavings); err != nil {
			return nil, fmt.Errorf("read spend history: %w", err)
		}
		b.Start = b.Start.UTC()
		b.WindowStart = b.Start
		out = append(out, b)
	}
	return out, rows.Err()
}

// hourly aggregates the per-request rows, and is therefore bounded by their
// retention rather than by the rollups'.
func (h *PostgresHistory) hourly(ctx context.Context, q Query) ([]Bucket, error) {
	source, subject := "spend_requests", ""
	switch q.Kind {
	case KindKey:
		subject = "key_hash"
	case KindDeployment:
		subject = "deployment"
	case KindModel:
		subject = "model_group"
	case KindScope:
		// A request is charged to every level of its chain, so the row has to
		// be counted once per scope it names — which is what unnest does, and
		// why this one reads from a join rather than from a column.
		source, subject = "spend_requests, unnest(scopes) AS s", "s"
	}
	sql := fmt.Sprintf(`SELECT date_trunc('hour', at AT TIME ZONE 'UTC') AS bucket, %s AS subject,
			count(*), count(*) FILTER (WHERE billable),
			sum(input_tokens), sum(output_tokens), sum(cache_read_tokens), sum(cache_write_tokens),
			sum(cost) FILTER (WHERE billable), sum(cache_savings) FILTER (WHERE billable)
		FROM %s
		WHERE at >= $1 AND at < $2 AND ($3 = '' OR %s = $3) AND %s <> ''
		GROUP BY 1, 2 ORDER BY 1, 2`, subject, source, subject, subject)
	if q.Limit > 0 {
		sql += fmt.Sprintf(" LIMIT %d", q.Limit)
	}
	rows, err := h.pool.Query(ctx, sql, q.From, q.To, q.Subject)
	if err != nil {
		return nil, fmt.Errorf("read spend history: %w", err)
	}
	defer rows.Close()

	out := make([]Bucket, 0)
	for rows.Next() {
		var (
			b                 Bucket
			cost, savings     *float64
			in, outTok        *int64
			cacheRd, cacheWr  *int64
			reqs, billableReq int64
		)
		if err := rows.Scan(&b.Start, &b.Subject, &reqs, &billableReq,
			&in, &outTok, &cacheRd, &cacheWr, &cost, &savings); err != nil {
			return nil, fmt.Errorf("read spend history: %w", err)
		}
		// sum() over no rows is NULL rather than zero, and a bucket can have
		// requests but no billable ones — passthrough traffic is exactly that.
		b.Requests, b.BillableRequests = int(reqs), int(billableReq)
		b.InputTokens, b.OutputTokens = intOf(in), intOf(outTok)
		b.CacheReadTokens, b.CacheWriteTokens = intOf(cacheRd), intOf(cacheWr)
		b.Cost, b.CacheSavings = floatOf(cost), floatOf(savings)
		b.Start = b.Start.UTC()
		b.WindowStart = b.Start
		out = append(out, b)
	}
	return out, rows.Err()
}

// EachRow implements History.
func (h *PostgresHistory) EachRow(ctx context.Context, q Query, fn func(Row) error) error {
	if err := q.Validate(); err != nil {
		return err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultRowLimit
	}

	where := "at >= $1 AND at < $2"
	args := []any{q.From, q.To}
	if q.Subject != "" {
		switch q.Kind {
		case KindScope:
			// Containment rather than equality on the innermost scope, so a
			// report on an organisation finds the rows its teams produced.
			where += " AND $3 = ANY(scopes)"
		case KindKey:
			where += " AND key_hash = $3"
		case KindDeployment:
			where += " AND deployment = $3"
		case KindModel:
			where += " AND model_group = $3"
		}
		args = append(args, q.Subject)
	}

	sql := fmt.Sprintf(`SELECT at, request_id, key_hash, key_alias, scope, model_group, deployment,
			input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			cost, billable, cache_savings
		FROM spend_requests WHERE %s ORDER BY at, id LIMIT %d`, where, limit)

	rows, err := h.pool.Query(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("read spend history: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			r                          Row
			in, outTok, cacheRd, cache int64
		)
		if err := rows.Scan(&r.At, &r.RequestID, &r.KeyHash, &r.KeyAlias, &r.Scope,
			&r.ModelGroup, &r.Deployment, &in, &outTok, &cacheRd, &cache,
			&r.Cost, &r.Billable, &r.CacheSavings); err != nil {
			return fmt.Errorf("read spend history: %w", err)
		}
		r.At = r.At.UTC()
		r.Usage = core.Usage{
			InputTokens: int(in), OutputTokens: int(outTok),
			CacheReadTokens: int(cacheRd), CacheWriteTokens: int(cache),
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

// PostgresTarget names a database for an operator-facing line, without its
// credentials.
func PostgresTarget(dsn string) string {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return "postgres"
	}
	return fmt.Sprintf("postgres %s/%s", cfg.ConnConfig.Host, cfg.ConnConfig.Database)
}

func intOf(p *int64) int {
	if p == nil {
		return 0
	}
	return int(*p)
}

func floatOf(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// sortDailyRows orders rollup upserts deterministically. Split out so the
// comparison reads as one thing rather than three lines inside rollup.
func sortDailyRows(rows []dailyRow) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if !a.day.Equal(b.day) {
			return a.day.Before(b.day)
		}
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		return a.subject < b.subject
	})
}
