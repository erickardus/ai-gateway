package otlp

import (
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erickardus/ai-gateway/internal/metrics"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestEndpointURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"http://localhost:4318", "http://localhost:4318/v1/metrics"},
		{"http://localhost:4318/", "http://localhost:4318/v1/metrics"},
		{"https://otlp.example.com", "https://otlp.example.com/v1/metrics"},
		// An endpoint that already names a path is taken as given, so a
		// collector behind a rewriting proxy stays reachable.
		{"https://otlp.example.com/custom/metrics", "https://otlp.example.com/custom/metrics"},
		{"https://otlp.example.com/v1/metrics", "https://otlp.example.com/v1/metrics"},
	}
	for _, tt := range tests {
		if got := endpointURL(tt.in); got != tt.want {
			t.Errorf("endpointURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestExportPostsProtobufToCollector(t *testing.T) {
	var (
		mu       sync.Mutex
		gotType  string
		gotAuth  string
		gotBody  []byte
		requests int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotType, gotAuth, gotBody, requests = r.Header.Get("Content-Type"), r.Header.Get("X-Api-Key"), body, requests+1
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	exp, err := New(sampleRegistry(t), Options{
		Endpoint: srv.URL,
		Headers:  map[string]string{"X-Api-Key": "secret-token"},
		Resource: sampleResource(),
	}, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := exp.Export(t.Context()); err != nil {
		t.Fatalf("Export: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if requests != 1 {
		t.Fatalf("collector saw %d requests, want 1", requests)
	}
	if gotType != "application/x-protobuf" {
		t.Errorf("Content-Type = %q, want application/x-protobuf", gotType)
	}
	if gotAuth != "secret-token" {
		t.Errorf("configured header not sent: X-Api-Key = %q", gotAuth)
	}
	findMetric(t, gotBody, metrics.MCost)
}

func TestExportCompresses(t *testing.T) {
	var (
		mu       sync.Mutex
		encoding string
		body     []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		encoding, body = r.Header.Get("Content-Encoding"), b
		mu.Unlock()
	}))
	defer srv.Close()

	exp, err := New(sampleRegistry(t), Options{Endpoint: srv.URL, Compress: true, Resource: sampleResource()}, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := exp.Export(t.Context()); err != nil {
		t.Fatalf("Export: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if encoding != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", encoding)
	}
	// The header must describe the bytes: a payload declared gzip that is not
	// gzip is rejected by the collector with an error about the schema.
	zr, err := gzip.NewReader(strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("body declared gzip but does not decompress: %v", err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	findMetric(t, plain, metrics.MCost)
}

// A collector that rejects an export must produce an error naming the status
// and whatever it said, because "export failed" alone is not actionable.
func TestExportSurfacesCollectorRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "unsupported content type")
	}))
	defer srv.Close()

	exp, err := New(sampleRegistry(t), Options{Endpoint: srv.URL}, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = exp.Export(t.Context())
	if err == nil {
		t.Fatal("Export succeeded against a rejecting collector")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "unsupported content type") {
		t.Errorf("error = %v, want it to carry the status and the collector's message", err)
	}
}

// The final export on shutdown is the point of running the loop at all: without
// it everything since the last tick is lost, which for a short-lived process is
// all of its telemetry.
func TestRunFlushesOnShutdown(t *testing.T) {
	received := make(chan []byte, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		select {
		case received <- b:
		default:
		}
	}))
	defer srv.Close()

	// An interval far longer than the test, so the only export that can happen
	// is the one on shutdown.
	exp, err := New(sampleRegistry(t), Options{
		Endpoint: srv.URL, Interval: time.Hour, Resource: sampleResource(),
	}, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		exp.Run(ctx)
		close(done)
	}()
	cancel()

	select {
	case body := <-received:
		findMetric(t, body, metrics.MCost)
	case <-time.After(5 * time.Second):
		t.Fatal("no export arrived after shutdown")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// An unreachable collector must not take the gateway down with it, and must not
// write a line per interval for the length of the outage either.
func TestExportFailureIsLoggedSparsely(t *testing.T) {
	exp, err := New(sampleRegistry(t), Options{
		Endpoint: "http://127.0.0.1:1", Timeout: 100 * time.Millisecond,
	}, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := exp.Export(t.Context()); err == nil {
		t.Fatal("Export succeeded against a dead collector")
	}
	for i := 1; i <= 12; i++ {
		exp.reportFailure(err)
	}
	if exp.consecutiveFailures != 12 {
		t.Errorf("consecutiveFailures = %d, want 12", exp.consecutiveFailures)
	}
	exp.reportSuccess()
	if exp.consecutiveFailures != 0 {
		t.Error("a successful export did not reset the failure count")
	}
}

func TestNewRejectsIncompleteOptions(t *testing.T) {
	if _, err := New(nil, Options{Endpoint: "http://x"}, quietLogger()); err == nil {
		t.Error("New accepted a nil registry")
	}
	if _, err := New(metrics.New(), Options{}, quietLogger()); err == nil {
		t.Error("New accepted an empty endpoint")
	}
}
