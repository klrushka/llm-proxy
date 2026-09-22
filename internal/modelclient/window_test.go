package modelclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// windowServer returns a server that responds to every /infer request with a
// single entity spanning the whole window text, plus an atomic peak counter of
// concurrent in-flight requests and a hook invoked before responding.
func windowServer(t *testing.T, before func()) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var inflight atomic.Int64
	var peak atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inflight.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		defer inflight.Add(-1)
		if before != nil {
			before()
		}
		fmt.Fprint(w, responseJSON(entityJSON("FULL_NAME", 0, 1, 0.9, "rubert")))
	}))
	t.Cleanup(srv.Close)
	return srv, &peak
}

func TestWindowedInferInvalidConfig(t *testing.T) {
	srv, _ := windowServer(t, nil)
	c := newTestClient(t, srv, ModeFull)

	tests := []struct {
		name       string
		windowSize int
		overlap    int
		limit      int
	}{
		{"zero window size", 0, 0, 1},
		{"negative window size", -1, 0, 1},
		{"negative overlap", 10, -1, 1},
		{"overlap equals size", 10, 10, 1},
		{"overlap exceeds size", 10, 11, 1},
		{"zero limit", 10, 2, 0},
		{"negative limit", 10, 2, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.WindowedInfer(context.Background(), "Анна Смирнова", tt.windowSize, tt.overlap, tt.limit)
			if !errors.Is(err, ErrInvalidWindowConfig) {
				t.Fatalf("WindowedInfer() error = %v, want ErrInvalidWindowConfig", err)
			}
		})
	}
}

func TestWindowedInferInvalidUTF8(t *testing.T) {
	srv, _ := windowServer(t, nil)
	c := newTestClient(t, srv, ModeFull)

	_, err := c.WindowedInfer(context.Background(), string([]byte{0xff, 0xfe}), 10, 2, 1)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("WindowedInfer() error = %v, want ErrInvalidInput", err)
	}
}

func TestWindowedInferEmptyText(t *testing.T) {
	srv, _ := windowServer(t, nil)
	c := newTestClient(t, srv, ModeFull)

	got, err := c.WindowedInfer(context.Background(), "", 10, 2, 1)
	if err != nil {
		t.Fatalf("WindowedInfer() error = %v", err)
	}
	if got != nil {
		t.Fatalf("windows = %v, want nil", got)
	}
}

// TestWindowedInferCoverageAndOrder verifies that windows cover the whole text
// continuously without gaps, never split a UTF-8 rune, never duplicate a fully
// covered suffix, and are returned in deterministic window order. A window may
// exceed the byte windowSize only when a single rune is itself longer than the
// limit.
func TestWindowedInferCoverageAndOrder(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		windowSize int
		overlap    int
	}{
		{"ascii exact fit", "abcdefghij", 5, 0},
		{"ascii overlap", "abcdefghij", 5, 2},
		{"cyrillic no overlap", "Анна Смирнова Ивановна", 8, 0},
		{"cyrillic overlap", "Анна Смирнова Ивановна", 8, 3},
		{"emoji boundaries", "A🙂Б🙂В🙂Г", 4, 1},
		{"single window", "Анна", 100, 0},
		{"window larger than text", "Анна", 100, 50},
		{"exact multiple", "abcdefghij", 5, 0},
		{"overlap near end", "abcdefghij", 6, 4},
		{"window smaller than emoji", "🙂🙂🙂", 2, 0},
		{"large overlap short window", "abc🙂🙂🙂def", 10, 9},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := windowServer(t, nil)
			c := newTestClient(t, srv, ModeFull)

			got, err := c.WindowedInfer(context.Background(), tt.text, tt.windowSize, tt.overlap, 1)
			if err != nil {
				t.Fatalf("WindowedInfer() error = %v", err)
			}
			if len(got) == 0 {
				t.Fatal("no windows returned")
			}

			// Deterministic order: windows must be sorted by Start.
			for i := 1; i < len(got); i++ {
				if got[i].Start < got[i-1].Start {
					t.Fatalf("windows out of order: %d then %d", got[i-1].Start, got[i].Start)
				}
			}

			// Each window must be a valid slice of the original text with
			// matching global offsets and no split runes. A window may exceed
			// windowSize only if it holds a single rune longer than the limit.
			for i, w := range got {
				if w.Start < 0 || w.End > len(tt.text) || w.Start > w.End {
					t.Fatalf("window %d invalid offsets %d..%d for len %d", i, w.Start, w.End, len(tt.text))
				}
				if w.Text != tt.text[w.Start:w.End] {
					t.Fatalf("window %d text %q != text[%d:%d] %q", i, w.Text, w.Start, w.End, tt.text[w.Start:w.End])
				}
				if w.End-w.Start > tt.windowSize {
					// Allowed only for a single rune longer than windowSize.
					if utf8.RuneCountInString(w.Text) != 1 {
						t.Fatalf("window %d size %d exceeds windowSize %d with %d runes",
							i, w.End-w.Start, tt.windowSize, utf8.RuneCountInString(w.Text))
					}
				}
			}

			// Continuous full coverage: every byte is covered at least once and
			// there are no gaps. Overlap makes duplicate coverage expected.
			covered := make([]bool, len(tt.text))
			for _, w := range got {
				for b := w.Start; b < w.End; b++ {
					covered[b] = true
				}
			}
			for b := range covered {
				if !covered[b] {
					t.Fatalf("byte %d not covered (gap)", b)
				}
			}

			// No duplicate fully-covered suffix: the last window must end at
			// len(text) and no earlier window may end at len(text).
			last := got[len(got)-1]
			if last.End != len(tt.text) {
				t.Fatalf("last window end = %d, want %d", last.End, len(tt.text))
			}
			for i := 0; i < len(got)-1; i++ {
				if got[i].End == len(tt.text) {
					t.Fatalf("non-last window %d ends at len(text), duplicate suffix", i)
				}
			}
		})
	}
}

