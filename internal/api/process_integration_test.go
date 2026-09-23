package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klrushka/llm-proxy/internal/process"
)

// newProcessIntegrationMux builds the real router wired to the real
// process.Operation and process.Store. Only the masking dependency is faked:
// mask is the deterministic MaskFunc at the true dependency boundary. The HTTP
// handler, the operation state machine, the record store and the concurrency
// coordination are all the real in-process implementations.
func newProcessIntegrationMux(mask process.MaskFunc) *http.ServeMux {
	op := process.NewOperation(process.NewStore(), mask)
	return NewRouter(nil, WithProcess(op.Handle))
}

// countingMask returns a deterministic MaskFunc that records the number of
// invocations and the payloads it was asked to mask. It never returns the
// plaintext payload as the result.
func countingMask(calls *atomic.Int64) process.MaskFunc {
	return func(_ context.Context, payload string) (string, error) {
		calls.Add(1)
		return "masked:" + payload, nil
	}
}

func TestProcessStoreCapacityReturnsSafe503(t *testing.T) {
	store := process.NewStoreWithLimits(time.Minute, 1, 64)
	op := process.NewOperation(store, func(context.Context, string) (string, error) { return "masked", nil })
	mux := NewRouter(nil, WithProcess(op.Handle))
	first := doProcessRequest(mux, `{"payload":"synthetic","payload_id":"first"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d", first.Code)
	}
	full := doProcessRequest(mux, `{"payload":"private synthetic","payload_id":"second"}`)
	if full.Code != http.StatusServiceUnavailable || strings.Contains(full.Body.String(), "private synthetic") || strings.Contains(full.Body.String(), "result") {
		t.Fatalf("capacity response: status=%d body=%q", full.Code, full.Body.String())
	}
}

// doProcessRequest performs a single POST /process request without using
// t.Fatal, so it is safe to call from concurrent goroutines.
func doProcessRequest(mux *http.ServeMux, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/process", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestProcessIntegrationFirstPayloadMaskedOnce proves that a first original
// payload for a new payload_id is masked exactly once and returned as the
// result, and that the record transitions to ready.
func TestProcessIntegrationFirstPayloadMaskedOnce(t *testing.T) {
	var calls atomic.Int64
	mux := newProcessIntegrationMux(countingMask(&calls))

	rec := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"synthetic original","payload_id":"id-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var resp process.Response
	decodeJSONResponse(t, rec, &resp)
	if resp.Result != "masked:synthetic original" {
		t.Errorf("result = %q, want %q", resp.Result, "masked:synthetic original")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("mask calls = %d, want 1", got)
	}
}

// TestProcessIntegrationRetryOriginalIsIdempotent proves that retrying the
// same original payload for the same payload_id returns the identical result
// without a second masking call.
func TestProcessIntegrationRetryOriginalIsIdempotent(t *testing.T) {
	var calls atomic.Int64
	mux := newProcessIntegrationMux(countingMask(&calls))

	first := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"synthetic original","payload_id":"id-1"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d", first.Code, http.StatusOK)
	}
	var firstResp process.Response
	decodeJSONResponse(t, first, &firstResp)

	retry := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"synthetic original","payload_id":"id-1"}`)
	if retry.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want %d", retry.Code, http.StatusOK)
	}
	var retryResp process.Response
	decodeJSONResponse(t, retry, &retryResp)
	if retryResp.Result != firstResp.Result {
		t.Errorf("retry result = %q, want %q", retryResp.Result, firstResp.Result)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("mask calls = %d, want 1 (no re-masking on retry)", got)
	}
}

// TestProcessIntegrationRestoreByMaskReturnsOriginal proves that sending the
// previously issued mask restores the original without rerunning masking/NER,
// and that the record stays ready (repeatable read).
func TestProcessIntegrationRestoreByMaskReturnsOriginal(t *testing.T) {
	var calls atomic.Int64
	mux := newProcessIntegrationMux(countingMask(&calls))

	first := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"synthetic original","payload_id":"id-1"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d", first.Code, http.StatusOK)
	}
	var firstResp process.Response
	decodeJSONResponse(t, first, &firstResp)

	restored := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"`+firstResp.Result+`","payload_id":"id-1"}`)
	if restored.Code != http.StatusOK {
		t.Fatalf("restore status = %d, want %d", restored.Code, http.StatusOK)
	}
	var restoredResp process.Response
	decodeJSONResponse(t, restored, &restoredResp)
	if restoredResp.Result != "synthetic original" {
		t.Errorf("restored result = %q, want %q", restoredResp.Result, "synthetic original")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("mask calls = %d, want 1 (no re-masking on restore)", got)
	}

	// Restore is a repeatable read: a second restore returns the same original
	// and the record is not mutated.
	again := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"`+firstResp.Result+`","payload_id":"id-1"}`)
	if again.Code != http.StatusOK {
		t.Fatalf("second restore status = %d, want %d", again.Code, http.StatusOK)
	}
	var againResp process.Response
	decodeJSONResponse(t, again, &againResp)
	if againResp.Result != "synthetic original" {
		t.Errorf("second restore result = %q, want %q", againResp.Result, "synthetic original")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("mask calls = %d, want 1 (repeatable read)", got)
	}
}

