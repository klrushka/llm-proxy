package api

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/policy"
	"github.com/klrushka/llm-proxy/internal/process"
	"github.com/klrushka/llm-proxy/internal/tokenization"
	"github.com/klrushka/llm-proxy/internal/vault"
)

// fakeLLM is a deterministic fake at the LLM boundary. It captures every
// protected input it receives and returns a token-preserving modified response
// produced by its transform function. It never inspects or alters tokens.
type fakeLLM struct {
	mu        sync.Mutex
	inputs    []string
	transform func(string) string
}

// newFakeLLM returns a fakeLLM that rewrites only ordinary surrounding text
// while leaving every opaque token byte-for-byte untouched, mirroring a real
// external LLM that must not see or alter the protected values.
func newFakeLLM() *fakeLLM {
	return &fakeLLM{
		transform: func(protected string) string {
			return strings.ReplaceAll(protected, "Клиент", "Уважаемый клиент")
		},
	}
}

// call records the protected input and returns the transformed response.
func (f *fakeLLM) call(_ context.Context, protected string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, protected)
	return f.transform(protected), nil
}

// calls returns a copy of the captured inputs.
func (f *fakeLLM) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.inputs...)
}

// newRuntimeMux builds the real router wired to the real pipeline (rules,
// contextual, merge, ownership, tokenization and the in-memory vault) plus the
// injected fake LLM boundary on the runtime route. Only the model worker is
// faked (noModel). It returns the mux and the fake LLM so the test can assert
// what the LLM received.
func newRuntimeMux(t *testing.T, llm *fakeLLM) *http.ServeMux {
	t.Helper()
	return newRuntimeMuxWithPolicy(t, llm, []string{
		string(detection.TypeEmail),
		string(detection.TypePhone),
	})
}

// newRuntimeMuxWithPolicy is newRuntimeMux with an explicit processing policy
// allowing exactly the given canonical types.
func newRuntimeMuxWithPolicy(t *testing.T, llm *fakeLLM, allowed []string) *http.ServeMux {
	t.Helper()
	v, err := vault.NewMemory(time.Hour)
	if err != nil {
		t.Fatalf("vault.NewMemory() error = %v", err)
	}
	g, err := tokenization.New()
	if err != nil {
		t.Fatalf("tokenization.New() error = %v", err)
	}
	p := policy.NewPolicy(allowed)
	pipe := NewPipeline(noModel, p, g, v)
	coord := pipe.RuntimeCoordinator(llm.call)
	return NewRouter(nil, WithRuntime(RuntimeFuncFromCoordinator(coord)))
}

// TestRuntimeE2EMaskLLMDemask is the automated E2E for task 12.7: the full
// product flow request -> mask/tokenize -> LLM -> demask/detokenize -> user
// response through the real pipeline and the real HTTP route, with only the
// LLM boundary faked. It proves the LLM receives only opaque protected tokens
// and none of the synthetic original PII, and that the final HTTP response
// contains the restored values embedded in the LLM's modified text.
func TestRuntimeE2EMaskLLMDemask(t *testing.T) {
	// The fake LLM rewrites only ordinary surrounding text while leaving every
	// opaque token byte-for-byte untouched, mirroring a real external LLM that
	// must not see or alter the protected values.
	llm := newFakeLLM()
	mux := newRuntimeMux(t, llm)

	const scope = "runtime-e2e-scope"
	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/runtime/chat",
		`{"text":"`+syntheticText+`","scope_id":"`+scope+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp RuntimeResponse
	decodeJSONResponse(t, rec, &resp)
	if resp.Result == "" {
		t.Fatal("result is empty, want restored user response")
	}

	// The LLM must have been invoked exactly once.
	calls := llm.calls()
	if len(calls) != 1 {
		t.Fatalf("llm calls = %d, want 1", len(calls))
	}
	llmInput := calls[0]

	// The LLM input must contain opaque tokens and none of the synthetic
	// original PII values.
	if !strings.Contains(llmInput, "<EMAIL_") || !strings.Contains(llmInput, "<PHONE_") {
		t.Errorf("llm input = %q, want EMAIL and PHONE tokens", llmInput)
	}
	if strings.Contains(llmInput, "ivanov@example.com") || strings.Contains(llmInput, "+7 900 123-45-67") {
		t.Errorf("llm input leaks plaintext PII: %q", llmInput)
	}

	// The final HTTP response must contain the restored personal values
	// embedded in the LLM's modified surrounding text.
	if !strings.Contains(resp.Result, "ivanov@example.com") {
		t.Errorf("result = %q, want email value restored", resp.Result)
	}
	if !strings.Contains(resp.Result, "+7 900 123-45-67") {
		t.Errorf("result = %q, want phone value restored", resp.Result)
	}
	if !strings.Contains(resp.Result, "Уважаемый клиент") {
		t.Errorf("result = %q, want LLM modified surrounding text preserved", resp.Result)
	}
	if strings.Contains(resp.Result, "<EMAIL_") || strings.Contains(resp.Result, "<PHONE_") {
		t.Errorf("result = %q, want no leftover tokens", resp.Result)
	}
}

