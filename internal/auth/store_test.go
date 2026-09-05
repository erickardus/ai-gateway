package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/erickardus/ai-gateway/internal/core"
)

func TestGenerateProducesDistinctPrefixedKeys(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		pt, hash, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if !strings.HasPrefix(pt, core.PrefixVirtualKey) {
			t.Fatalf("key %q lacks the %q prefix", pt, core.PrefixVirtualKey)
		}
		if core.IsUpstreamCredential(pt) {
			t.Fatalf("generated key %q looks like an upstream credential", pt)
		}
		if hash != HashKey(pt) {
			t.Fatal("returned hash does not match HashKey")
		}
		if seen[pt] {
			t.Fatal("Generate returned a duplicate key")
		}
		seen[pt] = true
	}
}

func TestFileStoreRoundTripStoresOnlyHashes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "keys.json")
	ctx := context.Background()

	store, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	plaintext, hash, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	key := &core.Key{Hash: hash, Alias: "test", Models: []string{"claude-*"}, RPMLimit: 10}
	if err := store.Put(ctx, key); err != nil {
		t.Fatalf("Put: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store file: %v", err)
	}
	if strings.Contains(string(raw), plaintext) {
		t.Fatal("the plaintext key was written to disk")
	}
	if !strings.Contains(string(raw), hash) {
		t.Fatal("the key hash was not written to disk")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("store file mode = %v, want 0600", perm)
	}

	reopened, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := reopened.Get(ctx, hash)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.Alias != "test" || got.RPMLimit != 10 {
		t.Errorf("round-tripped key mismatch: %+v", got)
	}

	if err := reopened.Delete(ctx, hash); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := reopened.Get(ctx, hash); !errors.Is(err, core.ErrKeyInvalid) {
		t.Errorf("Get after delete: got %v, want ErrKeyInvalid", err)
	}
}

func TestGetReturnsCopy(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	if err := store.Put(ctx, &core.Key{Hash: "h", Alias: "original"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(ctx, "h")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Alias = "mutated"
	again, _ := store.Get(ctx, "h")
	if again.Alias != "original" {
		t.Error("mutating a returned key corrupted the store")
	}
}

func TestKeyUsableAndModelMatching(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Hour), now.Add(time.Hour)

	if err := (&core.Key{Blocked: true}).Usable(now); !errors.Is(err, core.ErrKeyBlocked) {
		t.Errorf("blocked key: got %v", err)
	}
	if err := (&core.Key{ExpiresAt: &past}).Usable(now); !errors.Is(err, core.ErrKeyBlocked) {
		t.Errorf("expired key: got %v", err)
	}
	if err := (&core.Key{ExpiresAt: &future}).Usable(now); err != nil {
		t.Errorf("live key: got %v", err)
	}

	tests := []struct {
		models []string
		model  string
		want   bool
	}{
		{nil, "anything", true},
		{[]string{"*"}, "anything", true},
		{[]string{"claude-*"}, "claude-sonnet-4-5", true},
		{[]string{"claude-*"}, "gpt-5", false},
		{[]string{"exact"}, "exact", true},
		{[]string{"exact"}, "exactly", false},
		{[]string{"a", "b-*"}, "b-1", true},
	}
	for _, tc := range tests {
		k := &core.Key{Models: tc.models}
		if got := k.AllowsModel(tc.model); got != tc.want {
			t.Errorf("AllowsModel(%v, %q) = %v, want %v", tc.models, tc.model, got, tc.want)
		}
	}
}

// A malformed key file must be reported, not panic the process at startup.
func TestFileStoreRejectsMalformedEntries(t *testing.T) {
	for name, content := range map[string]string{
		"null entry":   `[null]`,
		"missing hash": `[{"alias":"x"}]`,
		"not json":     `{`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "keys.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			// Must return an error rather than panicking.
			if _, err := NewFileStore(path); err == nil {
				t.Fatal("expected an error for a malformed key store")
			}
		})
	}
}
