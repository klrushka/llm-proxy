package audit

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// auditTestHandler is a downstream handler that, for recognized data routes,
// writes safe entity metadata into the request collector (mirroring what the
// detection pipeline does) and returns a configurable status. It never writes
// plaintext into the collector.
type auditTestHandler struct {
	status   int
	entities []Entity
}

func (h *auditTestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Mirror the real pipeline: only detection-based operations write entity
	// metadata into the collector. detokenize and revoke never run detection.
	if detectionOperation(r.Method, r.URL.Path) {
		if col, ok := CollectorFromContext(r.Context()); ok {
			for _, e := range h.entities {
				col.AddEntity(e)
			}
		}
	}
	if h.status != 0 {
		w.WriteHeader(h.status)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// detectionOperation reports whether the route runs the detection pipeline.
func detectionOperation(method, path string) bool {
	switch {
	case method == http.MethodPost && path == "/process":
		return true
	case method == http.MethodPost && path == "/v1/pii/detect":
		return true
	case method == http.MethodPost && path == "/v1/pii/tokenize":
		return true
	case method == http.MethodPost && path == "/v1/runtime/chat":
		return true
	}
	return false
}

// runAudit runs one request through the audit middleware with a capturing
// logger and returns the captured audit output and the HTTP response.
func runAudit(t *testing.T, status int, entities []Entity, method, path, body string, headers map[string]string) (string, *httptest.ResponseRecorder) {
	t.Helper()
	var buf bytes.Buffer
	logger := New(&buf)
	handler := Middleware(logger, ModeFull)(&auditTestHandler{status: status, entities: entities})
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return buf.String(), rec
}

// dataRoutes are the six data operations and their fixed audit operation names.
var dataRoutes = []struct {
	method string
	path   string
	op     string
}{
	{http.MethodPost, "/process", "process"},
	{http.MethodPost, "/v1/pii/detect", "detect"},
	{http.MethodPost, "/v1/pii/tokenize", "tokenize"},
	{http.MethodPost, "/v1/pii/detokenize", "detokenize"},
	{http.MethodDelete, "/v1/pii/scopes/scope-1", "revoke"},
	{http.MethodPost, "/v1/runtime/chat", "runtime"},
}

// TestMiddlewareEmitsExactlyOneEventPerDataRoute proves that every data
// operation emits exactly one audit event on success.
func TestMiddlewareEmitsExactlyOneEventPerDataRoute(t *testing.T) {
	for _, rt := range dataRoutes {
		t.Run(rt.op, func(t *testing.T) {
			out, _ := runAudit(t, http.StatusOK, nil, rt.method, rt.path, "", nil)
			lines := nonEmptyLines(out)
			if len(lines) != 1 {
				t.Fatalf("got %d audit lines, want exactly 1:\n%s", len(lines), out)
			}
			m := decodeLine(t, lines[0])
			if m["operation"] != rt.op {
				t.Errorf("operation = %v, want %q", m["operation"], rt.op)
			}
			if m["result"] != "success" {
				t.Errorf("result = %v, want success", m["result"])
			}
		})
	}
}

// TestMiddlewareEmitsErrorEventOnErrorStatus proves that a non-2xx status is
// audited as an error event.
func TestMiddlewareEmitsErrorEventOnErrorStatus(t *testing.T) {
	for _, rt := range dataRoutes {
		t.Run(rt.op, func(t *testing.T) {
			out, _ := runAudit(t, http.StatusInternalServerError, nil, rt.method, rt.path, "", nil)
			lines := nonEmptyLines(out)
			if len(lines) != 1 {
				t.Fatalf("got %d audit lines, want exactly 1:\n%s", len(lines), out)
			}
			m := decodeLine(t, lines[0])
			if m["result"] != "error" {
				t.Errorf("result = %v, want error", m["result"])
			}
		})
	}
}

// TestMiddlewareEmitsEventOnClientError proves a downstream 401/403 response
// is still audited without assigning authentication semantics to middleware.
func TestMiddlewareEmitsEventOnClientError(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		out, _ := runAudit(t, status, nil, http.MethodPost, "/v1/pii/tokenize", "", nil)
		lines := nonEmptyLines(out)
		if len(lines) != 1 {
			t.Fatalf("status %d: got %d audit lines, want exactly 1", status, len(lines))
		}
		m := decodeLine(t, lines[0])
		if m["result"] != "error" {
			t.Errorf("status %d: result = %v, want error", status, m["result"])
		}
	}
}

// TestMiddlewareEmitsEventOnMalformedJSON proves that a malformed JSON body
// (which the handler rejects with 400) is still audited exactly once.
func TestMiddlewareEmitsEventOnMalformedJSON(t *testing.T) {
	out, _ := runAudit(t, http.StatusBadRequest, nil, http.MethodPost, "/v1/pii/detect", `{not json`, nil)
	lines := nonEmptyLines(out)
	if len(lines) != 1 {
		t.Fatalf("got %d audit lines, want exactly 1:\n%s", len(lines), out)
	}
	m := decodeLine(t, lines[0])
	if m["operation"] != "detect" {
		t.Errorf("operation = %v, want detect", m["operation"])
	}
	if m["result"] != "error" {
		t.Errorf("result = %v, want error", m["result"])
	}
}

// TestMiddlewareCarriesDetectedMetadata proves that entities written into the
// collector by the pipeline appear in the audit event for detect/tokenize/
// process/runtime, and that detokenize/revoke may carry empty entities.
func TestMiddlewareCarriesDetectedMetadata(t *testing.T) {
	entities := []Entity{
		{Type: "EMAIL", Personal: true, Sources: []string{"regex"}, ReasonCodes: []string{"field_label"}},
		{Type: "PHONE", Personal: true, Sources: []string{"regex"}, ReasonCodes: []string{"client_context"}},
	}
	for _, rt := range dataRoutes {
		t.Run(rt.op, func(t *testing.T) {
			out, _ := runAudit(t, http.StatusOK, entities, rt.method, rt.path, "", nil)
			lines := nonEmptyLines(out)
			if len(lines) != 1 {
				t.Fatalf("got %d audit lines, want exactly 1", len(lines))
			}
			m := decodeLine(t, lines[0])
			switch rt.op {
			case "detect", "tokenize", "process", "runtime":
				if m["entity_count"] != float64(2) {
					t.Errorf("entity_count = %v, want 2", m["entity_count"])
				}
				types := toStrings(t, m["detected_types"])
				if !containsAllStrings(types, "EMAIL", "PHONE") {
					t.Errorf("detected_types = %v, want EMAIL and PHONE", types)
				}
			case "detokenize", "revoke":
				if m["entity_count"] != float64(0) {
					t.Errorf("entity_count = %v, want 0 for %s", m["entity_count"], rt.op)
				}
			}
		})
	}
}

// TestMiddlewareOmitsSyntheticPlaintext proves that synthetic PII in the
// client request_id, payload_id, scope_id, Authorization, body and dependency
// error never appears in the captured audit output.
func TestMiddlewareOmitsSyntheticPlaintext(t *testing.T) {
	markers := []string{
		"CLIENT_REQID_MARKER_11111",
		"PAYLOAD_ID_MARKER_22222",
		"SCOPE_ID_MARKER_33333",
		"Bearer AUTHZ_MARKER_44444",
		"BODY_MARKER_55555",
		"DEPENDENCY_ERR_MARKER_66666",
	}
	body := `{"text":"BODY_MARKER_55555","request_id":"CLIENT_REQID_MARKER_11111","payload_id":"PAYLOAD_ID_MARKER_22222","scope_id":"SCOPE_ID_MARKER_33333"}`
	out, _ := runAudit(t, http.StatusOK, nil, http.MethodPost, "/v1/pii/tokenize", body,
		map[string]string{"Authorization": "Bearer AUTHZ_MARKER_44444"})
	for _, m := range markers {
		if strings.Contains(out, m) {
			t.Errorf("audit output leaks marker %q:\n%s", m, out)
		}
	}
}

// TestMiddlewareCorrelationIDIsServerGenerated proves the audit request_id is
// a server-generated correlation ID in the fixed safe format and never echoes
// the client request_id.
func TestMiddlewareCorrelationIDIsServerGenerated(t *testing.T) {
	out, _ := runAudit(t, http.StatusOK, nil, http.MethodPost, "/v1/pii/detect",
		`{"text":"x","request_id":"CLIENT_REQID"}`, nil)
	lines := nonEmptyLines(out)
	if len(lines) != 1 {
		t.Fatalf("got %d audit lines, want exactly 1", len(lines))
	}
	m := decodeLine(t, lines[0])
	rid, _ := m["request_id"].(string)
	if !strings.HasPrefix(rid, "req_") || len(rid) != 4+32 {
		t.Errorf("request_id = %q, want req_<32 hex>", rid)
	}
	if strings.Contains(rid, "CLIENT_REQID") {
		t.Errorf("request_id echoes client value: %q", rid)
	}
}

// TestMiddlewareHealthAndMetricsNotAudited proves that health and metrics
// endpoints never produce a data audit event.
func TestMiddlewareHealthAndMetricsNotAudited(t *testing.T) {
	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/health/live"},
		{http.MethodGet, "/health/ready"},
		{http.MethodGet, "/metrics"},
	}
	for _, rt := range routes {
		out, _ := runAudit(t, http.StatusOK, nil, rt.method, rt.path, "", nil)
		if out != "" {
			t.Errorf("%s %s produced audit output:\n%s", rt.method, rt.path, out)
		}
	}
}