// TestProcessIntegrationThirdUnrelatedPayloadConflicts proves that a third
// unrelated payload for the same payload_id returns a safe 409 and does not
// mutate the ready record.
func TestProcessIntegrationThirdUnrelatedPayloadConflicts(t *testing.T) {
	var calls atomic.Int64
	mux := newProcessIntegrationMux(countingMask(&calls))

	first := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"synthetic original","payload_id":"id-1"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d", first.Code, http.StatusOK)
	}
	var firstResp process.Response
	decodeJSONResponse(t, first, &firstResp)

	conflict := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"unrelated third payload","payload_id":"id-1"}`)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d, want %d", conflict.Code, http.StatusConflict)
	}
	var errResp errorResponse
	decodeJSONResponse(t, conflict, &errResp)
	if errResp.Error == "" {
		t.Errorf("error body missing generic message: %q", conflict.Body.String())
	}
	if strings.Contains(conflict.Body.String(), "unrelated third payload") {
		t.Errorf("body leaks payload: %q", conflict.Body.String())
	}
	if strings.Contains(conflict.Body.String(), "result") {
		t.Errorf("body leaks result field: %q", conflict.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("mask calls = %d, want 1 (no re-masking on conflict)", got)
	}

	// The ready record is unchanged: the original still restores and the mask
	// still restores the original.
	restored := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"`+firstResp.Result+`","payload_id":"id-1"}`)
	if restored.Code != http.StatusOK {
		t.Fatalf("restore after conflict status = %d, want %d", restored.Code, http.StatusOK)
	}
	var restoredResp process.Response
	decodeJSONResponse(t, restored, &restoredResp)
	if restoredResp.Result != "synthetic original" {
		t.Errorf("restored result = %q, want %q", restoredResp.Result, "synthetic original")
	}
}

// TestProcessIntegrationConcurrentFirstRequestsSingleWriter proves that
// concurrent first requests for the same payload_id have exactly one masking
// call and all callers receive a consistent response.
func TestProcessIntegrationConcurrentFirstRequestsSingleWriter(t *testing.T) {
	const (
		payload   = "synthetic original"
		payloadID = "id-1"
	)
	var calls atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	mask := func(_ context.Context, p string) (string, error) {
		calls.Add(1)
		close(entered)
		<-release
		return "masked:" + p, nil
	}
	mux := newProcessIntegrationMux(mask)

	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]string, n)
	statuses := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rec := doProcessRequest(mux, `{"payload":"`+payload+`","payload_id":"`+payloadID+`"}`)
			statuses[i] = rec.Code
			var resp process.Response
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			results[i] = resp.Result
		}(i)
	}
	close(start)
	// Wait until the winner has entered the masker before releasing it, so all
	// callers are racing on the same claim.
	<-entered
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("mask calls = %d, want 1", got)
	}
	for i := 0; i < n; i++ {
		if statuses[i] != http.StatusOK {
			t.Errorf("caller %d status = %d, want %d", i, statuses[i], http.StatusOK)
		}
		if results[i] != "masked:"+payload {
			t.Errorf("caller %d result = %q, want %q", i, results[i], "masked:"+payload)
		}
	}
}

