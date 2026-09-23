package modelclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Planning and inference share four worker slots; excess callers return a
// safe unavailable classification immediately for the process fallback.
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
	busy := 0
	for err := range errs {
		if errors.Is(err, ErrModelUnavailable) {
			busy++
		} else if err != nil {
			t.Fatalf("worker POST failed unexpectedly: %v (peak %d)", err, peak.Load())
		}
	}
	if busy == 0 {
		t.Fatal("full worker gate did not reject any excess call")
	}
	if got := peak.Load(); got > globalInferLimit {
		t.Fatalf("worker POST peak = %d, want <= %d", got, globalInferLimit)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestQueuedWorkerPostCancellationDoesNotLeakPermit(t *testing.T) {
	entered := make(chan struct{}, globalInferLimit)
	release := make(chan struct{})
	var calls atomic.Int64
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Path == "/plan_windows" {
			entered <- struct{}{}
			<-release
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(planJSON(1, 510, planWindowJSON(0, 3, 1)))), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(responseJSON(entityJSON("FULL_NAME", 0, 1, 0.9, "rubert"), entityJSON("PER", 0, 1, 0.9, "gliner")))), Header: make(http.Header)}, nil
	})
	c, err := New("http://worker.test", ModeFull, time.Second, WithTransport(rt))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < globalInferLimit; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = c.PlanWindows(context.Background(), "abc", 0) }()
	}
	for i := 0; i < globalInferLimit; i++ {
		<-entered
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Infer(ctx, "abc"); !errors.Is(err, context.Canceled) || !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("queued canceled Infer error = %v", err)
	}
	if got := calls.Load(); got != globalInferLimit {
		t.Fatalf("worker calls after canceled wait = %d, want %d", got, globalInferLimit)
	}
	close(release)
	wg.Wait()
	if _, err := c.Infer(context.Background(), "abc"); err != nil {
		t.Fatalf("post-release Infer = %v", err)
	}
}

func TestFullWorkerGateImmediatelyRejectsAllExpensivePosts(t *testing.T) {
	entered := make(chan struct{}, globalInferLimit)
	release := make(chan struct{})
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		entered <- struct{}{}
		<-release
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(responseJSON())), Header: make(http.Header)}, nil
	})
	c, err := New("http://worker.test", ModeFull, time.Second, WithTransport(rt))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < globalInferLimit; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = c.Infer(context.Background(), "synthetic") }()
	}
	for i := 0; i < globalInferLimit; i++ {
		<-entered
	}
	for name, call := range map[string]func() error{
		"infer": func() error { _, err := c.Infer(context.Background(), "synthetic"); return err },
		"plan":  func() error { _, err := c.PlanWindows(context.Background(), "synthetic", 0); return err },
		"count": func() error { _, err := c.CountTokens(context.Background(), "synthetic"); return err },
	} {
		start := time.Now()
		if err := call(); !errors.Is(err, ErrModelUnavailable) {
			t.Errorf("%s busy error = %v", name, err)
		}
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			t.Errorf("%s waited for busy worker: %v", name, elapsed)
		}
	}
	close(release)
	wg.Wait()
}
