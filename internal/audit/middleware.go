// Package audit provides the outer HTTP audit middleware. It emits exactly one
// safe structured JSON event per data request. It never
// records the request path, the client request_id, the body, restored text,
// scope_id, tokens, mappings, ciphertext, keys, authorization headers or
// internal error text. The operation is derived from a fixed method/path
// mapping and the correlation ID is generated server-side with crypto/rand.
package audit

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// fallbackCounter is a per-process counter used to make the crypto/rand
// fallback correlation ID unique across requests.
var fallbackCounter atomic.Uint64

// Middleware returns an http.Handler that wraps next with the outer audit
// observer. For every recognized data operation it creates a context-local
// collector, records the response status, and emits exactly one safe event
// after next returns. Emission is deferred so a panic from the router or
// pipeline still produces exactly one error event and is
// then re-raised unchanged. Health and metrics endpoints are never audited. A
// nil logger disables auditing entirely.
func Middleware(logger *Logger, mode ModelMode) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			op, ok := operationFor(r.Method, r.URL.Path, r.URL.EscapedPath())
			if !ok || logger == nil {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			collector := NewCollector()
			collector.SetModelMode(mode)
			ctx := WithCollector(r.Context(), collector)
			rec := &statusRecorder{ResponseWriter: w}

			// Emission is deferred so a panic downstream still produces exactly
			// one event. A panic is classified as error regardless of any status
			// already written, and is re-raised with its original value. The
			// event never carries the panic value, error detail, body or path.
			defer func() {
				entities, model := collector.Snapshot()
				result := ResultSuccess
				if rec.status >= 400 {
					result = ResultError
				}
				if p := recover(); p != nil {
					result = ResultError
					_ = logger.Log(Event{
						RequestID: newCorrelationID(),
						Operation: op,
						Entities:  entities,
						Duration:  time.Since(start),
						ModelMode: model,
						Result:    result,
					})
					panic(p)
				}
				// A write error must never change the user's HTTP response or be
				// logged another way; it is deliberately discarded.
				_ = logger.Log(Event{
					RequestID: newCorrelationID(),
					Operation: op,
					Entities:  entities,
					Duration:  time.Since(start),
					ModelMode: model,
					Result:    result,
				})
			}()

			next.ServeHTTP(rec, r.WithContext(ctx))
		})
	}
}

// statusRecorder records the first status written by the wrapped handler so
// the middleware can classify the outcome as success or error. It never
// inspects or stores the response body. It mirrors net/http semantics: only
// the first WriteHeader is forwarded to the underlying writer; subsequent
// calls are ignored.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

// WriteHeader records the first status and forwards it exactly once. A status
// of 0 means the handler never wrote a header; on a normal return that is an
// implicit 200/success, while a panic is classified separately by the
// middleware. Repeated calls after the first are ignored, matching net/http.
func (s *statusRecorder) WriteHeader(code int) {
	if s.status != 0 {
		return
	}
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Write forwards the body and records a 200 when no header was written yet.
func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// operationFor maps a known method/path pattern to its fixed audit operation.
// It never records the actual path value. The revoke route matches only
// DELETE /v1/pii/scopes/{one non-empty URL segment}; the scope_id segment is
// never captured. It uses the escaped path so a percent-encoded slash inside
// the scope segment (e.g. a%2Fb) is still recognized as a single segment,
// while a literal extra segment (a/b) is not.
func operationFor(method, path, escapedPath string) (Operation, bool) {
	switch {
	case method == http.MethodPost && path == "/process":
		return OpProcess, true
	case method == http.MethodPost && path == "/v1/pii/detect":
		return OpDetect, true
	case method == http.MethodPost && path == "/v1/pii/tokenize":
		return OpTokenize, true
	case method == http.MethodPost && path == "/v1/pii/detokenize":
		return OpDetokenize, true
	case method == http.MethodDelete && isRevokePath(escapedPath):
		return OpRevoke, true
	case method == http.MethodPost && path == "/v1/runtime/chat":
		return OpRuntime, true
	}
	return "", false
}

// isRevokePath reports whether path is exactly /v1/pii/scopes/{one non-empty
// segment}. It never inspects or returns the segment value.
func isRevokePath(path string) bool {
	const prefix = "/v1/pii/scopes/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := path[len(prefix):]
	return rest != "" && !strings.Contains(rest, "/")
}

// newCorrelationID returns a server-generated correlation ID in a fixed safe
// format: "req_" followed by 16 bytes hex-encoded. On a crypto/rand failure it
// falls back to a unique value derived from a local time/counter, so auditing
// never fails or leaks and no two requests share a fallback ID.
func newCorrelationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return "req_" + hex.EncodeToString(b[:])
	}
	fb := fallbackBytes()
	return "req_" + hex.EncodeToString(fb[:])
}

// fallbackBytes returns 16 bytes derived from the local monotonic clock and a
// per-process counter. It carries no client data and is unique across requests
// within a process.
func fallbackBytes() [16]byte {
	var b [16]byte
	now := time.Now().UnixNano()
	b[0] = byte(now >> 56)
	b[1] = byte(now >> 48)
	b[2] = byte(now >> 40)
	b[3] = byte(now >> 32)
	b[4] = byte(now >> 24)
	b[5] = byte(now >> 16)
	b[6] = byte(now >> 8)
	b[7] = byte(now)
	c := fallbackCounter.Add(1)
	b[8] = byte(c >> 56)
	b[9] = byte(c >> 48)
	b[10] = byte(c >> 40)
	b[11] = byte(c >> 32)
	b[12] = byte(c >> 24)
	b[13] = byte(c >> 16)
	b[14] = byte(c >> 8)
	b[15] = byte(c)
	return b
}
