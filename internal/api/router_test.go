package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func doRequest(t *testing.T, mux *http.ServeMux, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestLiveStatus(t *testing.T) {
	mux := NewRouter(nil)
	rec := doRequest(t, mux, http.MethodGet, "/health/live")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestLiveContentType(t *testing.T) {
	mux := NewRouter(nil)
	rec := doRequest(t, mux, http.MethodGet, "/health/live")
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestLiveBody(t *testing.T) {
	mux := NewRouter(nil)
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
	mux := NewRouter(func() error { return nil })
	rec := doRequest(t, mux, http.MethodGet, "/health/ready")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestReadyFailure(t *testing.T) {
	mux := NewRouter(func() error { return errors.New("db down") })
	rec := doRequest(t, mux, http.MethodGet, "/health/ready")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestReadyNilProbeFailsClosed(t *testing.T) {
	mux := NewRouter(nil)
	rec := doRequest(t, mux, http.MethodGet, "/health/ready")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestReadyErrorTextNotLeaked(t *testing.T) {
	mux := NewRouter(func() error { return errors.New("secret internal detail") })
	rec := doRequest(t, mux, http.MethodGet, "/health/ready")
	if strings.Contains(rec.Body.String(), "secret internal detail") {
		t.Errorf("body leaks internal error text: %q", rec.Body.String())
	}
}

func TestAPIRouterDoesNotServeMetrics(t *testing.T) {
	mux := NewRouter(nil)
	rec := doRequest(t, mux, http.MethodGet, "/metrics")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (metrics live on the internal listener)", rec.Code, http.StatusNotFound)
	}
}

func TestWrongMethodReturns405(t *testing.T) {
	mux := NewRouter(nil)
	for _, path := range []string{"/health/live", "/health/ready"} {
		rec := doRequest(t, mux, http.MethodPost, path)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s status = %d, want %d", path, rec.Code, http.StatusMethodNotAllowed)
		}
	}
}

func TestRouterIsServeMux(t *testing.T) {
	mux := NewRouter(nil)
	if _, ok := any(mux).(*http.ServeMux); !ok {
		t.Fatalf("NewRouter() = %T, want *http.ServeMux", mux)
	}
}
