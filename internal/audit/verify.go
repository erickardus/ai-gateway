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

// verifier walks records in order and checks each against the one before it.
//
// It is shared by every source a chain can be read from — a file, a stream of
// database rows, the two rows at the tip of one — so that "verified" means the
// same thing whichever an auditor is holding, and so a check added here cannot
// be added to only one of them.
//
// Three things are checked per record, and they catch different edits: the
// sequence must increment by one, which catches a deleted record; Prev must
// equal the previous record's hash, which catches a record replaced with a
// valid one from elsewhere; and the record's own hash must match its contents,
// which catches a field edited in place.
type verifier struct {
	summary Summary
	// origin is whether the first record pushed is expected to begin the
	// chain. A whole chain begins at sequence 1 with an empty Prev; a window
	// read from the tail of a long one does not, and demanding it there would
	// report every healthy gateway's boot check as tampering.
	origin bool
}

// push offers the verifier the next record. pos is where it was found — a line
// number in a file, a row ordinal in a query — and appears in the break report
// beside the sequence number, because those two disagree exactly when something
// was removed.
func (v *verifier) push(pos int, raw []byte) error {
	var rec Record
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Unknown fields are refused rather than ignored: a field the decoder
	// silently drops is a field that is not covered by the hash it then
	// recomputes, which would turn "the record was edited" into "the record
	// verifies".
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		return &BreakError{Line: pos, Reason: "record is not a valid audit record: " + err.Error()}
	}

	switch {
	case v.summary.Records == 0:
		// Sequence 1 must begin a chain whether or not the caller expected to
		// be reading the beginning of one, since no first record has anything
		// before it to continue.
		if (v.origin || rec.Seq == 1) && rec.Prev != "" {
			return &BreakError{Line: pos, Seq: rec.Seq,
				Reason: "the first record continues an earlier hash, so records before it were removed"}
		}
		v.summary.FirstSeq = rec.Seq
	case rec.Seq != v.summary.LastSeq+1:
		return &BreakError{Line: pos, Seq: rec.Seq, Reason: sequenceBreak(v.summary.LastSeq, rec.Seq)}
	case rec.Prev != v.summary.LastHash:
		return &BreakError{Line: pos, Seq: rec.Seq,
			Reason: "this record does not follow the one before it"}
	}

	want, err := hashOf(rec)
	if err != nil {
		return &BreakError{Line: pos, Seq: rec.Seq, Reason: err.Error()}
	}
	if want != rec.Hash {
		return &BreakError{Line: pos, Seq: rec.Seq,
			Reason: "the record's contents do not match its hash, so it was altered after it was written"}
	}

	v.summary.Records++
	v.summary.LastSeq, v.summary.LastHash = rec.Seq, rec.Hash
	return nil
}

// done applies the check that can only be made once the whole chain has been
// read: that it starts where a chain starts.
//
// It is deferred to the end rather than made on the first record so that the
// report can say how much of the source did verify.
func (v *verifier) done() error {
	if !v.origin || v.summary.Records == 0 || v.summary.FirstSeq == 1 {
		return nil
	}
	// Sequences start at 1, so a zero here is not a chain missing its
	// beginning but a record no version of this gateway ever wrote.
	reason := fmt.Sprintf("the chain starts at sequence %d, so the first %d record(s) were removed",
		v.summary.FirstSeq, v.summary.FirstSeq-1)
	if v.summary.FirstSeq == 0 {
		reason = "the chain starts at sequence 0, which no record this gateway writes ever carries"
	}
	return &BreakError{Line: 1, Seq: v.summary.FirstSeq, Reason: reason}
}

// Verify walks a whole chain of JSON Lines and reports the first record that
// breaks it.
//
// An empty input is a valid empty chain, not an error. A gateway that has taken
// no administrative actions has nothing to prove.
func Verify(r io.Reader) (Summary, error) {
	v := &verifier{origin: true}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxLine)

	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		if err := v.push(line, raw); err != nil {
			return v.summary, err
		}
	}
	if err := scanner.Err(); err != nil {
		return v.summary, fmt.Errorf("read audit log: %w", err)
	}
	return v.summary, v.done()
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
