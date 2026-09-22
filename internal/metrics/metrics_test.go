package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSnapshotEmpty(t *testing.T) {
	reg := NewRegistry(time.Minute)
	s := reg.Snapshot()
	if s.Requests != 0 {
		t.Errorf("Requests = %d, want 0", s.Requests)
	}
	if s.RPS != 0 || s.TPS != 0 {
		t.Errorf("RPS = %v, TPS = %v, want 0", s.RPS, s.TPS)
	}
	if s.LatencyAvg != 0 {
		t.Errorf("LatencyAvg = %v, want 0", s.LatencyAvg)
	}
}

func TestSnapshotAggregates(t *testing.T) {
	reg := NewRegistry(time.Minute)
	reg.Record(100*time.Millisecond, 10)
	reg.Record(200*time.Millisecond, 20)
	reg.Record(300*time.Millisecond, 30)

	s := reg.Snapshot()
	if s.Requests != 3 {
		t.Errorf("Requests = %d, want 3", s.Requests)
	}
	if s.Tokens != 60 {
		t.Errorf("Tokens = %d, want 60", s.Tokens)
	}
	if s.LatencyAvg != 200*time.Millisecond {
		t.Errorf("LatencyAvg = %v, want 200ms", s.LatencyAvg)
	}
	if s.LatencyP50 != 200*time.Millisecond {
		t.Errorf("LatencyP50 = %v, want 200ms", s.LatencyP50)
	}
	if s.LatencyP95 != 300*time.Millisecond {
		t.Errorf("LatencyP95 = %v, want 300ms", s.LatencyP95)
	}
	if s.LatencyP99 != 300*time.Millisecond {
		t.Errorf("LatencyP99 = %v, want 300ms", s.LatencyP99)
	}
	// RPS = 3 / 60s, TPS = 60 / 60s.
	if s.RPS != 0.05 {
		t.Errorf("RPS = %v, want 0.05", s.RPS)
	}
	if s.TPS != 1.0 {
		t.Errorf("TPS = %v, want 1.0", s.TPS)
	}
}

func TestSnapshotPrunesExpired(t *testing.T) {
	reg := NewRegistry(10 * time.Millisecond)
	reg.Record(1*time.Millisecond, 5)
	time.Sleep(20 * time.Millisecond)
	reg.Record(2*time.Millisecond, 7)

	s := reg.Snapshot()
	if s.Requests != 1 {
		t.Errorf("Requests = %d, want 1 (expired pruned)", s.Requests)
	}
	if s.Tokens != 7 {
		t.Errorf("Tokens = %d, want 7", s.Tokens)
	}
}

func TestCountTokens(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"", 0},
		{"   ", 0},
		{"один", 1},
		{"Клиент ТЕСТОВ ТЕСТ ТЕСТОВИЧ", 4},
		{"a\nb\tc", 3},
	}
	for _, tc := range cases {
		if got := CountTokens(tc.text); got != tc.want {
			t.Errorf("CountTokens(%q) = %d, want %d", tc.text, got, tc.want)
		}
	}
}

func TestHandlerExposesLatencyRPSAndTPS(t *testing.T) {
	reg := NewRegistry(time.Minute)
	reg.Record(100*time.Millisecond, 10)
	reg.Record(200*time.Millisecond, 20)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	reg.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"pii_latency_seconds",
		"pii_latency_p50_seconds",
		"pii_latency_p95_seconds",
		"pii_latency_p99_seconds",
		"pii_rps",
		"pii_tps",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q:\n%s", want, body)
		}
	}
}

func TestHandlerDoesNotLeakPlaintext(t *testing.T) {
	const marker = "SECRET_PLAINTEXT_MARKER_99999"
	reg := NewRegistry(time.Minute)
	reg.Record(100*time.Millisecond, 10)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	reg.Handler().ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), marker) {
		t.Errorf("metrics body leaks plaintext marker: %q", rec.Body.String())
	}
}
