package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/klrushka/llm-proxy/internal/api"
	"github.com/klrushka/llm-proxy/internal/process"
)

func TestQueuedAdmissionBoundsBytesAndDeadlineBeforeHandler(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	called := make(chan struct{}, 2)
	h := newQueuedAdmissionMiddleware(1, 2, 5, 40*time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- struct{}{}
		if r.URL.Path == "/hold" {
			close(entered)
			<-release
		}
		w.WriteHeader(http.StatusOK)
	}))
	activeDone := make(chan struct{})
	go func() {
		defer close(activeDone)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/hold", nil))
	}()
	<-entered
	queuedDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/process", strings.NewReader("12345")))
		queuedDone <- rec
	}()
	// The first queued body reserves the entire byte budget. A second body
	// must fail before it can reach the handler or create a claim.
	m := h.(*admissionMiddleware)
	deadline := time.Now().Add(time.Second)
	for len(m.waiters) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("request never entered bounded queue")
		}
		time.Sleep(time.Millisecond)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/process", strings.NewReader("x")))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" || strings.Contains(rec.Body.String(), "12345") {
		t.Fatalf("byte-bound rejection = %d %q", rec.Code, rec.Body.String())
	}
	select {
	case q := <-queuedDone:
		if q.Code != http.StatusTooManyRequests {
			t.Fatalf("expired queued status = %d, want 429", q.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("queued request outlived deadline")
	}
	if len(called) != 1 {
		t.Fatalf("handler calls = %d, want active request only", len(called))
	}
	close(release)
	<-activeDone
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/process", strings.NewReader("x")))
	if rec.Code != http.StatusOK {
		t.Fatalf("permit not reused: status %d", rec.Code)
	}
}

func TestCancelledQueueDoesNotCreateProcessClaim(t *testing.T) {
	store := process.NewStore()
	entered := make(chan struct{})
	release := make(chan struct{})
	op := process.NewOperation(store, func(_ context.Context, payload string) (string, error) {
		if payload == "first synthetic" {
			close(entered)
			<-release
		}
		return "masked synthetic", nil
	})
	h := newQueuedAdmissionMiddleware(1, 1, 1000, time.Second)(api.NewRouter(nil, api.WithProcess(op.Handle)))
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/process", strings.NewReader(`{"payload":"first synthetic","payload_id":"first"}`)))
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/process", strings.NewReader(`{"payload":"second synthetic","payload_id":"second"}`)).WithContext(ctx)
	queuedDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		queuedDone <- rec
	}()
	m := h.(*admissionMiddleware)
	deadline := time.Now().Add(time.Second)
	for len(m.waiters) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("request never entered queue")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if rec := <-queuedDone; rec.Code != http.StatusTooManyRequests {
		t.Fatalf("cancelled queued status = %d, want 429", rec.Code)
	}
	if _, err := store.Get("second"); !errors.Is(err, process.ErrNotFound) {
		t.Fatalf("cancelled request created claim: %v", err)
	}
	close(release)
	<-firstDone
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/process", strings.NewReader(`{"payload":"second synthetic","payload_id":"second"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("late retry status = %d, want 200", rec.Code)
	}
}
