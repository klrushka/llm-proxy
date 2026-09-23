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

// fixedRoute resolves every request to the given template.
func fixedRoute(tmpl string) func(*http.Request) string {
	return func(*http.Request) string { return tmpl }
}

func TestMiddlewareRecordsMethodRouteAndStatus(t *testing.T) {
	m := New(Options{})
	h := m.Middleware(fixedRoute("/v1/pii/scopes/{scope_id}"))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	req := httptest.NewRequest(http.MethodDelete, "/v1/pii/scopes/SCOPE_CANARY_4242", strings.NewReader("{}"))
	h.ServeHTTP(httptest.NewRecorder(), req)

	body := scrape(t, m)
	want := `http_server_request_duration_seconds_count{http_request_method="DELETE",http_response_status_code="403",http_route="/v1/pii/scopes/{scope_id}"} 1`
	if !strings.Contains(body, want) {
		t.Errorf("metrics body missing %q", want)
	}
	if !strings.Contains(body, `http_server_request_body_size_bytes_sum{http_request_method="DELETE",http_route="/v1/pii/scopes/{scope_id}"} 2`) {
		t.Errorf("metrics body missing request body size")
	}
	if strings.Contains(body, "SCOPE_CANARY_4242") {
		t.Errorf("metrics body leaks the actual path")
	}
}

func TestMiddlewareImplicitOKAndUnknownMethod(t *testing.T) {
	m := New(Options{})
	h := m.Middleware(fixedRoute(""))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("BREW", "/anything", nil))

	want := `http_server_request_duration_seconds_count{http_request_method="_OTHER",http_response_status_code="200",http_route=""} 1`
	if body := scrape(t, m); !strings.Contains(body, want) {
		t.Errorf("metrics body missing %q", want)
	}
}

func TestMiddlewareRecordsPanicAsServerError(t *testing.T) {
	m := New(Options{})
	h := m.Middleware(fixedRoute("/process"))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	func() {
		defer func() {
			if p := recover(); p != "boom" {
				t.Errorf("recovered %v, want original panic", p)
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/process", nil))
	}()

	body := scrape(t, m)
	if !strings.Contains(body, `http_server_request_duration_seconds_count{http_request_method="POST",http_response_status_code="500",http_route="/process"} 1`) {
		t.Errorf("panic not recorded as 500")
	}
	if !strings.Contains(body, `http_server_active_requests{http_request_method="POST"} 0`) {
		t.Errorf("active requests not released after panic")
	}
}

func TestNilMetricsMiddlewarePassesThrough(t *testing.T) {
	var m *Metrics
	called := false
	h := m.Middleware(fixedRoute("/x"))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if !called {
		t.Fatal("next handler was not called")
	}
}
