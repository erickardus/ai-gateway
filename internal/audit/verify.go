package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// Summary describes a verified chain. LastSeq and LastHash are what an operator
// copies somewhere the gateway cannot write, which is the only thing that makes
// truncation of the tail detectable — see the package comment.
type Summary struct {
	Records  int
	FirstSeq uint64
	LastSeq  uint64
	LastHash string
}

// BreakError reports where a chain stopped verifying.
//
// It names the line as well as the sequence number because those two disagree
// exactly when something was removed, and the difference between them is the
// most useful number in the whole report.
type BreakError struct {
	Line   int
	Seq    uint64
	Reason string
}

func (e *BreakError) Error() string {
	if e.Seq == 0 {
		return fmt.Sprintf("audit chain broken at line %d: %s", e.Line, e.Reason)
	}
	return fmt.Sprintf("audit chain broken at line %d (seq %d): %s", e.Line, e.Seq, e.Reason)
}

// maxLine bounds one record. A record is a few hundred bytes; anything past
// this is a corrupted or hostile file rather than a long user agent, and
// bufio.Scanner's default 64 KiB would refuse it with a less useful error.
const maxLine = 1 << 20

// Verify walks a chain and reports the first record that breaks it.
//
// Three things are checked per record, and they catch different edits: the
// sequence must increment by one, which catches a deleted line; Prev must equal
// the previous record's hash, which catches a line replaced with a valid record
// from elsewhere; and the record's own hash must match its contents, which
// catches a field edited in place.
//
// An empty input is a valid empty chain, not an error. A gateway that has taken
// no administrative actions has nothing to prove.
func Verify(r io.Reader) (Summary, error) {
	var summary Summary
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxLine)

	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}

		var rec Record
		dec := json.NewDecoder(bytes.NewReader(raw))
		// Unknown fields are refused rather than ignored: a field the decoder
		// silently drops is a field that is not covered by the hash it then
		// recomputes, which would turn "the record was edited" into "the record
		// verifies".
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rec); err != nil {
			return summary, &BreakError{Line: line, Reason: "record is not a valid audit record: " + err.Error()}
		}

		switch {
		case summary.Records == 0:
			if rec.Prev != "" {
				return summary, &BreakError{Line: line, Seq: rec.Seq,
					Reason: "the first record continues an earlier hash, so records before it were removed"}
			}
			summary.FirstSeq = rec.Seq
		case rec.Seq != summary.LastSeq+1:
			return summary, &BreakError{Line: line, Seq: rec.Seq,
				Reason: sequenceBreak(summary.LastSeq, rec.Seq)}
		case rec.Prev != summary.LastHash:
			return summary, &BreakError{Line: line, Seq: rec.Seq,
				Reason: "this record does not follow the one before it"}
		}

		want, err := hashOf(rec)
		if err != nil {
			return summary, &BreakError{Line: line, Seq: rec.Seq, Reason: err.Error()}
		}
		if want != rec.Hash {
			return summary, &BreakError{Line: line, Seq: rec.Seq,
				Reason: "the record's contents do not match its hash, so it was altered after it was written"}
		}

		summary.Records++
		summary.LastSeq, summary.LastHash = rec.Seq, rec.Hash
	}
	if err := scanner.Err(); err != nil {
		return summary, fmt.Errorf("read audit log: %w", err)
	}

	// A chain that does not start at 1 has had its beginning removed. This is
	// checked at the end rather than on the first record so that the report
	// says how much of the file did verify.
	if summary.Records > 0 && summary.FirstSeq != 1 {
		// Sequences start at 1, so a zero here is not a chain missing its
		// beginning but a record no version of this gateway ever wrote.
		reason := fmt.Sprintf("the chain starts at sequence %d, so the first %d record(s) were removed", summary.FirstSeq, summary.FirstSeq-1)
		if summary.FirstSeq == 0 {
			reason = "the chain starts at sequence 0, which no record this gateway writes ever carries"
		}
		return summary, &BreakError{Line: 1, Seq: summary.FirstSeq, Reason: reason}
	}
	return summary, nil
}

// VerifyFile verifies a chain on disk. A file that does not exist returns an
// error wrapping fs.ErrNotExist, which OpenFile reads as "no chain yet".
func VerifyFile(path string) (Summary, error) {
	f, err := os.Open(path)
	if err != nil {
		return Summary{}, fmt.Errorf("open audit log %s: %w", path, err)
	}
	defer f.Close()

	summary, err := Verify(f)
	if err != nil {
		return summary, fmt.Errorf("%s: %w", path, err)
	}
	return summary, nil
}

// sequenceBreak describes a sequence that did not increment by one.
//
// A jump forward means records were removed, and saying how many is the useful
// report. Anything else — a repeat, a step backwards — means the file was
// rewritten rather than trimmed, and there is no removal count to give. The
// distinction is not only editorial: these are unsigned, so subtracting the
// wrong way round would report a removal of nine quintillion records.
func sequenceBreak(last, got uint64) string {
	if got > last+1 {
		return fmt.Sprintf("sequence jumped from %d, so %d record(s) were removed", last, got-last-1)
	}
	return fmt.Sprintf("sequence went from %d to %d rather than forward by one, so records were repeated, reordered or rewritten", last, got)
}
