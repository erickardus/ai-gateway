package audit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Reader is implemented by sinks that can read their own chain back.
//
// It is a second interface rather than a method on Sink because the ability to
// write a chain and the ability to read one are genuinely different properties:
// the stdout sink hands records to whatever collects the process's logs and can
// never fetch one back, and a Sink method it had to stub would be a method that
// lies. A caller asks with a type assertion and reports "this sink cannot be
// read here" when the assertion fails, which is a truthful answer rather than
// an error.
type Reader interface {
	// Tail returns at most limit records, newest first. A chain with nothing in
	// it yields no records and no error: a gateway that has taken no
	// administrative actions has nothing to show.
	Tail(ctx context.Context, limit int) ([]Record, error)
}

// Verifier is implemented by sinks that can check their own chain on demand, at
// a cost a caller is willing to pay while somebody waits for a page.
//
// It is deliberately not implemented by every sink that can be read. Verifying
// means walking a whole chain, and how much that costs depends on where the
// chain lives: a file holds one host's administrative history and is local, so
// re-reading it is the same work OpenFile already does at every start, while the
// Postgres chain holds the fleet's and is across a network. PostgresSink.Verify
// exists for `gateway -verify-audit` and for a nightly job, and satisfies this
// interface — the judgement about whether a given caller should pay for it
// belongs to that caller, which is why this only says the operation exists.
type Verifier interface {
	Verify(ctx context.Context) (Summary, error)
}

var (
	_ Reader   = (*FileSink)(nil)
	_ Reader   = (*PostgresSink)(nil)
	_ Verifier = (*FileSink)(nil)
	_ Verifier = (*PostgresSink)(nil)
)

// Tail reads the end of the file's chain.
//
// The whole file is scanned rather than seeked into from the end, because a
// record is a line of unbounded length and finding the nth from last without
// reading forwards would mean scanning backwards for newlines that may sit
// inside no record this gateway wrote. The cost is one pass over a file bounded
// by how many administrative actions this host has ever taken — the same read
// OpenFile makes at every start.
//
// It is taken under the chain's own lock, which is what stops a read landing in
// the middle of an append: FileSink writes one record with one Write and then
// fsyncs, and a reader that arrived between those two would see a line the
// writer is still finishing.
func (s *FileSink) Tail(_ context.Context, limit int) ([]Record, error) {
	if limit < 1 {
		return []Record{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.Open(s.f.Name())
	if err != nil {
		// A log that was configured but never written to is not a failure to
		// report; it is a gateway that has recorded nothing yet.
		if errors.Is(err, fs.ErrNotExist) {
			return []Record{}, nil
		}
		return nil, fmt.Errorf("open audit log %s: %w", s.f.Name(), err)
	}
	defer f.Close()

	// A ring, so a file with a hundred thousand records costs the memory of the
	// page being asked for rather than the memory of the whole history.
	held := make([]Record, 0, limit)
	oldest := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), maxLine)
	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		rec, err := decodeRecord(raw, line)
		if err != nil {
			return nil, err
		}
		if len(held) < limit {
			held = append(held, rec)
			continue
		}
		held[oldest] = rec
		oldest = (oldest + 1) % limit
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read audit log %s: %w", s.f.Name(), err)
	}

	out := make([]Record, 0, len(held))
	for i := len(held) - 1; i >= 0; i-- {
		out = append(out, held[(oldest+i)%len(held)])
	}
	return out, nil
}

// Verify re-reads the whole file and reports where, if anywhere, the chain
// stops verifying. It is the same walk OpenFile makes, and returns the same
// *BreakError, so a caller can tell a broken chain from an unreadable file.
func (s *FileSink) Verify(_ context.Context) (Summary, error) {
	return VerifyFile(s.f.Name())
}

// Tail reads the newest records from the shared chain.
//
// Newest-first in the query rather than in this process, so the database returns
// only the rows asked for: the table holds the fleet's whole administrative
// history, and ordering it here would mean fetching all of it to show a page.
func (s *PostgresSink) Tail(parent context.Context, limit int) ([]Record, error) {
	if limit < 1 {
		return []Record{}, nil
	}
	ctx, cancel := context.WithTimeout(parent, s.timeout)
	defer cancel()

	raws, err := scanRecords(ctx, s.pool,
		"SELECT record FROM gateway_audit ORDER BY seq DESC LIMIT $1", limit)
	if err != nil {
		return nil, fmt.Errorf("audit log: read the newest records: %w", err)
	}
	out := make([]Record, 0, len(raws))
	for i, raw := range raws {
		rec, err := decodeRecord(raw, i+1)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// decodeRecord parses one stored record for display.
//
// Unlike the verifier's decode this accepts unknown fields. The two are asking
// different questions: verification recomputes a hash and must refuse a field it
// would silently drop, because dropping one turns "this record was edited" into
// "this record verifies". Reading for display recomputes nothing, and a
// record written by a newer gateway is better shown with the fields this binary
// understands than refused outright.
func decodeRecord(raw []byte, pos int) (Record, error) {
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Record{}, fmt.Errorf("audit record %d is not readable: %w", pos, err)
	}
	return rec, nil
}
