// Package api provides the minimal explicit user-facing runtime route
// POST /v1/runtime/chat. It is the product flow: user request -> mask/tokenize
// -> configurable LLM boundary -> demask/detokenize -> user response. It is
// separate from the POST /process benchmark adapter, which remains a thin
// mask/restore loop and never invokes the LLM.
package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/klrushka/llm-proxy/internal/runtime"
)

// RuntimeRequest is the JSON body accepted by POST /v1/runtime/chat. Both
// fields are required and must be strings.
type RuntimeRequest struct {
	// Text is the user request text. It may contain personal data that is
	// tokenized before reaching the LLM.
	Text string `json:"text"`
	// ScopeID scopes the tokenization and restoration. Tokens are reversible
	// only within this scope.
	ScopeID string `json:"scope_id"`
}

// RuntimeResponse is the JSON body returned by a successful
// POST /v1/runtime/chat. It contains exactly one field, result, and no others.
type RuntimeResponse struct {
	// Result is the restored user response after the LLM output has been
	// demasked. It is empty on any fail-closed error.
	Result string `json:"result"`
}

// RuntimeFunc performs the runtime chat operation.
type RuntimeFunc func(ctx context.Context, req RuntimeRequest) (RuntimeResponse, error)

// WithRuntime wires the POST /v1/runtime/chat operation into the router. A nil
// RuntimeFunc leaves the route registered but failing closed with 503.
func WithRuntime(fn RuntimeFunc) Option {
	return func(o *options) { o.runtime = fn }
}

// registerRuntimeRoute registers the POST /v1/runtime/chat route on mux. A nil
// fn fails closed with 503.
func registerRuntimeRoute(mux *http.ServeMux, fn RuntimeFunc) {
	mux.HandleFunc("POST /v1/runtime/chat", handleRuntime(fn))
}

func handleRuntime(fn RuntimeFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fn == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "operation unavailable")
			return
		}
		var req RuntimeRequest
		if !decodeBody(w, r, &req) {
			return
		}
		if strings.TrimSpace(req.Text) == "" {
			writeJSONError(w, http.StatusBadRequest, "text is required")
			return
		}
		if strings.TrimSpace(req.ScopeID) == "" {
			writeJSONError(w, http.StatusBadRequest, "scope_id is required")
			return
		}
		resp, err := fn(r.Context(), req)
		if err != nil {
			// Fail closed: a fixed, generic body with no result, no original
			// text, no protected text, no token, no mapping and no upstream
			// error detail.
			writeJSONError(w, http.StatusInternalServerError, "runtime failed")
			return
		}
		writeJSONBody(w, http.StatusOK, resp)
	}
}

// runtimeFromCoordinator adapts a *runtime.Coordinator to a RuntimeFunc. It
// never leaks the coordinator's safe sentinel error text into the response;
// the handler writes a fixed generic body on any error.
func runtimeFromCoordinator(c *runtime.Coordinator) RuntimeFunc {
	return func(ctx context.Context, req RuntimeRequest) (RuntimeResponse, error) {
		result, err := c.Run(ctx, req.ScopeID, req.Text)
		if err != nil {
			return RuntimeResponse{}, err
		}
		return RuntimeResponse{Result: result}, nil
	}
}
