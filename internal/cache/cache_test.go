package cache

import (
	"context"
	"net/http"
	"testing"
	"testing/synctest"
	"time"
)

func testEntry(body string) *Entry {
	return &Entry{Status: 200, Header: http.Header{}, Body: []byte(body)}
}

func TestMemoryRoundTrip(t *testing.T) {
	ctx := context.Background()
	c := NewMemory(10)

	if _, hit, _ := c.Get(ctx, "absent"); hit {
		t.Error("a miss reported a hit")
	}
	if err := c.Put(ctx, "k", testEntry("hello"), time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, hit, err := c.Get(ctx, "k")
	if err != nil || !hit {
		t.Fatalf("Get = %v, %v", hit, err)
	}
	if string(got.Body) != "hello" {
		t.Errorf("body = %q", got.Body)
	}
}

func TestEntriesExpire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := NewMemory(10)
		c.Put(ctx, "k", testEntry("v"), time.Minute)

		time.Sleep(59 * time.Second)
		if _, hit, _ := c.Get(ctx, "k"); !hit {
			t.Fatal("entry expired early")
		}
		time.Sleep(2 * time.Second)
		if _, hit, _ := c.Get(ctx, "k"); hit {
			t.Fatal("entry outlived its TTL")
		}
	})
}

func TestZeroTTLDoesNotStore(t *testing.T) {
	ctx := context.Background()
	c := NewMemory(10)
	c.Put(ctx, "k", testEntry("v"), 0)
	if _, hit, _ := c.Get(ctx, "k"); hit {
		t.Error("a zero TTL should store nothing")
	}
}

// The cache must stay bounded, evicting what was used least recently.
func TestLRUEviction(t *testing.T) {
	ctx := context.Background()
	c := NewMemory(3)
	for _, k := range []string{"a", "b", "c"} {
		c.Put(ctx, k, testEntry(k), time.Minute)
	}
	// Touch "a" so "b" becomes the least recently used.
	if _, hit, _ := c.Get(ctx, "a"); !hit {
		t.Fatal("a should still be present")
	}
	c.Put(ctx, "d", testEntry("d"), time.Minute)

	if c.Len() != 3 {
		t.Errorf("Len = %d, want 3", c.Len())
	}
	if _, hit, _ := c.Get(ctx, "b"); hit {
		t.Error("b should have been evicted as least recently used")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, hit, _ := c.Get(ctx, k); !hit {
			t.Errorf("%s should have been retained", k)
		}
	}
}

func TestPurge(t *testing.T) {
	ctx := context.Background()
	c := NewMemory(10)
	c.Put(ctx, "a", testEntry("a"), time.Minute)
	c.Put(ctx, "b", testEntry("b"), time.Minute)
	if err := c.Purge(ctx); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if c.Len() != 0 {
		t.Errorf("Len = %d after purge", c.Len())
	}
}

// Different scopes must never collide: this is what keeps one caller from
// seeing another's completion.
func TestKeyIsolation(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"secret"}]}`)

	a := Key("key-hash-a", "anthropic", "m", body)
	b := Key("key-hash-b", "anthropic", "m", body)
	if a == b {
		t.Fatal("two virtual keys produced the same cache key: one caller could be served another's response")
	}

	shared1 := Key("", "anthropic", "m", body)
	shared2 := Key("", "anthropic", "m", body)
	if shared1 != shared2 {
		t.Error("shared scope should produce a stable key")
	}
	if shared1 == a {
		t.Error("shared and per-key scopes must not collide")
	}
}

func TestKeyVariesWithEveryComponent(t *testing.T) {
	base := Key("s", "anthropic", "m", []byte("body"))
	for name, got := range map[string]string{
		"scope":  Key("s2", "anthropic", "m", []byte("body")),
		"format": Key("s", "openai", "m", []byte("body")),
		"model":  Key("s", "anthropic", "m2", []byte("body")),
		"body":   Key("s", "anthropic", "m", []byte("body2")),
	} {
		if got == base {
			t.Errorf("changing the %s did not change the key", name)
		}
	}
}

// Length prefixing prevents field boundaries from being ambiguous, so that
// concatenations which differ only in where one field ends cannot collide.
func TestKeyFieldsAreUnambiguous(t *testing.T) {
	a := Key("ab", "c", "m", []byte("x"))
	b := Key("a", "bc", "m", []byte("x"))
	if a == b {
		t.Error("field boundaries are ambiguous: ('ab','c') collided with ('a','bc')")
	}
}

func TestConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	c := NewMemory(50)
	done := make(chan struct{})
	for i := range 20 {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			for j := range 50 {
				k := string(rune('a' + (i+j)%20))
				c.Put(ctx, k, testEntry(k), time.Minute)
				c.Get(ctx, k)
			}
		}(i)
	}
	for range 20 {
		<-done
	}
	if c.Len() > 50 {
		t.Errorf("Len = %d, exceeded the bound", c.Len())
	}
}
