package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/policy"
	"github.com/klrushka/llm-proxy/internal/runtime"
	"github.com/klrushka/llm-proxy/internal/tokenization"
	"github.com/klrushka/llm-proxy/internal/vault"
)

// failingVault is a Vault whose Save always fails, simulating vault
// persistence unavailability. Resolve and RevokeScope are unused by the
// protection stage.
type failingVault struct{}

func (failingVault) Save(context.Context, string, string, string) error {
	return errors.New("sensitive vault detail")
}
func (failingVault) Resolve(context.Context, string, string) (string, error) {
	return "", vault.ErrNotFound
}
func (failingVault) RevokeScope(context.Context, string) error { return nil }

// TestRuntimeHTTPFailsClosedOnProtectError proves that a protection failure
// (tokenization validation or vault persistence) returns a fixed safe 500 body
// with no result, no original text, no token and no upstream detail.
func TestRuntimeHTTPFailsClosedOnProtectError(t *testing.T) {
	const text = "synthetic REQ_BODY_MARKER_11111 CVV_MARKER_66666"
	coord := runtime.New(
		func(_ context.Context, _, _ string) (string, error) {
			return "", errors.New("sensitive tokenization detail")
		},
		func(_ context.Context, _ string) (string, error) { return "", nil },
		func(_ context.Context, _, _ string) (string, error) { return "", nil },
	)
	mux := NewRouter(nil, WithRuntime(RuntimeFuncFromCoordinator(coord)))

	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/runtime/chat",
		`{"text":"`+text+`","scope_id":"scope-1"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	assertSafeRuntimeErrorBody(t, rec, text)
}

// TestRuntimeHTTPFailsClosedOnLLMError proves that an LLM failure returns a
// fixed safe 500 body with no result, no original text, no token and no
// upstream detail.
func TestRuntimeHTTPFailsClosedOnLLMError(t *testing.T) {
	const text = "synthetic REQ_BODY_MARKER_11111 PIN_MARKER_77777"
	coord := runtime.New(
		func(_ context.Context, _, _ string) (string, error) {
			return "<EMAIL_00000000000000000000000000000000>", nil
		},
		func(_ context.Context, _ string) (string, error) {
			return "", errors.New("sensitive llm detail")
		},
		func(_ context.Context, _, _ string) (string, error) { return "", nil },
	)
	mux := NewRouter(nil, WithRuntime(RuntimeFuncFromCoordinator(coord)))

	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/runtime/chat",
		`{"text":"`+text+`","scope_id":"scope-1"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	assertSafeRuntimeErrorBody(t, rec, text)
}

// TestRuntimeHTTPFailsClosedOnRestoreError proves that a detokenization
// failure returns a fixed safe 500 body with no result, no original text, no
// token and no upstream detail.
func TestRuntimeHTTPFailsClosedOnRestoreError(t *testing.T) {
	const text = "synthetic REQ_BODY_MARKER_11111"
	coord := runtime.New(
		func(_ context.Context, _, _ string) (string, error) {
			return "<EMAIL_00000000000000000000000000000000>", nil
		},
		func(_ context.Context, _ string) (string, error) {
			return "modified <EMAIL_00000000000000000000000000000000>", nil
		},
		func(_ context.Context, _, _ string) (string, error) {
			return "", errors.New("sensitive detokenization detail")
		},
	)
	mux := NewRouter(nil, WithRuntime(RuntimeFuncFromCoordinator(coord)))

	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/runtime/chat",
		`{"text":"`+text+`","scope_id":"scope-1"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	assertSafeRuntimeErrorBody(t, rec, text)
}

// TestRuntimeHTTPVaultSaveFailsClosedEndToEnd proves that a real pipeline
// vault persistence failure fails closed through the HTTP route: the response
// is a fixed safe 500 with no result and no plaintext fallback.
func TestRuntimeHTTPVaultSaveFailsClosedEndToEnd(t *testing.T) {
	g, err := tokenization.New()
	if err != nil {
		t.Fatalf("tokenization.New() error = %v", err)
	}
	p := policy.NewPolicy([]string{
		string(detection.TypeEmail),
		string(detection.TypePhone),
	})
	pipe := NewPipeline(noModel, p, g, failingVault{})
	coord := pipe.RuntimeCoordinator(func(_ context.Context, _ string) (string, error) {
		return "", errors.New("llm must not be reached")
	})
	mux := NewRouter(nil, WithRuntime(RuntimeFuncFromCoordinator(coord)))

	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/runtime/chat",
		`{"text":"`+syntheticText+`","scope_id":"scope-1"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	assertSafeRuntimeErrorBody(t, rec, syntheticText)
}

// TestRuntimeHTTPInvalidContractRejected proves that missing or malformed
// contract input returns a safe 400 without invoking the coordinator.
func TestRuntimeHTTPInvalidContractRejected(t *testing.T) {
	called := false
	coord := runtime.New(
		func(_ context.Context, _, _ string) (string, error) {
			called = true
			return "", nil
		},
		func(_ context.Context, _ string) (string, error) { return "", nil },
		func(_ context.Context, _, _ string) (string, error) { return "", nil },
	)
	mux := NewRouter(nil, WithRuntime(RuntimeFuncFromCoordinator(coord)))

	cases := []struct {
		name string
		body string
	}{
		{"missing text", `{"scope_id":"s1"}`},
		{"missing scope_id", `{"text":"x"}`},
		{"empty text", `{"text":"  ","scope_id":"s1"}`},
		{"empty scope_id", `{"text":"x","scope_id":"  "}`},
		{"wrong text type", `{"text":123,"scope_id":"s1"}`},
		{"malformed JSON", `{not json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSONRequest(t, mux, http.MethodPost, "/v1/runtime/chat", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			var errResp errorResponse
			decodeJSONResponse(t, rec, &errResp)
			if errResp.Error == "" {
				t.Errorf("error body missing generic message: %q", rec.Body.String())
			}
		})
	}
	if called {
		t.Error("coordinator invoked for invalid input")
	}
}

// TestRuntimeNilOperationFailsClosed proves that a nil runtime operation
// leaves the route registered but failing closed with 503.
func TestRuntimeNilOperationFailsClosed(t *testing.T) {
	mux := NewRouter(nil)
	rec := doJSONRequest(t, mux, http.MethodPost, "/v1/runtime/chat",
		`{"text":"x","scope_id":"s1"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

// TestRuntimeWrongMethodReturns405 proves the runtime route rejects non-POST
// methods.
func TestRuntimeWrongMethodReturns405(t *testing.T) {
	mux := NewRouter(nil)
	rec := doJSONRequest(t, mux, http.MethodGet, "/v1/runtime/chat", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/runtime/chat status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

// assertSafeRuntimeErrorBody asserts that a runtime error response is a fixed
// generic body that never leaks the request text, a token, a result field or
// any forbidden marker.
func assertSafeRuntimeErrorBody(t *testing.T, rec *httptest.ResponseRecorder, text string) {
	t.Helper()
	var errResp errorResponse
	decodeJSONResponse(t, rec, &errResp)
	if errResp.Error == "" {
		t.Errorf("error body missing generic message: %q", rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "result") {
		t.Errorf("body leaks result field: %q", body)
	}
	if strings.Contains(body, text) {
		t.Errorf("body leaks request text: %q", body)
	}
	if strings.Contains(body, "<EMAIL_") || strings.Contains(body, "<PHONE_") {
		t.Errorf("body leaks a token: %q", body)
	}
	for _, m := range forbiddenMarkers {
		if strings.Contains(body, m) {
			t.Errorf("body leaks forbidden marker %q: %q", m, body)
		}
	}
}
