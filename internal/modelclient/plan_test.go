package modelclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// planWindowJSON builds a single window object for a /plan_windows response.
func planWindowJSON(start, end, tokenCount int) string {
	return `{"start":` + strconv.Itoa(start) +
		`,"end":` + strconv.Itoa(end) +
		`,"token_count":` + strconv.Itoa(tokenCount) + `}`
}

// planJSON builds a /plan_windows response body with the exact primary model id.
func planJSON(totalCount, maxWindowTokens int, windows ...string) string {
	return `{"model":` + strconv.Quote(rubertModelID) +
		`,"total_count":` + strconv.Itoa(totalCount) +
		`,"max_window_tokens":` + strconv.Itoa(maxWindowTokens) +
		`,"windows":[` + strings.Join(windows, ",") + `]}`
}

// planServer returns a server that answers POST /plan_windows with planBody and
// delegates every other request to infer.
func planServer(t *testing.T, planBody string, infer http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/plan_windows" {
			fmt.Fprint(w, planBody)
			return
		}
		if infer != nil {
			infer(w, r)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPlanWindowsStrictValidation proves that every malformed plan response
// fails closed with ErrInvalidResponse.
func TestPlanWindowsStrictValidation(t *testing.T) {
	const text = "abcdefghij"
	tests := []struct {
		name string
		body string
	}{
		{"invalid json", `{not json`},
		{"trailing json", planJSON(1, 510, planWindowJSON(0, 5, 3)) + ` extra`},
		{"missing model", `{"total_count":1,"max_window_tokens":510,"windows":[{"start":0,"end":5,"token_count":3}]}`},
		{"wrong model", `{"model":"other/model","total_count":1,"max_window_tokens":510,"windows":[{"start":0,"end":5,"token_count":3}]}`},
		{"non-string model", `{"model":123,"total_count":1,"max_window_tokens":510,"windows":[{"start":0,"end":5,"token_count":3}]}`},
		{"missing total_count", `{"model":"` + rubertModelID + `","max_window_tokens":510,"windows":[{"start":0,"end":5,"token_count":3}]}`},
		{"missing max_window_tokens", `{"model":"` + rubertModelID + `","total_count":1,"windows":[{"start":0,"end":5,"token_count":3}]}`},
		{"missing windows", `{"model":"` + rubertModelID + `","total_count":1,"max_window_tokens":510}`},
		{"unknown top level field", `{"model":"` + rubertModelID + `","total_count":1,"max_window_tokens":510,"windows":[{"start":0,"end":5,"token_count":3}],"extra":1}`},
		{"negative total_count", planJSON(-1, 510, planWindowJSON(0, 5, 3))},
		{"zero max_window_tokens", planJSON(1, 0, planWindowJSON(0, 5, 3))},
		{"negative max_window_tokens", planJSON(1, -1, planWindowJSON(0, 5, 3))},
		{"window missing field", `{"model":"` + rubertModelID + `","total_count":1,"max_window_tokens":510,"windows":[{"start":0,"end":5}]}`},
		{"window unknown field", `{"model":"` + rubertModelID + `","total_count":1,"max_window_tokens":510,"windows":[{"start":0,"end":5,"token_count":3,"extra":1}]}`},
		{"negative start", planJSON(1, 510, planWindowJSON(-1, 5, 3))},
		{"negative end", planJSON(1, 510, planWindowJSON(0, -1, 3))},
		{"reversed window", planJSON(1, 510, planWindowJSON(5, 0, 3))},
		{"empty window", planJSON(1, 510, planWindowJSON(0, 0, 3))},
		{"zero token_count", planJSON(1, 510, planWindowJSON(0, 5, 0))},
		{"negative token_count", planJSON(1, 510, planWindowJSON(0, 5, -1))},
		{"token_count exceeds capacity", planJSON(1, 2, planWindowJSON(0, 5, 3))},
		{"offset beyond text", planJSON(1, 510, planWindowJSON(0, 100, 3))},
		{"first window not at 0", planJSON(1, 510, planWindowJSON(1, 5, 3))},
		{"gap between windows", planJSON(2, 510, planWindowJSON(0, 3, 3), planWindowJSON(5, 8, 3))},
		{"non-progressing window", planJSON(2, 510, planWindowJSON(0, 5, 3), planWindowJSON(2, 8, 3))},
		{"non-progressing window end", planJSON(2, 510, planWindowJSON(0, 5, 3), planWindowJSON(3, 4, 3))},
		{"windows do not overlap", planJSON(2, 510, planWindowJSON(0, 5, 3), planWindowJSON(5, 10, 3))},
		{"does not cover full text", planJSON(1, 510, planWindowJSON(0, 5, 3))},
		{"empty plan mismatch", planJSON(1, 510)},
		{"total_count zero with non-empty windows", planJSON(0, 510, planWindowJSON(0, 5, 3))},
		{"capacity not greater than overlap", planJSON(1, 64, planWindowJSON(0, 5, 3))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := planServer(t, tt.body, nil)
			c := newTestClient(t, srv, ModeFull)
			_, err := c.PlanWindows(context.Background(), text, 64)
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("PlanWindows() error = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

// TestPlanWindowsEmptyText proves that only a consistent empty plan is accepted
// for empty text, and that a non-empty plan for empty text fails closed.
func TestPlanWindowsEmptyText(t *testing.T) {
	t.Run("consistent empty plan", func(t *testing.T) {
		srv := planServer(t, planJSON(0, 510), nil)
		c := newTestClient(t, srv, ModeFull)
		plan, err := c.PlanWindows(context.Background(), "", 64)
		if err != nil {
			t.Fatalf("PlanWindows() error = %v", err)
		}
		if plan == nil || len(plan.Windows) != 0 || plan.TotalCount != 0 {
			t.Fatalf("plan = %+v, want empty with total_count 0", plan)
		}
	})

	t.Run("non-empty plan for empty text", func(t *testing.T) {
		srv := planServer(t, planJSON(1, 510, planWindowJSON(0, 5, 3)), nil)
		c := newTestClient(t, srv, ModeFull)
		_, err := c.PlanWindows(context.Background(), "", 64)
		if !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("PlanWindows() error = %v, want ErrInvalidResponse", err)
		}
	})
}

// TestPlanWindowsAllWhitespace proves that non-empty all-whitespace text with
// total_count 0 and no windows is a valid empty plan, and that any other
// empty-plan mismatch is rejected.
func TestPlanWindowsAllWhitespace(t *testing.T) {
	t.Run("all whitespace valid", func(t *testing.T) {
		srv := planServer(t, planJSON(0, 510), nil)
		c := newTestClient(t, srv, ModeFull)
		plan, err := c.PlanWindows(context.Background(), "   \t\n  ", 64)
		if err != nil {
			t.Fatalf("PlanWindows() error = %v", err)
		}
		if plan == nil || len(plan.Windows) != 0 || plan.TotalCount != 0 {
			t.Fatalf("plan = %+v, want empty with total_count 0", plan)
		}
	})

	t.Run("empty plan mismatch rejected", func(t *testing.T) {
		srv := planServer(t, planJSON(1, 510), nil)
		c := newTestClient(t, srv, ModeFull)
		_, err := c.PlanWindows(context.Background(), "   ", 64)
		if !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("PlanWindows() error = %v, want ErrInvalidResponse", err)
		}
	})
}

// TestPlanWindowsTrailingWhitespace proves that a plan for text with trailing
// whitespace must still cover the full text including the trailing whitespace.
func TestPlanWindowsTrailingWhitespace(t *testing.T) {
	const text = "abc   "
	// 6 code points; a plan ending at 3 (before the trailing whitespace) is
	// rejected, while a plan covering the full text is accepted.
	t.Run("must cover trailing whitespace", func(t *testing.T) {
		srv := planServer(t, planJSON(1, 510, planWindowJSON(0, 3, 3)), nil)
		c := newTestClient(t, srv, ModeFull)
		_, err := c.PlanWindows(context.Background(), text, 0)
		if !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("PlanWindows() error = %v, want ErrInvalidResponse", err)
		}
	})

	t.Run("covers full text", func(t *testing.T) {
		srv := planServer(t, planJSON(1, 510, planWindowJSON(0, 6, 3)), nil)
		c := newTestClient(t, srv, ModeFull)
		plan, err := c.PlanWindows(context.Background(), text, 0)
		if err != nil {
			t.Fatalf("PlanWindows() error = %v", err)
		}
		if len(plan.Windows) != 1 || plan.Windows[0].End != len(text) {
			t.Fatalf("plan = %+v, want one window ending at %d", plan, len(text))
		}
	})
}

// TestPlanWindowsMultiWindowCyrillicEmojiOffsets proves that code-point offsets
// are converted to UTF-8 byte offsets and Window.Text is a slice of the
// original text across multiple windows of Cyrillic and emoji text.
func TestPlanWindowsMultiWindowCyrillicEmojiOffsets(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		windows [][2]int
		want    [][2]int
	}{
		{
			"cyrillic", "Анна Смирнова",
			[][2]int{{0, 4}, {4, 13}},
			[][2]int{{0, 8}, {8, 25}},
		},
		{
			"emoji", "A🙂Б🙂В",
			[][2]int{{0, 2}, {2, 5}},
			[][2]int{{0, 5}, {5, 13}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			windows := make([]string, 0, len(tt.windows))
			for _, w := range tt.windows {
				windows = append(windows, planWindowJSON(w[0], w[1], 3))
			}
			srv := planServer(t, planJSON(len(tt.windows), 510, windows...), nil)
			c := newTestClient(t, srv, ModeFull)
			plan, err := c.PlanWindows(context.Background(), tt.text, 0)
			if err != nil {
				t.Fatalf("PlanWindows() error = %v", err)
			}
			if len(plan.Windows) != len(tt.want) {
				t.Fatalf("len(windows) = %d, want %d", len(plan.Windows), len(tt.want))
			}
			for i, w := range plan.Windows {
				if w.Start != tt.want[i][0] || w.End != tt.want[i][1] {
					t.Errorf("window %d offsets = [%d,%d), want [%d,%d)", i, w.Start, w.End, tt.want[i][0], tt.want[i][1])
				}
				if w.Text != tt.text[w.Start:w.End] {
					t.Errorf("window %d text = %q, want %q", i, w.Text, tt.text[w.Start:w.End])
				}
			}
		})
	}
}

// TestPlanWindowsCyrillicEmojiOffsets proves that code-point offsets are
// converted to UTF-8 byte offsets and Window.Text is a slice of the original
// text, for Cyrillic and emoji text.
func TestPlanWindowsCyrillicEmojiOffsets(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		start int
		end   int
		wantS int
		wantE int
	}{
		{"cyrillic", "Анна Смирнова", 0, 13, 0, 25},
		{"emoji", "A🙂Б🙂В", 0, 5, 0, 13},
		{"mixed", "Иван Ivan", 0, 9, 0, 13},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := planServer(t, planJSON(1, 510, planWindowJSON(tt.start, tt.end, 3)), nil)
			c := newTestClient(t, srv, ModeFull)
			plan, err := c.PlanWindows(context.Background(), tt.text, 64)
			if err != nil {
				t.Fatalf("PlanWindows() error = %v", err)
			}
			if len(plan.Windows) != 1 {
				t.Fatalf("len(windows) = %d, want 1", len(plan.Windows))
			}
			w := plan.Windows[0]
			if w.Start != tt.wantS || w.End != tt.wantE {
				t.Errorf("window offsets = [%d,%d), want [%d,%d)", w.Start, w.End, tt.wantS, tt.wantE)
			}
			if w.Text != tt.text[w.Start:w.End] {
				t.Errorf("window text = %q, want %q", w.Text, tt.text[w.Start:w.End])
			}
		})
	}
}

