// Package spend records what each virtual key and deployment consumed, and
// enforces per-key budgets.
//
// Cost is attributed only where the gateway's operator actually pays it. On a
// passthrough deployment the upstream bills the caller's own subscription, so
// those requests carry usage but no cost — reporting a dollar figure there would
// invent a charge nobody receives.
package spend

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/erickardus/ai-gateway/internal/core"
)

// Entry is one completed request's accounting.
type Entry struct {
	KeyHash      string
	KeyAlias     string
	ModelGroup   string
	DeploymentID string
	Usage        core.Usage
	// Cost is what the operator was charged. It is zero for a passthrough
	// deployment, where the caller's own credential was billed.
	Cost float64
	// Billable distinguishes "cost is zero because it was free" from "cost is
	// zero because no pricing was configured", which are different facts and
	// look identical in a bare float.
	Billable bool
	// CacheSavings is what the provider's prompt cache took off this request's
	// bill, relative to the same tokens charged as ordinary input.
	CacheSavings float64
	// Scopes are the ledger subjects of the project, team and organisation this
	// request's key belongs to, innermost first. Each receives the whole of
	// this entry, which is what makes a scope budget a pool rather than a
	// second copy of the key's own: every key beneath a team writes to the same
	// subject, so the team's cap is drawn down by all of them together.
	//
	// A request is therefore recorded once per level plus once for its key. The
	// duplication is deliberate — these are independent ledgers answering
	// independent questions, and deriving a team's spend by summing its members
	// would need the membership list at read time, which the ledger does not
	// have and which changes underneath historical spend.
	Scopes []string
	// ScopeAliases labels those subjects for reports, parallel to Scopes.
	ScopeAliases []string
}

// Totals is accumulated consumption over a window.
type Totals struct {
	Requests         int     `json:"requests"`
	InputTokens      int     `json:"input_tokens"`
	OutputTokens     int     `json:"output_tokens"`
	CacheReadTokens  int     `json:"cache_read_tokens"`
	CacheWriteTokens int     `json:"cache_write_tokens"`
	Cost             float64 `json:"cost"`
	// BillableRequests counts requests the operator paid for, so a key serving
	// only subscription traffic is visibly distinct from an idle one.
	BillableRequests int `json:"billable_requests"`
	// CacheSavings is what the provider's prompt cache took off the bill over
	// this window. It sits beside Cost rather than inside it: Cost is what was
	// charged, and this is what was not.
	CacheSavings float64   `json:"cache_savings"`
	WindowStart  time.Time `json:"window_start"`
}

func (t *Totals) add(e Entry) {
	t.Requests++
	t.InputTokens += e.Usage.InputTokens
	t.OutputTokens += e.Usage.OutputTokens
	t.CacheReadTokens += e.Usage.CacheReadTokens
	t.CacheWriteTokens += e.Usage.CacheWriteTokens
	if e.Billable {
		t.BillableRequests++
		t.Cost += e.Cost
		t.CacheSavings += e.CacheSavings
	}
}

// Summary reports one subject's consumption.
type Summary struct {
	Subject string `json:"subject"`
	Alias   string `json:"alias,omitempty"`
	Totals
}

// Kind names a keyspace within the ledger.
//
// Key hashes and scope ids are kept apart rather than sharing one namespace
// addressed by a differently shaped subject: a scope id is operator-chosen text
// and a key hash is a digest, and one namespace holding both would let a scope
// named after a hash draw on that key's window.
type Kind string

const (
	// KindKey addresses one virtual key's own spend.
	KindKey Kind = "key"
	// KindScope addresses an organisation's, team's or project's pool.
	KindScope Kind = "scope"
	// KindDeployment addresses one upstream's totals.
	KindDeployment Kind = "deployment"
)

// Subject names one budget to read, with the window its cap applies over.
type Subject struct {
	Kind   Kind
	ID     string
	Window time.Duration
}