// TestProcessIntegrationConcurrentDifferentFirstPayloadsOneWinner proves that
// when concurrent first requests carry different payloads for the same
// payload_id, exactly one wins the claim and masks, and the loser receives a
// safe 409 without overwriting the record.
func TestProcessIntegrationConcurrentDifferentFirstPayloadsOneWinner(t *testing.T) {
	const payloadID = "id-1"
	var calls atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	mask := func(_ context.Context, p string) (string, error) {
		calls.Add(1)
		close(entered)
		<-release
		return "masked:" + p, nil
	}
	mux := newProcessIntegrationMux(mask)

	start := make(chan struct{})
	var wg sync.WaitGroup
	type outcome struct {
		status int
		result string
	}
	outcomes := make([]outcome, 2)
	payloads := []string{"first payload", "second payload"}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rec := doProcessRequest(mux, `{"payload":"`+payloads[i]+`","payload_id":"`+payloadID+`"}`)
			var resp process.Response
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			outcomes[i] = outcome{status: rec.Code, result: resp.Result}
		}(i)
	}
	close(start)
	<-entered
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("mask calls = %d, want 1", got)
	}

	var winner, loser int
	if outcomes[0].status == http.StatusOK {
		winner, loser = 0, 1
	} else {
		winner, loser = 1, 0
	}
	if outcomes[winner].status != http.StatusOK {
		t.Fatalf("winner status = %d, want %d", outcomes[winner].status, http.StatusOK)
	}
	if outcomes[winner].result != "masked:"+payloads[winner] {
		t.Errorf("winner result = %q, want %q", outcomes[winner].result, "masked:"+payloads[winner])
	}
	if outcomes[loser].status != http.StatusConflict {
		t.Errorf("loser status = %d, want %d", outcomes[loser].status, http.StatusConflict)
	}
	if outcomes[loser].result != "" {
		t.Errorf("loser result = %q, want empty", outcomes[loser].result)
	}

	// The record holds the winner's original/result and was not overwritten.
	restored := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"`+outcomes[winner].result+`","payload_id":"`+payloadID+`"}`)
	if restored.Code != http.StatusOK {
		t.Fatalf("restore status = %d, want %d", restored.Code, http.StatusOK)
	}
	var restoredResp process.Response
	decodeJSONResponse(t, restored, &restoredResp)
	if restoredResp.Result != payloads[winner] {
		t.Errorf("restored result = %q, want %q", restoredResp.Result, payloads[winner])
	}
}

// TestProcessIntegrationInvalidContractInputRejected proves that malformed or
// invalid contract input returns a safe 400 without invoking the masking
// dependency.
func TestProcessIntegrationInvalidContractInputRejected(t *testing.T) {
	var calls atomic.Int64
	mux := newProcessIntegrationMux(countingMask(&calls))

	cases := []struct {
		name string
		body string
	}{
		{"missing payload", `{"payload_id":"id-1"}`},
		{"missing payload_id", `{"payload":"x"}`},
		{"empty payload", `{"payload":"  ","payload_id":"id-1"}`},
		{"empty payload_id", `{"payload":"x","payload_id":"  "}`},
		{"wrong payload type", `{"payload":123,"payload_id":"id-1"}`},
		{"wrong payload_id type", `{"payload":"x","payload_id":123}`},
		{"malformed JSON", `{not json`},
		{"second JSON value", `{"payload":"x","payload_id":"id-1"} {"payload":"y","payload_id":"id-2"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSONRequest(t, mux, http.MethodPost, "/process", tc.body)
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
	if got := calls.Load(); got != 0 {
		t.Errorf("mask calls = %d, want 0 (operation not invoked for invalid input)", got)
	}
}

// TestProcessIntegrationMaskingFailureFailsClosed proves that a masking
// dependency failure returns the documented safe status/body without result
// leakage. A generic masking failure maps to 500; a vault-unavailable failure
// maps to 503. Neither leaks the payload, the dependency detail or a result.
func TestProcessIntegrationMaskingFailureFailsClosed(t *testing.T) {
	const payload = "synthetic payload"
	cases := []struct {
		name string
		mask process.MaskFunc
		want int
	}{
		{"generic masking failure", func(_ context.Context, _ string) (string, error) {
			return "", process.ErrMaskingFailed
		}, http.StatusInternalServerError},
		{"vault unavailable", func(_ context.Context, _ string) (string, error) {
			return "", process.ErrVaultUnavailable
		}, http.StatusServiceUnavailable},
		{"model unavailable", func(_ context.Context, _ string) (string, error) {
			return "", process.ErrModelUnavailable
		}, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := newProcessIntegrationMux(tc.mask)
			rec := doJSONRequest(t, mux, http.MethodPost, "/process",
				`{"payload":"`+payload+`","payload_id":"id-1"}`)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
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
				t.Errorf("body leaks payload: %q", rec.Body.String())
			}
		})
	}
}
