package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klrushka/llm-proxy/internal/process"
)

func doJSONRequest(t *testing.T, mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func decodeJSONResponse(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("invalid JSON body %q: %v", rec.Body.String(), err)
	}
}

func TestDetectSuccess(t *testing.T) {
	h := PIIHandlers{
		Detect: func(_ context.Context, req DetectRequest) (DetectResponse, error) {
			return DetectResponse{
				RequestID:       req.RequestID,
				HasPersonalData: true,
				DetectedTypes:   []string{"FULL_NAME"},
				Entities: []Entity{{
					Type: "FULL_NAME", Start: 0, End: 10, Confidence: 0.99,
					Personal: true, Sources: []string{"regex"}, ReasonCodes: []string{"ok"},
					OwnerID: "owner-1", OwnerType: "PERSON", OwnershipScore: 0.95,
				}},
			}, nil
		},
	}
	mux := NewRouter(nil, WithPIIHandlers(h))
	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/pii/detect",
		`{"text":"Клиент ТЕСТОВ ТЕСТ ТЕСТОВИЧ","request_id":"r1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var resp DetectResponse
	decodeJSONResponse(t, rec, &resp)
	if resp.RequestID != "r1" {
		t.Errorf("request_id = %q, want r1", resp.RequestID)
	}
	if !resp.HasPersonalData {
		t.Error("has_personal_data = false, want true")
	}
	if len(resp.DetectedTypes) != 1 || resp.DetectedTypes[0] != "FULL_NAME" {
		t.Errorf("detected_types = %v, want [FULL_NAME]", resp.DetectedTypes)
	}
	if len(resp.Entities) != 1 || resp.Entities[0].Type != "FULL_NAME" {
		t.Errorf("entities = %+v, want one FULL_NAME", resp.Entities)
	}
	e := resp.Entities[0]
	if e.OwnerType != "PERSON" {
		t.Errorf("owner_type = %q, want PERSON", e.OwnerType)
	}
	if e.OwnershipScore != 0.95 {
		t.Errorf("ownership_score = %v, want 0.95", e.OwnershipScore)
	}
	if e.OwnerID != "owner-1" {
		t.Errorf("owner_id = %q, want owner-1", e.OwnerID)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	var entities []map[string]json.RawMessage
	if err := json.Unmarshal(raw["entities"], &entities); err != nil {
		t.Fatalf("invalid entities: %v", err)
	}
	if _, ok := entities[0]["value"]; ok {
		t.Error("entity JSON contains plaintext value field")
	}
}

func TestDetectMissingText(t *testing.T) {
	called := false
	h := PIIHandlers{
		Detect: func(_ context.Context, _ DetectRequest) (DetectResponse, error) {
			called = true
			return DetectResponse{}, nil
		},
	}
	mux := NewRouter(nil, WithPIIHandlers(h))
	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/pii/detect", `{"request_id":"r1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if called {
		t.Error("operation called for invalid request")
	}
	var errResp errorResponse
	decodeJSONResponse(t, rec, &errResp)
	if errResp.Error == "" {
		t.Error("error message is empty")
	}
}

func TestDetectMalformedJSON(t *testing.T) {
	mux := NewRouter(nil, WithPIIHandlers(PIIHandlers{
		Detect: func(_ context.Context, _ DetectRequest) (DetectResponse, error) {
			return DetectResponse{}, nil
		},
	}))
	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/pii/detect", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// TestOversizedBodyReturns413NoOperation proves that a body exceeding the 8 MiB
// limit returns a fixed safe 413, never invokes the downstream operation, and
// never reflects a synthetic canary from the oversized body.
func TestOversizedBodyReturns413NoOperation(t *testing.T) {
	const canary = "OVERSIZED_CANARY_99999"
	called := false
	h := PIIHandlers{
		Detect: func(_ context.Context, _ DetectRequest) (DetectResponse, error) {
			called = true
			return DetectResponse{}, nil
		},
	}
	mux := NewRouter(nil, WithPIIHandlers(h))

	// A valid JSON prefix followed by enough padding to exceed 8 MiB.
	body := `{"text":"` + canary + `","request_id":"r1"}` + strings.Repeat(" ", maxRequestBodyBytes)
	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/pii/detect", body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if called {
		t.Error("operation called for oversized body")
	}
	var errResp errorResponse
	decodeJSONResponse(t, rec, &errResp)
	if errResp.Error != "request body too large" {
		t.Errorf("error = %q, want %q", errResp.Error, "request body too large")
	}
	if strings.Contains(rec.Body.String(), canary) {
		t.Errorf("body leaks oversized canary: %q", rec.Body.String())
	}
}

