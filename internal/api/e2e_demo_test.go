package api

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// tokenPattern matches the opaque token format issued by the tokenization
// layer: <PII_TYPE_32-lowercase-hex>. It is used to prove the outbound text
// carries only opaque tokens and to preserve them byte-for-byte through the
// simulated external processor.
var tokenPattern = regexp.MustCompile(`<[A-Z_]+_[0-9a-f]{32}>`)

// externalProcessor simulates the true external boundary: a deterministic
// processor that rewrites only ordinary surrounding text while leaving every
// opaque token byte-for-byte untouched. It never inspects or alters tokens.
func externalProcessor(protected string) string {
	return strings.ReplaceAll(protected, "Клиент", "Уважаемый клиент")
}

// TestE2EDemoMaskExternalProcessRestore is the automated E2E demo for task
// 12.4: mask -> external processing preserving tokens -> restore. It drives the
// real in-process HTTP router and the real /v1/pii/tokenize and
// /v1/pii/detokenize endpoints, with only the model worker faked (noModel).
//
// The external processor is a tiny deterministic test function because it is
// the true external boundary; the service itself never calls it.
func TestE2EDemoMaskExternalProcessRestore(t *testing.T) {
	mux := newIntegrationMux(t)
	const scope = "e2e-demo-scope"

	// 1. Mask: tokenize the synthetic request through the real endpoint.
	tok := tokenizeText(t, mux, syntheticText, scope)

	// The outbound protected text must contain opaque tokens and no original
	// PII values.
	if !strings.Contains(tok.TokenizedText, "<EMAIL_") || !strings.Contains(tok.TokenizedText, "<PHONE_") {
		t.Fatalf("tokenized_text = %q, want EMAIL and PHONE tokens", tok.TokenizedText)
	}
	if strings.Contains(tok.TokenizedText, "ivanov@example.com") || strings.Contains(tok.TokenizedText, "+7 900 123-45-67") {
		t.Fatalf("tokenized_text leaks plaintext: %q", tok.TokenizedText)
	}

	// Capture the exact tokens so we can prove the external processor preserves
	// them byte-for-byte.
	tokens := tokenPattern.FindAllString(tok.TokenizedText, -1)
	if len(tokens) != 2 {
		t.Fatalf("tokenized_text = %q, want exactly 2 tokens, got %d", tok.TokenizedText, len(tokens))
	}

	// 2. External processing: rewrite only ordinary surrounding text, leaving
	// every token byte-for-byte untouched.
	processed := externalProcessor(tok.TokenizedText)
	if processed == tok.TokenizedText {
		t.Fatal("external processor made no change to surrounding text")
	}
	for _, tok := range tokens {
		if !strings.Contains(processed, tok) {
			t.Fatalf("external processor dropped token %q from %q", tok, processed)
		}
	}
	if strings.Contains(processed, "ivanov@example.com") || strings.Contains(processed, "+7 900 123-45-67") {
		t.Fatalf("external processor reintroduced plaintext: %q", processed)
	}

	// 3. Restore: detokenize the externally modified text through the real
	// endpoint and assert the personal values return in the modified text.
	det := detokenizeText(t, mux, processed, scope, "strict")
	if det.ResolvedTokenCount != 2 {
		t.Errorf("resolved_token_count = %d, want 2", det.ResolvedTokenCount)
	}
	if len(det.UnresolvedTokens) != 0 {
		t.Errorf("unresolved_tokens = %v, want none", det.UnresolvedTokens)
	}

	// The restored text must contain the original personal values embedded in
	// the externally modified surrounding text.
	if !strings.Contains(det.RestoredText, "ivanov@example.com") {
		t.Errorf("restored_text = %q, want email value restored", det.RestoredText)
	}
	if !strings.Contains(det.RestoredText, "+7 900 123-45-67") {
		t.Errorf("restored_text = %q, want phone value restored", det.RestoredText)
	}
	if !strings.Contains(det.RestoredText, "Уважаемый клиент") {
		t.Errorf("restored_text = %q, want externally modified surrounding text preserved", det.RestoredText)
	}
	if strings.Contains(det.RestoredText, "<EMAIL_") || strings.Contains(det.RestoredText, "<PHONE_") {
		t.Errorf("restored_text = %q, want no leftover tokens", det.RestoredText)
	}

	// The restored text must equal the original text with the external
	// processor's surrounding-text change applied.
	want := externalProcessor(syntheticText)
	if det.RestoredText != want {
		t.Errorf("restored_text = %q, want %q", det.RestoredText, want)
	}
}

// TestE2EDemoMaskExternalProcessRestoreHTTPStatus proves the E2E path returns
// 200 through the real router for both endpoints.
func TestE2EDemoMaskExternalProcessRestoreHTTPStatus(t *testing.T) {
	mux := newIntegrationMux(t)
	const scope = "e2e-demo-scope-http"

	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/pii/tokenize",
		`{"text":"`+syntheticText+`","scope_id":"`+scope+`","ttl_seconds":3600}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("tokenize status = %d, want %d", rec.Code, http.StatusOK)
	}
	var tok TokenizeResponse
	decodeJSONResponse(t, rec, &tok)

	processed := externalProcessor(tok.TokenizedText)

	rec = doJSONRequest(t, mux, http.MethodPost, "/v1/pii/detokenize",
		`{"text":"`+processed+`","scope_id":"`+scope+`","mode":"strict"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("detokenize status = %d, want %d", rec.Code, http.StatusOK)
	}
}
