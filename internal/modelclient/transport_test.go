package modelclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// countingTransport counts requests and delegates to the default transport.
type countingTransport struct{ calls atomic.Int32 }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return http.DefaultTransport.RoundTrip(r)
}

func TestWithTransportRoutesWorkerCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entities":[]}`))
	}))
	defer srv.Close()

	rt := &countingTransport{}
	c, err := New(srv.URL, ModeFull, time.Second, WithTransport(rt))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := c.Infer(context.Background(), "текст"); err != nil {
		t.Fatalf("Infer() error = %v", err)
	}
	if got := rt.calls.Load(); got != 1 {
		t.Errorf("transport calls = %d, want 1", got)
	}
}

func TestOperationUsesFixedNames(t *testing.T) {
	cases := map[string]string{
		"http://w:8000/infer":             "infer",
		"http://w:8000/base/count_tokens": "count_tokens",
		"http://w:8000/plan_windows":      "plan_windows",
		"http://w:8000/PII_CANARY":        "other",
	}
	for u, want := range cases {
		r := httptest.NewRequest(http.MethodPost, u, nil)
		if got := Operation(r); got != want {
			t.Errorf("Operation(%s) = %q, want %q", u, got, want)
		}
	}
}
