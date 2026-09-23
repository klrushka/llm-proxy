package api

import (
	"context"
	"net/http"
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
