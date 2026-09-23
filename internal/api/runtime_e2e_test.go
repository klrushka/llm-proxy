package api

import (
	"context"
	"encoding/json"
	"net/http"
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
	v, err := vault.NewMemory(time.Hour)
	if err != nil {
		t.Fatalf("vault.NewMemory() error = %v", err)
	}
	g, err := tokenization.New()
	if err != nil {
		t.Fatalf("tokenization.New() error = %v", err)
	}
	p := policy.NewPolicy(policy.DefaultConsumerID, []string{
		string(detection.TypeEmail),
		string(detection.TypePhone),
	})
	pipe := NewPipeline(noModel, p, g, v)
	coord := pipe.RuntimeCoordinator(llm.call)
	return NewRouter(nil, nil, WithRuntime(RuntimeFuncFromCoordinator(coord)))
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
	p := policy.NewPolicy(policy.DefaultConsumerID, []string{
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
	mux := NewRouter(nil, nil,
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
