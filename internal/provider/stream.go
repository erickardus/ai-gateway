package provider

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"

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
// It returns any usage reported by the upstream, for token accounting. The
// format decides how those counters are read: the two disagree about whether a
// reported input figure includes the tokens that came from the prompt cache,
// and reading one with the other's rule bills cached tokens twice.
func Relay(w http.ResponseWriter, body io.Reader, format core.Format) (core.Usage, error) {
	rc := http.NewResponseController(w)
	sniffer := newUsageSniffer(format)
	buf := make([]byte, relayBufferSize)

	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, writeErr := w.Write(chunk); writeErr != nil {
				return sniffer.usage, fmt.Errorf("write to client: %w", writeErr)
			}
			// Ignore an unsupported flush: some ResponseWriters in tests do not
			// implement it, and failing to flush is not a reason to drop the
			// response.
			if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return sniffer.usage, fmt.Errorf("flush to client: %w", err)
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
				return sniffer.usage, nil
			}
			return sniffer.usage, fmt.Errorf("read from upstream: %w", readErr)
		}
	}
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
