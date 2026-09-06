package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// event builds a plausible administrative record for the tests to chain.
func event(n int) Event {
	return Event{
		Action:     ActionKeyGenerate,
		Actor:      MasterKeyActor(),
		TargetKind: TargetKey,
		Target:     fmt.Sprintf("hash-%d", n),
		Outcome:    OutcomeSuccess,
		SourceIP:   "10.0.0.1",
		RequestID:  fmt.Sprintf("req-%04d", n),
		UserAgent:  "curl/8.4.0",
		Detail:     map[string]string{"alias": fmt.Sprintf("key-%d", n), "via": "api"},
	}
}

func writeN(t *testing.T, sink Sink, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if _, err := sink.Record(context.Background(), event(i)); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

func writeLines(t *testing.T, path string, lines []string) {
	t.Helper()
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// A restart must continue the chain it finds rather than beginning a new one.
// Starting over would leave a file whose sequence numbers repeat and whose
// records no longer link, which is indistinguishable from tampering.
func TestFileSinkContinuesTheChainAcrossARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	first, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	writeN(t, first, 3)
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := OpenFile(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	rec, err := second.Record(context.Background(), event(4))
	if err != nil {
		t.Fatalf("record after restart: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if rec.Seq != 4 {
		t.Errorf("first record after a restart has seq %d, want 4", rec.Seq)
	}
	summary, err := VerifyFile(path)
	if err != nil {
		t.Fatalf("verify a chain written across two processes: %v", err)
	}
	if summary.Records != 4 || summary.LastSeq != 4 {
		t.Errorf("summary = %+v, want 4 records ending at seq 4", summary)
	}
}

// A closed sink refuses records rather than dropping them, which is the whole
// posture of this package in one method.
func TestClosedSinkRefusesRecords(t *testing.T) {
	sink := NewWriterSink(&bytes.Buffer{})
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := sink.Record(context.Background(), event(1)); err == nil {
		t.Fatal("a closed sink accepted a record")
	}
}

// Each of these edits is one an attacker would actually attempt on a JSONL
// file, and each must be caught by a different one of the three checks Verify
// makes.
func TestVerifyDetectsTampering(t *testing.T) {
	tests := []struct {
		name string
		// tamper rewrites the file's lines. It receives the four records the
		// harness wrote.
		tamper func(lines []string) []string
		want   string
	}{
		{
			name: "a field edited in place",
			tamper: func(lines []string) []string {
				lines[1] = strings.Replace(lines[1], `"hash-2"`, `"hash-X"`, 1)
				return lines
			},
			want: "do not match its hash",
		},
		{
			name: "an outcome rewritten from refused to success",
			tamper: func(lines []string) []string {
				lines[2] = strings.Replace(lines[2], `"outcome":"success"`, `"outcome":"refused"`, 1)
				return lines
			},
			want: "do not match its hash",
		},
		{
			name: "a record removed from the middle",
			tamper: func(lines []string) []string {
				return append(lines[:1:1], lines[2:]...)
			},
			want: "sequence jumped",
		},
		{
			name: "the first record removed",
			tamper: func(lines []string) []string {
				return lines[1:]
			},
			want: "records before it were removed",
		},
		{
			name: "a record replaced with a valid record from another chain",
			tamper: func(lines []string) []string {
				// Sealed correctly and numbered correctly, but not the record
				// that follows the one before it.
				other := chain{now: time.Now, emit: func([]byte) error { return nil }}
				other.seq = 2
				rec, err := other.record(event(99))
				if err != nil {
					t.Fatalf("build a foreign record: %v", err)
				}
				line, err := json.Marshal(rec)
				if err != nil {
					t.Fatalf("encode a foreign record: %v", err)
				}
				lines[2] = string(line)
				return lines
			},
			want: "does not follow the one before it",
		},
		{
			name: "a field the schema does not have, smuggled in",
			tamper: func(lines []string) []string {
				lines[1] = strings.Replace(lines[1], `{"seq"`, `{"note":"approved","seq"`, 1)
				return lines
			},
			want: "not a valid audit record",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "audit.jsonl")
			sink, err := OpenFile(path)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			writeN(t, sink, 4)
			if err := sink.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if _, err := VerifyFile(path); err != nil {
				t.Fatalf("the untampered chain did not verify: %v", err)
			}

			writeLines(t, path, tc.tamper(readLines(t, path)))

			_, err = VerifyFile(path)
			if err == nil {
				t.Fatal("the tampered chain verified")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			var broken *BreakError
			if !errors.As(err, &broken) {
				t.Errorf("error is %T, want a *BreakError naming the line", err)
			}
		})
	}
}

// A chain whose earlier records no longer verify must not be quietly extended:
// the resulting file would be half evidence with nothing marking the boundary.
func TestOpenFileRefusesABrokenChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	sink, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	writeN(t, sink, 3)
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := readLines(t, path)
	writeLines(t, path, append(lines[:1:1], lines[2:]...))

	if _, err := OpenFile(path); err == nil {
		t.Fatal("a gateway would have started on a broken audit chain")
	}
}

// The truncation this cannot catch, stated as a test so that the day someone
// changes it, they change this too. See the package comment.
func TestVerifyCannotDetectATruncatedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	sink, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	writeN(t, sink, 4)
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	full, err := VerifyFile(path)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	writeLines(t, path, readLines(t, path)[:2])

	short, err := VerifyFile(path)
	if err != nil {
		t.Fatalf("a truncated chain is internally consistent and should verify: %v", err)
	}
	// What closes the gap is the anchor, not the chain: the recorded tail no
	// longer matches, which is only visible to someone who kept a copy of it.
	if short.LastSeq == full.LastSeq {
		t.Fatal("truncation left the sequence unchanged; the anchor would not detect it either")
	}
}

