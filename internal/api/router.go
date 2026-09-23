// Package api provides the base HTTP routing for the service health
// endpoints. It uses Go 1.23 ServeMux method patterns and returns a
// *http.ServeMux so later tasks can register /v1 and /process routes on the
// same mux without rewriting it.
package api

import (
	"encoding/json"
	"net/http"

	"github.com/klrushka/llm-proxy/internal/audit"
)

// ReadyFunc reports whether the service is ready to serve traffic. A nil
// error means ready; any error means not ready.
type ReadyFunc func() error

// Option configures the router.
type Option func(*options)

type options struct {
	pii          PIIHandlers
	process      ProcessFunc
	runtime      RuntimeFunc
	processAudit *audit.Logger
}

// WithPIIHandlers wires the extended /v1/pii/* operations into the router.
// A zero PIIHandlers value leaves the routes registered but failing closed
// with 503.
func WithPIIHandlers(h PIIHandlers) Option {
	return func(o *options) { o.pii = h }
}

// NewRouter builds the base router. ready is the injected readiness probe; a
// nil ready probe fails closed as not ready. Options register the extended
// /v1/pii/* routes. GET /metrics is deliberately not served here: metrics live
// on the separate internal metrics listener.
func NewRouter(ready ReadyFunc, opts ...Option) *http.ServeMux {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", handleLive)
	mux.HandleFunc("GET /health/ready", handleReady(ready))
	registerPIIRoutes(mux, o.pii)
	registerProcessRoute(mux, o.process, o.processAudit)
	registerRuntimeRoute(mux, o.runtime)
	return mux
}

func handleLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleReady(ready ReadyFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if ready == nil || ready() != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}

func writeJSON(w http.ResponseWriter, status int, body map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