// TestInferBoundedExactlyLimitAllowsMultipleInfer proves that a total_count of
// exactly MaxTokens is accepted and triggers multiple /infer calls.
func TestInferBoundedExactlyLimitAllowsMultipleInfer(t *testing.T) {
	var inferCalls int32
	srv := planServer(t, planJSON(MaxTokens, 510,
		planWindowJSON(0, 6, 3),
		planWindowJSON(4, 10, 3),
	), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&inferCalls, 1)
		fmt.Fprint(w, responseJSON(entityJSON("FULL_NAME", 0, 1, 0.9, "rubert")))
	}))
	c := newTestClient(t, srv, ModeFull)

	got, err := c.InferBounded(context.Background(), "abcdefghij", 64, 2)
	if err != nil {
		t.Fatalf("InferBounded() error = %v", err)
	}
	if atomic.LoadInt32(&inferCalls) < 2 {
		t.Fatalf("infer calls = %d, want >= 2", inferCalls)
	}
	if len(got) == 0 {
		t.Fatal("no entities returned")
	}
}

// TestInferBoundedOverLimitRejectsNoInfer proves that a total_count above
// MaxTokens returns ErrTokenLimitExceeded and never issues an /infer call.
func TestInferBoundedOverLimitRejectsNoInfer(t *testing.T) {
	var inferCalls int32
	srv := planServer(t, planJSON(MaxTokens+1, 510,
		planWindowJSON(0, 6, 3),
		planWindowJSON(4, 10, 3),
	), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&inferCalls, 1)
		fmt.Fprint(w, responseJSON())
	}))
	c := newTestClient(t, srv, ModeFull)

	got, err := c.InferBounded(context.Background(), "abcdefghij", 64, 2)
	if !errors.Is(err, ErrTokenLimitExceeded) {
		t.Fatalf("InferBounded() error = %v, want ErrTokenLimitExceeded", err)
	}
	if got != nil {
		t.Fatalf("entities = %v, want nil on failure", got)
	}
	if atomic.LoadInt32(&inferCalls) != 0 {
		t.Fatalf("infer calls = %d, want 0", inferCalls)
	}
}

