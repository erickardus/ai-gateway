package translate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/erickardus/ai-gateway/internal/core"
)

const (
	// maxEventBytes bounds one SSE event. An upstream that never sends a
	// newline would otherwise grow the line buffer without limit, and a
	// translating gateway has to hold a whole event to rewrite it, so the
	// bound is the difference between a slow upstream and an exhausted one.
	maxEventBytes = 4 << 20
	// maxBufferedBodyBytes bounds a non-streamed reply, which must be held
	// whole because there is no way to rewrite half a JSON document.
	maxBufferedBodyBytes = 32 << 20
	// readBufferSize is the line reader's window. Events are small; this only
	// sets how often the reader refills.
	readBufferSize = 16 << 10
)

// errEventTooLarge is returned when one SSE event exceeds maxEventBytes.
var errEventTooLarge = errors.New("upstream sent an SSE event larger than the translator will buffer")

// transformer converts one upstream SSE line into whatever the client's format
// says at that point, appending to out. A transformer emits nothing for a line
// that carries no information in the target format, which is most of them:
// blank separators, comments, and the event: name lines whose payload arrives
// on the following data: line.
type transformer interface {
	line(line []byte, out *bytes.Buffer)
	// eof is called once the upstream body ends, so a transformer can close a
	// message the upstream left open. An upstream that dies mid-stream still
	// produced a partial reply, and a client holding an unterminated message
	// waits for an event that will never come.
	eof(out *bytes.Buffer)
}

// streamReader converts an upstream SSE body to the client's format as it
// arrives.
//
// It reads a line at a time and releases whatever that line produced
// immediately, which is what keeps a translated stream a stream: Claude Code
// renders tokens as they land and aborts a stream silent for 300 seconds, so a
// translator that buffered a whole reply before emitting it would turn every
// long generation into a timeout.
type streamReader struct {
	src  io.ReadCloser
	br   *bufio.Reader
	tr   transformer
	out  bytes.Buffer
	done bool
	err  error
}

func newStreamReader(from, to core.Format, src io.ReadCloser) io.ReadCloser {
	var tr transformer
	switch {
	case from == core.FormatOpenAI && to == core.FormatAnthropic:
		tr = newOpenAIToAnthropicStream()
	case from == core.FormatAnthropic && to == core.FormatOpenAI:
		tr = newAnthropicToOpenAIStream()
	default:
		return src
	}
	return &streamReader{src: src, br: bufio.NewReaderSize(src, readBufferSize), tr: tr}
}

func (r *streamReader) Read(p []byte) (int, error) {
	for r.out.Len() == 0 && !r.done {
		line, err := readEventLine(r.br)
		if len(line) > 0 {
			r.tr.line(line, &r.out)
		}
		if err != nil {
			r.done = true
			r.tr.eof(&r.out)
			if !errors.Is(err, io.EOF) {
				r.err = err
			}
		}
	}
	if r.out.Len() > 0 {
		return r.out.Read(p)
	}
	if r.err != nil {
		return 0, r.err
	}
	return 0, io.EOF
}

func (r *streamReader) Close() error { return r.src.Close() }

// readEventLine returns one line with its terminator stripped, refusing a line
// longer than maxEventBytes rather than allocating whatever the upstream sends.
func readEventLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(buf)+len(chunk) > maxEventBytes {
			return nil, errEventTooLarge
		}
		buf = append(buf, chunk...)
		if err == nil {
			return bytes.TrimRight(buf, "\r\n"), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimRight(buf, "\r\n"), err
	}
}

// writeEvent appends one SSE event in Anthropic's shape, which names the event
// on its own line. Anthropic clients dispatch on that name.
func writeEvent(out *bytes.Buffer, name string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	out.WriteString("event: ")
	out.WriteString(name)
	out.WriteString("\ndata: ")
	out.Write(data)
	out.WriteString("\n\n")
}

// writeData appends one SSE event in OpenAI's shape, which carries no event
// name at all.
func writeData(out *bytes.Buffer, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	out.WriteString("data: ")
	out.Write(data)
	out.WriteString("\n\n")
}

// eventPayload returns the JSON carried by a data: line, or nil for a line that
// carries none — a blank separator, a comment, an event: name, or OpenAI's
// [DONE] sentinel, which is a terminator rather than a document.
func eventPayload(line []byte) []byte {
	rest, found := bytes.CutPrefix(line, []byte("data:"))
	if !found {
		return nil
	}
	rest = bytes.TrimSpace(rest)
	if len(rest) == 0 || rest[0] != '{' {
		return nil
	}
	return rest
}

// isDone reports the OpenAI stream terminator.
func isDone(line []byte) bool {
	rest, found := bytes.CutPrefix(line, []byte("data:"))
	return found && bytes.Equal(bytes.TrimSpace(rest), []byte("[DONE]"))
}
