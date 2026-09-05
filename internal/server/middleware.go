package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

type requestIDKey struct{}

type infoKey struct{}

// requestInfo is a mutable holder planted by the outermost middleware and filled
// in by the handler. A handler cannot pass values back through its own
// r.WithContext, because that replaces only its local copy of the request, so
// the access log would never see them.
type requestInfo struct {
	mu       sync.Mutex
	keyLabel string
	model    string
}

func (i *requestInfo) set(keyLabel, model string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.keyLabel, i.model = keyLabel, model
}

func (i *requestInfo) get() (string, string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.keyLabel, i.model
}

// AnnotateRequest records who is calling and what they asked for, so the access
// log can attribute a request without the credential itself reaching a log line.
// It is a no-op when the middleware chain is not installed.
func AnnotateRequest(ctx context.Context, keyLabel, model string) {
	if info, ok := ctx.Value(infoKey{}).(*requestInfo); ok {
		info.set(keyLabel, model)
	}
}

// RequestIDFrom returns the request ID assigned by the middleware.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// statusRecorder captures the response status for access logging without
// buffering the body, which would break streaming.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if !r.wrote {
		r.status, r.wrote = http.StatusOK, true
	}
	return r.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the underlying writer, so the
// streaming relay can still flush through this wrapper.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// withMiddleware orders the chain so that each layer can see what the layers
// below it did: the request ID is planted first, the access log wraps the
// response so it can observe the final status, and recovery sits innermost
// where the *statusRecorder is visible and a panic is still attributable to a
// request that gets logged.
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return s.withRequestID(s.withAccessLog(s.withRecovery(next)))
}

func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 8)
		id := "req-unknown"
		if _, err := rand.Read(buf); err == nil {
			id = "req-" + hex.EncodeToString(buf)
		}
		w.Header().Set("x-gateway-request-id", id)
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		ctx = context.WithValue(ctx, infoKey{}, &requestInfo{})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// withRecovery keeps a panic in one handler from taking down the process, and
// never leaks the panic value or a stack trace to the client.
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic in handler",
					"path", r.URL.Path,
					"request_id", RequestIDFrom(r.Context()),
					"panic", v)
				// If the handler already started writing, the response is
				// committed: appending an error envelope would corrupt the
				// body the client is mid-way through parsing.
				if rec, ok := w.(*statusRecorder); ok && rec.wrote {
					return
				}
				writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withAccessLog records one line per request. No credential material is logged:
// keys are identified by alias, never by value.
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// r.Context() is read after the handler runs, so values the handler
		// added — the key label in particular — are visible here.
		ctx := r.Context()
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", RequestIDFrom(ctx),
		}
		if info, ok := ctx.Value(infoKey{}).(*requestInfo); ok {
			if label, model := info.get(); label != "" {
				attrs = append(attrs, "key", label)
				if model != "" {
					attrs = append(attrs, "model", model)
				}
			}
		}
		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		} else if rec.status >= 400 {
			level = slog.LevelWarn
		}
		s.log.Log(ctx, level, "request", attrs...)
	})
}