// TestInferBoundedOverlapPreservesSpans proves that entities from overlapping
// windows are all preserved with correct global offsets after reconstruction.
func TestInferBoundedOverlapPreservesSpans(t *testing.T) {
	srv := planServer(t, planJSON(2, 510,
		planWindowJSON(0, 6, 3),
		planWindowJSON(4, 10, 3),
	), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, responseJSON(entityJSON("FULL_NAME", 0, 2, 0.9, "rubert")))
	}))
	c := newTestClient(t, srv, ModeFull)

	got, err := c.InferBounded(context.Background(), "abcdefghij", 64, 2)
	if err != nil {
		t.Fatalf("InferBounded() error = %v", err)
	}
	// window1 entity [0,2) -> global [0,2); window2 entity [0,2) -> global [4,6).
	if len(got) != 2 {
		t.Fatalf("len(entities) = %d, want 2 (both preserved)", len(got))
	}
	seen := map[[2]int]bool{}
	for _, e := range got {
		seen[[2]int{e.Start, e.End}] = true
	}
	if !seen[[2]int{0, 2}] || !seen[[2]int{4, 6}] {
		t.Errorf("entities = %+v, want both [0,2) and [4,6)", got)
	}
}

// TestInferBoundedBoundedConcurrency proves actual concurrency never exceeds
// the limit across the /infer calls of a single request.
func TestInferBoundedBoundedConcurrency(t *testing.T) {
	const limit = 3
	var inflight, peak atomic.Int64
	plan := planJSON(MaxTokens, 510, manyPlanWindows(40, 5)...)
	srv := planServer(t, plan, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inflight.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		defer inflight.Add(-1)
		time.Sleep(5 * time.Millisecond)
		fmt.Fprint(w, responseJSON(entityJSON("FULL_NAME", 0, 1, 0.9, "rubert")))
	}))
	c := newTestClient(t, srv, ModeFull)

	_, err := c.InferBounded(context.Background(), strings.Repeat("abcdefghij", 20), 0, limit)
	if err != nil {
		t.Fatalf("InferBounded() error = %v", err)
	}
	if got := peak.Load(); got > limit {
		t.Fatalf("peak concurrency = %d, want <= %d", got, limit)
	}
}

