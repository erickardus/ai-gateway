// Package otlp exports the gateway's metrics over OTLP/HTTP.
//
// It speaks the protocol directly rather than through the OpenTelemetry SDK.
// The SDK and its exporter pull in gRPC and the protobuf runtime — a large
// dependency tree for a process whose entire published surface is a few dozen
// fixed metric families, in a project whose stated dependencies are a YAML
// parser and a Redis client. What the SDK would buy here is instrumentation
// libraries and dynamic views, neither of which this gateway uses: the metric
// set is decided at compile time in internal/metrics.
//
// Only the metrics signal is implemented. Traces and logs would each be another
// schema and another exporter, and the gateway already emits structured logs
// with a request ID that a collector can correlate on.
package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/erickardus/ai-gateway/internal/metrics"
)

// Protocol selects the payload encoding.
type Protocol string

const (
	ProtocolProtobuf Protocol = "http/protobuf"
	ProtocolJSON     Protocol = "http/json"
)

// defaultPath is what the OTLP/HTTP spec appends to a base endpoint for the
// metrics signal. An endpoint that already names a path is used as given, so a
// collector behind a rewriting proxy stays reachable.
const defaultPath = "/v1/metrics"

// Options configures an Exporter.
type Options struct {
	// Endpoint is the collector's base URL, e.g. http://localhost:4318.
	Endpoint string
	Protocol Protocol
	// Headers are sent on every export, typically an API key for a hosted
	// collector.
	Headers map[string]string
	// Interval is how often metrics are pushed.
	Interval time.Duration
	// Timeout bounds one export attempt.
	Timeout time.Duration
	// Compress sends the payload gzipped.
	Compress bool
	// Resource identifies this process to the collector.
	Resource Resource
	// Client is exposed for tests. Nil builds one from Timeout.
	Client *http.Client
}

// Exporter pushes metric snapshots to an OTLP collector.
type Exporter struct {
	opts   Options
	url    string
	source *metrics.Registry
	log    *slog.Logger
	client *http.Client

	// consecutiveFailures throttles the log rather than the export. A collector
	// that is down for an hour would otherwise write one error line per
	// interval for an hour, burying everything else in the log.
	consecutiveFailures int
}

// New builds an Exporter. It does not start it.
func New(source *metrics.Registry, opts Options, log *slog.Logger) (*Exporter, error) {
	if source == nil {
		return nil, errors.New("otlp: no metrics registry")
	}
	if opts.Endpoint == "" {
		return nil, errors.New("otlp: endpoint is required")
	}
	if opts.Protocol == "" {
		opts.Protocol = ProtocolProtobuf
	}
	if opts.Interval <= 0 {
		opts.Interval = 60 * time.Second
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: opts.Timeout}
	}
	return &Exporter{
		opts:   opts,
		url:    endpointURL(opts.Endpoint),
		source: source,
		log:    log,
		client: client,
	}, nil
}

// endpointURL appends the metrics path unless the endpoint already carries one.
func endpointURL(endpoint string) string {
	trimmed := strings.TrimSuffix(endpoint, "/")
	// Anything after the host is treated as a deliberate path. Checking for a
	// slash past the scheme is enough: "http://host" has none, "http://host/x"
	// does.
	if i := strings.Index(trimmed, "://"); i >= 0 {
		if strings.Contains(trimmed[i+3:], "/") {
			return trimmed
		}
	}
	return trimmed + defaultPath
}

// Run pushes on the interval until ctx is cancelled, then pushes once more.
//
// The final export is the point of doing this on shutdown at all: without it
// everything since the last tick is lost, which on a short-lived process — a
// job, a canary, a container that failed its first health check — is the whole
// of its telemetry.
func (e *Exporter) Run(ctx context.Context) {
	ticker := time.NewTicker(e.opts.Interval)
	defer ticker.Stop()

	e.log.Info("otlp exporter started",
		"endpoint", e.url, "protocol", string(e.opts.Protocol), "interval", e.opts.Interval)

	for {
		select {
		case <-ctx.Done():
			// A cancelled context cannot carry the final request, so the flush
			// gets its own deadline.
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.opts.Timeout)
			if err := e.Export(flushCtx); err != nil {
				e.log.Warn("final otlp export failed", "error", err)
			}
			cancel()
			return
		case <-ticker.C:
			if err := e.Export(ctx); err != nil {
				e.reportFailure(err)
				continue
			}
			e.reportSuccess()
		}
	}
}

// Export sends one snapshot.
func (e *Exporter) Export(ctx context.Context) error {
	families := e.source.Collect()
	if len(families) == 0 {
		return nil
	}
	now := time.Now()

	var (
		body        []byte
		contentType string
		err         error
	)
	switch e.opts.Protocol {
	case ProtocolJSON:
		contentType = "application/json"
		body, err = EncodeJSON(families, e.opts.Resource, e.source.StartedAt(), now)
	default:
		contentType = "application/x-protobuf"
		body = EncodeProtobuf(families, e.opts.Resource, e.source.StartedAt(), now)
	}
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}

	encoding := ""
	if e.opts.Compress {
		if body, err = gzipBytes(body); err != nil {
			return fmt.Errorf("compress: %w", err)
		}
		encoding = "gzip"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	for k, v := range e.opts.Headers {
		req.Header.Set(k, v)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("post to %s: %w", e.url, err)
	}
	defer resp.Body.Close()
	// The response body must be drained for the connection to be reused, and a
	// rejected export explains itself there — but a collector returning
	// megabytes of complaint should not be allowed to hold memory, so it is
	// read under a cap.
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("collector returned %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	return nil
}

// reportFailure logs the first failure, then backs off logarithmically so a
// long outage stays visible without flooding.
func (e *Exporter) reportFailure(err error) {
	e.consecutiveFailures++
	n := e.consecutiveFailures
	if n == 1 || n == 10 || n%100 == 0 {
		e.log.Error("otlp export failed; metrics are still served on /metrics",
			"error", err, "endpoint", e.url, "consecutive_failures", n)
	}
}

func (e *Exporter) reportSuccess() {
	if e.consecutiveFailures > 0 {
		e.log.Info("otlp export recovered", "endpoint", e.url, "after_failures", e.consecutiveFailures)
	}
	e.consecutiveFailures = 0
}

func gzipBytes(b []byte) ([]byte, error) {
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
