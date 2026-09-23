package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/policy"
	"github.com/klrushka/llm-proxy/internal/tokenization"
	"github.com/klrushka/llm-proxy/internal/vault"
)

// syntheticText is a synthetic fixture with two rule-detectable personal
// entities: a phone and an email. It contains no real personal data.
const syntheticText = "Клиент Иванов Иван, телефон +7 900 123-45-67, email ivanov@example.com"

// noModel is a deterministic fake at the model-worker boundary: it contributes
// no model candidates, so the rules provide all detection candidates. This is
// the only external boundary faked in the integration slice.
func noModel(_ context.Context, _ string) ([]detection.Candidate, error) {
	return nil, nil
}

// newIntegrationMux builds the real router wired to the real pipeline: rules,
// contextual, merge, ownership, tokenization and the in-memory vault. Only the
// model worker is faked (noModel).
func newIntegrationMux(t *testing.T) *http.ServeMux {
	t.Helper()
	v, err := vault.NewMemory(time.Hour)
	if err != nil {
		t.Fatalf("vault.NewMemory() error = %v", err)
	}
	g, err := tokenization.New()
	if err != nil {
		t.Fatalf("tokenization.New() error = %v", err)
	}
	p := policy.NewPolicy([]string{
		string(detection.TypeEmail),
		string(detection.TypePhone),
	})
	pipe := NewPipeline(noModel, p, g, v)
	return NewRouter(nil, nil, WithPIIHandlers(pipe.Handlers()))
}

func tokenizeText(t *testing.T, mux *http.ServeMux, text, scope string) TokenizeResponse {
	t.Helper()
	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/pii/tokenize",
		`{"text":"`+text+`","scope_id":"`+scope+`","ttl_seconds":3600}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("tokenize status = %d, want %d", rec.Code, http.StatusOK)
	}
	var resp TokenizeResponse
	decodeJSONResponse(t, rec, &resp)
	return resp
}

func detokenizeText(t *testing.T, mux *http.ServeMux, text, scope, mode string) DetokenizeResponse {
	t.Helper()
	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/pii/detokenize",
		`{"text":"`+text+`","scope_id":"`+scope+`","mode":"`+mode+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("detokenize status = %d, want %d", rec.Code, http.StatusOK)
	}
	var resp DetokenizeResponse
	decodeJSONResponse(t, rec, &resp)
	return resp
}

