// Package api provides the POST /process benchmark adapter contract. It
// wires the process.Request/Response domain types into the router through an
// injectable operation function. Record store, state machine, idempotency,
// restore, conflict and concurrency behavior live in later tasks.
package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/klrushka/llm-proxy/internal/process"
)

// ProcessFunc performs the /process benchmark operation.
type ProcessFunc func(ctx context.Context, req process.Request) (process.Response, error)

// WithProcess wires the POST /process operation into the router. A nil
// ProcessFunc leaves the route registered but failing closed with 503.
func WithProcess(fn ProcessFunc) Option {
	return func(o *options) { o.process = fn }
}

// registerProcessRoute registers the POST /process route on mux.
func registerProcessRoute(mux *http.ServeMux, fn ProcessFunc) {
	mux.HandleFunc("POST /process", handleProcess(fn))
}

func handleProcess(fn ProcessFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fn == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "operation unavailable")
			return
		}
		var req process.Request
		if !decodeBody(w, r, &req) {
			return
		}
		if strings.TrimSpace(req.Payload) == "" {
			writeJSONError(w, http.StatusBadRequest, "payload is required")
			return
		}
		if strings.TrimSpace(req.PayloadID) == "" {
			writeJSONError(w, http.StatusBadRequest, "payload_id is required")
			return
		}
		resp, err := fn(r.Context(), req)
		if err != nil {
			if errors.Is(err, process.ErrOverloaded) {
				// Safe overload: generic body, no payload/result/internal detail.
				w.Header().Set("Retry-After", "1")
				writeJSONError(w, http.StatusTooManyRequests, "service overloaded")
				return
			}
			if errors.Is(err, process.ErrConflict) {
				// Safe conflict: generic body, no payload/result/internal detail.
				writeJSONError(w, http.StatusConflict, "payload conflicts with existing record")
				return
			}
			writeJSONError(w, http.StatusInternalServerError, "process failed")
			return
		}
		writeJSONBody(w, http.StatusOK, resp)
	}
}
