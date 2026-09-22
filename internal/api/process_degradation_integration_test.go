package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/klrushka/llm-proxy/internal/audit"
	"github.com/klrushka/llm-proxy/internal/process"
)

// newProcessDegradationMux builds the real router wired to the real
// process.Operation, process.Store and, when limit > 0, the real
// process.Limiter. Only the masking dependency is faked at the true external
// boundary. When logger is non-nil the real audit seam is wired so each
// request emits a captured structured log line.
func newProcessDegradationMux(mask process.MaskFunc, limit int, logger *audit.Logger) *http.ServeMux {
	op := process.NewOperation(process.NewStore(), mask)
	var h process.HandlerFunc = op.Handle
	if limit > 0 {
		l, err := process.NewLimiter(limit, h)
		if err != nil {
			panic(err)
		}
		h = l.Handle
	}
	opts := []Option{WithProcess(ProcessFunc(h))}
	if logger != nil {
		opts = append(opts, WithProcessAudit(logger))
	}
	return NewRouter(nil, nil, opts...)
}

// assertNoLeak fails the test if any marker appears in haystack. It is used to
// prove that failure/overload paths never expose synthetic plaintext, request
// bodies, Authorization headers, dependency error details, tokens, ciphertext,
// keys, CVV or PIN in HTTP responses or captured structured logs.
func assertNoLeak(t *testing.T, where, haystack string, markers []string) {
	t.Helper()
	for _, m := range markers {
		if strings.Contains(haystack, m) {
			t.Errorf("%s leaks marker %q:\n%s", where, m, haystack)
		}
	}
}

