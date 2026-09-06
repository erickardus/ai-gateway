package reqlog

import (
	"strconv"
	"sync"
	"testing"
)

func record(id, group string) Record {
	return Record{ID: id, ModelGroup: group, Outcome: OutcomeSuccess}
}

func TestRecentReturnsNewestFirst(t *testing.T) {
	r := NewRing(4)
	for i := range 3 {
		r.Add(record(strconv.Itoa(i), "g"))
	}
	got := r.Recent(10, Filter{})
	if len(got) != 3 {
		t.Fatalf("Recent returned %d records, want 3", len(got))
	}
	for i, want := range []string{"2", "1", "0"} {
		if got[i].ID != want {
			t.Fatalf("Recent()[%d].ID = %q, want %q", i, got[i].ID, want)
		}
	}
}

// The buffer is fixed size on purpose: an unbounded log inside a long-lived
// proxy is a memory leak. Overwriting must be silent and must not reorder what
// survives.
func TestRingEvictsOldest(t *testing.T) {
	r := NewRing(3)
	for i := range 5 {
		r.Add(record(strconv.Itoa(i), "g"))
	}
	if r.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", r.Len())
	}
	got := r.Recent(10, Filter{})
	for i, want := range []string{"4", "3", "2"} {
		if got[i].ID != want {
			t.Fatalf("after wrapping, Recent()[%d].ID = %q, want %q", i, got[i].ID, want)
		}
	}
}

func TestRecentHonoursLimit(t *testing.T) {
	r := NewRing(10)
	for i := range 8 {
		r.Add(record(strconv.Itoa(i), "g"))
	}
	if got := r.Recent(3, Filter{}); len(got) != 3 {
		t.Fatalf("Recent(3) returned %d records, want 3", len(got))
	}
	if got := r.Recent(0, Filter{}); len(got) != 0 {
		t.Fatalf("Recent(0) returned %d records, want 0", len(got))
	}
}

func TestFilters(t *testing.T) {
	r := NewRing(10)
	r.Add(Record{ID: "a", ModelGroup: "anthropic", Deployment: "d1", SpendSubject: "h1", Outcome: OutcomeSuccess})
	r.Add(Record{ID: "b", ModelGroup: "openai", Deployment: "d2", SpendSubject: "h2", Outcome: "upstream_error"})
	r.Add(Record{ID: "c", ModelGroup: "anthropic", Deployment: "d2", SpendSubject: "h1", Outcome: "rejected"})

	for _, tc := range []struct {
		name   string
		filter Filter
		want   []string
	}{
		{"none", Filter{}, []string{"c", "b", "a"}},
		{"group", Filter{ModelGroup: "anthropic"}, []string{"c", "a"}},
		{"deployment", Filter{Deployment: "d2"}, []string{"c", "b"}},
		{"key", Filter{SpendSubject: "h1"}, []string{"c", "a"}},
		{"outcome", Filter{Outcome: "rejected"}, []string{"c"}},
		{"errors", Filter{Errors: true}, []string{"c", "b"}},
		{"combined", Filter{Errors: true, ModelGroup: "openai"}, []string{"b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := r.Recent(10, tc.filter)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d records, want %d", len(got), len(tc.want))
			}
			for i, want := range tc.want {
				if got[i].ID != want {
					t.Fatalf("record %d = %q, want %q", i, got[i].ID, want)
				}
			}
		})
	}
}

// A capacity of zero is how an operator turns recording off, and it must be a
// no-op rather than a panic on every request.
func TestZeroCapacityRecordsNothing(t *testing.T) {
	r := NewRing(0)
	if r.Enabled() {
		t.Fatal("a zero-capacity ring reports itself enabled")
	}
	r.Add(record("a", "g"))
	if r.Len() != 0 || len(r.Recent(10, Filter{})) != 0 {
		t.Fatal("a zero-capacity ring stored a record")
	}
}

// The ring is written from every request and read from an admin page, so the
// race detector needs to see both happening at once.
func TestConcurrentAddAndRead(t *testing.T) {
	r := NewRing(64)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				r.Add(record(strconv.Itoa(w*1000+i), "g"))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 500 {
			_ = r.Recent(20, Filter{ModelGroup: "g"})
			_ = r.Len()
		}
	}()
	wg.Wait()
	if r.Len() != 64 {
		t.Fatalf("Len() = %d, want 64", r.Len())
	}
}
