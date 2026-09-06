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
// administrative actions the gateway has ever taken — a number in the
// thousands, not the millions, because inference is not audited here.
//
// A broken chain refuses to open, so the gateway refuses to start. That is the
// deliberate choice: continuing a chain whose earlier records no longer verify
// would produce one file that is half evidence and half not, with nothing in it
// saying where the boundary is. The operator's move is to archive the existing
// file and let a new chain begin, which is a decision a person should make.
func OpenFile(path string) (*FileSink, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		// 0700: the log names people and the keys they hold, so it is not
		// something to leave group-readable by default on a shared host.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create audit log directory %s: %w", dir, err)
		}
	}

	summary, err := VerifyFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit log %s: %w", path, err)
	}

	s := &FileSink{f: f}
	s.now = time.Now
	s.emit = s.append
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
	if _, err := s.f.Write(line); err != nil {
		return fmt.Errorf("write audit record: %w", err)
	}
	if err := s.f.Sync(); err != nil {
		return fmt.Errorf("sync audit log: %w", err)
	}
	return nil
}

// Close flushes and closes the file.
func (s *FileSink) Close() error { return s.close(s.f.Close) }

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
func (s *WriterSink) Close() error { return s.close(nil) }