// The sequence and the hash chain must stay consistent when several
// administrative requests land at once. Run under -race, this is also the
// assertion that the sink is safe to share between handlers.
func TestConcurrentRecordsProduceOneOrderedChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	sink, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	const writers, each = 8, 25
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := sink.Record(context.Background(), event(w*each+i)); err != nil {
					t.Errorf("record: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	summary, err := VerifyFile(path)
	if err != nil {
		t.Fatalf("concurrent writes broke the chain: %v", err)
	}
	if summary.Records != writers*each {
		t.Errorf("records = %d, want %d", summary.Records, writers*each)
	}
}

// The writer sink is chained too. Its records reach a collector rather than a
// file, and a record removed from that stream must be as visible as one removed
// from disk.
func TestWriterSinkChainsItsRecords(t *testing.T) {
	var buf bytes.Buffer
	sink := NewWriterSink(&buf)
	writeN(t, sink, 3)

	summary, err := Verify(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if summary.Records != 3 || summary.FirstSeq != 1 {
		t.Errorf("summary = %+v, want 3 records starting at seq 1", summary)
	}
}

// A gateway that has administered nothing has an empty log, and an empty log is
// a valid chain rather than a failure to report.
func TestVerifyAcceptsAnEmptyChain(t *testing.T) {
	summary, err := Verify(strings.NewReader(""))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if summary.Records != 0 {
		t.Errorf("records = %d, want 0", summary.Records)
	}
}

// Nothing in a record may carry credential material. The server package's own
// callers are what keep secrets out of Detail; this holds the shape of the
// record itself to it, so a field added here has to be argued for.
func TestRecordCarriesNoBodyOrCredentialField(t *testing.T) {
	sink := NewWriterSink(&bytes.Buffer{})
	rec, err := sink.Record(context.Background(), event(1))
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(line, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The whole schema, listed so that adding a field is a deliberate act. A
	// record has no place to put a prompt, a completion or a key even by
	// accident.
	allowed := map[string]bool{
		"seq": true, "at": true, "prev": true, "action": true, "actor": true,
		"target_kind": true, "target": true, "outcome": true, "reason": true,
		"source_ip": true, "request_id": true, "user_agent": true,
		"detail": true, "hash": true,
	}
	for name := range decoded {
		if !allowed[name] {
			t.Errorf("audit records carry an undeclared field %q", name)
		}
	}
}

// A hostile User-Agent must not be able to write a record that the chain can
// never read back.
//
// Verify reads with a bounded scanner and OpenFile refuses a chain it cannot
// verify, so before the fields were clipped a caller who could reach any
// audited endpoint could choose a header long enough to stop the gateway
// booting again — permanently, since the record it left behind is durable.
func TestAnOversizedFieldCannotBreakTheChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	sink, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	// Comfortably past maxLine, and past it again once JSON escaping is done:
	// an ampersand becomes six bytes as &.
	hostile := strings.Repeat("&", 4<<20)
	if _, err := sink.Record(context.Background(), Event{
		Action:    ActionConsoleSignIn,
		Actor:     Unauthenticated(),
		Outcome:   OutcomeRefused,
		UserAgent: hostile,
		Reason:    hostile,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	summary, err := VerifyFile(path)
	if err != nil {
		t.Fatalf("a record with an oversized field broke the chain: %v", err)
	}
	if summary.Records != 1 {
		t.Fatalf("records = %d, want 1", summary.Records)
	}
	// And the gateway can still open it, which is the property that actually
	// matters: a chain that does not verify is a gateway that will not start.
	reopened, err := OpenFile(path)
	if err != nil {
		t.Fatalf("reopening the chain: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
}

// A record that was not written whole must leave the file as it found it.
//
// The chain advances its sequence only on a successful write, so bytes left
// behind by a failed one would make the next record reuse a sequence the file
// has already used — and a chain with a repeated sequence never verifies again,
// which turns a transient disk error into a gateway that cannot be started.
func TestAnIncompleteWriteIsRolledBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	sink, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })

	if _, err := sink.Record(context.Background(), Event{
		Action: ActionKeyGenerate, Actor: MasterKeyActor(),
		TargetKind: TargetKey, Target: "abc123", Outcome: OutcomeSuccess,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// Stand in for a write that reached the file and then failed: half a line,
	// exactly what an ENOSPC leaves behind.
	if _, err := sink.f.Write([]byte(`{"seq":2,"action":"key.del`)); err != nil {
		t.Fatalf("partial write: %v", err)
	}
	if err := sink.rollback(int64(len(good))); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after rollback: %v", err)
	}
	if string(after) != string(good) {
		t.Fatalf("rollback left the file changed:\n got %q\nwant %q", after, good)
	}

	// The next record takes the sequence the failed one did not, and the chain
	// still verifies — the whole point of rolling back.
	if _, err := sink.Record(context.Background(), Event{
		Action: ActionKeyDelete, Actor: MasterKeyActor(),
		TargetKind: TargetKey, Target: "abc123", Outcome: OutcomeSuccess,
	}); err != nil {
		t.Fatalf("Record after rollback: %v", err)
	}
	summary, err := VerifyFile(path)
	if err != nil {
		t.Fatalf("chain broken after a rolled-back write: %v", err)
	}
	if summary.Records != 2 || summary.LastSeq != 2 {
		t.Fatalf("records = %d, last seq = %d; want 2 and 2", summary.Records, summary.LastSeq)
	}
}

// A sequence that repeats or goes backwards is a rewritten file, not a trimmed
// one, and the report must not subtract its way into an unsigned wraparound.
func TestASequenceThatDoesNotAdvanceReportsNoAbsurdCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	sink, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := sink.Record(context.Background(), Event{
			Action: ActionKeyGenerate, Actor: MasterKeyActor(),
			TargetKind: TargetKey, Target: "abc123", Outcome: OutcomeSuccess,
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Duplicate the second line, which is what a rolled-back write used to
	// leave behind.
	lines := strings.Split(strings.TrimRight(readFile(t, path), "\n"), "\n")
	replayed := strings.Join(append(lines, lines[1]), "\n") + "\n"
	if err := os.WriteFile(path, []byte(replayed), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err = VerifyFile(path)
	if err == nil {
		t.Fatal("a repeated sequence verified")
	}
	var broken *BreakError
	if !errors.As(err, &broken) {
		t.Fatalf("error is %T, want *BreakError: %v", err, err)
	}
	// 18446744073709551615 is what uint64(2)-uint64(2)-1 reports.
	if strings.Contains(broken.Reason, "18446744073709551615") {
		t.Errorf("the break reason underflowed: %s", broken.Reason)
	}
	if !strings.Contains(broken.Reason, "repeated") {
		t.Errorf("reason = %q, want it to name a repeat rather than a removal", broken.Reason)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
