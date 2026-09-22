package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	mux := NewRouter(nil, nil, WithPIIHandlers(h))
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
	mux := NewRouter(nil, nil, WithPIIHandlers(h))
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
	mux := NewRouter(nil, nil, WithPIIHandlers(PIIHandlers{
		Detect: func(_ context.Context, _ DetectRequest) (DetectResponse, error) {
			return DetectResponse{}, nil
		},
	}))
	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/pii/detect", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestDetectOperationErrorIsSafe(t *testing.T) {
	h := PIIHandlers{
		Detect: func(_ context.Context, _ DetectRequest) (DetectResponse, error) {
			return DetectResponse{}, errors.New("secret internal detail")
		},
	}
	mux := NewRouter(nil, nil, WithPIIHandlers(h))
	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/pii/detect", `{"text":"x"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if strings.Contains(rec.Body.String(), "secret internal detail") {
		t.Errorf("body leaks internal error: %q", rec.Body.String())
	}
}

func TestDetectNilOperationFailsClosed(t *testing.T) {
	mux := NewRouter(nil, nil, WithPIIHandlers(PIIHandlers{}))
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
	mux := NewRouter(nil, nil, WithPIIHandlers(h))
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
	mux := NewRouter(nil, nil, WithPIIHandlers(PIIHandlers{
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
	mux := NewRouter(nil, nil, WithPIIHandlers(h))
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
	mux := NewRouter(nil, nil, WithPIIHandlers(PIIHandlers{
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
	mux := NewRouter(nil, nil, WithPIIHandlers(h))
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
	mux := NewRouter(nil, nil, WithPIIHandlers(h))
	rec := doJSONRequest(t, mux, http.MethodDelete, "/v1/pii/scopes/s1", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if strings.Contains(rec.Body.String(), "vault secret detail") {
		t.Errorf("body leaks internal error: %q", rec.Body.String())
	}
}

func TestPIIWrongMethodReturns405(t *testing.T) {
	mux := NewRouter(nil, nil)
	for _, path := range []string{"/v1/pii/detect", "/v1/pii/tokenize", "/v1/pii/detokenize"} {
		rec := doJSONRequest(t, mux, http.MethodGet, path, "")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s status = %d, want %d", path, rec.Code, http.StatusMethodNotAllowed)
		}
	}
}
