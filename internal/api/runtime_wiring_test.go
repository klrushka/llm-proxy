package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/llmclient"
	"github.com/klrushka/llm-proxy/internal/policy"
	"github.com/klrushka/llm-proxy/internal/process"
	"github.com/klrushka/llm-proxy/internal/tokenization"
	"github.com/klrushka/llm-proxy/internal/vault"
)

// downstreamLLM is a real httptest downstream chat-completions endpoint. It
// captures the outbound request body and returns a token-preserving modified
// response, mirroring a real external LLM that must not see or alter the
// protected values.
type downstreamLLM struct {
	mu       sync.Mutex
	requests []map[string]any
	status   int
	body     string
}

// newDownstreamLLM returns a downstream that rewrites only ordinary surrounding
// text while leaving every opaque token byte-for-byte untouched.
func newDownstreamLLM() *downstreamLLM {
	return &downstreamLLM{
		status: http.StatusOK,
		body:   `{"choices":[{"message":{"content":"__REPLACE__"}}]}`,
	}
}

// handler returns the httptest handler. It captures the request body and
// returns the configured status/body, substituting the captured user message
// into the response content so tokens are preserved.
func (d *downstreamLLM) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		d.mu.Lock()
		d.requests = append(d.requests, req)
		d.mu.Unlock()

		user := ""
		if msgs, ok := req["messages"].([]any); ok && len(msgs) > 0 {
			if last, ok := msgs[len(msgs)-1].(map[string]any); ok {
				user, _ = last["content"].(string)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(d.status)
		io.WriteString(w, strings.ReplaceAll(d.body, "__REPLACE__", user))
	}
}

// calls returns a copy of the captured request bodies.
func (d *downstreamLLM) calls() []map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]map[string]any(nil), d.requests...)
}

// newWiredRuntimeMux builds the real router wired to the real pipeline plus a
// real llmclient pointed at the httptest downstream. It returns the mux and the
// downstream so the test can assert what the LLM received.
func newWiredRuntimeMux(t *testing.T, downstream *downstreamLLM) *http.ServeMux {
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

	srv := httptest.NewServer(downstream.handler())
	t.Cleanup(srv.Close)

	llm, err := llmclient.New(llmclient.Config{
		URL:     srv.URL,
		Model:   "test-model",
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("llmclient.New() error = %v", err)
	}
	coord := pipe.RuntimeCoordinator(llm.Complete)
	return NewRouter(nil, nil, WithRuntime(RuntimeFuncFromCoordinator(coord)))
}

// TestRuntimeWiringDownstreamReceivesTokensNotPlaintext proves the full
// production wiring: the real pipeline tokenizes the synthetic plaintext, the
// real llmclient sends only opaque tokens to the httptest downstream, and the
// final runtime response restores the original values.
func TestRuntimeWiringDownstreamReceivesTokensNotPlaintext(t *testing.T) {
	downstream := newDownstreamLLM()
	mux := newWiredRuntimeMux(t, downstream)

	const scope = "wiring-scope"
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

	calls := downstream.calls()
	if len(calls) != 1 {
		t.Fatalf("downstream calls = %d, want 1", len(calls))
	}
	req := calls[0]
	msgs, _ := req["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	userMsg, _ := msgs[1].(map[string]any)
	user, _ := userMsg["content"].(string)

	// The downstream must receive opaque tokens and none of the synthetic
	// plaintext PII values.
	if !strings.Contains(user, "<EMAIL_") || !strings.Contains(user, "<PHONE_") {
		t.Errorf("downstream user = %q, want EMAIL and PHONE tokens", user)
	}
	if strings.Contains(user, "ivanov@example.com") || strings.Contains(user, "+7 900 123-45-67") {
		t.Errorf("downstream user leaks plaintext PII: %q", user)
	}

	// The final runtime response must restore the original values.
	if !strings.Contains(resp.Result, "ivanov@example.com") {
		t.Errorf("result = %q, want email value restored", resp.Result)
	}
	if !strings.Contains(resp.Result, "+7 900 123-45-67") {
		t.Errorf("result = %q, want phone value restored", resp.Result)
	}
	if strings.Contains(resp.Result, "<EMAIL_") || strings.Contains(resp.Result, "<PHONE_") {
		t.Errorf("result = %q, want no leftover tokens", resp.Result)
	}
}

// TestRuntimeWiringUpstreamFailureFailsClosed proves that an upstream failure
// fails closed without leaking markers: the response is a fixed safe 500 with
// no result and no forbidden marker.
func TestRuntimeWiringUpstreamFailureFailsClosed(t *testing.T) {
	downstream := newDownstreamLLM()
	downstream.status = http.StatusInternalServerError
	downstream.body = "upstream error detail that must not leak"
	mux := newWiredRuntimeMux(t, downstream)

	const text = "synthetic REQ_BODY_MARKER_11111 CVV_MARKER_66666"
	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/runtime/chat",
		`{"text":"`+text+`","scope_id":"scope-1"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	assertSafeRuntimeErrorBody(t, rec, text)
}

// TestRuntimeWiringProcessNeverCallsDownstream proves that POST /process
// remains a separate mask/restore loop and never calls the downstream LLM, even
// when the runtime route and the process route share the same router and the
// same real llmclient.
func TestRuntimeWiringProcessNeverCallsDownstream(t *testing.T) {
	downstream := newDownstreamLLM()
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

	srv := httptest.NewServer(downstream.handler())
	defer srv.Close()
	llm, err := llmclient.New(llmclient.Config{URL: srv.URL, Model: "m", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("llmclient.New() error = %v", err)
	}
	coord := pipe.RuntimeCoordinator(llm.Complete)

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

	if got := len(downstream.calls()); got != 0 {
		t.Errorf("downstream calls = %d, want 0 (POST /process must not call the LLM)", got)
	}
}
