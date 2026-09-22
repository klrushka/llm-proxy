package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klrushka/llm-proxy/internal/metrics"
	"github.com/klrushka/llm-proxy/internal/process"
)

func doRequest(t *testing.T, mux *http.ServeMux, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestLiveStatus(t *testing.T) {
	mux := NewRouter(nil, nil)
	rec := doRequest(t, mux, http.MethodGet, "/health/live")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestLiveContentType(t *testing.T) {
	mux := NewRouter(nil, nil)
	rec := doRequest(t, mux, http.MethodGet, "/health/live")
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestLiveBody(t *testing.T) {
	mux := NewRouter(nil, nil)
	rec := doRequest(t, mux, http.MethodGet, "/health/live")
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want ok", body["status"])
	}
}

func TestReadySuccess(t *testing.T) {
	mux := NewRouter(func() error { return nil }, nil)
	rec := doRequest(t, mux, http.MethodGet, "/health/ready")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestReadyFailure(t *testing.T) {
	mux := NewRouter(func() error { return errors.New("db down") }, nil)
	rec := doRequest(t, mux, http.MethodGet, "/health/ready")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestReadyNilProbeFailsClosed(t *testing.T) {
	mux := NewRouter(nil, nil)
	rec := doRequest(t, mux, http.MethodGet, "/health/ready")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestReadyErrorTextNotLeaked(t *testing.T) {
	mux := NewRouter(func() error { return errors.New("secret internal detail") }, nil)
	rec := doRequest(t, mux, http.MethodGet, "/health/ready")
	if strings.Contains(rec.Body.String(), "secret internal detail") {
		t.Errorf("body leaks internal error text: %q", rec.Body.String())
	}
}

func TestMetricsHandlerCalled(t *testing.T) {
	called := false
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux := NewRouter(nil, metrics)
	rec := doRequest(t, mux, http.MethodGet, "/metrics")
	if !called {
		t.Error("metrics handler was not called")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("body = %q, want ok", rec.Body.String())
	}
}

func TestMetricsNilReturns503(t *testing.T) {
	mux := NewRouter(nil, nil)
	rec := doRequest(t, mux, http.MethodGet, "/metrics")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestWrongMethodReturns405(t *testing.T) {
	mux := NewRouter(nil, nil)
	for _, path := range []string{"/health/live", "/health/ready", "/metrics"} {
		rec := doRequest(t, mux, http.MethodPost, path)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s status = %d, want %d", path, rec.Code, http.StatusMethodNotAllowed)
		}
	}
}

func TestRouterIsServeMux(t *testing.T) {
	mux := NewRouter(nil, nil)
	if _, ok := any(mux).(*http.ServeMux); !ok {
		t.Fatalf("NewRouter() = %T, want *http.ServeMux", mux)
	}
}

func TestMetricsRegistryServesLatencyRPSAndTPS(t *testing.T) {
	reg := metrics.NewRegistry(0)
	reg.Record(100_000_000, 10)
	reg.Record(200_000_000, 20)
	mux := NewRouter(nil, nil, WithMetrics(reg))

	rec := doRequest(t, mux, http.MethodGet, "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{"pii_latency_seconds", "pii_rps", "pii_tps"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q:\n%s", want, body)
		}
	}
}

func TestMetricsRegistryTakesPrecedenceOverPositionalHandler(t *testing.T) {
	reg := metrics.NewRegistry(0)
	reg.Record(100_000_000, 10)
	positional := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	mux := NewRouter(nil, positional, WithMetrics(reg))

	rec := doRequest(t, mux, http.MethodGet, "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (registry handler must win)", rec.Code, http.StatusOK)
	}
}

func TestProcessRecordsMetricsObservation(t *testing.T) {
	reg := metrics.NewRegistry(0)
	h := ProcessFunc(func(_ context.Context, req process.Request) (process.Response, error) {
		return process.Response{Result: "masked:" + req.Payload}, nil
	})
	mux := NewRouter(nil, nil, WithProcess(h), WithMetrics(reg))

	rec := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"Клиент ТЕСТОВ ТЕСТ","payload_id":"p1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	s := reg.Snapshot()
	if s.Requests != 1 {
		t.Errorf("Requests = %d, want 1", s.Requests)
	}
	if s.Tokens != 3 {
		t.Errorf("Tokens = %d, want 3 (three whitespace-separated tokens)", s.Tokens)
	}
}

func TestProcessWithoutMetricsDoesNotRecord(t *testing.T) {
	reg := metrics.NewRegistry(0)
	h := ProcessFunc(func(_ context.Context, req process.Request) (process.Response, error) {
		return process.Response{Result: "masked:" + req.Payload}, nil
	})
	mux := NewRouter(nil, nil, WithProcess(h))

	rec := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"Клиент ТЕСТОВ","payload_id":"p1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if s := reg.Snapshot(); s.Requests != 0 {
		t.Errorf("Requests = %d, want 0 (registry not wired)", s.Requests)
	}
}