// TestRuntimeE2EProcessNeverCallsLLM proves that POST /process remains a
// separate mask/restore loop and never invokes the injected LLM, even when the
// runtime route and the process route share the same router and the same fake
// LLM boundary.
func TestRuntimeE2EProcessNeverCallsLLM(t *testing.T) {
	llm := newFakeLLM()
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
	coord := pipe.RuntimeCoordinator(llm.call)

	// Wire both the runtime route and the real /process operation on the same
	// router. The /process masker is the real pipeline tokenize path.
	op := process.NewOperation(process.NewStore(), func(ctx context.Context, payload string) (string, error) {
		res, err := pipe.tokenize(ctx, TokenizeRequest{Text: payload, ScopeID: "process-scope"})
		if err != nil {
			return "", err
		}
		return res.TokenizedText, nil
	})
	mux := NewRouter(nil,
		WithRuntime(RuntimeFuncFromCoordinator(coord)),
		WithProcess(op.Handle),
	)

	rec := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"`+syntheticText+`","payload_id":"id-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("process status = %d, want %d; body = %q", rec.Code, http.StatusOK, rec.Body.String())
	}
	var procResp process.Response
	decodeJSONResponse(t, rec, &procResp)
	if procResp.Result == "" {
		t.Fatal("process result is empty")
	}

	// The LLM must never have been invoked by the /process call.
	if got := len(llm.calls()); got != 0 {
		t.Errorf("llm calls = %d, want 0 (POST /process must not invoke the LLM)", got)
	}
}