// TestInferBoundedCancellation proves that cancellation stops new work and
// returns without leaking goroutines.
func TestInferBoundedCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := planServer(t, planJSON(MaxTokens, 510, manyPlanWindows(40, 5)...), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		fmt.Fprint(w, responseJSON(entityJSON("FULL_NAME", 0, 1, 0.9, "rubert")))
	}))
	c := newTestClient(t, srv, ModeFull)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.InferBounded(ctx, strings.Repeat("abcdefghij", 20), 0, 2)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	close(release)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("InferBounded did not return after cancellation (goroutine leak)")
	}
}

// TestInferBoundedWindowErrorNoPartial proves that an error in one window fails
// the whole call closed with no partial result and no window text in the error.
func TestInferBoundedWindowErrorNoPartial(t *testing.T) {
	const marker = "WINDOW_MARKER_12345"
	var calls int32
	srv := planServer(t, planJSON(MaxTokens, 510, manyPlanWindows(38, 5)...), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 2 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, responseJSON(entityJSON("FULL_NAME", 0, 1, 0.9, "rubert")))
	}))
	c := newTestClient(t, srv, ModeFull)

	got, err := c.InferBounded(context.Background(), strings.Repeat(marker, 10), 0, 2)
	if err == nil {
		t.Fatal("InferBounded() expected error")
	}
	if got != nil {
		t.Fatalf("entities = %v, want nil on failure (fail closed)", got)
	}
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("InferBounded() error = %v, want ErrModelUnavailable", err)
	}
	if strings.Contains(err.Error(), marker) {
		t.Errorf("error leaks window text: %q", err.Error())
	}
}

