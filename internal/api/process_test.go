package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/klrushka/llm-proxy/internal/process"
)

func TestProcessSuccessExactContract(t *testing.T) {
	h := ProcessFunc(func(_ context.Context, req process.Request) (process.Response, error) {
		return process.Response{Result: "masked:" + req.Payload}, nil
	})
	mux := NewRouter(nil, nil, WithProcess(h))
	rec := doJSONRequest(t, mux, http.MethodPost, "/process",
		`{"payload":"Клиент ТЕСТОВ","payload_id":"p1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	if len(raw) != 1 {
		t.Errorf("response has %d fields, want exactly 1: %v", len(raw), raw)
	}
	var resp process.Response
	decodeJSONResponse(t, rec, &resp)
	if resp.Result != "masked:Клиент ТЕСТОВ" {
		t.Errorf("result = %q", resp.Result)
	}
}

func TestProcessMissingPayload(t *testing.T) {
	called := false
	h := ProcessFunc(func(_ context.Context, _ process.Request) (process.Response, error) {
		called = true
		return process.Response{}, nil
	})
	mux := NewRouter(nil, nil, WithProcess(h))
	rec := doJSONRequest(t, mux, http.MethodPost, "/process", `{"payload_id":"p1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if called {
		t.Error("operation called for invalid request")
	}
}

func TestProcessMissingPayloadID(t *testing.T) {
	mux := NewRouter(nil, nil, WithProcess(ProcessFunc(func(_ context.Context, _ process.Request) (process.Response, error) {
		return process.Response{}, nil
	})))
	rec := doJSONRequest(t, mux, http.MethodPost, "/process", `{"payload":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestProcessWrongTypeFields(t *testing.T) {
	mux := NewRouter(nil, nil, WithProcess(ProcessFunc(func(_ context.Context, _ process.Request) (process.Response, error) {
		return process.Response{}, nil
	})))
	rec := doJSONRequest(t, mux, http.MethodPost, "/process", `{"payload":123,"payload_id":"p1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestProcessMalformedJSON(t *testing.T) {
	mux := NewRouter(nil, nil, WithProcess(ProcessFunc(func(_ context.Context, _ process.Request) (process.Response, error) {
		return process.Response{}, nil
	})))
	rec := doJSONRequest(t, mux, http.MethodPost, "/process", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestProcessNilOperationFailsClosed(t *testing.T) {
	mux := NewRouter(nil, nil)
	rec := doJSONRequest(t, mux, http.MethodPost, "/process", `{"payload":"x","payload_id":"p1"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestProcessOperationErrorIsSafe(t *testing.T) {
	h := ProcessFunc(func(_ context.Context, _ process.Request) (process.Response, error) {
		return process.Response{}, errors.New("secret internal detail")
	})
	mux := NewRouter(nil, nil, WithProcess(h))
	rec := doJSONRequest(t, mux, http.MethodPost, "/process", `{"payload":"x","payload_id":"p1"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if strings.Contains(rec.Body.String(), "secret internal detail") {
		t.Errorf("body leaks internal error: %q", rec.Body.String())
	}
}

func TestProcessWrongMethodReturns405(t *testing.T) {
	mux := NewRouter(nil, nil)
	rec := doJSONRequest(t, mux, http.MethodGet, "/process", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /process status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}
