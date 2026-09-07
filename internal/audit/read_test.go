package audit

import (
	"context"
	"io"
	"path/filepath"
	"testing"
)

// Tail keeps a window of the newest records rather than the whole file, so the
// index arithmetic that recycles the window is what a chain longer than the
// limit exercises. Getting it wrong returns the right number of records in the
// wrong order, which reads as a chain out of sequence.
func TestFileSinkTailReturnsTheNewestRecordsNewestFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	sink, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer sink.Close()
	writeN(t, sink, 25)

	got, err := sink.Tail(context.Background(), 10)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("got %d records, want 10", len(got))
	}
	for i, rec := range got {
		if want := uint64(25 - i); rec.Seq != want {
			t.Fatalf("record %d has seq %d, want %d", i, rec.Seq, want)
		}
	}
	// The record has to arrive whole, not merely in order: the console renders
	// the actor and the target, and a Tail that returned only the envelope would
	// look correct in a sequence check and empty on the page.
	if got[0].Target != "hash-25" || got[0].Actor.Kind != ActorMasterKey {
		t.Fatalf("newest record = %+v", got[0])
	}
}

// A chain shorter than the limit returns all of it, and one with nothing in it
// returns nothing rather than an error: a gateway that has taken no
// administrative actions has nothing to show.
func TestFileSinkTailHandlesShortAndEmptyChains(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	sink, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer sink.Close()

	got, err := sink.Tail(context.Background(), 100)
	if err != nil {
		t.Fatalf("Tail on an empty chain: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("an empty chain returned %d records", len(got))
	}

	writeN(t, sink, 3)
	got, err = sink.Tail(context.Background(), 100)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 3 || got[0].Seq != 3 || got[2].Seq != 1 {
		t.Fatalf("got %+v, want three records newest first", got)
	}
}

// The stdout sink writes to a stream it cannot read back, so it must not claim
// it can. A caller distinguishes the two by this assertion, and a WriterSink
// that satisfied Reader would have to lie in it.
func TestWriterSinkIsNotAReader(t *testing.T) {
	if _, ok := any(NewWriterSink(io.Discard)).(Reader); ok {
		t.Fatal("the writer sink claims it can read back what it wrote")
	}
}
