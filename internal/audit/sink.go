package audit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// FileSink appends JSON Lines to a file, continuing whatever chain it finds
// there.
//
// One record per line, so the file is greppable, tailable, and readable by
// anything that reads logs — which matters because the people who will read it
// are auditors with jq rather than a service with a client library.
type FileSink struct {
	chain
	f *os.File
}

// OpenFile opens or creates an audit log and positions the chain at its tail.
//
// The whole existing file is verified, not just its last line. Reading only the
// tail would continue the chain correctly and miss the thing the chain is for:
// a record altered or removed from the middle leaves a perfectly good last
// line. The cost is a full read at startup, which is bounded by how many
// administrative actions this host has ever recorded — thousands, not millions,
// because inference is not audited here.
//
// A file whose chain does not verify opens **sealed**: the sink is returned,
// it appends nothing, and every record offered to it is refused with the
// reason. The error return is for a file that could not be read or created at
// all, which is a different thing — see the package comment on why one of those
// stops the gateway starting and the other must not. A caller that cares
// (cmd/gateway does) asks Sealed.
func OpenFile(path string) (*FileSink, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		// 0700: the log names people and the keys they hold, so it is not
		// something to leave group-readable by default on a shared host.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create audit log directory %s: %w", dir, err)
		}
	}

	summary, err := VerifyFile(path)
	var sealReason string
	var broken *BreakError
	switch {
	case err == nil, errors.Is(err, fs.ErrNotExist):
	case errors.As(err, &broken):
		sealReason = err.Error()
	default:
		// Not a verification failure but an unreadable file — a permission
		// problem, a directory where a log should be. Retrying is what fixes
		// those, and a restart is how an orchestrator retries.
		return nil, err
	}

	f, ferr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if ferr != nil {
		return nil, fmt.Errorf("open audit log %s: %w", path, ferr)
	}

	s := &FileSink{f: f}
	s.now = time.Now
	s.emit = s.append
	if sealReason != "" {
		// The position is deliberately left at zero rather than at the tail: a
		// sealed sink writes nothing, so resuming a chain that does not verify
		// would only be arranging for bytes that will never be written.
		s.seal(sealReason)
		return s, nil
	}
	s.resume(summary.LastSeq, summary.LastHash)
	return s, nil
}

// Record seals an event and returns only once it is on disk.
func (s *FileSink) Record(_ context.Context, e Event) (Record, error) {
	return s.record(e)
}

// append writes one sealed line and waits for it to be durable.
//
// The fsync is the point of the whole arrangement: a record that is in the page
// cache when the machine loses power is a record that did not happen, and the
// action it was authorizing did. It is affordable because administrative
// actions are rare — see the package comment on what is deliberately not
// audited.
func (s *FileSink) append(line []byte) error {
	// Where the file ends before this record, so that a write which does not
	// complete can be undone.
	//
	// The chain advances its sequence only once this returns nil, so bytes left
	// behind by a failed write would put the file and the chain permanently out
	// of step: the next record would carry a sequence the file has already
	// used, and a chain with a repeated sequence never verifies again. Because
	// OpenFile refuses a chain that does not verify, that is a gateway which can
	// no longer be started — so a transient ENOSPC, or an fsync that fails after
	// the write reached the page cache, would take the gateway down for good
	// rather than failing one administrative action. Rolling back is what keeps
	// the file holding exactly the records the chain counted.
	before, err := s.f.Stat()
	if err != nil {
		return fmt.Errorf("size the audit log: %w", err)
	}
	if _, err := s.f.Write(line); err != nil {
		return errors.Join(fmt.Errorf("write audit record: %w", err), s.rollback(before.Size()))
	}
	if err := s.f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync audit log: %w", err), s.rollback(before.Size()))
	}
	return nil
}

// rollback discards a record that was not written whole.
//
// Its own failure is joined to the write's rather than replacing it: the
// mutation is being refused either way, and the operator needs to know both
// that the record could not be written and that the log may now need a hand.
func (s *FileSink) rollback(to int64) error {
	if err := s.f.Truncate(to); err != nil {
		return fmt.Errorf("roll back the incomplete audit record, so the log may hold a partial line after byte %d: %w", to, err)
	}
	// Made durable for the same reason the record was: a truncation still in
	// the page cache is a partial line that survives a power loss, which is the
	// state this function exists to prevent.
	if err := s.f.Sync(); err != nil {
		return fmt.Errorf("sync the rolled-back audit log: %w", err)
	}
	return nil
}

// Close flushes and closes the file.
func (s *FileSink) Close() error { return s.shut(s.f.Close) }

// Path is the file being written, for the startup log line.
func (s *FileSink) Path() string { return s.f.Name() }

// WriterSink writes the same JSON Lines to an io.Writer, which for the stdout
// sink is the process's own stdout.
//
// Its chain begins at sequence 1 on every start, because a writer cannot read
// back what it wrote. That is not a defect to be worked around but the shape of
// the thing: stdout hands the records to whatever is collecting the process's
// logs, and continuity across restarts is that collector's to provide. Within
// one process the chain is exactly as tamper-evident as the file sink's, which
// is what catches a record removed from a shipped stream.
type WriterSink struct {
	chain
	w io.Writer
}

// NewWriterSink records to w.
func NewWriterSink(w io.Writer) *WriterSink {
	s := &WriterSink{w: w}
	s.now = time.Now
	s.emit = s.write
	return s
}

// NewStdoutSink records to the process's stdout, beside its ordinary logs.
func NewStdoutSink() *WriterSink { return NewWriterSink(os.Stdout) }

// Record seals an event and writes it.
func (s *WriterSink) Record(_ context.Context, e Event) (Record, error) {
	return s.record(e)
}

func (s *WriterSink) write(line []byte) error {
	if _, err := s.w.Write(line); err != nil {
		return fmt.Errorf("write audit record: %w", err)
	}
	return nil
}

// Close stops the sink. The writer is not closed: this sink does not own it,
// and closing stdout would take the process's logging with it.
func (s *WriterSink) Close() error { return s.shut(nil) }
