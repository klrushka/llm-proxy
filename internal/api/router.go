// Package api provides the base HTTP routing for the service health and
// metrics endpoints. It uses Go 1.23 ServeMux method patterns and returns a
// *http.ServeMux so later tasks can register /v1 and /process routes on the
// same mux without rewriting it.
package api

import (
	"encoding/json"
	"net/http"
)

// ReadyFunc reports whether the service is ready to serve traffic. A nil
// error means ready; any error means not ready.
type ReadyFunc func() error

// Option configures the router.
type Option func(*options)

type options struct {
	pii PIIHandlers
}

// WithPIIHandlers wires the extended /v1/pii/* operations into the router.
// A zero PIIHandlers value leaves the routes registered but failing closed
// with 503.
func WithPIIHandlers(h PIIHandlers) Option {
	return func(o *options) { o.pii = h }
}

// NewRouter builds the base router. ready is the injected readiness probe;
// metrics is the injected metrics handler. A nil ready probe fails closed as
// not ready. A nil metrics handler leaves the endpoint registered and
// returns 503 instead of panicking. Options register the extended /v1/pii/*
// routes.
func NewRouter(ready ReadyFunc, metrics http.Handler, opts ...Option) *http.ServeMux {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", handleLive)
	mux.HandleFunc("GET /health/ready", handleReady(ready))
	mux.Handle("GET /metrics", handleMetrics(metrics))
	registerPIIRoutes(mux, o.pii)
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

func handleMetrics(metrics http.Handler) http.Handler {
	if metrics == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		})
	}
	return metrics
}

func writeJSON(w http.ResponseWriter, status int, body map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