// TestInferBoundedFastModeNoHTTP proves that fast mode makes no HTTP request
// and returns no candidates.
func TestInferBoundedFastModeNoHTTP(t *testing.T) {
	var calls int32
	srv := planServer(t, planJSON(1, 510, planWindowJSON(0, 5, 3)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	c := newTestClient(t, srv, ModeFast)

	got, err := c.InferBounded(context.Background(), "abcdefghij", 64, 2)
	if err != nil {
		t.Fatalf("InferBounded() error = %v", err)
	}
	if got != nil {
		t.Fatalf("entities = %v, want nil", got)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("made %d HTTP calls, want 0", calls)
	}
}

// TestInferBoundedEmptyText proves that empty text returns no entities without
// error.
func TestInferBoundedEmptyText(t *testing.T) {
	srv := planServer(t, planJSON(0, 510), nil)
	c := newTestClient(t, srv, ModeFull)

	got, err := c.InferBounded(context.Background(), "", 64, 2)
	if err != nil {
		t.Fatalf("InferBounded() error = %v", err)
	}
	if got != nil {
		t.Fatalf("entities = %v, want nil", got)
	}
}

// TestInferBoundedInvalidUTF8NoRequest proves that invalid UTF-8 input is
// rejected before any HTTP request.
func TestInferBoundedInvalidUTF8NoRequest(t *testing.T) {
	var calls int32
	srv := planServer(t, planJSON(1, 510, planWindowJSON(0, 5, 3)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	c := newTestClient(t, srv, ModeFull)

	_, err := c.InferBounded(context.Background(), string([]byte{0xff, 0xfe}), 64, 2)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("InferBounded() error = %v, want ErrInvalidInput", err)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("made %d HTTP calls, want 0", calls)
	}
}

// TestInferBoundedInvalidConfigNoRequest proves that invalid overlap_tokens or
// concurrency is rejected locally before any HTTP request, so the original text
// is never sent to the worker.
func TestInferBoundedInvalidConfigNoRequest(t *testing.T) {
	var calls int32
	srv := planServer(t, planJSON(1, 510, planWindowJSON(0, 5, 3)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	c := newTestClient(t, srv, ModeFull)

	tests := []struct {
		name        string
		overlap     int
		concurrency int
	}{
		{"negative overlap", -1, 2},
		{"zero concurrency", 64, 0},
		{"negative concurrency", 64, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.InferBounded(context.Background(), "abcdefghij", tt.overlap, tt.concurrency)
			if !errors.Is(err, ErrInvalidWindowConfig) {
				t.Fatalf("InferBounded() error = %v, want ErrInvalidWindowConfig", err)
			}
			if atomic.LoadInt32(&calls) != 0 {
				t.Fatalf("made %d HTTP calls, want 0", calls)
			}
		})
	}
}

// TestInferBoundedInvalidConfigFastModeNoRequest proves that invalid
// overlap_tokens or concurrency is rejected even in fast mode, before the
// fast-mode return, and without any HTTP request.
func TestInferBoundedInvalidConfigFastModeNoRequest(t *testing.T) {
	var calls int32
	srv := planServer(t, planJSON(1, 510, planWindowJSON(0, 5, 3)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	c := newTestClient(t, srv, ModeFast)

	_, err := c.InferBounded(context.Background(), "abcdefghij", -1, 2)
	if !errors.Is(err, ErrInvalidWindowConfig) {
		t.Fatalf("InferBounded() error = %v, want ErrInvalidWindowConfig", err)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("made %d HTTP calls, want 0", calls)
	}
}

// TestInferBoundedGlobalBackpressure proves that the shared per-Client limiter
// caps the aggregate number of concurrent /infer calls across multiple
// concurrent InferBounded requests at globalInferLimit, even when each request
// asks for a higher per-request concurrency.
func TestInferBoundedGlobalBackpressure(t *testing.T) {
	var inflight, peak atomic.Int64
	plan := planJSON(MaxTokens, 510, manyPlanWindows(40, 5)...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/plan_windows" {
			fmt.Fprint(w, plan)
			return
		}
		cur := inflight.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		defer inflight.Add(-1)
		time.Sleep(10 * time.Millisecond)
		fmt.Fprint(w, responseJSON(entityJSON("FULL_NAME", 0, 1, 0.9, "rubert")))
	}))
	defer srv.Close()
	c := newTestClient(t, srv, ModeFull)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.InferBounded(context.Background(), strings.Repeat("abcdefghij", 20), 0, 8)
		}()
	}
	wg.Wait()

	if got := peak.Load(); got > globalInferLimit {
		t.Fatalf("aggregate peak concurrency = %d, want <= %d", got, globalInferLimit)
	}
}

// TestInferBoundedCommonDeadline proves that the stored client timeout bounds
// the whole InferBounded (plan plus all windows), not each window separately.
// The plan responds immediately and every /infer blocks until its request
// context is done, so the whole call must return near the client timeout rather
// than after per-window timeouts.
func TestInferBoundedCommonDeadline(t *testing.T) {
	plan := planJSON(MaxTokens, 510, manyPlanWindows(20, 5)...)
	srv := planServer(t, plan, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	c, err := New(srv.URL, ModeFull, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	start := time.Now()
	_, err = c.InferBounded(context.Background(), strings.Repeat("abcdefghij", 10), 0, 4)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("InferBounded() error = %v, want ErrModelUnavailable", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("InferBounded() error = %v, want context.DeadlineExceeded", err)
	}
	// The whole operation must be bounded by the client timeout, not per-window
	// (which would be ~20 * 50ms).
	if elapsed > 500*time.Millisecond {
		t.Fatalf("InferBounded took %v, want bounded by ~50ms client timeout", elapsed)
	}
}

// TestInferBoundedCallerDeadlinePreserved proves that a caller deadline shorter
// than the client timeout is preserved and bounds the whole InferBounded.
func TestInferBoundedCallerDeadlinePreserved(t *testing.T) {
	plan := planJSON(MaxTokens, 510, manyPlanWindows(20, 5)...)
	srv := planServer(t, plan, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	c, err := New(srv.URL, ModeFull, 5*time.Second)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = c.InferBounded(ctx, strings.Repeat("abcdefghij", 10), 0, 4)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("InferBounded() error = %v, want ErrModelUnavailable", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("InferBounded() error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("InferBounded took %v, want bounded by ~50ms caller deadline", elapsed)
	}
}

// manyPlanWindows builds a plan body with n contiguous windows of size size
// covering the whole text, each with token_count 3.
func manyPlanWindows(n, size int) []string {
	windows := make([]string, 0, n)
	for i := 0; i < n; i++ {
		start := i * size
		end := start + size
		windows = append(windows, planWindowJSON(start, end, 3))
	}
	return windows
}
