package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scrape returns the /metrics body served by m.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

func TestHandlerExposesRuntimeProcessAndBuildInfo(t *testing.T) {
	body := scrape(t, New(Options{Version: "1.2.3", ModelMode: "fast"}))
	for _, want := range []string{
		"go_goroutines",
		"go_memstats_heap_alloc_bytes",
		`pii_build_info{model_mode="fast",version="1.2.3"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q", want)
		}
	}
}

func TestHandlerDropsLegacyWindowGauges(t *testing.T) {
	body := scrape(t, New(Options{}))
	for _, legacy := range []string{"pii_latency_seconds", "pii_rps", "pii_tps"} {
		if strings.Contains(body, legacy+" ") {
			t.Errorf("legacy gauge %q is still exposed", legacy)
		}
	}
}

func TestNilMetricsHandlerFailsClosed(t *testing.T) {
	var m *Metrics
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
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
