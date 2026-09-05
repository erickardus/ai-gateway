package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"
)

type requestIDKey struct{}

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

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return s.withRecovery(s.withRequestID(s.withAccessLog(next)))
}

func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 8)
		id := "req-unknown"
		if _, err := rand.Read(buf); err == nil {
			id = "req-" + hex.EncodeToString(buf)
		}
		w.Header().Set("x-gateway-request-id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
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
				// committed and adding a status would only corrupt it.
				if rec, ok := w.(*statusRecorder); !ok || !rec.wrote {
					writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
				}
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

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", RequestIDFrom(r.Context()),
		}
		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		} else if rec.status >= 400 {
			level = slog.LevelWarn
		}
		s.log.Log(r.Context(), level, "request", attrs...)
	})
}