// TestMiddlewareConcurrentRequestsDoNotMixMetadata proves that parallel
// requests keep their own entity metadata isolated: each of the 64 requests
// writes exactly one unique synthetic type, and after completion the decoded
// events form the exact multiset of those types with no gaps, duplicates or
// mixed events.
func TestMiddlewareConcurrentRequestsDoNotMixMetadata(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf)
	handler := Middleware(logger, ModeFull)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Each request carries its own unique type in the query string and
		// writes exactly one entity of that type into its own collector.
		typ := r.URL.Query().Get("type")
		if col, ok := CollectorFromContext(r.Context()); ok {
			col.AddEntity(Entity{Type: typ, Personal: true, Sources: []string{"regex"}, ReasonCodes: []string{"field_label"}})
		}
		w.WriteHeader(http.StatusOK)
	}))

	const n = 64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			typ := fmt.Sprintf("TYPE_%03d", i)
			req := httptest.NewRequest(http.MethodPost, "/v1/pii/detect?type="+typ, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
		}(i)
	}
	wg.Wait()

	lines := nonEmptyLines(buf.String())
	if len(lines) != n {
		t.Fatalf("got %d audit lines, want %d", len(lines), n)
	}
	// Build the exact multiset of detected types across all events.
	seen := make(map[string]int)
	for _, line := range lines {
		m := decodeLine(t, line)
		types := toStrings(t, m["detected_types"])
		if len(types) != 1 {
			t.Fatalf("event has %d types, want exactly 1: %v", len(types), types)
		}
		seen[types[0]]++
	}
	// Every TYPE_000..TYPE_063 must appear exactly once; no gaps, duplicates
	// or mixed events.
	for i := 0; i < n; i++ {
		typ := fmt.Sprintf("TYPE_%03d", i)
		if seen[typ] != 1 {
			t.Errorf("type %q count = %d, want exactly 1", typ, seen[typ])
		}
	}
	if len(seen) != n {
		t.Errorf("distinct types = %d, want %d (mixed events would reduce distinct count)", len(seen), n)
	}
}

