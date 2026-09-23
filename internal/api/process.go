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
	"time"

	"github.com/klrushka/llm-proxy/internal/audit"
	"github.com/klrushka/llm-proxy/internal/process"
)

// ProcessFunc performs the /process benchmark operation.
type ProcessFunc func(ctx context.Context, req process.Request) (process.Response, error)

// WithProcess wires the POST /process operation into the router. A nil
// ProcessFunc leaves the route registered but failing closed with 503.
func WithProcess(fn ProcessFunc) Option {
	return func(o *options) { o.process = fn }
}

// WithProcessAudit wires an audit.Logger into the POST /process operation.
// Each request emits one safe structured audit event carrying only
// operation-level metadata (operation, result, duration) and no plaintext,
// request body, Authorization, dependency error detail, token, ciphertext,
// key, CVV or PIN. It is the audit/logging seam for the process path; a nil
// logger leaves the operation un-audited.
func WithProcessAudit(logger *audit.Logger) Option {
	return func(o *options) { o.processAudit = logger }
}

// registerProcessRoute registers the POST /process route on mux. When logger is
// non-nil the operation is wrapped to emit one safe structured audit event per
// request.
func registerProcessRoute(mux *http.ServeMux, fn ProcessFunc, logger *audit.Logger) {
	if logger != nil && fn != nil {
		fn = auditProcess(logger, fn)
	}
	mux.HandleFunc("POST /process", handleProcess(fn))
}

// auditProcess wraps fn to emit one safe structured audit event per request.
// The event carries only the operation name, the outcome and the duration; it
// never carries the payload, the result, a token value, a dependency error
// message or any other plaintext. The process adapter does not track entity
// metadata, so the event carries no entities.
func auditProcess(logger *audit.Logger, fn ProcessFunc) ProcessFunc {
	return func(ctx context.Context, req process.Request) (process.Response, error) {
		start := time.Now()
		resp, err := fn(ctx, req)
		result := audit.ResultSuccess
		if err != nil {
			result = audit.ResultError
		}
		_ = logger.Log(audit.Event{
			Operation: audit.OpProcess,
			Duration:  time.Since(start),
			Result:    result,
		})
		return resp, err
	}
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
			if errors.Is(err, process.ErrReviewRequired) {
				writeReviewRequired(w)
				return
			}
			if errors.Is(err, process.ErrVaultUnavailable) {
				// Safe fail-closed: generic body, no result/payload/internal
				// detail, and no Retry-After (distinct from overload).
				writeJSONError(w, http.StatusServiceUnavailable, "vault unavailable")
				return
			}
			if errors.Is(err, process.ErrModelUnavailable) {
				// Safe fail-closed: generic body, no result/payload/internal
				// detail, and no Retry-After (distinct from overload).
				writeJSONError(w, http.StatusServiceUnavailable, "model worker unavailable")
				return
			}
			writeJSONError(w, http.StatusInternalServerError, "process failed")
			return
		}
		writeJSONBody(w, http.StatusOK, resp)
	}
}
