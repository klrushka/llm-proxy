// Package api provides the extended /v1/pii/* HTTP contract: detect,
// tokenize, detokenize and scope revoke. It defines the JSON request/response
// types, required-field validation and injectable operation functions so the
// business logic (detection, tokenization, vault) can be wired in later tasks
// without changing the wire contract.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// maxRequestBodyBytes bounds the JSON body accepted by every route that uses
// decodeBody. It is a fixed resource boundary independent of any per-route
// configuration. Exceeding it returns HTTP 413 with a fixed safe body and the
// downstream operation is never invoked.
const maxRequestBodyBytes = 8 << 20 // 8 MiB

// DetectFunc performs PII detection for a detect request.
type DetectFunc func(ctx context.Context, req DetectRequest) (DetectResponse, error)

// TokenizeFunc performs scoped tokenization for a tokenize request.
type TokenizeFunc func(ctx context.Context, req TokenizeRequest) (TokenizeResponse, error)

// DetokenizeFunc restores tokens for a detokenize request.
type DetokenizeFunc func(ctx context.Context, req DetokenizeRequest) (DetokenizeResponse, error)

// RevokeScopeFunc revokes all mappings of a scope.
type RevokeScopeFunc func(ctx context.Context, scopeID string) error

// PIIHandlers bundles the injectable operations backing the /v1/pii/* routes.
// A nil field leaves the corresponding route registered but failing closed
// with 503, matching the metrics handler convention.
type PIIHandlers struct {
	Detect      DetectFunc
	Tokenize    TokenizeFunc
	Detokenize  DetokenizeFunc
	RevokeScope RevokeScopeFunc
}

// Entity is the metadata returned for a detected or tokenized entity. It
// never carries the plaintext value.
type Entity struct {
	Type              string   `json:"type"`
	Start             int      `json:"start"`
	End               int      `json:"end"`
	Confidence        float64  `json:"confidence"`
	Personal          bool     `json:"personal"`
	Sources           []string `json:"sources"`
	ReasonCodes       []string `json:"reason_codes"`
	OwnerID           string   `json:"owner_id"`
	OwnerType         string   `json:"owner_type"`
	OwnershipScore    float64  `json:"ownership_score"`
	ReviewRecommended bool     `json:"review_recommended,omitempty"`
}

// DetectRequest is the JSON body accepted by POST /v1/pii/detect.
type DetectRequest struct {
	Text               string `json:"text"`
	IncludeNonPersonal bool   `json:"include_non_personal"`
	RequestID          string `json:"request_id"`
}

// DetectResponse is the JSON body returned by a successful detect.
type DetectResponse struct {
	RequestID       string   `json:"request_id"`
	HasPersonalData bool     `json:"has_personal_data"`
	DetectedTypes   []string `json:"detected_types"`
	Entities        []Entity `json:"entities"`
}

// TokenizeRequest is the JSON body accepted by POST /v1/pii/tokenize.
type TokenizeRequest struct {
	Text       string `json:"text"`
	ScopeID    string `json:"scope_id"`
	TTLSeconds int64  `json:"ttl_seconds"`
	RequestID  string `json:"request_id"`
}

// TokenizeResponse is the JSON body returned by a successful tokenize.
type TokenizeResponse struct {
	TokenizedText string   `json:"tokenized_text"`
	DetectedTypes []string `json:"detected_types"`
	Entities      []Entity `json:"entities"`
	ScopeID       string   `json:"scope_id"`
}

// DetokenizeRequest is the JSON body accepted by POST /v1/pii/detokenize.
type DetokenizeRequest struct {
	Text      string `json:"text"`
	ScopeID   string `json:"scope_id"`
	Mode      string `json:"mode"`
	RequestID string `json:"request_id"`
}

// DetokenizeResponse is the JSON body returned by a successful detokenize.
type DetokenizeResponse struct {
	RestoredText       string   `json:"restored_text"`
	ResolvedTokenCount int      `json:"resolved_token_count"`
	UnresolvedTokens   []string `json:"unresolved_tokens"`
}

// Detokenize modes.
const (
	ModeStrict   = "strict"
	ModePreserve = "preserve"
)

// errorResponse is the safe JSON error body. It never carries internal
// details, request bodies or secrets.
type errorResponse struct {
	Error string `json:"error"`
}

// registerPIIRoutes registers the extended /v1/pii/* routes on mux. A nil
// handlers field fails closed with 503 for the corresponding operation.
func registerPIIRoutes(mux *http.ServeMux, h PIIHandlers) {
	mux.HandleFunc("POST /v1/pii/detect", handleDetect(h.Detect))
	mux.HandleFunc("POST /v1/pii/tokenize", handleTokenize(h.Tokenize))
	mux.HandleFunc("POST /v1/pii/detokenize", handleDetokenize(h.Detokenize))
	mux.HandleFunc("DELETE /v1/pii/scopes/{scope_id}", handleRevokeScope(h.RevokeScope))
}

func handleDetect(op DetectFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if op == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "operation unavailable")
			return
		}
		var req DetectRequest
		if !decodeBody(w, r, &req) {
			return
		}
		if strings.TrimSpace(req.Text) == "" {
			writeJSONError(w, http.StatusBadRequest, "text is required")
			return
		}
		resp, err := op(r.Context(), req)
		if errors.Is(err, ErrModelUnavailable) {
			writeJSONError(w, http.StatusServiceUnavailable, "model worker unavailable")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "detect failed")
			return
		}
		writeJSONBody(w, http.StatusOK, resp)
	}
}

func handleTokenize(op TokenizeFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if op == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "operation unavailable")
			return
		}
		var req TokenizeRequest
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
		resp, err := op(r.Context(), req)
		if errors.Is(err, ErrModelUnavailable) {
			writeJSONError(w, http.StatusServiceUnavailable, "model worker unavailable")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "tokenize failed")
			return
		}
		writeJSONBody(w, http.StatusOK, resp)
	}
}

func handleDetokenize(op DetokenizeFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if op == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "operation unavailable")
			return
		}
		var req DetokenizeRequest
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
		if req.Mode != ModeStrict && req.Mode != ModePreserve {
			writeJSONError(w, http.StatusBadRequest, "mode must be strict or preserve")
			return
		}
		resp, err := op(r.Context(), req)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "detokenize failed")
			return
		}
		writeJSONBody(w, http.StatusOK, resp)
	}
}

func handleRevokeScope(op RevokeScopeFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if op == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "operation unavailable")
			return
		}
		scopeID := r.PathValue("scope_id")
		if strings.TrimSpace(scopeID) == "" {
			writeJSONError(w, http.StatusBadRequest, "scope_id is required")
			return
		}
		if err := op(r.Context(), scopeID); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "revoke failed")
			return
		}
		writeJSONBody(w, http.StatusOK, map[string]string{"status": "revoked"})
	}
}

// decodeBody decodes a JSON request body into dst. The body is bounded by
// maxRequestBodyBytes via http.MaxBytesReader; an oversized body returns a
// fixed safe 413 and never invokes the downstream operation. On malformed JSON
// it writes a safe 400 and returns false. After decoding dst it requires the
// body to contain no further JSON value; trailing whitespace is allowed, but
// any second JSON value or non-whitespace trailing content is rejected.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// writeJSONBody writes body as JSON with the given status.
func writeJSONBody(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeJSONError writes a safe JSON error body. The message is a fixed,
// generic string and never includes internal error details.
func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSONBody(w, status, errorResponse{Error: message})
}
