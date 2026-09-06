package provider

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/erickardus/ai-gateway/internal/core"
)

// relayBufferSize is deliberately small. Each read is flushed straight through,
// so a large buffer would only add latency to the first token.
const relayBufferSize = 4 << 10

// Relay copies an upstream response body to the client, flushing after every
// chunk.
//
// Two constraints drive this. Claude Code reads the stream as it arrives, so a
// gateway that buffers a complete response before relaying stalls it. And Claude
// Code aborts a stream that goes silent for 300 seconds, counting every byte
// relayed including SSE ping events and comment lines, which are the only
// traffic during long thinking pauses. So nothing here filters, coalesces or
// reinterprets the stream: bytes are passed straight through.
//
// It returns what it observed while passing the bytes through. Usage is what
// the upstream reported, for token accounting: the format decides how those
// counters are read, because the two disagree about whether a reported input
// figure includes the tokens that came from the prompt cache, and reading one
// with the other's rule bills cached tokens twice.
//
// Bytes and FirstChunkAt are measurements only this loop can take. Time to
// first token is the latency a user actually feels, and it is invisible in a
// request's total duration — a fast first token followed by a long generation
// and a slow first token followed by a short one produce the same total.
func Relay(w http.ResponseWriter, body io.Reader, format core.Format) (Stats, error) {
	rc := http.NewResponseController(w)
	sniffer := newUsageSniffer(format)
	buf := make([]byte, relayBufferSize)
	var stats Stats

	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			written, writeErr := w.Write(chunk)
			stats.Bytes += int64(written)
			if stats.FirstChunkAt.IsZero() && written > 0 {
				stats.FirstChunkAt = time.Now()
			}
			if writeErr != nil {
				stats.Usage = sniffer.usage
				return stats, fmt.Errorf("write to client: %w", writeErr)
			}
			// Ignore an unsupported flush: some ResponseWriters in tests do not
			// implement it, and failing to flush is not a reason to drop the
			// response.
			if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
				stats.Usage = sniffer.usage
				return stats, fmt.Errorf("flush to client: %w", err)
			}
			// Sniff only after the bytes are on their way, so parsing never
			// sits between the upstream and the client.
			sniffer.observe(chunk)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				// A non-streaming body is a single object with no trailing
				// newline, so the last line is still buffered here.
				sniffer.flush()
				stats.Usage = sniffer.usage
				return stats, nil
			}
			stats.Usage = sniffer.usage
			return stats, fmt.Errorf("read from upstream: %w", readErr)
		}
	}
}

// Stats is what one relay observed. It is returned even when the relay fails
// part way, because a stream that died after ten seconds and 3 kB still
// happened and still cost money.
type Stats struct {
	// Usage is the token accounting the upstream reported, or the zero value
	// where it reported none.
	Usage core.Usage
	// Bytes is what was actually written to the client.
	Bytes int64
	// FirstChunkAt is when the first byte reached the client, or the zero time
	// if nothing did. It is a wall-clock instant rather than a duration because
	// the interval that matters — how long the caller waited — starts before
	// this function is entered, and only the caller knows when.
	FirstChunkAt time.Time
}

// maxSnifferBuffer bounds the sniffer's carry-over so a stream with no newlines
// cannot grow it without limit.
const maxSnifferBuffer = 64 << 10

// usageSniffer watches a relayed stream for the last reported token usage,
// without altering or delaying it. It is best-effort: a stream shape it does not
// recognize simply yields zero usage.
type usageSniffer struct {
	buf    bytes.Buffer
	usage  core.Usage
	format core.Format
}

func newUsageSniffer(format core.Format) *usageSniffer {
	return &usageSniffer{format: format}
}

// observe feeds a relayed chunk to the sniffer. The chunk has already been
// written to the client, so nothing here can affect what the caller receives.
func (s *usageSniffer) observe(chunk []byte) {
	if s.buf.Len()+len(chunk) > maxSnifferBuffer {
		s.buf.Reset()
	}
	s.buf.Write(chunk)

	for {
		line, err := s.buf.ReadBytes('\n')
		if err != nil {
			// Incomplete line: keep it for the next chunk.
			s.buf.Reset()
			s.buf.Write(line)
			return
		}
		s.scanLine(bytes.TrimSpace(line))
	}
}

// flush scans whatever partial line remains once the stream ends.
func (s *usageSniffer) flush() {
	if s.buf.Len() > 0 {
		s.scanLine(bytes.TrimSpace(s.buf.Bytes()))
		s.buf.Reset()
	}
}

func (s *usageSniffer) scanLine(line []byte) {
	payload := line
	if after, found := bytes.CutPrefix(line, []byte("data:")); found {
		payload = bytes.TrimSpace(after)
	}
	if len(payload) == 0 || payload[0] != '{' || !bytes.Contains(payload, []byte(`"usage"`)) {
		return
	}
	if reported, ok := UsageFromBody(payload, s.format); ok {
		mergeUsage(&s.usage, reported)
	}
}