// Store accumulates entries and answers budget questions. Every method is
// context-first and returns an error so a database-backed implementation fits
// behind the same interface.
type Store interface {
	// Record accounts for a completed request.
	Record(ctx context.Context, e Entry) error
	// Spends returns what each subject has spent in its own window, in the
	// order asked.
	//
	// It takes a set rather than one subject because a request is checked
	// against its key and every scope above it, and asking separately makes
	// that a round trip per level on the hot path of every request. One call
	// lets a network-backed implementation pipeline them.
	Spends(ctx context.Context, subjects []Subject) ([]float64, error)
	// Keys summarizes consumption per virtual key.
	Keys(ctx context.Context) ([]Summary, error)
	// Scopes summarizes consumption per organisation, team and project.
	Scopes(ctx context.Context) ([]Summary, error)
	// Deployments summarizes consumption per deployment.
	Deployments(ctx context.Context) ([]Summary, error)
}

// Ledger is an in-memory Store, optionally persisted to a JSON file so that
// budgets survive a restart. It is safe for concurrent use.
type Ledger struct {
	mu          sync.RWMutex
	keys        map[string]*Totals
	aliases     map[string]string
	scopes      map[string]*Totals
	deployments map[string]*Totals

	path    string
	writeMu sync.Mutex
	dirty   bool
	now     func() time.Time
}

// New returns an unpersisted Ledger.
func New() *Ledger { return newLedger("") }

// NewFileLedger returns a Ledger backed by a JSON file, loading any existing
// contents.
func NewFileLedger(path string) (*Ledger, error) {
	l := newLedger(path)
	if err := l.load(); err != nil {
		return nil, err
	}
	return l, nil
}

func newLedger(path string) *Ledger {
	return &Ledger{
		keys:        make(map[string]*Totals),
		aliases:     make(map[string]string),
		scopes:      make(map[string]*Totals),
		deployments: make(map[string]*Totals),
		path:        path,
		now:         time.Now,
	}
}

// snapshot is the on-disk shape.
type snapshot struct {
	Keys    map[string]*Totals `json:"keys"`
	Aliases map[string]string  `json:"aliases"`
	// Scopes is absent from a ledger written before the hierarchy existed, and
	// an absent map decodes to nil rather than to an error — so an older file
	// loads with no pooled spend recorded rather than refusing to load at all.
	Scopes      map[string]*Totals `json:"scopes,omitempty"`
	Deployments map[string]*Totals `json:"deployments"`
}

func (l *Ledger) load() error {
	raw, err := os.ReadFile(l.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read spend ledger %s: %w", l.path, err)
	}
	var snap snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return fmt.Errorf("parse spend ledger %s: %w", l.path, err)
	}
	for k, v := range snap.Keys {
		if v != nil {
			l.keys[k] = v
		}
	}
	for k, v := range snap.Scopes {
		if v != nil {
			l.scopes[k] = v
		}
	}
	for k, v := range snap.Deployments {
		if v != nil {
			l.deployments[k] = v
		}
	}
	for k, v := range snap.Aliases {
		l.aliases[k] = v
	}
	return nil
}

// Record implements Store.
func (l *Ledger) Record(_ context.Context, e Entry) error {
	if e.KeyHash == "" && e.DeploymentID == "" && len(e.Scopes) == 0 {
		return nil
	}
	l.mu.Lock()
	if e.KeyHash != "" {
		l.totalsLocked(l.keys, e.KeyHash).add(e)
		if e.KeyAlias != "" {
			l.aliases[e.KeyHash] = e.KeyAlias
		}
	}
	for i, subject := range e.Scopes {
		if subject == "" {
			continue
		}
		l.totalsLocked(l.scopes, subject).add(e)
		if i < len(e.ScopeAliases) && e.ScopeAliases[i] != "" {
			l.aliases[subject] = e.ScopeAliases[i]
		}
	}
	if e.DeploymentID != "" {
		l.totalsLocked(l.deployments, e.DeploymentID).add(e)
	}
	l.dirty = true
	l.mu.Unlock()
	return nil
}

func (l *Ledger) totalsLocked(m map[string]*Totals, key string) *Totals {
	t, ok := m[key]
	if !ok {
		t = &Totals{WindowStart: l.now()}
		m[key] = t
	}
	return t
}