// TestProcessIntegrationOverloadReturns429WithRetryAfter proves that when the
// bounded concurrency limit is reached the real router returns 429 with a
// Retry-After header and no result or payload.
func TestProcessIntegrationOverloadReturns429WithRetryAfter(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	mask := func(_ context.Context, p string) (string, error) {
		close(entered)
		<-release
		return "masked:" + p, nil
	}
	mux := newProcessDegradationMux(mask, 1, nil)

	// Fill the single permit with a blocking first request.
	done := make(chan struct{})
	go func() {
		defer close(done)
		doProcessRequest(mux, `{"payload":"synthetic","payload_id":"id-1"}`)
	}()
	<-entered

	// A second request is rejected immediately as overloaded.
	rec := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"synthetic overload","payload_id":"id-2"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	if strings.Contains(rec.Body.String(), "result") {
		t.Errorf("body leaks result field: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "synthetic overload") {
		t.Errorf("body leaks payload: %q", rec.Body.String())
	}

	close(release)
	<-done
}

// TestProcessIntegrationModelUnavailableFailsClosedWithoutPermission proves
// that a model-worker failure without explicit consumer permission fails
// closed with 503, never invokes the rules-only fallback, and returns no
// result or payload.
func TestProcessIntegrationModelUnavailableFailsClosedWithoutPermission(t *testing.T) {
	var rulesCalls atomic.Int64
	primary := func(_ context.Context, _ string) (string, error) {
		return "", process.ErrModelUnavailable
	}
	rules := func(_ context.Context, p string) (string, error) {
		rulesCalls.Add(1)
		return "rules:" + p, nil
	}
	mask := process.WithRulesOnlyFallback(false, primary, rules)
	mux := newProcessDegradationMux(mask, 0, nil)

	rec := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"synthetic","payload_id":"id-1"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if got := rulesCalls.Load(); got != 0 {
		t.Errorf("rules-only calls = %d, want 0 (denied)", got)
	}
	if strings.Contains(rec.Body.String(), "result") {
		t.Errorf("body leaks result field: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "synthetic") {
		t.Errorf("body leaks payload: %q", rec.Body.String())
	}
}

// TestProcessIntegrationModelUnavailableFallsBackWithPermission proves that
// the same model-worker failure with explicit rules-only permission invokes
// the fallback exactly once and returns only its protected result.
func TestProcessIntegrationModelUnavailableFallsBackWithPermission(t *testing.T) {
	var rulesCalls atomic.Int64
	primary := func(_ context.Context, _ string) (string, error) {
		return "", process.ErrModelUnavailable
	}
	rules := func(_ context.Context, p string) (string, error) {
		rulesCalls.Add(1)
		return "rules:" + p, nil
	}
	mask := process.WithRulesOnlyFallback(true, primary, rules)
	mux := newProcessDegradationMux(mask, 0, nil)

	rec := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"synthetic","payload_id":"id-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var resp process.Response
	decodeJSONResponse(t, rec, &resp)
	if resp.Result != "rules:synthetic" {
		t.Errorf("result = %q, want %q", resp.Result, "rules:synthetic")
	}
	if got := rulesCalls.Load(); got != 1 {
		t.Errorf("rules-only calls = %d, want exactly 1", got)
	}
}

// TestProcessIntegrationVaultUnavailableFailsClosed proves that a vault
// unavailability fails closed with 503 and returns no result or payload.
func TestProcessIntegrationVaultUnavailableFailsClosed(t *testing.T) {
	mask := func(_ context.Context, _ string) (string, error) {
		return "", process.ErrVaultUnavailable
	}
	mux := newProcessDegradationMux(mask, 0, nil)

	rec := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"synthetic","payload_id":"id-1"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if strings.Contains(rec.Body.String(), "result") {
		t.Errorf("body leaks result field: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "synthetic") {
		t.Errorf("body leaks payload: %q", rec.Body.String())
	}
}

// leakMarkers are synthetic plaintext values that must never appear in HTTP
// responses or captured structured logs on failure/overload paths. They cover
// a full name, a bank card, CVV, PIN, an email, an Authorization header, a
// token, ciphertext and an encryption key.
var leakMarkers = []string{
	"ТЕСТОВ ТЕСТ ТЕСТОВИЧ",
	"1234 5678 9012 3456",
	"CVV 739",
	"PIN 8642",
	"test@example.com",
	"Bearer LEAK_AUTHZ_77777",
	"<EMAIL_LEAKTOKEN_99999>",
	"CIPHERTEXT_LEAK_88888",
	"ENCKEY_LEAK_66666",
}

const leakPayload = "Клиент ТЕСТОВ ТЕСТ ТЕСТОВИЧ, карта 1234 5678 9012 3456, CVV 739, PIN 8642, email test@example.com"

// TestProcessIntegrationFailurePathsDoNotLeakSecrets proves that every
// failure path (model unavailable denied, vault unavailable, generic masking
// failure) returns the documented status and exposes none of the synthetic
// plaintext markers in the HTTP response body or in the captured structured
// audit logs.
func TestProcessIntegrationFailurePathsDoNotLeakSecrets(t *testing.T) {
	cases := []struct {
		name string
		mask process.MaskFunc
		want int
	}{
		{"model unavailable denied", process.WithRulesOnlyFallback(false,
			func(_ context.Context, _ string) (string, error) { return "", process.ErrModelUnavailable },
			func(_ context.Context, _ string) (string, error) { return "rules", nil }), http.StatusServiceUnavailable},
		{"vault unavailable", func(_ context.Context, _ string) (string, error) {
			return "", process.ErrVaultUnavailable
		}, http.StatusServiceUnavailable},
		{"generic masking failure", func(_ context.Context, _ string) (string, error) {
			return "", process.ErrMaskingFailed
		}, http.StatusInternalServerError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			logger := audit.New(&logBuf)
			mux := newProcessDegradationMux(tc.mask, 0, logger)

			req := httptest.NewRequest(http.MethodPost, "/process",
				strings.NewReader(`{"payload":"`+leakPayload+`","payload_id":"id-1"}`))
			req.Header.Set("Authorization", "Bearer LEAK_AUTHZ_77777")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if logBuf.Len() == 0 {
				t.Error("no structured audit log captured from the process path")
			}
			assertNoLeak(t, "response body", rec.Body.String(), leakMarkers)
			assertNoLeak(t, "structured logs", logBuf.String(), leakMarkers)
		})
	}
}

// TestProcessIntegrationOverloadDoesNotLeakSecrets proves that the overload
// path returns 429 and exposes none of the synthetic plaintext markers in the
// HTTP response body or in the captured structured audit logs.
func TestProcessIntegrationOverloadDoesNotLeakSecrets(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	mask := func(_ context.Context, p string) (string, error) {
		close(entered)
		<-release
		return "masked:" + p, nil
	}
	var logBuf bytes.Buffer
	logger := audit.New(&logBuf)
	mux := newProcessDegradationMux(mask, 1, logger)

	done := make(chan struct{})
	go func() {
		defer close(done)
		doProcessRequest(mux, `{"payload":"synthetic","payload_id":"id-1"}`)
	}()
	<-entered

	req := httptest.NewRequest(http.MethodPost, "/process",
		strings.NewReader(`{"payload":"`+leakPayload+`","payload_id":"id-2"}`))
	req.Header.Set("Authorization", "Bearer LEAK_AUTHZ_77777")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if logBuf.Len() == 0 {
		t.Error("no structured audit log captured from the overload path")
	}
	assertNoLeak(t, "response body", rec.Body.String(), leakMarkers)
	assertNoLeak(t, "structured logs", logBuf.String(), leakMarkers)

	close(release)
	<-done
}
