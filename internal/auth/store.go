package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/erickardus/ai-gateway/internal/core"
)

// KeyStore persists virtual keys. Implementations are safe for concurrent use.
// Every method takes a context and returns an error so that a network-backed
// store (Redis, Postgres) can satisfy the same interface unchanged.
type KeyStore interface {
	// Get returns the key with the given storage hash, or ErrKeyInvalid.
	Get(ctx context.Context, hash string) (*core.Key, error)
	// Put creates or replaces a key.
	Put(ctx context.Context, key *core.Key) error
	// Delete removes a key. Deleting an absent key is not an error.
	Delete(ctx context.Context, hash string) error
	// List returns every stored key.
	List(ctx context.Context) ([]*core.Key, error)
}

// MemStore keeps keys in memory, optionally persisting them to a JSON file.
// Only hashes are written; a plaintext key exists solely in the response that
// first issued it.
type MemStore struct {
	mu   sync.RWMutex
	keys map[string]*core.Key
	path string
}

// NewMemStore returns a store with no persistence.
func NewMemStore() *MemStore {
	return &MemStore{keys: make(map[string]*core.Key)}
}

// NewFileStore returns a store backed by a JSON file, loading any existing
// contents. The file is created on first write.
func NewFileStore(path string) (*MemStore, error) {
	s := &MemStore{keys: make(map[string]*core.Key), path: path}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *MemStore) load() error {
	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read key store %s: %w", s.path, err)
	}
	var keys []*core.Key
	if err := json.Unmarshal(raw, &keys); err != nil {
		return fmt.Errorf("parse key store %s: %w", s.path, err)
	}
	for _, k := range keys {
		s.keys[k.Hash] = k
	}
	return nil
}

// persist writes the store atomically: a temporary file in the same directory
// followed by a rename, so a crash mid-write cannot truncate the real file.
// Callers must hold at least a read lock.
func (s *MemStore) persist() error {
	if s.path == "" {
		return nil
	}
	keys := make([]*core.Key, 0, len(s.keys))
	for _, k := range s.keys {
		keys = append(keys, k)
	}
	raw, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return fmt.Errorf("encode key store: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create key store dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".keys-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp key store: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp key store: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp key store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp key store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp key store: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace key store: %w", err)
	}
	return nil
}

// Get implements KeyStore.
func (s *MemStore) Get(_ context.Context, hash string) (*core.Key, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.keys[hash]
	if !ok {
		return nil, core.ErrKeyInvalid
	}
	clone := *k
	return &clone, nil
}

// Put implements KeyStore.
func (s *MemStore) Put(_ context.Context, key *core.Key) error {
	if key == nil || key.Hash == "" {
		return fmt.Errorf("put key: hash is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := *key
	if clone.CreatedAt.IsZero() {
		clone.CreatedAt = time.Now().UTC()
	}
	s.keys[clone.Hash] = &clone
	return s.persist()
}

// Delete implements KeyStore.
func (s *MemStore) Delete(_ context.Context, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.keys, hash)
	return s.persist()
}

// List implements KeyStore.
func (s *MemStore) List(_ context.Context) ([]*core.Key, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*core.Key, 0, len(s.keys))
	for _, k := range s.keys {
		clone := *k
		out = append(out, &clone)
	}
	return out, nil
}
