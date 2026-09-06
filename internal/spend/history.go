package spend

import (
	"context"
	"fmt"
	"time"

	"github.com/erickardus/ai-gateway/internal/core"
)

// History keeps what a Store deliberately does not: one row per request, and
// day rollups derived from them, both retained after the budget window they
// were spent in has rolled over.
//
// # Why this is not the Store
//
// The Store answers "may this request proceed", on the inference path, for
// every request, across a fleet. That is a current-window number, it wants to
// be a single fast read, and Redis is the right shape for it. This answers
// "what did the Payments team spend in August", off the request path, for a
// person — a range scan and an aggregation, which is the shape Redis is worst
// at and a relational database is best at.
//
// Trying to serve both from one store makes each worse. A Postgres Store would
// put an INSERT and a SUM on every inference request; a Redis History would
// need a key per bucket per subject and could still not answer a range query
// without scanning them. So the two live side by side, and WithHistory wires
// one entry into both.
//
// # What that costs
//
// Two systems can disagree. The Store's number is what is *enforced*; this one
// is what is *reported* over time, and it is best effort — see Append. A
// dropped history row does not let anybody exceed a budget, which is the
// property that makes best effort acceptable here and would not make it
// acceptable in internal/audit.
type History interface {
	// Append records one completed request. It does not block the caller and
	// does not fail the request: spend history is a report, not an
	// authorization, and an inference request must not wait on a database that
	// answers reports.
	Append(ctx context.Context, e Entry) error
	// Series returns consumption bucketed over time, for a chart or a monthly
	// figure.
	Series(ctx context.Context, q Query) ([]Bucket, error)
	// EachRow streams the per-request rows a query selects, in time order, for
	// an export. It is a callback rather than a slice because an export of a
	// month of a fleet's traffic is millions of rows and belongs on the wire as
	// it is read, not in memory first.
	EachRow(ctx context.Context, q Query, fn func(Row) error) error
	// Close flushes what is buffered and releases the store.
	Close() error
}

// Interval is the width of a Series bucket.
//
// Two rather than an arbitrary duration, because they are answered from
// different places: a day comes from the rollup table and is available for as
// long as rollups are kept, while an hour is aggregated from the per-request
// rows and is therefore bounded by whatever retention those have.
type Interval string

const (
	IntervalDay  Interval = "day"
	IntervalHour Interval = "hour"
)

// KindModel addresses one model group's totals. It exists only in history:
// the Store's subjects are the things that carry budgets, and a model group
// does not.
const KindModel Kind = "model"

// Query selects what a report covers.
type Query struct {
	// Kind is the dimension being reported. Required.
	Kind Kind
	// Subject narrows the report to one key, scope, deployment or model group.
	// Empty means every subject of that kind, which is what a leaderboard
	// wants and what a single chart line does not.
	Subject string
	// From is inclusive and To exclusive, so consecutive months tile without
	// counting a boundary request twice.
	From, To time.Time
	// Interval is the bucket width for Series, ignored by EachRow.
	Interval Interval
	// Limit caps rows returned. Zero means the implementation's default rather
	// than unlimited: an export with no limit is a way to ask a gateway to
	// serialize a year of traffic in one request.
	Limit int
}

// Validate reports a query that cannot be answered, in the words an operator
// who typed the parameters would need.
func (q Query) Validate() error {
	switch q.Kind {
	case KindKey, KindScope, KindDeployment, KindModel:
	default:
		return fmt.Errorf("kind: must be %q, %q, %q or %q, got %q",
			KindKey, KindScope, KindDeployment, KindModel, q.Kind)
	}
	if q.From.IsZero() || q.To.IsZero() {
		return fmt.Errorf("from and to: both are required")
	}
	if !q.To.After(q.From) {
		return fmt.Errorf("to: must be after from, got from=%s to=%s",
			q.From.Format(time.RFC3339), q.To.Format(time.RFC3339))
	}
	switch q.Interval {
	case "", IntervalDay, IntervalHour:
	default:
		return fmt.Errorf("interval: must be %q or %q, got %q", IntervalDay, IntervalHour, q.Interval)
	}
	if q.Limit < 0 {
		return fmt.Errorf("limit: must be >= 0, got %d", q.Limit)
	}
	return nil
}

// Bucket is one subject's consumption over one interval.
type Bucket struct {
	// Start is the beginning of the interval, in UTC. Days are UTC days
	// deliberately: a fleet spans time zones, and a rollup keyed on the
	// operator's local day would move under a gateway deployed in another
	// region. A finance team that needs local months converts at the edge.
	Start   time.Time `json:"start"`
	Subject string    `json:"subject"`
	// Alias is the human label, where the rollup has one. Hour buckets are
	// aggregated from per-request rows and carry no alias, because pairing a
	// scope with its label there costs a join for a string the caller already
	// has from /spend/scopes.
	Alias string `json:"alias,omitempty"`
	Totals
}

// Row is one completed request, as an export sees it.
type Row struct {
	At        time.Time `json:"at"`
	RequestID string    `json:"request_id,omitempty"`
	KeyHash   string    `json:"key_hash,omitempty"`
	KeyAlias  string    `json:"key_alias,omitempty"`
	// Scope is the innermost project, team or organisation the key belonged to
	// when the request was served. It is stored rather than looked up, because
	// a key can move between teams and this row is what that team was charged.
	Scope        string     `json:"scope,omitempty"`
	ModelGroup   string     `json:"model_group,omitempty"`
	Deployment   string     `json:"deployment,omitempty"`
	Usage        core.Usage `json:"usage"`
	Cost         float64    `json:"cost"`
	Billable     bool       `json:"billable"`
	CacheSavings float64    `json:"cache_savings"`
}

// WithHistory returns a Store that records into store and, alongside it, into
// history.
//
// A decorator rather than a field on each Store implementation: there are two
// Stores and there would be two copies of this, and the wiring is a deployment
// decision that belongs where the gateway is assembled rather than inside
// either of them.
func WithHistory(store Store, history History) Store {
	if history == nil {
		return store
	}
	return &teed{Store: store, history: history}
}

type teed struct {
	Store
	history History
}

// Record implements Store. The history write cannot fail the request: it is
// non-blocking, and its own failures are counted where it can see them.
func (t *teed) Record(ctx context.Context, e Entry) error {
	err := t.Store.Record(ctx, e)
	_ = t.history.Append(ctx, e)
	return err
}

// Forget drops a revoked key's *current window* from the enforcing store, and
// deliberately leaves its history alone.
//
// The two mean different things. Forgetting the window is about enforcement:
// the key is gone, so nothing should still be counted against it. Forgetting
// the history would delete the record of what the key spent while it existed,
// which is the record a chargeback is built from — and would make revoking a
// key a way to erase a month of somebody's costs.
//
// It is defined here at all because the server reaches the underlying store's
// Forget through a type assertion, which a decorator would otherwise hide.
func (t *teed) Forget(keyHash string) {
	if f, ok := t.Store.(interface{ Forget(string) }); ok {
		f.Forget(keyHash)
	}
}

// Flush passes through for the same reason Forget does: main closes the ledger
// through this interface and would otherwise flush nothing.
func (t *teed) Flush() error {
	if f, ok := t.Store.(interface{ Flush() error }); ok {
		return f.Flush()
	}
	return nil
}
