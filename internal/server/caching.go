package server

import (
	"bytes"
	"net/http"
	"strings"

	"github.com/erickardus/ai-gateway/internal/auth"
	"github.com/erickardus/ai-gateway/internal/cache"
	"github.com/erickardus/ai-gateway/internal/core"
	"github.com/erickardus/ai-gateway/internal/provider"
)

// cacheHeader tells the caller whether their response came from the cache.
const cacheHeader = "x-gateway-cache"

// cacheKeyFor derives the key for a request, or reports false when caching is
// off for it.
//
// Under the default per-key scope the key hash is part of the digest, so one
// caller can never be served another's completion. Under the shared scope the
// scope component is empty and every caller draws from the same pool — which
// configuration validation only permits when no passthrough deployment exists.
func (s *Server) cacheKeyFor(authCtx *auth.Context, format core.Format, model string, body []byte) (string, bool) {
	if s.cache == nil {
		return "", false
	}
	scopeID := ""
	if s.cfg.Cache.Scope == cache.ScopeKey && authCtx.Key != nil {
		scopeID = authCtx.Key.Hash
	}
	return cache.Key(scopeID, string(format), model, body), true
}

// serveFromCache writes a stored response, reporting whether it did.
//
// A streamed response is replayed as the bytes that were originally relayed. The
// timing is not reproduced — the whole point is that it arrives at once — but
// the event sequence the client parses is identical.
func (s *Server) serveFromCache(w http.ResponseWriter, entry *cache.Entry, model string) {
	for name, values := range entry.Header {
		if strings.EqualFold(name, "Content-Length") {
			// The body is written directly, so let net/http size it.
			continue
		}
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.Header().Set(cacheHeader, "hit")
	w.Header().Set("x-gateway-model-id", model)
	w.WriteHeader(entry.Status)

	if entry.Streaming {
		// Replay through the same relay path, so flushing behaves as it does
		// for a live stream and a client reading incrementally is not stalled.
		_, _ = provider.Relay(w, bytes.NewReader(entry.Body))
		return
	}
	_, _ = w.Write(entry.Body)
}

// cacheable reports whether a response should be stored.
//
// Only successful responses are cached. An error is a fact about one moment — a
// rate limit, an overloaded upstream — and replaying it for the TTL would turn a
// transient failure into a sticky one.
//
// The status check is defence in depth rather than the mechanism: the transport
// already turns any status at or above 400 into an error, so a failed response
// never reaches this path. It stays because that is a property of another
// package, and this one should not silently start caching errors if it changes.
func (s *Server) cacheable(status int, body []byte) bool {
	if s.cache == nil || status < 200 || status >= 300 {
		return false
	}
	return int64(len(body)) <= s.cfg.Cache.MaxEntryBytes
}

// teeWriter relays to the client while capturing a copy for the cache.
//
// The client's writes are never delayed by the capture, and the capture stops
// silently once it exceeds the size limit so an outsized response costs nothing
// beyond the bytes already held.
type teeWriter struct {
	http.ResponseWriter
	capture    *bytes.Buffer
	limit      int64
	overflowed bool
}

func (t *teeWriter) Write(p []byte) (int, error) {
	n, err := t.ResponseWriter.Write(p)
	if n > 0 && !t.overflowed {
		if int64(t.capture.Len()+n) > t.limit {
			t.overflowed = true
			t.capture.Reset()
		} else {
			t.capture.Write(p[:n])
		}
	}
	return n, err
}

// Unwrap lets http.ResponseController reach the real writer, so the relay can
// still flush each chunk through this wrapper.
func (t *teeWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// Overflowed reports whether the response exceeded the size limit.
func (t *teeWriter) Overflowed() bool { return t.overflowed }

// cacheableHeaders keeps the headers that describe the payload and drops those
// that describe one particular exchange, which would be wrong to replay.
func cacheableHeaders(src http.Header) http.Header {
	out := http.Header{}
	for name, values := range src {
		switch strings.ToLower(name) {
		case "content-type", "anthropic-version":
			out[name] = values
		}
	}
	return out
}