// TestOversizedTrailingContentReturns413 proves that a valid first JSON value
// followed by oversized trailing content is rejected with 413 and the operation
// is not invoked.
func TestOversizedTrailingContentReturns413(t *testing.T) {
	called := false
	h := PIIHandlers{
		Detect: func(_ context.Context, _ DetectRequest) (DetectResponse, error) {
			called = true
			return DetectResponse{}, nil
		},
	}
	mux := NewRouter(nil, WithPIIHandlers(h))

	// A valid first JSON object followed by a huge valid JSON string that
	// pushes the total body over the 8 MiB limit.
	body := `{"text":"x","request_id":"r1"}` + `"` + strings.Repeat("a", maxRequestBodyBytes) + `"`
	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/pii/detect", body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if called {
		t.Error("operation called for oversized trailing content")
	}
	var errResp errorResponse
	decodeJSONResponse(t, rec, &errResp)
	if errResp.Error != "request body too large" {
		t.Errorf("error = %q, want %q", errResp.Error, "request body too large")
	}
}

// TestOversizedBodyAllJSONRoutes proves every JSON data route that uses
// decodeBody rejects an oversized body with 413 and does not invoke its
// operation.
func TestOversizedBodyAllJSONRoutes(t *testing.T) {
	called := false
	h := PIIHandlers{
		Detect: func(_ context.Context, _ DetectRequest) (DetectResponse, error) {
			called = true
			return DetectResponse{}, nil
		},
		Tokenize: func(_ context.Context, _ TokenizeRequest) (TokenizeResponse, error) {
			called = true
			return TokenizeResponse{}, nil
		},
		Detokenize: func(_ context.Context, _ DetokenizeRequest) (DetokenizeResponse, error) {
			called = true
			return DetokenizeResponse{}, nil
		},
	}
	mux := NewRouter(nil, WithPIIHandlers(h), WithProcess(ProcessFunc(func(_ context.Context, _ process.Request) (process.Response, error) {
		called = true
		return process.Response{}, nil
	})), WithRuntime(RuntimeFunc(func(_ context.Context, _ RuntimeRequest) (RuntimeResponse, error) {
		called = true
		return RuntimeResponse{}, nil
	})))

	cases := []struct {
		name string
		path string
		body string
	}{
		{"detect", "/v1/pii/detect", `{"text":"x"}`},
		{"tokenize", "/v1/pii/tokenize", `{"text":"x","scope_id":"s1"}`},
		{"detokenize", "/v1/pii/detokenize", `{"text":"x","scope_id":"s1","mode":"strict"}`},
		{"process", "/process", `{"payload":"x","payload_id":"p1"}`},
		{"runtime", "/v1/runtime/chat", `{"text":"x","scope_id":"s1"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called = false
			body := tc.body + strings.Repeat(" ", maxRequestBodyBytes)
			rec := doJSONRequest(t, mux, http.MethodPost, tc.path, body)
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
			}
			if called {
				t.Error("operation called for oversized body")
			}
			var errResp errorResponse
			decodeJSONResponse(t, rec, &errResp)
			if errResp.Error != "request body too large" {
				t.Errorf("error = %q, want %q", errResp.Error, "request body too large")
			}
		})
	}
}

func TestDetectOperationErrorIsSafe(t *testing.T) {
	h := PIIHandlers{
		Detect: func(_ context.Context, _ DetectRequest) (DetectResponse, error) {
			return DetectResponse{}, errors.New("secret internal detail")
		},
	}
	mux := NewRouter(nil, WithPIIHandlers(h))
	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/pii/detect", `{"text":"x"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if strings.Contains(rec.Body.String(), "secret internal detail") {
		t.Errorf("body leaks internal error: %q", rec.Body.String())
	}
}

func TestDetectNilOperationFailsClosed(t *testing.T) {
	mux := NewRouter(nil, WithPIIHandlers(PIIHandlers{}))
	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/pii/detect", `{"text":"x"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestTokenizeSuccess(t *testing.T) {
	h := PIIHandlers{
		Tokenize: func(_ context.Context, req TokenizeRequest) (TokenizeResponse, error) {
			return TokenizeResponse{
				TokenizedText: "<FULL_NAME>",
				DetectedTypes: []string{"FULL_NAME"},
				Entities: []Entity{{
					Type: "FULL_NAME", Start: 0, End: 10, Confidence: 0.99, Personal: true,
				}},
				ScopeID: req.ScopeID,
			}, nil
		},
	}
	mux := NewRouter(nil, WithPIIHandlers(h))
	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/pii/tokenize",
		`{"text":"Клиент ТЕСТОВ","scope_id":"s1","ttl_seconds":900}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var resp TokenizeResponse
	decodeJSONResponse(t, rec, &resp)
	if resp.TokenizedText != "<FULL_NAME>" {
		t.Errorf("tokenized_text = %q", resp.TokenizedText)
	}
	if resp.ScopeID != "s1" {
		t.Errorf("scope_id = %q, want s1", resp.ScopeID)
	}
}

func TestTokenizeMissingScopeID(t *testing.T) {
	mux := NewRouter(nil, WithPIIHandlers(PIIHandlers{
		Tokenize: func(_ context.Context, _ TokenizeRequest) (TokenizeResponse, error) {
			return TokenizeResponse{}, nil
		},
	}))
	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/pii/tokenize", `{"text":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestDetokenizeSuccess(t *testing.T) {
	h := PIIHandlers{
		Detokenize: func(_ context.Context, req DetokenizeRequest) (DetokenizeResponse, error) {
			return DetokenizeResponse{
				RestoredText:       "Клиент ТЕСТОВ",
				ResolvedTokenCount: 1,
				UnresolvedTokens:   []string{},
			}, nil
		},
	}
	mux := NewRouter(nil, WithPIIHandlers(h))
	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/pii/detokenize",
		`{"text":"<FULL_NAME>","scope_id":"s1","mode":"preserve"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var resp DetokenizeResponse
	decodeJSONResponse(t, rec, &resp)
	if resp.RestoredText != "Клиент ТЕСТОВ" {
		t.Errorf("restored_text = %q", resp.RestoredText)
	}
	if resp.ResolvedTokenCount != 1 {
		t.Errorf("resolved_token_count = %d, want 1", resp.ResolvedTokenCount)
	}
}

func TestDetokenizeInvalidMode(t *testing.T) {
	mux := NewRouter(nil, WithPIIHandlers(PIIHandlers{
		Detokenize: func(_ context.Context, _ DetokenizeRequest) (DetokenizeResponse, error) {
			return DetokenizeResponse{}, nil
		},
	}))
	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/pii/detokenize",
		`{"text":"x","scope_id":"s1","mode":"bogus"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestRevokeScopeSuccess(t *testing.T) {
	var got string
	h := PIIHandlers{
		RevokeScope: func(_ context.Context, scopeID string) error {
			got = scopeID
			return nil
		},
	}
	mux := NewRouter(nil, WithPIIHandlers(h))
	rec := doJSONRequest(t, mux, http.MethodDelete, "/v1/pii/scopes/s1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got != "s1" {
		t.Errorf("revoked scope = %q, want s1", got)
	}
}

func TestRevokeScopeOperationErrorIsSafe(t *testing.T) {
	h := PIIHandlers{
		RevokeScope: func(_ context.Context, _ string) error {
			return errors.New("vault secret detail")
		},
	}
	mux := NewRouter(nil, WithPIIHandlers(h))
	rec := doJSONRequest(t, mux, http.MethodDelete, "/v1/pii/scopes/s1", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if strings.Contains(rec.Body.String(), "vault secret detail") {
		t.Errorf("body leaks internal error: %q", rec.Body.String())
	}
}

func TestPIIWrongMethodReturns405(t *testing.T) {
	mux := NewRouter(nil)
	for _, path := range []string{"/v1/pii/detect", "/v1/pii/tokenize", "/v1/pii/detokenize"} {
		rec := doJSONRequest(t, mux, http.MethodGet, path, "")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s status = %d, want %d", path, rec.Code, http.StatusMethodNotAllowed)
		}
	}
}

// forbiddenMarkers are unique synthetic values that must never appear in any
// HTTP error response body. They cover request body, response body,
// Authorization, ciphertext, encryption key, CVV and PIN.
var forbiddenMarkers = []string{
	"REQ_BODY_MARKER_11111",
	"RESP_BODY_MARKER_22222",
	"AUTHZ_MARKER_33333",
	"CIPHERTEXT_MARKER_44444",
	"ENCKEY_MARKER_55555",
	"CVV_MARKER_66666",
	"PIN_MARKER_77777",
}

// TestPIIErrorResponseDoesNotLeakMarkers proves that /v1/pii/* error responses
// never reflect the request body, the Authorization header, or an injected
// dependency error that embeds forbidden markers.
func TestPIIErrorResponseDoesNotLeakMarkers(t *testing.T) {
	const authz = "Bearer AUTHZ_MARKER_33333"
	const text = "text REQ_BODY_MARKER_11111 CVV_MARKER_66666 PIN_MARKER_77777"
	depErr := errors.New(strings.Join(forbiddenMarkers, " "))

	cases := []struct {
		name string
		path string
		body string
		h    PIIHandlers
	}{
		{"detect", "/v1/pii/detect", `{"text":"` + text + `"}`,
			PIIHandlers{Detect: func(_ context.Context, _ DetectRequest) (DetectResponse, error) {
				return DetectResponse{}, depErr
			}}},
		{"tokenize", "/v1/pii/tokenize", `{"text":"` + text + `","scope_id":"s1"}`,
			PIIHandlers{Tokenize: func(_ context.Context, _ TokenizeRequest) (TokenizeResponse, error) {
				return TokenizeResponse{}, depErr
			}}},
		{"detokenize", "/v1/pii/detokenize", `{"text":"` + text + `","scope_id":"s1","mode":"strict"}`,
			PIIHandlers{Detokenize: func(_ context.Context, _ DetokenizeRequest) (DetokenizeResponse, error) {
				return DetokenizeResponse{}, depErr
			}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := NewRouter(nil, WithPIIHandlers(tc.h))
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", authz)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
			}
			body := rec.Body.String()
			for _, m := range forbiddenMarkers {
				if strings.Contains(body, m) {
					t.Errorf("response leaks forbidden marker %q: %q", m, body)
				}
			}
			if strings.Contains(body, text) {
				t.Errorf("response leaks request body: %q", body)
			}
			if strings.Contains(body, authz) {
				t.Errorf("response leaks Authorization header: %q", body)
			}
		})
	}
}
