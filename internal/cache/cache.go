// Package cache stores upstream responses so an identical request can be served
// without calling the provider again.
//
// The design question that matters here is not storage, it is who may see a
// cached response. A cache shared across virtual keys serves one tenant's answer
// to another, and prompts and completions are the most sensitive thing this
// gateway handles. So the default scope is per-key: a key only ever sees its own
// responses. Sharing across keys is available, opt-in, and refused for
// passthrough deployments, whose responses were generated under one person's
// personal subscription.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/erickardus/ai-gateway/internal/core"
)

// Scope decides which callers share cached responses.
type Scope string

const (
	// ScopeKey confines cached responses to the virtual key that produced them.
	// It is the default: it cannot leak one caller's completion to another.
	ScopeKey Scope = "key"
	// ScopeShared lets every caller reuse any cached response. It saves far
	// more, and is only appropriate when all callers are equally trusted.
	ScopeShared Scope = "shared"
)

// Entry is a stored response.
type Entry struct {
	Status int         `json:"status"`
	Header http.Header `json:"header"`
	Body   []byte      `json:"body"`
	Usage  core.Usage  `json:"usage"`
	// Streaming records whether the body is a server-sent event stream, which
	// is replayed rather than written in one piece.
	Streaming bool      `json:"streaming"`
	StoredAt  time.Time `json:"stored_at"`
}

// Cache stores and retrieves responses. Implementations are safe for concurrent
// use, and a miss is not an error.
type Cache interface {
	Get(ctx context.Context, key string) (*Entry, bool, error)
	Put(ctx context.Context, key string, e *Entry, ttl time.Duration) error
	// Purge empties the cache, for operators who need to invalidate everything.
	Purge(ctx context.Context) error
}

// Key derives the cache key for a request.
//
// The body is hashed whole, so any difference in the prompt, the tools, the
// sampling parameters or the system blocks produces a different key. Model group
// and wire format are included because the same body means different things on
// different routes, and scopeID confines the entry to one key unless sharing was
// explicitly configured.
func Key(scopeID, format, model string, body []byte) string {
	h := sha256.New()
	// Length-prefix each field so that concatenation cannot be ambiguous: two
	// different (scope, model) pairs must never hash alike.
	for _, part := range []string{scopeID, format, model} {
		h.Write([]byte(strconv.Itoa(len(part))))
		h.Write([]byte(":"))
		h.Write([]byte(part))
	}
	h.Write([]byte(strconv.Itoa(len(body))))
	h.Write([]byte(":"))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// entry is a stored value with its expiry.
type entry struct {
	value     *Entry
	expiresAt time.Time
	// element positions this entry in the recency list.
	element *node
}

// node is one link of the intrusive recency list.
type node struct {
	key        string
	prev, next *node
}

// Memory is an in-process cache bounded by entry count, evicting least recently
// used entries first.
type Memory struct {
	mu      sync.Mutex
	entries map[string]*entry
	head    *node // most recently used
	tail    *node // least recently used
	max     int
	now     func() time.Time
}

// NewMemory returns a cache holding at most maxEntries.
func NewMemory(maxEntries int) *Memory {
	if maxEntries <= 0 {
		maxEntries = 1000
	}
	return &Memory{entries: make(map[string]*entry), max: maxEntries, now: time.Now}
}

// Get implements Cache.
func (m *Memory) Get(_ context.Context, key string) (*Entry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.entries[key]
	if !ok {
		return nil, false, nil
	}
	if m.now().After(e.expiresAt) {
		m.removeLocked(key, e)
		return nil, false, nil
	}
	m.touchLocked(e)
	return e.value, true, nil
}

// Put implements Cache.
func (m *Memory) Put(_ context.Context, key string, value *Entry, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.entries[key]; ok {
		existing.value = value
		existing.expiresAt = m.now().Add(ttl)
		m.touchLocked(existing)
		return nil
	}

	n := &node{key: key}
	e := &entry{value: value, expiresAt: m.now().Add(ttl), element: n}
	m.entries[key] = e
	m.pushFrontLocked(n)

	for len(m.entries) > m.max && m.tail != nil {
		evict := m.tail
		m.removeLocked(evict.key, m.entries[evict.key])
	}
	return nil
}

// Purge implements Cache.
func (m *Memory) Purge(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = make(map[string]*entry)
	m.head, m.tail = nil, nil
	return nil
}

// Len reports how many entries are held, for tests and diagnostics.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

func (m *Memory) pushFrontLocked(n *node) {
	n.prev, n.next = nil, m.head
	if m.head != nil {
		m.head.prev = n
	}
	m.head = n
	if m.tail == nil {
		m.tail = n
	}
}

func (m *Memory) unlinkLocked(n *node) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		m.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		m.tail = n.prev
	}
	n.prev, n.next = nil, nil
}

func (m *Memory) touchLocked(e *entry) {
	m.unlinkLocked(e.element)
	m.pushFrontLocked(e.element)
}

func (m *Memory) removeLocked(key string, e *entry) {
	if e == nil {
		return
	}
	m.unlinkLocked(e.element)
	delete(m.entries, key)
}
