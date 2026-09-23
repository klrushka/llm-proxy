package modelclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This synthetic worker rejects the fifth concurrent POST, as production does.
// Planning and inference must use the same four worker slots.
func TestAllWorkerPostsShareCapacity(t *testing.T) {
	var active, peak atomic.Int64
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		if n > globalInferLimit {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		}
		time.Sleep(15 * time.Millisecond)
		var body string
		switch r.URL.Path {
		case "/plan_windows":
			body = planJSON(1, 510, planWindowJSON(0, 3, 1))
		case "/infer":
			body = responseJSON(entityJSON("FULL_NAME", 0, 1, 0.9, "rubert"), entityJSON("PER", 0, 1, 0.9, "gliner"))
		default:
			return nil, fmt.Errorf("unexpected path")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	c, err := New("http://worker.test", ModeFull, time.Second, WithTransport(rt))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, err := c.PlanWindows(context.Background(), "abc", 0); errs <- err }()
		go func() { defer wg.Done(); _, err := c.Infer(context.Background(), "abc"); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("worker POST failed: %v (peak %d)", err, peak.Load())
		}
	}
	if got := peak.Load(); got > globalInferLimit {
		t.Fatalf("worker POST peak = %d, want <= %d", got, globalInferLimit)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
