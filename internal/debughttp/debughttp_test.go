package debughttp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestMiddlewareRecordsBodies(t *testing.T) {
	var out bytes.Buffer
	logger := New(&out)
	h := logger.Middleware(func(*http.Request) string { return "/v1/pii/tokenize" }, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(append([]byte(`{"saved":`), append(body, '}')...))
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/pii/tokenize?secret=query", strings.NewReader(`{"text":"Анна"}`))
	req.Header.Set("Authorization", "Bearer secret")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusCreated {
		t.Fatalf("status = %d", res.Code)
	}
	var got event
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Route != "/v1/pii/tokenize" || got.RequestBody != `{"text":"Анна"}` || got.Status != http.StatusCreated {
		t.Fatalf("event = %+v", got)
	}
	if strings.Contains(out.String(), "secret") || strings.Contains(out.String(), "?secret") {
		t.Fatalf("event contains request metadata: %s", out.String())
	}
}

func TestMiddlewareSkipsNonDataRoute(t *testing.T) {
	var out bytes.Buffer
	h := New(&out).Middleware(func(*http.Request) string { return "/health/live" }, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if out.Len() != 0 {
		t.Fatalf("unexpected log: %s", out.String())
	}
}

func TestMiddlewareRecordsErrorResponse(t *testing.T) {
	var out bytes.Buffer
	h := New(&out).Middleware(func(*http.Request) string { return "/process" }, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid"}`))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/process", strings.NewReader(`{"payload":"bad"}`)))
	var got event
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != http.StatusBadRequest || got.ResponseBody != `{"error":"invalid"}` {
		t.Fatalf("event = %+v", got)
	}
}

func TestLoggerConcurrentWritesDoNotInterleave(t *testing.T) {
	var out bytes.Buffer
	logger := New(&out)
	const count = 32
	var wg sync.WaitGroup
	wg.Add(count)
	for i := 0; i < count; i++ {
		go func() {
			defer wg.Done()
			if err := logger.log(event{Method: http.MethodPost, Route: "/process", Status: http.StatusOK}); err != nil {
				t.Errorf("log() error = %v", err)
			}
		}()
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != count {
		t.Fatalf("lines = %d, want %d", len(lines), count)
	}
	for _, line := range lines {
		var got event
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("invalid line %q: %v", line, err)
		}
	}
}

func TestOpenCreatesSecureAppendFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "debug.jsonl")
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("first\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("second\n"); err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, err = %v", info.Mode(), err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(data), "first\n") {
		t.Fatalf("data = %q, err = %v", data, err)
	}
}

func TestOpenRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	path := filepath.Join(dir, "debug.jsonl")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open() succeeded for symlink")
	}
}
