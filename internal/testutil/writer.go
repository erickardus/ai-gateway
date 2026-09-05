// Package testutil holds helpers shared by the gateway's tests.
package testutil

import (
	"net/http"
	"strings"
	"sync"
)

// SyncWriter is an http.ResponseWriter whose buffer may be read while another
// goroutine writes to it. httptest.ResponseRecorder is not safe for that, and
// any test that observes a stream while it is still being relayed needs it.
type SyncWriter struct {
	mu     sync.Mutex
	buf    strings.Builder
	header http.Header
	status int
}

// NewSyncWriter returns a ready SyncWriter.
func NewSyncWriter() *SyncWriter {
	return &SyncWriter{header: http.Header{}, status: http.StatusOK}
}

// Header implements http.ResponseWriter.
func (w *SyncWriter) Header() http.Header { return w.header }

// Write implements http.ResponseWriter.
func (w *SyncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// WriteHeader implements http.ResponseWriter.
func (w *SyncWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = code
}

// Flush satisfies http.ResponseController without extra work.
func (w *SyncWriter) Flush() {}

// String returns what has been written so far.
func (w *SyncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// Status returns the recorded status code.
func (w *SyncWriter) Status() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}
