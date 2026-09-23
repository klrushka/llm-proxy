package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klrushka/llm-proxy/internal/process"
)

func TestProcessSuccessExactContract(t *testing.T) {
	h := ProcessFunc(func(_ context.Context, req process.Request) (process.Response, error) {
		return process.Response{Result: "masked:" + req.Payload}, nil
	})
	mux := NewRouter(nil, WithProcess(h))
	rec := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"Клиент ТЕСТОВ","payload_id":"p1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	if len(raw) != 1 {
		t.Errorf("response has %d fields, want exactly 1: %v", len(raw), raw)
	}
	var resp process.Response
	decodeJSONResponse(t, rec, &resp)
	if resp.Result != "masked:Клиент ТЕСТОВ" {
		t.Errorf("result = %q", resp.Result)
	}
}

func TestProcessMissingPayload(t *testing.T) {
	called := false
	h := ProcessFunc(func(_ context.Context, _ process.Request) (process.Response, error) {
		called = true
		return process.Response{}, nil
	})
	mux := NewRouter(nil, WithProcess(h))
	rec := doJSONRequest(t, mux, http.MethodPost, "/process", `{"payload_id":"p1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if called {
		t.Error("operation called for invalid request")
	}
}

func TestProcessMissingPayloadID(t *testing.T) {
	mux := NewRouter(nil, WithProcess(ProcessFunc(func(_ context.Context, _ process.Request) (process.Response, error) {
		return process.Response{}, nil
	})))
	rec := doJSONRequest(t, mux, http.MethodPost, "/process", `{"payload":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestProcessWrongTypeFields(t *testing.T) {
	mux := NewRouter(nil, WithProcess(ProcessFunc(func(_ context.Context, _ process.Request) (process.Response, error) {
		return process.Response{}, nil
	})))
	rec := doJSONRequest(t, mux, http.MethodPost, "/process", `{"payload":123,"payload_id":"p1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestProcessMalformedJSON(t *testing.T) {
	mux := NewRouter(nil, WithProcess(ProcessFunc(func(_ context.Context, _ process.Request) (process.Response, error) {
		return process.Response{}, nil
	})))
	rec := doJSONRequest(t, mux, http.MethodPost, "/process", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestProcessNilOperationFailsClosed(t *testing.T) {
	mux := NewRouter(nil)
	rec := doJSONRequest(t, mux, http.MethodPost, "/process", `{"payload":"x","payload_id":"p1"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestProcessOperationErrorIsSafe(t *testing.T) {
	h := ProcessFunc(func(_ context.Context, _ process.Request) (process.Response, error) {
		return process.Response{}, errors.New("secret internal detail")
	})
	mux := NewRouter(nil, WithProcess(h))
	rec := doJSONRequest(t, mux, http.MethodPost, "/process", `{"payload":"x","payload_id":"p1"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if strings.Contains(rec.Body.String(), "secret internal detail") {
		t.Errorf("body leaks internal error: %q", rec.Body.String())
	}
}

func TestProcessConflictReturns409GenericBody(t *testing.T) {
	h := ProcessFunc(func(_ context.Context, _ process.Request) (process.Response, error) {
		return process.Response{}, process.ErrConflict
	})
	mux := NewRouter(nil, WithProcess(h))
	rec := doJSONRequest(t, mux, http.MethodPost, "/process", `{"payload":"unrelated","payload_id":"p1"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
	var errResp errorResponse
	decodeJSONResponse(t, rec, &errResp)
	if errResp.Error == "" {
		t.Errorf("error body missing generic message: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "unrelated") {
		t.Errorf("body leaks payload: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "result") {
		t.Errorf("body leaks result field: %q", rec.Body.String())
	}
}

func TestProcessWrongMethodReturns405(t *testing.T) {
	mux := NewRouter(nil)
	rec := doJSONRequest(t, mux, http.MethodGet, "/process", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /process status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestProcessOverloadReturns429WithRetryAfter(t *testing.T) {
	const payload = "unique-synthetic-overload-payload"
	h := ProcessFunc(func(_ context.Context, _ process.Request) (process.Response, error) {
		return process.Response{}, fmt.Errorf("sensitive internal detail: %w", process.ErrOverloaded)
	})
	mux := NewRouter(nil, WithProcess(h))
	rec := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"`+payload+`","payload_id":"p1"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want %q", got, "1")
	}
	var errResp errorResponse
	decodeJSONResponse(t, rec, &errResp)
	if errResp.Error == "" {
		t.Errorf("error body missing generic message: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "result") {
		t.Errorf("body leaks result field: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), payload) {
		t.Errorf("body leaks request payload: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sensitive internal detail") {
		t.Errorf("body leaks internal detail: %q", rec.Body.String())
	}
}

func TestProcessVaultUnavailableReturns503NoResult(t *testing.T) {
	const payload = "unique-synthetic-vault-payload"
	h := ProcessFunc(func(_ context.Context, _ process.Request) (process.Response, error) {
		return process.Response{}, fmt.Errorf("sensitive vault detail: %w", process.ErrVaultUnavailable)
	})
	mux := NewRouter(nil, WithProcess(h))
	rec := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"`+payload+`","payload_id":"p1"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var errResp errorResponse
	decodeJSONResponse(t, rec, &errResp)
	if errResp.Error == "" {
		t.Errorf("error body missing generic message: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "result") {
		t.Errorf("body leaks result field: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), payload) {
		t.Errorf("body leaks request payload: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sensitive vault detail") {
		t.Errorf("body leaks internal detail: %q", rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q, want absent (distinct from overload)", got)
	}
}

func TestProcessModelUnavailableReturns503NoResult(t *testing.T) {
	const payload = "unique-synthetic-model-payload"
	h := ProcessFunc(func(_ context.Context, _ process.Request) (process.Response, error) {
		return process.Response{}, fmt.Errorf("sensitive worker detail: %w", process.ErrModelUnavailable)
	})
	mux := NewRouter(nil, WithProcess(h))
	rec := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"`+payload+`","payload_id":"p1"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var errResp errorResponse
	decodeJSONResponse(t, rec, &errResp)
	if errResp.Error == "" {
		t.Errorf("error body missing generic message: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "result") {
		t.Errorf("body leaks result field: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), payload) {
		t.Errorf("body leaks request payload: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sensitive worker detail") {
		t.Errorf("body leaks internal detail: %q", rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q, want absent (distinct from overload)", got)
	}
}

func TestProcessTrailingWhitespaceAccepted(t *testing.T) {
	called := false
	h := ProcessFunc(func(_ context.Context, req process.Request) (process.Response, error) {
		called = true
		return process.Response{Result: "masked:" + req.Payload}, nil
	})
	mux := NewRouter(nil, WithProcess(h))
	rec := doJSONRequest(t, mux, http.MethodPost, "/process",
		"{\"payload\":\"Клиент ТЕСТОВ\",\"payload_id\":\"p1\"}   \n\t")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !called {
		t.Error("operation not called for valid request with trailing whitespace")
	}
}

func TestProcessSecondJSONValueRejected(t *testing.T) {
	called := false
	h := ProcessFunc(func(_ context.Context, _ process.Request) (process.Response, error) {
		called = true
		return process.Response{}, nil
	})
	mux := NewRouter(nil, WithProcess(h))
	rec := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"x","payload_id":"p1"} {"payload":"y","payload_id":"p2"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if called {
		t.Error("operation called for body with a second JSON value")
	}
	var errResp errorResponse
	decodeJSONResponse(t, rec, &errResp)
	if errResp.Error != "invalid JSON body" {
		t.Errorf("error = %q, want %q", errResp.Error, "invalid JSON body")
	}
}

func TestProcessTrailingNonWhitespaceRejected(t *testing.T) {
	called := false
	h := ProcessFunc(func(_ context.Context, _ process.Request) (process.Response, error) {
		called = true
		return process.Response{}, nil
	})
	mux := NewRouter(nil, WithProcess(h))
	rec := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"x","payload_id":"p1"} garbage`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if called {
		t.Error("operation called for body with trailing non-whitespace content")
	}
}

func TestProcessRetryAfterAbsentOnNonOverload(t *testing.T) {
	cases := []struct {
		name string
		fn   ProcessFunc
		want int
	}{
		{"success", func(_ context.Context, _ process.Request) (process.Response, error) {
			return process.Response{Result: "masked"}, nil
		}, http.StatusOK},
		{"conflict", func(_ context.Context, _ process.Request) (process.Response, error) {
			return process.Response{}, process.ErrConflict
		}, http.StatusConflict},
		{"vault unavailable", func(_ context.Context, _ process.Request) (process.Response, error) {
			return process.Response{}, process.ErrVaultUnavailable
		}, http.StatusServiceUnavailable},
		{"model unavailable", func(_ context.Context, _ process.Request) (process.Response, error) {
			return process.Response{}, process.ErrModelUnavailable
		}, http.StatusServiceUnavailable},
		{"generic", func(_ context.Context, _ process.Request) (process.Response, error) {
			return process.Response{}, errors.New("boom")
		}, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := NewRouter(nil, WithProcess(tc.fn))
			rec := doJSONRequest(t, mux, http.MethodPost, "/process", `{"payload":"x","payload_id":"p1"}`)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if got := rec.Header().Get("Retry-After"); got != "" {
				t.Errorf("Retry-After = %q, want absent", got)
			}
		})
	}
}

// forbiddenMarkers are unique synthetic values that must never appear in any
// HTTP error response body. They are declared in pii_test.go (same package).
// They cover request body, response body, Authorization, ciphertext, encryption
// key, CVV and PIN.

// TestProcessErrorResponseDoesNotLeakMarkers proves that a /process error
// response never reflects the request body, the Authorization header, or an
// injected dependency error that embeds forbidden markers.
func TestProcessErrorResponseDoesNotLeakMarkers(t *testing.T) {
	const authz = "Bearer AUTHZ_MARKER_33333"
	const payload = "payload REQ_BODY_MARKER_11111 CVV_MARKER_66666 PIN_MARKER_77777"
	depErr := errors.New(strings.Join(forbiddenMarkers, " "))
	h := ProcessFunc(func(_ context.Context, _ process.Request) (process.Response, error) {
		return process.Response{}, depErr
	})
	mux := NewRouter(nil, WithProcess(h))

	req := httptest.NewRequest(http.MethodPost, "/process", strings.NewReader(`{"payload":"`+payload+`","payload_id":"p1"}`))
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
	if strings.Contains(body, payload) {
		t.Errorf("response leaks request body: %q", body)
	}
	if strings.Contains(body, authz) {
		t.Errorf("response leaks Authorization header: %q", body)
	}
}