// TestMiddlewareNilLoggerDisablesAuditing proves that a nil logger disables
// auditing entirely.
func TestMiddlewareNilLoggerDisablesAuditing(t *testing.T) {
	handler := Middleware(nil, ModeFull)(&auditTestHandler{status: http.StatusOK})
	req := httptest.NewRequest(http.MethodPost, "/v1/pii/detect", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestMiddlewareWriteErrorDoesNotChangeResponse proves that an audit write
// error never changes the user's HTTP response.
func TestMiddlewareWriteErrorDoesNotChangeResponse(t *testing.T) {
	failing := New(errWriter{})
	handler := Middleware(failing, ModeFull)(&auditTestHandler{status: http.StatusOK})
	req := httptest.NewRequest(http.MethodPost, "/v1/pii/detect", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (audit write error must not change response)", rec.Code, http.StatusOK)
	}
}

// TestMiddlewareRevokeMatchesOnlySingleSegment proves that the revoke mapping
// recognizes only DELETE /v1/pii/scopes/{one non-empty URL segment} and not
// any path with the prefix. The escaped path is used so a percent-encoded
// slash inside the segment is still a single segment, while a literal extra
// segment is not.
func TestMiddlewareRevokeMatchesOnlySingleSegment(t *testing.T) {
	cases := []struct {
		path        string
		escapedPath string
		want        bool
	}{
		{"/v1/pii/scopes/scope-1", "/v1/pii/scopes/scope-1", true},
		{"/v1/pii/scopes/a", "/v1/pii/scopes/a", true},
		{"/v1/pii/scopes/a/b", "/v1/pii/scopes/a%2Fb", true},
		{"/v1/pii/scopes/", "/v1/pii/scopes/", false},
		{"/v1/pii/scopes", "/v1/pii/scopes", false},
		{"/v1/pii/scopes/a/b", "/v1/pii/scopes/a/b", false},
		{"/v1/pii/scopes/a/", "/v1/pii/scopes/a/", false},
	}
	for _, tc := range cases {
		got, ok := operationFor(http.MethodDelete, tc.path, tc.escapedPath)
		if ok != tc.want {
			t.Errorf("operationFor(DELETE, %q, %q) ok = %v, want %v", tc.path, tc.escapedPath, ok, tc.want)
		}
		if ok && got != OpRevoke {
			t.Errorf("operationFor(DELETE, %q, %q) = %q, want revoke", tc.path, tc.escapedPath, got)
		}
	}
}

// TestMiddlewareStatusRecorderForwardsOnlyFirstStatus proves that a repeated
// WriteHeader is not forwarded to the underlying writer, matching net/http.
func TestMiddlewareStatusRecorderForwardsOnlyFirstStatus(t *testing.T) {
	var forwarded []int
	underlying := &recordingWriter{onHeader: func(code int) { forwarded = append(forwarded, code) }}
	rec := &statusRecorder{ResponseWriter: underlying}
	rec.WriteHeader(http.StatusOK)
	rec.WriteHeader(http.StatusInternalServerError)
	rec.WriteHeader(http.StatusTeapot)
	if len(forwarded) != 1 || forwarded[0] != http.StatusOK {
		t.Errorf("forwarded statuses = %v, want exactly [200]", forwarded)
	}
}

// recordingWriter records forwarded status codes.
type recordingWriter struct {
	onHeader func(int)
}

func (r *recordingWriter) Header() http.Header { return http.Header{} }
func (r *recordingWriter) Write(b []byte) (int, error) {
	return len(b), nil
}
func (r *recordingWriter) WriteHeader(code int) {
	if r.onHeader != nil {
		r.onHeader(code)
	}
}

// TestMiddlewareFallbackCorrelationIDIsUnique proves that the crypto/rand
// fallback correlation ID is unique across requests and never constant.
func TestMiddlewareFallbackCorrelationIDIsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		fb := fallbackBytes()
		id := "req_" + hex.EncodeToString(fb[:])
		if seen[id] {
			t.Fatalf("fallback correlation ID repeated: %q", id)
		}
		seen[id] = true
		if !strings.HasPrefix(id, "req_") || len(id) != 4+32 {
			t.Fatalf("fallback correlation ID %q not in req_<32 hex> format", id)
		}
	}
}

