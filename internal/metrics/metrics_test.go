package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/klrushka/llm-proxy/internal/audit"
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

func TestInstrumentTransportRecordsStatusAndOperation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	m := New(Options{})
	client := &http.Client{Transport: m.InstrumentTransport("model_worker", func(*http.Request) string { return "infer" }, nil)}
	resp, err := client.Post(srv.URL+"/infer?text=PII_CANARY_555", "application/json", strings.NewReader(`{"text":"PII_CANARY_555"}`))
	if err != nil {
		t.Fatalf("Post() error = %v", err)
	}
	resp.Body.Close()

	body := scrape(t, m)
	want := `http_client_request_duration_seconds_count{error_type="",http_request_method="POST",http_response_status_code="429",operation="infer",server="model_worker"} 1`
	if !strings.Contains(body, want) {
		t.Errorf("metrics body missing %q", want)
	}
	if strings.Contains(body, "PII_CANARY_555") || strings.Contains(body, srv.URL) {
		t.Errorf("metrics body leaks request data")
	}
}

func TestInstrumentTransportClassifiesTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)

	m := New(Options{})
	client := &http.Client{
		Timeout:   50 * time.Millisecond,
		Transport: m.InstrumentTransport("llm", func(*http.Request) string { return "chat" }, nil),
	}
	if _, err := client.Get(srv.URL); err == nil {
		t.Fatal("Get() error = nil, want timeout")
	}

	// http.Client returns on its timeout before the transport unwinds, so the
	// observation may land shortly after Get returns.
	want := `http_client_request_duration_seconds_count{error_type="timeout",http_request_method="GET",http_response_status_code="",operation="chat",server="llm"} 1`
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(scrape(t, m), want) {
		if time.Now().After(deadline) {
			t.Fatalf("metrics body missing %q; got:\n%s", want, grepLines(scrape(t, m), "http_client_request_duration_seconds_count"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestInstrumentTransportClassifiesConnectionFailure(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	m := New(Options{})
	client := &http.Client{Transport: m.InstrumentTransport("model_worker", func(*http.Request) string { return "infer" }, nil)}
	if _, err := client.Get(url); err == nil {
		t.Fatal("Get() error = nil, want connection failure")
	}
	if body := scrape(t, m); !strings.Contains(body, `error_type="transport"`) {
		t.Errorf("connection failure not classified as transport")
	}
}

func TestNilMetricsInstrumentTransportReturnsNext(t *testing.T) {
	var m *Metrics
	if rt := m.InstrumentTransport("x", nil, nil); rt != http.DefaultTransport {
		t.Errorf("InstrumentTransport() = %T, want http.DefaultTransport", rt)
	}
}

func TestMiddlewareCountsConfirmedEntitiesByCanonicalType(t *testing.T) {
	m := New(Options{EntityTypes: []string{"EMAIL", "PHONE"}})
	h := m.Middleware(fixedRoute("/v1/pii/detect"))(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		col, ok := audit.CollectorFromContext(r.Context())
		if !ok {
			t.Fatal("no collector in request context")
		}
		col.AddEntity(audit.Entity{Type: "EMAIL", Personal: true})
		col.AddEntity(audit.Entity{Type: "EMAIL", Personal: true})
		col.AddEntity(audit.Entity{Type: "EMAIL", Personal: false})
		col.AddEntity(audit.Entity{Type: "PII_CANARY_TYPE", Personal: true})
		col.AddInputTokens(7)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/pii/detect", nil))

	body := scrape(t, m)
	for _, want := range []string{
		`pii_entities_detected_total{type="EMAIL"} 2`,
		`pii_entities_detected_total{type="PHONE"} 0`,
		`pii_entities_detected_total{type="other"} 1`,
		`pii_input_tokens_total 7`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q", want)
		}
	}
	if strings.Contains(body, "PII_CANARY_TYPE") {
		t.Errorf("non-canonical type leaked into labels")
	}
}

func TestMiddlewareReusesAuditCollector(t *testing.T) {
	m := New(Options{EntityTypes: []string{"EMAIL"}})
	outer := audit.NewCollector()
	h := m.Middleware(fixedRoute("/process"))(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		col, _ := audit.CollectorFromContext(r.Context())
		if col != outer {
			t.Error("middleware replaced the audit collector")
		}
		col.AddEntity(audit.Entity{Type: "EMAIL", Personal: true})
	}))
	req := httptest.NewRequest(http.MethodPost, "/process", nil)
	h.ServeHTTP(httptest.NewRecorder(), req.WithContext(audit.WithCollector(req.Context(), outer)))

	if body := scrape(t, m); !strings.Contains(body, `pii_entities_detected_total{type="EMAIL"} 1`) {
		t.Errorf("entity from audit collector not counted")
	}
}

func TestRegisterVaultMappings(t *testing.T) {
	m := New(Options{})
	n := 3
	m.RegisterVaultMappings(func() int { return n })
	if body := scrape(t, m); !strings.Contains(body, "pii_vault_mappings 3") {
		t.Errorf("metrics body missing pii_vault_mappings 3")
	}
	var nilMetrics *Metrics
	nilMetrics.RegisterVaultMappings(func() int { return 1 })
}

// grepLines returns the lines of s that start with prefix.
func grepLines(s, prefix string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