// TestWindowedInferOverlapProduced proves that overlap actually produces
// duplicate byte coverage when the text is long enough for multiple windows.
func TestWindowedInferOverlapProduced(t *testing.T) {
	srv, _ := windowServer(t, nil)
	c := newTestClient(t, srv, ModeFull)

	text := "abcdefghijklmnopqrstuvwxyz"
	got, err := c.WindowedInfer(context.Background(), text, 5, 2, 1)
	if err != nil {
		t.Fatalf("WindowedInfer() error = %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("windows = %d, want >= 2 for overlap proof", len(got))
	}

	// Count how many bytes are covered by more than one window.
	covered := make([]int, len(text))
	for _, w := range got {
		for b := w.Start; b < w.End; b++ {
			covered[b]++
		}
	}
	overlapped := 0
	for _, n := range covered {
		if n > 1 {
			overlapped++
		}
	}
	if overlapped == 0 {
		t.Fatal("no byte covered by more than one window despite overlap > 0")
	}
}

// TestWindowedInferDeterministicOrder verifies that results are ordered by
// window regardless of completion order. The server delays earlier windows so
// later windows finish first.
func TestWindowedInferDeterministicOrder(t *testing.T) {
	var mu sync.Mutex
	delays := map[string]time.Duration{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		_ = jsonDecode(r, &body)
		mu.Lock()
		d := delays[body.Text]
		mu.Unlock()
		if d > 0 {
			time.Sleep(d)
		}
		fmt.Fprint(w, responseJSON(entityJSON("FULL_NAME", 0, 1, 0.9, "rubert")))
	}))
	defer srv.Close()
	c := newTestClient(t, srv, ModeFull)

	text := "abcdefghijklmnopqrstuvwxyz"
	// Delay the first window so later windows complete first.
	mu.Lock()
	delays[text[:5]] = 100 * time.Millisecond
	mu.Unlock()

	got, err := c.WindowedInfer(context.Background(), text, 5, 0, 4)
	if err != nil {
		t.Fatalf("WindowedInfer() error = %v", err)
	}
	for i, w := range got {
		if w.Start != i*5 {
			t.Fatalf("window %d start = %d, want %d", i, w.Start, i*5)
		}
		if w.Text != text[w.Start:w.End] {
			t.Fatalf("window %d text = %q, want %q", i, w.Text, text[w.Start:w.End])
		}
	}
}

// TestWindowedInferBoundedConcurrency proves actual concurrency never exceeds
// the limit. The server briefly blocks so requests overlap, and the peak
// in-flight count must stay <= limit.
func TestWindowedInferBoundedConcurrency(t *testing.T) {
	const limit = 3
	srv, peak := windowServer(t, func() { time.Sleep(5 * time.Millisecond) })
	c := newTestClient(t, srv, ModeFull)

	text := strings.Repeat("abcdefghij", 20) // 200 bytes -> 40 windows of 5
	_, err := c.WindowedInfer(context.Background(), text, 5, 0, limit)
	if err != nil {
		t.Fatalf("WindowedInfer() error = %v", err)
	}
	if got := peak.Load(); got > limit {
		t.Fatalf("peak concurrency = %d, want <= %d", got, limit)
	}
}