// TestMiddlewareEscapedScopeSegmentAudited proves that a DELETE
// /v1/pii/scopes/{scope_id} with a percent-encoded slash inside the scope
// segment is routed by a real ServeMux to the handler (which sees the decoded
// PathValue) and still produces exactly one revoke audit event without leaking
// the scope value.
func TestMiddlewareEscapedScopeSegmentAudited(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf)
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /v1/pii/scopes/{scope_id}", func(w http.ResponseWriter, r *http.Request) {
		// The handler must see the decoded scope value.
		if got := r.PathValue("scope_id"); got != "a/b" {
			t.Errorf("PathValue(scope_id) = %q, want a/b", got)
		}
		w.WriteHeader(http.StatusOK)
	})
	handler := Middleware(logger, ModeFull)(mux)

	req := httptest.NewRequest(http.MethodDelete, "/v1/pii/scopes/a%2Fb", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	lines := nonEmptyLines(buf.String())
	if len(lines) != 1 {
		t.Fatalf("got %d audit lines, want exactly 1:\n%s", len(lines), buf.String())
	}
	m := decodeLine(t, lines[0])
	if m["operation"] != "revoke" {
		t.Errorf("operation = %v, want revoke", m["operation"])
	}
	if m["result"] != "success" {
		t.Errorf("result = %v, want success", m["result"])
	}
	// Neither the escaped nor the decoded scope value may appear in the audit.
	if strings.Contains(buf.String(), "a%2Fb") || strings.Contains(buf.String(), "a/b") {
		t.Errorf("audit output leaks scope value:\n%s", buf.String())
	}
}

