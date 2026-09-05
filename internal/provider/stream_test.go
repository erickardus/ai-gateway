package provider

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRelayDoesNotBuffer proves chunks reach the client as they arrive. A
// gateway that buffered the whole response would stall Claude Code.
func TestRelayDoesNotBuffer(t *testing.T) {
	firstWritten := make(chan struct{})
	release := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		rc.Flush()
		close(firstWritten)
		<-release // hold the stream open
		io.WriteString(w, "event: message_delta\ndata: {\"usage\":{\"output_tokens\":7}}\n\n")
		rc.Flush()
	}))
	defer upstream.Close()

	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	<-firstWritten

	// ResponseRecorder is not safe for concurrent access, and this test reads
	// the output while the relay is still writing.
	rec := &syncWriter{header: http.Header{}}
	done := make(chan error, 1)
	go func() {
		_, err := Relay(rec, resp.Body)
		done <- err
	}()

	// The first chunk must be observable while the upstream is still open.
	deadline := time.After(3 * time.Second)
	for {
		if strings.Contains(rec.String(), "message_start") {
			break
		}
		select {
		case <-deadline:
			t.Fatal("first chunk did not reach the client before the upstream finished: the relay is buffering")
		case <-time.After(5 * time.Millisecond):
		}
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if !strings.Contains(rec.String(), "message_delta") {
		t.Error("second chunk missing")
	}
}

// SSE comment lines and ping events are the only traffic during long thinking
// pauses, so they must be relayed rather than filtered.
func TestRelayPreservesPingsAndComments(t *testing.T) {
	in := "event: ping\ndata: {\"type\":\"ping\"}\n\n: this is a comment keep-alive\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	rec := httptest.NewRecorder()
	if _, err := Relay(rec, strings.NewReader(in)); err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if got := rec.Body.String(); got != in {
		t.Errorf("stream was altered.\n got: %q\nwant: %q", got, in)
	}
}

func TestRelayExtractsUsage(t *testing.T) {
	in := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"usage":{"input_tokens":25,"output_tokens":1}}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","usage":{"input_tokens":25,"output_tokens":143}}`,
		"",
	}, "\n")

	rec := httptest.NewRecorder()
	usage, err := Relay(rec, strings.NewReader(in))
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if usage.InputTokens != 25 {
		t.Errorf("InputTokens = %d, want 25", usage.InputTokens)
	}
	if usage.OutputTokens != 143 {
		t.Errorf("OutputTokens = %d, want 143 (the final tally must win)", usage.OutputTokens)
	}
	if usage.Total() != 168 {
		t.Errorf("Total = %d, want 168", usage.Total())
	}
}

func TestRelayPropagatesUpstreamReadError(t *testing.T) {
	rec := httptest.NewRecorder()
	_, err := Relay(rec, io.MultiReader(strings.NewReader("partial"), errReader{}))
	if err == nil {
		t.Fatal("expected the upstream read error to surface")
	}
	if !strings.Contains(rec.Body.String(), "partial") {
		t.Error("bytes read before the error should still have been relayed")
	}
}

// syncWriter is a minimal http.ResponseWriter whose buffer may be read while
// another goroutine writes to it.
type syncWriter struct {
	mu     sync.Mutex
	buf    strings.Builder
	header http.Header
}

func (w *syncWriter) Header() http.Header { return w.header }

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) WriteHeader(int) {}

// Flush makes http.ResponseController's flush succeed without extra work.
func (w *syncWriter) Flush() {}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("upstream exploded") }

func TestSniffUsageNonStreaming(t *testing.T) {
	body := []byte(`{"id":"msg_1","content":[],"usage":{"input_tokens":10,"output_tokens":20}}`)
	if got := SniffUsage(body); got.InputTokens != 10 || got.OutputTokens != 20 {
		t.Errorf("SniffUsage = %+v, want 10/20", got)
	}
	openai := []byte(`{"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
	if got := SniffUsage(openai); got.InputTokens != 3 || got.OutputTokens != 4 {
		t.Errorf("SniffUsage(openai) = %+v, want 3/4", got)
	}
}