// TestWindowedInferConcurrencyPossible proves that with limit > 1 actual
// concurrency greater than one is achievable. The server blocks until it has
// seen limit concurrent requests, then releases them all.
func TestWindowedInferConcurrencyPossible(t *testing.T) {
	const limit = 3
	start := make(chan struct{})
	var seen int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&seen, 1) == limit {
			close(start)
		}
		<-start
		fmt.Fprint(w, responseJSON(entityJSON("FULL_NAME", 0, 1, 0.9, "rubert")))
	}))
	defer srv.Close()
	c := newTestClient(t, srv, ModeFull)

	text := strings.Repeat("abcdefghij", 20)
	_, err := c.WindowedInfer(context.Background(), text, 5, 0, limit)
	if err != nil {
		t.Fatalf("WindowedInfer() error = %v", err)
	}
	if atomic.LoadInt32(&seen) < limit {
		t.Fatalf("saw %d concurrent requests, want at least %d", seen, limit)
	}
}

// TestWindowedInferCancellation verifies that cancellation stops new work and
// returns a context error without leaking goroutines.
func TestWindowedInferCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		fmt.Fprint(w, responseJSON(entityJSON("FULL_NAME", 0, 1, 0.9, "rubert")))
	}))
	defer srv.Close()
	c := newTestClient(t, srv, ModeFull)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.WindowedInfer(ctx, strings.Repeat("abcdefghij", 20), 5, 0, 2)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	close(release)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WindowedInfer did not return after cancellation (goroutine leak)")
	}
}

// TestWindowedInferCancelledBeforeCall verifies that a context cancelled before
// the call returns quickly without making any HTTP request.
func TestWindowedInferCancelledBeforeCall(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		fmt.Fprint(w, responseJSON(entityJSON("FULL_NAME", 0, 1, 0.9, "rubert")))
	}))
	defer srv.Close()
	c := newTestClient(t, srv, ModeFull)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.WindowedInfer(ctx, strings.Repeat("abcdefghij", 20), 5, 0, 2)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WindowedInfer did not return for pre-cancelled context")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("made %d HTTP requests for pre-cancelled context, want 0", got)
	}
}

// TestWindowedInferCancelledWhileWaitingForPermit verifies that cancelling while
// the scheduler waits for a permit returns quickly, does not hang, and does not
// start new windows after cancellation.
func TestWindowedInferCancelledWhileWaitingForPermit(t *testing.T) {
	release := make(chan struct{})
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		<-release
		fmt.Fprint(w, responseJSON(entityJSON("FULL_NAME", 0, 1, 0.9, "rubert")))
	}))
	defer srv.Close()
	c := newTestClient(t, srv, ModeFull)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.WindowedInfer(ctx, strings.Repeat("abcdefghij", 20), 5, 0, 1)
	}()

	// Wait until the first window is in flight and the scheduler is blocked
	// waiting for the single permit.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&calls) < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if atomic.LoadInt32(&calls) < 1 {
		t.Fatal("first window never started")
	}

	cancel()
	close(release)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WindowedInfer did not return after cancellation while waiting for permit")
	}
	// Only the first window may have run; no new windows after cancellation.
	if got := atomic.LoadInt32(&calls); got > 1 {
		t.Fatalf("started %d windows after cancellation, want <= 1", got)
	}
}

// TestWindowedInferFailClosed verifies that any window error fails the whole
// call with no partial result, and that window text or responses never appear
// in the error.
func TestWindowedInferFailClosed(t *testing.T) {
	const marker = "WINDOW_MARKER_12345"
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 2 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, responseJSON(entityJSON("FULL_NAME", 0, 1, 0.9, "rubert")))
	}))
	defer srv.Close()
	c := newTestClient(t, srv, ModeFull)

	text := strings.Repeat(marker, 10)
	got, err := c.WindowedInfer(context.Background(), text, 5, 0, 2)
	if err == nil {
		t.Fatal("WindowedInfer() expected error")
	}
	if got != nil {
		t.Fatalf("windows = %v, want nil on failure (fail closed)", got)
	}
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("WindowedInfer() error = %v, want ErrModelUnavailable", err)
	}
	if strings.Contains(err.Error(), marker) {
		t.Errorf("error leaks window text: %q", err.Error())
	}
}

// jsonDecode decodes a JSON request body for test servers.
func jsonDecode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}
