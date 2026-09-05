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

// Store accumulates entries and answers budget questions. Every method is
// context-first and returns an error so a database-backed implementation fits
// behind the same interface.
type Store interface {
	// Record accounts for a completed request.
	Record(ctx context.Context, e Entry) error
	// KeySpend returns what a key has spent in its current budget window.
	KeySpend(ctx context.Context, keyHash string, window time.Duration) (float64, error)
	// Keys summarizes consumption per virtual key.
	Keys(ctx context.Context) ([]Summary, error)
	// Deployments summarizes consumption per deployment.
	Deployments(ctx context.Context) ([]Summary, error)
}

// Ledger is an in-memory Store, optionally persisted to a JSON file so that
// budgets survive a restart. It is safe for concurrent use.
type Ledger struct {
	mu          sync.RWMutex
	keys        map[string]*Totals
	aliases     map[string]string
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
		deployments: make(map[string]*Totals),
		path:        path,
		now:         time.Now,
	}
}

// snapshot is the on-disk shape.
type snapshot struct {
	Keys        map[string]*Totals `json:"keys"`
	Aliases     map[string]string  `json:"aliases"`
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
	if e.KeyHash == "" && e.DeploymentID == "" {
		return nil
	}
	l.mu.Lock()
	if e.KeyHash != "" {
		l.totalsLocked(l.keys, e.KeyHash).add(e)
		if e.KeyAlias != "" {
			l.aliases[e.KeyHash] = e.KeyAlias
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

// KeySpend implements Store. A window of zero means the key's spend is tracked
// for its whole lifetime; otherwise the tally resets once the window elapses.
func (l *Ledger) KeySpend(_ context.Context, keyHash string, window time.Duration) (float64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	t, ok := l.keys[keyHash]
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
		Deployments: make(map[string]*Totals, len(l.deployments)),
	}
	for k, v := range l.keys {
		clone := *v
		snap.Keys[k] = &clone
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