func containsAll(haystack []string, needles ...string) bool {
	for _, n := range needles {
		found := false
		for _, h := range haystack {
			if h == n {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// TestPIIIntegrationDetectHappyPath exercises POST /v1/pii/detect through the
// real pipeline and asserts the metadata contract without any plaintext value.
func TestPIIIntegrationDetectHappyPath(t *testing.T) {
	mux := newIntegrationMux(t)
	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/pii/detect",
		`{"text":"`+syntheticText+`","request_id":"r1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var resp DetectResponse
	decodeJSONResponse(t, rec, &resp)
	if !resp.HasPersonalData {
		t.Error("has_personal_data = false, want true")
	}
	if !containsAll(resp.DetectedTypes, "EMAIL", "PHONE") {
		t.Errorf("detected_types = %v, want EMAIL and PHONE", resp.DetectedTypes)
	}
	if len(resp.Entities) != 2 {
		t.Fatalf("entities = %d, want 2", len(resp.Entities))
	}
	for _, e := range resp.Entities {
		if !e.Personal {
			t.Errorf("entity %s personal = false, want true", e.Type)
		}
		if e.Type != "EMAIL" && e.Type != "PHONE" {
			t.Errorf("unexpected entity type %q", e.Type)
		}
	}

	// The response must not carry the plaintext value field.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	var entities []map[string]json.RawMessage
	if err := json.Unmarshal(raw["entities"], &entities); err != nil {
		t.Fatalf("invalid entities: %v", err)
	}
	for _, e := range entities {
		if _, ok := e["value"]; ok {
			t.Error("entity JSON contains plaintext value field")
		}
	}
}

// TestPIIIntegrationTokenizeDetokenizeRoundTrip exercises the tokenize then
// detokenize happy path through the real pipeline and vault.
func TestPIIIntegrationTokenizeDetokenizeRoundTrip(t *testing.T) {
	mux := newIntegrationMux(t)

	tok := tokenizeText(t, mux, syntheticText, "scope-1")
	if tok.ScopeID != "scope-1" {
		t.Errorf("scope_id = %q, want scope-1", tok.ScopeID)
	}
	if !strings.Contains(tok.TokenizedText, "<EMAIL_") || !strings.Contains(tok.TokenizedText, "<PHONE_") {
		t.Errorf("tokenized_text = %q, want EMAIL and PHONE tokens", tok.TokenizedText)
	}
	if strings.Contains(tok.TokenizedText, "ivanov@example.com") || strings.Contains(tok.TokenizedText, "+7 900 123-45-67") {
		t.Errorf("tokenized_text leaks plaintext: %q", tok.TokenizedText)
	}

	det := detokenizeText(t, mux, tok.TokenizedText, "scope-1", "strict")
	if det.RestoredText != syntheticText {
		t.Errorf("restored_text = %q, want %q", det.RestoredText, syntheticText)
	}
	if det.ResolvedTokenCount != 2 {
		t.Errorf("resolved_token_count = %d, want 2", det.ResolvedTokenCount)
	}
	if len(det.UnresolvedTokens) != 0 {
		t.Errorf("unresolved_tokens = %v, want none", det.UnresolvedTokens)
	}
}

// TestPIIIntegrationTokenPreservationAndScopeSeparation proves tokens are
// reused within a scope and differ across scopes, and that each scope restores
// its own original.
func TestPIIIntegrationTokenPreservationAndScopeSeparation(t *testing.T) {
	mux := newIntegrationMux(t)

	tok1 := tokenizeText(t, mux, syntheticText, "scope-1")
	tok2 := tokenizeText(t, mux, syntheticText, "scope-1")
	if tok1.TokenizedText != tok2.TokenizedText {
		t.Errorf("same scope tokens differ:\n%q\n%q", tok1.TokenizedText, tok2.TokenizedText)
	}

	tok3 := tokenizeText(t, mux, syntheticText, "scope-2")
	if tok1.TokenizedText == tok3.TokenizedText {
		t.Errorf("different scopes reused tokens: %q", tok1.TokenizedText)
	}

	if restored := detokenizeText(t, mux, tok1.TokenizedText, "scope-1", "strict"); restored.RestoredText != syntheticText {
		t.Errorf("scope-1 restored_text = %q, want %q", restored.RestoredText, syntheticText)
	}
	if restored := detokenizeText(t, mux, tok3.TokenizedText, "scope-2", "strict"); restored.RestoredText != syntheticText {
		t.Errorf("scope-2 restored_text = %q, want %q", restored.RestoredText, syntheticText)
	}
}

// TestPIIIntegrationScopeRevoke proves that after DELETE /v1/pii/scopes/{id}
// the mappings are no longer disclosed: strict detokenize fails closed and
// preserve detokenize leaves the tokens unresolved.
func TestPIIIntegrationScopeRevoke(t *testing.T) {
	mux := newIntegrationMux(t)

	tok := tokenizeText(t, mux, syntheticText, "scope-1")

	rec := doJSONRequest(t, mux, http.MethodDelete, "/v1/pii/scopes/scope-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke status = %d, want %d", rec.Code, http.StatusOK)
	}

	rec = doJSONRequest(t, mux, http.MethodPost, "/v1/pii/detokenize",
		`{"text":"`+tok.TokenizedText+`","scope_id":"scope-1","mode":"strict"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("strict detokenize after revoke status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	rec = doJSONRequest(t, mux, http.MethodPost, "/v1/pii/detokenize",
		`{"text":"`+tok.TokenizedText+`","scope_id":"scope-1","mode":"preserve"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("preserve detokenize after revoke status = %d, want %d", rec.Code, http.StatusOK)
	}
	var det DetokenizeResponse
	decodeJSONResponse(t, rec, &det)
	if det.RestoredText != tok.TokenizedText {
		t.Errorf("preserve restored_text = %q, want unchanged %q", det.RestoredText, tok.TokenizedText)
	}
	if len(det.UnresolvedTokens) != 2 {
		t.Errorf("unresolved_tokens = %v, want 2", det.UnresolvedTokens)
	}
}
