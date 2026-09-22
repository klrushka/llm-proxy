package loadbench

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientDoSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":"masked"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	got, err := c.Do(context.Background(), Request{Payload: "x", PayloadID: "p1"})
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if got != "masked" {
		t.Errorf("result = %q, want %q", got, "masked")
	}
}

func TestClientDoRejectsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	if _, err := c.Do(context.Background(), Request{Payload: "x", PayloadID: "p1"}); err == nil {
		t.Fatal("Do() expected error for non-200 status")
	}
}

func TestClientDoRejectsExtraFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"masked","extra":"x"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	if _, err := c.Do(context.Background(), Request{Payload: "x", PayloadID: "p1"}); err == nil {
		t.Fatal("Do() expected error for response with extra fields")
	}
}

func TestClientDoRejectsMissingResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	if _, err := c.Do(context.Background(), Request{Payload: "x", PayloadID: "p1"}); err == nil {
		t.Fatal("Do() expected error for response missing result")
	}
}

func TestClientDoRejectsNonStringResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":123}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	if _, err := c.Do(context.Background(), Request{Payload: "x", PayloadID: "p1"}); err == nil {
		t.Fatal("Do() expected error for non-string result")
	}
}

func TestClientDoRejectsInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	if _, err := c.Do(context.Background(), Request{Payload: "x", PayloadID: "p1"}); err == nil {
		t.Fatal("Do() expected error for invalid JSON")
	}
}

func TestClientDoRejectsSecondJSONObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"masked"} {"result":"other"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	if _, err := c.Do(context.Background(), Request{Payload: "x", PayloadID: "p1"}); err == nil {
		t.Fatal("Do() expected error for a second JSON object")
	}
}

func TestClientDoRejectsTrailingGarbage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"masked"} garbage`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	if _, err := c.Do(context.Background(), Request{Payload: "x", PayloadID: "p1"}); err == nil {
		t.Fatal("Do() expected error for trailing non-whitespace content")
	}
}

func TestClientDoAcceptsTrailingWhitespace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"result\":\"masked\"}   \n\t"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	got, err := c.Do(context.Background(), Request{Payload: "x", PayloadID: "p1"})
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if got != "masked" {
		t.Errorf("result = %q, want %q", got, "masked")
	}
}

func TestClientDoErrorDoesNotLeakPayload(t *testing.T) {
	const payload = "synthetic-secret-payload"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	_, err := c.Do(context.Background(), Request{Payload: payload, PayloadID: "p1"})
	if err == nil {
		t.Fatal("Do() expected error")
	}
	if strings.Contains(err.Error(), payload) {
		t.Errorf("error leaks payload: %v", err)
	}
}

func TestClientBaseURLTrailingSlash(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"masked"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/", time.Second)
	if _, err := c.Do(context.Background(), Request{Payload: "x", PayloadID: "p1"}); err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if gotPath != "/process" {
		t.Errorf("path = %q, want /process", gotPath)
	}
}