// TestRuntimeE2EHTTPStatus proves the runtime route returns 200 through the
// real router and that the response carries exactly the result field.
func TestRuntimeE2EHTTPStatus(t *testing.T) {
	llm := newFakeLLM()
	mux := newRuntimeMux(t, llm)

	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/runtime/chat",
		`{"text":"`+syntheticText+`","scope_id":"runtime-e2e-http"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusOK, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	if len(raw) != 1 {
		t.Errorf("response has %d fields, want exactly 1: %v", len(raw), raw)
	}
}

// explicitHighRiskText is a synthetic rules-only fixture with no common word
// "клиент" and no name co-occurrence. It carries one value per explicit
// high-risk canonical type whose detection is structural, validator-backed or
// marker-based. All values are synthetic and reused from existing tests.
const explicitHighRiskText = "email ivanov@example.com, телефон +7 900 123-45-67, " +
	"паспорт 00 00 000000, гражданство: Российская Федерация, кем выдан: ОВД района, " +
	"код подразделения 000-000, дата выдачи 03.04.2020, " +
	"водительское удостоверение 7777 123456, ИНН физлица 123456789047, " +
	"Карта 4111111111111111, CVV: 123, PIN: 4567, Cardholder: Ivan Ivanov"

// explicitHighRiskValues are the exact synthetic plaintext values that must be
// masked before the LLM and restored exactly after the response.
var explicitHighRiskValues = []string{
	"ivanov@example.com",
	"+7 900 123-45-67",
	"00 00 000000",
	"Российская Федерация",
	"ОВД района",
	"000-000",
	"03.04.2020",
	"7777 123456",
	"123456789047",
	"4111111111111111",
	"123",
	"4567",
	"Ivan Ivanov",
}

// explicitHighRiskTokenPrefixes are the opaque token prefixes that must appear
// in the LLM input, one per explicit high-risk type.
var explicitHighRiskTokenPrefixes = []string{
	"<EMAIL_", "<PHONE_", "<PASSPORT_NUMBER_", "<CITIZENSHIP_",
	"<PASSPORT_ISSUER_", "<PASSPORT_DIVISION_CODE_", "<PASSPORT_ISSUE_DATE_",
	"<DRIVER_LICENSE_NUMBER_", "<INN_PERSON_", "<BANK_CARD_NUMBER_",
	"<CARD_CVV_", "<CARD_PIN_", "<CARDHOLDER_NAME_",
}

// canonicalTokenRe matches an opaque token of the form <PII_TYPE_32hex> emitted
// by the tokenization layer. It is used to strip every canonical token from the
// LLM input so plaintext-leak checks search only the remaining ordinary text and
// never a random hex suffix that could coincidentally contain a short value.
var canonicalTokenRe = regexp.MustCompile(`<[A-Z_]+_[0-9a-f]{32}>`)

// stripCanonicalTokens returns a copy of s with every canonical opaque token
// removed, leaving only the ordinary surrounding text.
func stripCanonicalTokens(s string) string {
	return canonicalTokenRe.ReplaceAllString(s, "")
}

// TestRuntimeE2EExplicitHighRiskMaskLLMDemask is the rules-only runtime E2E for
// the explicit high-risk security package. The synthetic text contains no
// common word "клиент" and no name co-occurrence, so every masked value is
// personal purely because its canonical type is explicit high-risk. It proves
// that each value is replaced by an opaque token before the fake LLM, that none
// of the original values reach the LLM, and that after the response every value
// is restored exactly.
func TestRuntimeE2EExplicitHighRiskMaskLLMDemask(t *testing.T) {
	llm := newFakeLLM()
	allowed := []string{
		string(detection.TypeEmail), string(detection.TypePhone),
		string(detection.TypePassportNumber), string(detection.TypePassportDivisionCode),
		string(detection.TypePassportIssueDate), string(detection.TypePassportIssuer),
		string(detection.TypeDriverLicenseNumber), string(detection.TypeINNPerson),
		string(detection.TypeBankCardNumber), string(detection.TypeCardCVV),
		string(detection.TypeCardPIN), string(detection.TypeCardholderName),
		string(detection.TypeCitizenship),
	}
	mux := newRuntimeMuxWithPolicy(t, llm, allowed)

	const scope = "runtime-e2e-explicit-high-risk"
	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/runtime/chat",
		`{"text":"`+explicitHighRiskText+`","scope_id":"`+scope+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp RuntimeResponse
	decodeJSONResponse(t, rec, &resp)
	if resp.Result == "" {
		t.Fatal("result is empty, want restored user response")
	}

	// The LLM must have been invoked exactly once.
	calls := llm.calls()
	if len(calls) != 1 {
		t.Fatalf("llm calls = %d, want 1", len(calls))
	}
	llmInput := calls[0]

	// The LLM input must contain one opaque token per explicit high-risk type.
	for _, prefix := range explicitHighRiskTokenPrefixes {
		if !strings.Contains(llmInput, prefix) {
			t.Errorf("llm input = %q, want token prefix %q", llmInput, prefix)
		}
	}

	// None of the synthetic original values may remain in the ordinary text
	// passed to the LLM. Strip every canonical opaque token first so a short
	// value (e.g. "123") is never matched inside a random 32-hex token suffix.
	llmPlaintext := stripCanonicalTokens(llmInput)
	for _, v := range explicitHighRiskValues {
		if strings.Contains(llmPlaintext, v) {
			t.Errorf("llm input leaks plaintext value %q: %q", v, llmInput)
		}
	}

	// The final HTTP response must contain every restored value and no leftover
	// opaque tokens.
	for _, v := range explicitHighRiskValues {
		if !strings.Contains(resp.Result, v) {
			t.Errorf("result = %q, want value %q restored", resp.Result, v)
		}
	}
	for _, prefix := range explicitHighRiskTokenPrefixes {
		if strings.Contains(resp.Result, prefix) {
			t.Errorf("result = %q, want no leftover token prefix %q", resp.Result, prefix)
		}
	}
}

// bankContextText is a synthetic rules-only fixture where an explicit high-risk
// email and phone appear next to the organization word "банк". The email and
// phone must stay personal and be masked before the LLM.
const bankContextText = "Банк просит клиента указать email ivanov@example.com и телефон +7 900 123-45-67"

// TestRuntimeE2EBankContextMasksHighRisk proves that explicit high-risk values
// are masked even when a neighboring organization word such as "банк" appears
// in the local context. Both the email and the phone are replaced by opaque
// tokens before the fake LLM, none of the original values reach the LLM, and
// both are restored exactly after the response.
func TestRuntimeE2EBankContextMasksHighRisk(t *testing.T) {
	llm := newFakeLLM()
	allowed := []string{
		string(detection.TypeEmail),
		string(detection.TypePhone),
	}
	mux := newRuntimeMuxWithPolicy(t, llm, allowed)

	const scope = "runtime-e2e-bank-context"
	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/runtime/chat",
		`{"text":"`+bankContextText+`","scope_id":"`+scope+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp RuntimeResponse
	decodeJSONResponse(t, rec, &resp)
	if resp.Result == "" {
		t.Fatal("result is empty, want restored user response")
	}

	// The LLM must have been invoked exactly once.
	calls := llm.calls()
	if len(calls) != 1 {
		t.Fatalf("llm calls = %d, want 1", len(calls))
	}
	llmInput := calls[0]

	// The LLM input must contain EMAIL and PHONE tokens and none of the
	// original values in the ordinary text.
	if !strings.Contains(llmInput, "<EMAIL_") || !strings.Contains(llmInput, "<PHONE_") {
		t.Errorf("llm input = %q, want EMAIL and PHONE tokens", llmInput)
	}
	llmPlaintext := stripCanonicalTokens(llmInput)
	for _, v := range []string{"ivanov@example.com", "+7 900 123-45-67"} {
		if strings.Contains(llmPlaintext, v) {
			t.Errorf("llm input leaks plaintext value %q: %q", v, llmInput)
		}
	}

	// The final HTTP response must contain both restored values and no leftover
	// opaque tokens.
	for _, v := range []string{"ivanov@example.com", "+7 900 123-45-67"} {
		if !strings.Contains(resp.Result, v) {
			t.Errorf("result = %q, want value %q restored", resp.Result, v)
		}
	}
	if strings.Contains(resp.Result, "<EMAIL_") || strings.Contains(resp.Result, "<PHONE_") {
		t.Errorf("result = %q, want no leftover tokens", resp.Result)
	}
}
