package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/klrushka/llm-proxy/internal/process"
)

func TestProcessHTTPTransientRetryAndStableRestore(t *testing.T) {
	var calls atomic.Int32
	mux := newProcessIntegrationMux(func(_ context.Context, _ string) (string, error) {
		if calls.Add(1) == 1 {
			return "", process.ErrModelUnavailable
		}
		return "masked synthetic", nil
	})
	first := doProcessRequest(mux, `{"payload":"synthetic original","payload_id":"id"}`)
	if first.Code != http.StatusServiceUnavailable || first.Body.String() == "" {
		t.Fatalf("first status = %d", first.Code)
	}
	conflict := doProcessRequest(mux, `{"payload":"different synthetic","payload_id":"id"}`)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflicting retry status = %d", conflict.Code)
	}
	retry := doProcessRequest(mux, `{"payload":"synthetic original","payload_id":"id"}`)
	if retry.Code != http.StatusOK {
		t.Fatalf("retry status = %d", retry.Code)
	}
	stable := doProcessRequest(mux, `{"payload":"synthetic original","payload_id":"id"}`)
	if stable.Code != http.StatusOK || stable.Body.String() != retry.Body.String() {
		t.Fatalf("ready retry changed result")
	}
	restore := doProcessRequest(mux, `{"payload":"masked synthetic","payload_id":"id"}`)
	if restore.Code != http.StatusOK || restore.Body.String() != `{"result":"synthetic original"}`+"\n" {
		t.Fatalf("restore status = %d", restore.Code)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("mask calls = %d, want 2", got)
	}
}

func TestProcessHTTPReviewRequiredIsSafeAndStable(t *testing.T) {
	const marker = "SYNTHETIC_PRIVATE_MARKER"
	var calls atomic.Int32
	mux := newProcessIntegrationMux(func(context.Context, string) (string, error) {
		calls.Add(1)
		return "", process.ErrReviewRequired
	})
	body := `{"payload":"` + marker + `","payload_id":"id"}`
	first := doProcessRequest(mux, body)
	retry := doProcessRequest(mux, body)
	for _, rec := range []*httptest.ResponseRecorder{first, retry} {
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"review_required":true`) || strings.Contains(rec.Body.String(), marker) || strings.Contains(rec.Body.String(), `"result"`) {
			t.Fatalf("review response = %d %q", rec.Code, rec.Body.String())
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("review-required retry remasked %d times", got)
	}
	conflict := doProcessRequest(mux, `{"payload":"different synthetic","payload_id":"id"}`)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflicting review retry = %d", conflict.Code)
	}
}