// Spends implements Store. Each subject is read under its own window; there is
// nothing to pipeline here, so it is the loop the interface exists to let a
// network-backed store avoid.
func (l *Ledger) Spends(_ context.Context, subjects []Subject) ([]float64, error) {
	out := make([]float64, len(subjects))
	for i, s := range subjects {
		v, err := l.spend(l.bucket(s.Kind), s.ID, s.Window)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// bucket returns the totals map a kind addresses. An unknown kind gets the
// deployment map rather than a nil one, so a caller cannot panic the ledger by
// asking a question it does not understand.
func (l *Ledger) bucket(k Kind) map[string]*Totals {
	switch k {
	case KindKey:
		return l.keys
	case KindScope:
		return l.scopes
	default:
		return l.deployments
	}
}

// KeySpend returns what a key has spent in its current budget window. A window
// of zero means the key's spend is tracked for its whole lifetime; otherwise
// the tally resets once the window elapses.
func (l *Ledger) KeySpend(_ context.Context, keyHash string, window time.Duration) (float64, error) {
	return l.spend(l.keys, keyHash, window)
}

// ScopeSpend reads the pooled totals rather than the per-key ones. The window
// semantics are identical: a scope's window starts at the first request any of
// its members made, and resets on the first request after it elapses.
func (l *Ledger) ScopeSpend(_ context.Context, subject string, window time.Duration) (float64, error) {
	return l.spend(l.scopes, subject, window)
}

// spend reads one subject's cost, rolling its window over if it has elapsed.
func (l *Ledger) spend(m map[string]*Totals, subject string, window time.Duration) (float64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	t, ok := m[subject]
	if !ok {
		return 0, nil
	}
	if window > 0 && l.now().Sub(t.WindowStart) >= window {
		// The window rolled over: the budget starts fresh, and the accumulated
		// totals go with it so the reported spend matches what is enforced.
		*t = Totals{WindowStart: l.now()}
		l.dirty = true
	}
	return t.Cost, nil
}

// Keys implements Store.
func (l *Ledger) Keys(_ context.Context) ([]Summary, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return summarize(l.keys, l.aliases), nil
}

// Scopes implements Store.
func (l *Ledger) Scopes(_ context.Context) ([]Summary, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return summarize(l.scopes, l.aliases), nil
}

// Deployments implements Store.
func (l *Ledger) Deployments(_ context.Context) ([]Summary, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return summarize(l.deployments, nil), nil
}

func summarize(m map[string]*Totals, aliases map[string]string) []Summary {
	out := make([]Summary, 0, len(m))
	for subject, t := range m {
		out = append(out, Summary{Subject: subject, Alias: aliases[subject], Totals: *t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subject < out[j].Subject })
	return out
}

// Forget drops a key's record, called when the key is revoked.
func (l *Ledger) Forget(keyHash string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.keys, keyHash)
	delete(l.aliases, keyHash)
	l.dirty = true
}

// Flush writes the ledger to disk if it is persisted and has changed.
//
// Serialization happens on a snapshot taken under the lock and written outside
// it, so a slow disk cannot stall the request path that reads spend.
func (l *Ledger) Flush() error {
	if l.path == "" {
		return nil
	}

	l.mu.Lock()
	if !l.dirty {
		l.mu.Unlock()
		return nil
	}
	snap := snapshot{
		Keys:        make(map[string]*Totals, len(l.keys)),
		Aliases:     make(map[string]string, len(l.aliases)),
		Scopes:      make(map[string]*Totals, len(l.scopes)),
		Deployments: make(map[string]*Totals, len(l.deployments)),
	}
	for k, v := range l.keys {
		clone := *v
		snap.Keys[k] = &clone
	}
	for k, v := range l.scopes {
		clone := *v
		snap.Scopes[k] = &clone
	}
	for k, v := range l.deployments {
		clone := *v
		snap.Deployments[k] = &clone
	}
	for k, v := range l.aliases {
		snap.Aliases[k] = v
	}
	l.dirty = false
	l.mu.Unlock()

	l.writeMu.Lock()
	defer l.writeMu.Unlock()

	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("encode spend ledger: %w", err)
	}
	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create spend ledger dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".spend-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp spend ledger: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp spend ledger: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp spend ledger: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp spend ledger: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp spend ledger: %w", err)
	}
	if err := os.Rename(tmpName, l.path); err != nil {
		return fmt.Errorf("replace spend ledger: %w", err)
	}
	return nil
}