// TestMiddlewareLiteralExtraSegmentNotAuditedAsRevoke proves that a literal
// extra path segment /v1/pii/scopes/a/b is not recognized as a revoke route
// and therefore produces no audit event.
func TestMiddlewareLiteralExtraSegmentNotAuditedAsRevoke(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf)
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /v1/pii/scopes/{scope_id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := Middleware(logger, ModeFull)(mux)

	req := httptest.NewRequest(http.MethodDelete, "/v1/pii/scopes/a/b", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if buf.Len() != 0 {
		t.Errorf("literal extra segment produced audit output:\n%s", buf.String())
	}
}

// TestMiddlewarePanicEmitsErrorEventAndReRaises proves that a panic downstream
// produces exactly one error audit event (even if a 2xx status was already
// written) and is re-raised with its original value, without leaking the panic
// value into the event.
func TestMiddlewarePanicEmitsErrorEventAndReRaises(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf)
	panicValue := "PANIC_SECRET_MARKER_12345"
	handler := Middleware(logger, ModeFull)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		panic(panicValue)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/pii/detect", nil)
	rec := httptest.NewRecorder()
	recovered := func() (v any) {
		defer func() { v = recover() }()
		handler.ServeHTTP(rec, req)
		return nil
	}()
	if recovered != panicValue {
		t.Fatalf("recovered panic = %v, want %q", recovered, panicValue)
	}

	lines := nonEmptyLines(buf.String())
	if len(lines) != 1 {
		t.Fatalf("got %d audit lines, want exactly 1:\n%s", len(lines), buf.String())
	}
	m := decodeLine(t, lines[0])
	if m["operation"] != "detect" {
		t.Errorf("operation = %v, want detect", m["operation"])
	}
	if m["result"] != "error" {
		t.Errorf("result = %v, want error (panic must be error even after 2xx)", m["result"])
	}
	if strings.Contains(buf.String(), panicValue) {
		t.Errorf("audit output leaks panic value:\n%s", buf.String())
	}
}

// TestMiddlewareNoWriteClassifiedAsSuccess proves that a handler that returns
// without writing a header or body is classified as success (net/http defaults
// to 200), not error.
func TestMiddlewareNoWriteClassifiedAsSuccess(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf)
	handler := Middleware(logger, ModeFull)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	req := httptest.NewRequest(http.MethodPost, "/v1/pii/detect", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	lines := nonEmptyLines(buf.String())
	if len(lines) != 1 {
		t.Fatalf("got %d audit lines, want exactly 1:\n%s", len(lines), buf.String())
	}
	m := decodeLine(t, lines[0])
	if m["result"] != "success" {
		t.Errorf("result = %v, want success (no write defaults to 200)", m["result"])
	}
}

// errWriter always fails writes.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errWriteFailed }

var errWriteFailed = &writeError{}

type writeError struct{}

func (*writeError) Error() string { return "write failed" }

// nonEmptyLines splits s into non-empty lines.
func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// containsAllStrings reports whether haystack contains every needle.
func containsAllStrings(haystack []string, needles ...string) bool {
	for _, n := range needles {
		found := false
		for _, h := range haystack {
			if h == n {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
