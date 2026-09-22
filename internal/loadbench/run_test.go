package loadbench

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProcessServer implements a minimal POST /process state machine for tests:
// a new payload_id masks the original (deterministically), a retry of the
// original returns the stored mask, and passing the stored mask restores the
// original. It mirrors the real adapter contract so the load run can exercise
// both the mask and restore paths.
type fakeProcessServer struct {
	mu      sync.Mutex
	records map[string]fakeRecord
}

type fakeRecord struct {
	original string
	mask     string
}

func newFakeProcessServer() *fakeProcessServer {
	return &fakeProcessServer{records: make(map[string]fakeRecord)}
}

func (s *fakeProcessServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		rec, ok := s.records[req.PayloadID]
		if !ok {
			mask := "masked-" + req.PayloadID
			s.records[req.PayloadID] = fakeRecord{original: req.Payload, mask: mask}
			writeResult(w, mask)
			return
		}
		switch req.Payload {
		case rec.original:
			writeResult(w, rec.mask)
		case rec.mask:
			writeResult(w, rec.original)
		default:
			w.WriteHeader(http.StatusConflict)
		}
	})
}

func writeResult(w http.ResponseWriter, result string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"result": result})
}

// TestRunEndToEnd exercises a full load run against an httptest server. It uses
// a short duration and low RPS so the test is fast and deterministic; it avoids
// flaky wall-clock assertions by checking the report invariants rather than
// exact timing.
func TestRunEndToEnd(t *testing.T) {
	srv := httptest.NewServer(newFakeProcessServer().handler())
	defer srv.Close()

	cfg := Config{
		BaseURL:       srv.URL,
		RPS:           50,
		Duration:      200 * time.Millisecond,
		PoolSize:      8,
		Concurrency:   16,
		Timeout:       time.Second,
		TargetLatency: time.Second,
	}
	client := NewClient(cfg.BaseURL, cfg.Timeout)
	pool := NewPool(cfg.PoolSize)

	report, err := Run(context.Background(), cfg, client, pool)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if report.Scheduled == 0 {
		t.Error("Scheduled = 0, want > 0")
	}
	if report.Planned != report.Scheduled {
		t.Errorf("Planned = %d, want %d (all planned slots scheduled)", report.Planned, report.Scheduled)
	}
	if report.Unschedulable != 0 {
		t.Errorf("Unschedulable = %d, want 0", report.Unschedulable)
	}
	if report.Completed != report.Scheduled {
		t.Errorf("Completed = %d, want %d (no errors expected)", report.Completed, report.Scheduled)
	}
	if report.Errors != 0 {
		t.Errorf("Errors = %d, want 0", report.Errors)
	}
	if report.RequestedRPS != 50 {
		t.Errorf("RequestedRPS = %v, want 50", report.RequestedRPS)
	}
	if report.Duration <= 0 {
		t.Errorf("Duration = %v, want > 0", report.Duration)
	}
	if !report.TargetIsAppendix {
		t.Error("TargetIsAppendix = false, want true")
	}
}

// TestRunReportsErrorsOnServerFailure proves that a server returning errors
// causes Run to return a non-nil error and a report with a non-zero error count.
// The server succeeds on prewarm (mask requests) but fails on restore requests,
// so the timed run observes errors while prewarm completes.
func TestRunReportsErrorsOnServerFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if strings.HasPrefix(req.Payload, "masked-") {
			// Restore request: fail closed.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// Mask request: succeed (covers prewarm and mask-path timed requests).
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"result": "masked-" + req.PayloadID})
	}))
	defer srv.Close()

	cfg := Config{
		BaseURL:       srv.URL,
		RPS:           50,
		Duration:      100 * time.Millisecond,
		PoolSize:      4,
		Concurrency:   8,
		Timeout:       time.Second,
		TargetLatency: time.Second,
	}
	client := NewClient(cfg.BaseURL, cfg.Timeout)
	pool := NewPool(cfg.PoolSize)

	report, err := Run(context.Background(), cfg, client, pool)
	if err == nil {
		t.Fatal("Run() expected error on server failure")
	}
	if report.Errors == 0 {
		t.Error("Errors = 0, want > 0")
	}
}

// TestRunRejectsInvalidConfig proves that an invalid configuration fails before
// any request is sent.
func TestRunRejectsInvalidConfig(t *testing.T) {
	cfg := Config{RPS: 0}
	client := NewClient("http://127.0.0.1:1", time.Second)
	pool := NewPool(1)
	if _, err := Run(context.Background(), cfg, client, pool); err == nil {
		t.Fatal("Run() expected error for invalid config")
	}
}

// TestRunCancellationReturnsPartialReport proves that cancelling the run
// context stops scheduling, waits for launched requests to finish, and returns
// a partial report together with the cancellation error rather than returning
// while worker goroutines still mutate run state.
func TestRunCancellationReturnsPartialReport(t *testing.T) {
	srv := httptest.NewServer(newFakeProcessServer().handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := Config{
		BaseURL:       srv.URL,
		RPS:           1000,
		Duration:      10 * time.Second,
		PoolSize:      8,
		Concurrency:   16,
		Timeout:       time.Second,
		TargetLatency: time.Second,
	}
	client := NewClient(cfg.BaseURL, cfg.Timeout)
	pool := NewPool(cfg.PoolSize)

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	report, err := Run(ctx, cfg, client, pool)
	if err == nil {
		t.Fatal("Run() expected cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if report.Scheduled == 0 {
		t.Error("Scheduled = 0, want > 0 (partial report)")
	}
}

// TestRunReportsUnschedulable proves that when concurrency is too low to keep
// up with the planned rate, Run returns a non-nil error and the report records
// the unschedulable slots so the JSON explains the failure. A slow server makes
// each request take longer than the 1ms slot interval, so with concurrency 1
// most planned slots cannot launch.
func TestRunReportsUnschedulable(t *testing.T) {
	h := newFakeProcessServer().handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()

	cfg := Config{
		BaseURL:       srv.URL,
		RPS:           1000,
		Duration:      100 * time.Millisecond,
		PoolSize:      8,
		Concurrency:   1,
		Timeout:       time.Second,
		TargetLatency: time.Second,
	}
	client := NewClient(cfg.BaseURL, cfg.Timeout)
	pool := NewPool(cfg.PoolSize)

	report, err := Run(context.Background(), cfg, client, pool)
	if err == nil {
		t.Fatal("Run() expected error when slots are unschedulable")
	}
	if report.Unschedulable == 0 {
		t.Error("Unschedulable = 0, want > 0")
	}
	if report.Planned != report.Scheduled+report.Unschedulable {
		t.Errorf("planned %d != scheduled %d + unschedulable %d", report.Planned, report.Scheduled, report.Unschedulable)
	}
}
