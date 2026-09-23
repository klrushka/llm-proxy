package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/klrushka/llm-proxy/internal/api"
	"github.com/klrushka/llm-proxy/internal/audit"
	"github.com/klrushka/llm-proxy/internal/config"
	"github.com/klrushka/llm-proxy/internal/process"
)

// newAuditHandler builds the public router wrapped by the outer audit
// middleware, exactly as run() does.
func newAuditHandler(t *testing.T) (http.Handler, *bytes.Buffer) {
	t.Helper()
	pipe, handlers := newTestPipeline(t)
	op := process.NewOperation(process.NewStore(), func(ctx context.Context, payload string) (string, error) {
		res, err := handlers.Tokenize(ctx, api.TokenizeRequest{Text: payload, ScopeID: processScope})
		if err != nil {
			return "", err
		}
		return res.TokenizedText, nil
	})
	cfg := config.Config{LLM: config.LLMConfig{Timeout: config.DefaultLLMTimeout}}
	handler, err := buildRouter(cfg, pipe, handlers, op, nil)
	if err != nil {
		t.Fatalf("buildRouter() error = %v", err)
	}
	var buf bytes.Buffer
	logger := audit.New(&buf)
	return audit.Middleware(logger, audit.ModelMode(cfg.ModelMode))(handler), &buf
}

// auditLines returns the non-empty audit JSON lines captured in buf.
func auditLines(buf *bytes.Buffer) []string {
	var out []string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// auditEvent decodes one audit line into a map.
func auditEvent(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("invalid audit JSON: %v\n%s", err, line)
	}
	return m
}

// assertNoLeak fails the test if any marker appears in the audit output.
func assertNoLeak(t *testing.T, buf *bytes.Buffer, markers ...string) {
	t.Helper()
	for _, m := range markers {
		if strings.Contains(buf.String(), m) {
			t.Errorf("audit output leaks marker %q:\n%s", m, buf.String())
		}
	}
}

// TestAuditProductionDetectEmitsOneEventWithMetadata proves that a successful
// detect through the full production composition emits exactly one audit event
// carrying the actually detected synthetic types, never leaks the synthetic
// plaintext values or the client request_id, and the public response still
// echoes the client request_id unchanged.
func TestAuditProductionDetectEmitsOneEventWithMetadata(t *testing.T) {
	handler, buf := newAuditHandler(t)
	const (
		fullName  = "Иванов Иван Иванович"
		phone     = "+7 911 222-33-44"
		email     = "ivanov.audit@example.com"
		clientRID = "CLIENT_REQID_DETECT"
	)
	text := "Клиент " + fullName + ", телефон " + phone + ", email " + email
	rec := doJSON(t, handler, http.MethodPost, "/v1/pii/detect",
		`{"text":"`+text+`","request_id":"`+clientRID+`"}`,
		map[string]string{"Authorization": "Bearer key-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusOK, rec.Body.String())
	}

	lines := auditLines(buf)
	if len(lines) != 1 {
		t.Fatalf("got %d audit lines, want exactly 1:\n%s", len(lines), buf.String())
	}
	m := auditEvent(t, lines[0])
	if m["operation"] != "detect" {
		t.Errorf("operation = %v, want detect", m["operation"])
	}
	if m["result"] != "success" {
		t.Errorf("result = %v, want success", m["result"])
	}
	types := toStrings(t, m["detected_types"])
	if !containsAll(types, "EMAIL", "PHONE") {
		t.Errorf("detected_types = %v, want EMAIL and PHONE", types)
	}
	if m["entity_count"] != float64(2) {
		t.Errorf("entity_count = %v, want 2", m["entity_count"])
	}
	// The synthetic plaintext values and the client request_id must never
	// appear in the audit output.
	assertNoLeak(t, buf, fullName, phone, email, clientRID)
	// The public response must still echo the client request_id unchanged.
	var resp api.DetectResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid response: %v", err)
	}
	if resp.RequestID != clientRID {
		t.Errorf("response request_id = %q, want %q", resp.RequestID, clientRID)
	}
}

// TestAuditProductionTokenizeEmitsOneEventWithMetadata proves that a successful
// tokenize emits exactly one audit event carrying the detected synthetic types
// and never leaks the synthetic plaintext, the client request_id, the scope_id
// or the issued token.
func TestAuditProductionTokenizeEmitsOneEventWithMetadata(t *testing.T) {
	handler, buf := newAuditHandler(t)
	const (
		fullName  = "Петров Пётр Петрович"
		phone     = "+7 922 333-44-55"
		email     = "petrov.audit@example.com"
		clientRID = "CLIENT_REQID_TOKENIZE"
		scope     = "SCOPE_TOKENIZE_77777"
	)
	text := "Клиент " + fullName + ", телефон " + phone + ", email " + email
	rec := doJSON(t, handler, http.MethodPost, "/v1/pii/tokenize",
		`{"text":"`+text+`","scope_id":"`+scope+`","request_id":"`+clientRID+`"}`,
		map[string]string{"Authorization": "Bearer key-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusOK, rec.Body.String())
	}

	lines := auditLines(buf)
	if len(lines) != 1 {
		t.Fatalf("got %d audit lines, want exactly 1:\n%s", len(lines), buf.String())
	}
	m := auditEvent(t, lines[0])
	if m["operation"] != "tokenize" {
		t.Errorf("operation = %v, want tokenize", m["operation"])
	}
	if m["result"] != "success" {
		t.Errorf("result = %v, want success", m["result"])
	}
	types := toStrings(t, m["detected_types"])
	if !containsAll(types, "EMAIL", "PHONE") {
		t.Errorf("detected_types = %v, want EMAIL and PHONE", types)
	}
	if m["entity_count"] != float64(2) {
		t.Errorf("entity_count = %v, want 2", m["entity_count"])
	}
	// The public response carries the issued token; it must never appear in the
	// audit output, nor the plaintext, client request_id or scope_id.
	var resp api.TokenizeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid response: %v", err)
	}
	assertNoLeak(t, buf, fullName, phone, email, clientRID, scope, resp.TokenizedText)
}

// TestAuditProductionProcessEmitsOneEventWithMetadata proves that POST /process
// through the full composition emits exactly one audit event carrying the
// actually detected synthetic types and never leaks the synthetic plaintext or
// the payload_id (the old process-only audit is removed from production
// composition).
func TestAuditProductionProcessEmitsOneEventWithMetadata(t *testing.T) {
	handler, buf := newAuditHandler(t)
	const (
		fullName  = "Сидоров Сидор Сидорович"
		phone     = "+7 933 444-55-66"
		email     = "sidorov.audit@example.com"
		payloadID = "PAYLOAD_ID_PROCESS_88888"
	)
	text := "Клиент " + fullName + ", телефон " + phone + ", email " + email
	rec := doJSON(t, handler, http.MethodPost, "/process",
		`{"payload":"`+text+`","payload_id":"`+payloadID+`"}`,
		nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusOK, rec.Body.String())
	}

	lines := auditLines(buf)
	if len(lines) != 1 {
		t.Fatalf("got %d audit lines, want exactly 1 (no duplicate process audit):\n%s", len(lines), buf.String())
	}
	m := auditEvent(t, lines[0])
	if m["operation"] != "process" {
		t.Errorf("operation = %v, want process", m["operation"])
	}
	if m["result"] != "success" {
		t.Errorf("result = %v, want success", m["result"])
	}
	types := toStrings(t, m["detected_types"])
	if !containsAll(types, "EMAIL", "PHONE") {
		t.Errorf("detected_types = %v, want EMAIL and PHONE", types)
	}
	if m["entity_count"] != float64(2) {
		t.Errorf("entity_count = %v, want 2", m["entity_count"])
	}
	assertNoLeak(t, buf, fullName, phone, email, payloadID)
}

// TestAuditPublicPolicyMetadata proves the public full-type policy marks both
// detected synthetic entities as personal in document order.
func TestAuditPublicPolicyMetadata(t *testing.T) {
	handler, buf := newAuditHandler(t)
	const (
		phone = "+7 944 555-66-77"
		email = "postpolicy.audit@example.com"
	)
	text := "телефон " + phone + ", email " + email
	rec := doJSON(t, handler, http.MethodPost, "/v1/pii/detect",
		`{"text":"`+text+`","request_id":"POSTPOLICY_RID"}`,
		map[string]string{"Authorization": "Bearer key-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusOK, rec.Body.String())
	}

	lines := auditLines(buf)
	if len(lines) != 1 {
		t.Fatalf("got %d audit lines, want exactly 1:\n%s", len(lines), buf.String())
	}
	m := auditEvent(t, lines[0])
	if m["operation"] != "detect" {
		t.Errorf("operation = %v, want detect", m["operation"])
	}
	types := toStrings(t, m["detected_types"])
	if !containsAll(types, "PHONE", "EMAIL") {
		t.Errorf("detected_types = %v, want PHONE and EMAIL", types)
	}
	if m["entity_count"] != float64(2) {
		t.Errorf("entity_count = %v, want 2", m["entity_count"])
	}
	flags := toBools(t, m["personal_flags"])
	if len(flags) != 2 || !flags[0] || !flags[1] {
		t.Errorf("personal_flags = %v, want [true true] in document order", flags)
	}
	reasons := toStrings(t, m["reason_codes"])
	if containsAll(reasons, "type_disabled_by_policy") {
		t.Errorf("reason_codes = %v, must not contain type_disabled_by_policy", reasons)
	}
	assertNoLeak(t, buf, phone, email)
}

// TestAuditProductionLifecycleTokenizeDetokenizeRevoke proves the real
// lifecycle: tokenize issues a token and persists a mapping, detokenize of that
// token restores the synthetic original, and DELETE of the same non-empty scope
// revokes the mapping. Each request emits exactly one event of its operation,
// and the accumulated audit output never contains the original, the token or
// the scope.
func TestAuditProductionLifecycleTokenizeDetokenizeRevoke(t *testing.T) {
	handler, buf := newAuditHandler(t)
	const (
		email = "lifecycle.audit@example.com"
		scope = "SCOPE_LIFECYCLE_12345"
	)
	text := "email " + email

	// 1. Tokenize: issues a token and persists the mapping.
	tokRec := doJSON(t, handler, http.MethodPost, "/v1/pii/tokenize",
		`{"text":"`+text+`","scope_id":"`+scope+`"}`,
		map[string]string{"Authorization": "Bearer key-a"})
	if tokRec.Code != http.StatusOK {
		t.Fatalf("tokenize status = %d, want %d; body = %q", tokRec.Code, http.StatusOK, tokRec.Body.String())
	}
	var tok api.TokenizeResponse
	if err := json.Unmarshal(tokRec.Body.Bytes(), &tok); err != nil {
		t.Fatalf("invalid tokenize response: %v", err)
	}
	if !strings.Contains(tok.TokenizedText, "<EMAIL_") {
		t.Fatalf("tokenized text %q missing EMAIL token", tok.TokenizedText)
	}

	// 2. Detokenize the same token: restores the synthetic original.
	detRec := doJSON(t, handler, http.MethodPost, "/v1/pii/detokenize",
		`{"text":"`+tok.TokenizedText+`","scope_id":"`+scope+`","mode":"strict"}`,
		map[string]string{"Authorization": "Bearer key-a"})
	if detRec.Code != http.StatusOK {
		t.Fatalf("detokenize status = %d, want %d; body = %q", detRec.Code, http.StatusOK, detRec.Body.String())
	}
	var det api.DetokenizeResponse
	if err := json.Unmarshal(detRec.Body.Bytes(), &det); err != nil {
		t.Fatalf("invalid detokenize response: %v", err)
	}
	if det.RestoredText != text {
		t.Errorf("restored_text = %q, want %q", det.RestoredText, text)
	}

	// 3. Revoke the same non-empty scope: revokes the mapping.
	revRec := doJSON(t, handler, http.MethodDelete, "/v1/pii/scopes/"+scope, "",
		map[string]string{"Authorization": "Bearer key-a"})
	if revRec.Code != http.StatusOK {
		t.Fatalf("revoke status = %d, want %d; body = %q", revRec.Code, http.StatusOK, revRec.Body.String())
	}

	// Exactly one event per operation, in order.
	lines := auditLines(buf)
	if len(lines) != 3 {
		t.Fatalf("got %d audit lines, want exactly 3:\n%s", len(lines), buf.String())
	}
	ops := []string{}
	for _, line := range lines {
		m := auditEvent(t, line)
		ops = append(ops, m["operation"].(string))
		if m["result"] != "success" {
			t.Errorf("operation %v result = %v, want success", m["operation"], m["result"])
		}
	}
	if ops[0] != "tokenize" || ops[1] != "detokenize" || ops[2] != "revoke" {
		t.Errorf("operations = %v, want [tokenize detokenize revoke]", ops)
	}
	// The accumulated audit output must never contain the original, the token
	// or the scope.
	assertNoLeak(t, buf, email, scope, tok.TokenizedText)
}

// TestAuditPublicDetokenizeErrorEmitsOneEvent proves an unresolved token emits
// exactly one safe error event from the public detokenization path.
func TestAuditPublicDetokenizeErrorEmitsOneEvent(t *testing.T) {
	handler, buf := newAuditHandler(t)
	const (
		authz = "Bearer key-b"
		body  = "BODY_DEMASK_11111"
		scope = "SCOPE_DEMASK_22222"
		token = "<EMAIL_33333333333333333333333333333333>"
	)
	rec := doJSON(t, handler, http.MethodPost, "/v1/pii/detokenize",
		`{"text":"`+token+`","scope_id":"`+scope+`","mode":"strict"}`,
		map[string]string{"Authorization": authz})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}

	lines := auditLines(buf)
	if len(lines) != 1 {
		t.Fatalf("got %d audit lines, want exactly 1:\n%s", len(lines), buf.String())
	}
	m := auditEvent(t, lines[0])
	if m["operation"] != "detokenize" {
		t.Errorf("operation = %v, want detokenize", m["operation"])
	}
	if m["result"] != "error" {
		t.Errorf("result = %v, want error", m["result"])
	}
	assertNoLeak(t, buf, authz, body, scope, token)
}

// TestAuditPublicRequestIgnoresAuthorization proves an arbitrary Authorization
// header does not deny a public request and is never copied into audit output.
func TestAuditPublicRequestIgnoresAuthorization(t *testing.T) {
	handler, buf := newAuditHandler(t)
	markers := []string{
		"Bearer AUTHZ_SECRET_11111",
		"BODY_SECRET_22222",
		"CLIENT_REQID_SECRET_33333",
		"PAYLOAD_ID_SECRET_44444",
		"SCOPE_ID_SECRET_55555",
	}
	body := `{"text":"BODY_SECRET_22222","request_id":"CLIENT_REQID_SECRET_33333","payload_id":"PAYLOAD_ID_SECRET_44444","scope_id":"SCOPE_ID_SECRET_55555"}`
	rec := doJSON(t, handler, http.MethodPost, "/v1/pii/tokenize", body,
		map[string]string{"Authorization": "Bearer AUTHZ_SECRET_11111"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	lines := auditLines(buf)
	if len(lines) != 1 {
		t.Fatalf("got %d audit lines, want exactly 1:\n%s", len(lines), buf.String())
	}
	m := auditEvent(t, lines[0])
	if m["operation"] != "tokenize" {
		t.Errorf("operation = %v, want tokenize", m["operation"])
	}
	if m["result"] != "success" {
		t.Errorf("result = %v, want success", m["result"])
	}
	assertNoLeak(t, buf, markers...)
}

// TestAuditProductionMalformedJSONEmitsOneEvent proves that a malformed JSON
// body is audited exactly once with an error result.
func TestAuditProductionMalformedJSONEmitsOneEvent(t *testing.T) {
	handler, buf := newAuditHandler(t)
	rec := doJSON(t, handler, http.MethodPost, "/v1/pii/detect", `{not json`,
		map[string]string{"Authorization": "Bearer key-a"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	lines := auditLines(buf)
	if len(lines) != 1 {
		t.Fatalf("got %d audit lines, want exactly 1:\n%s", len(lines), buf.String())
	}
	m := auditEvent(t, lines[0])
	if m["operation"] != "detect" {
		t.Errorf("operation = %v, want detect", m["operation"])
	}
	if m["result"] != "error" {
		t.Errorf("result = %v, want error", m["result"])
	}
}

// TestAuditProductionHealthAndMetricsNotAudited proves that health and metrics
// endpoints never produce a data audit event through the full composition.
func TestAuditProductionHealthAndMetricsNotAudited(t *testing.T) {
	handler, buf := newAuditHandler(t)
	for _, path := range []string{"/health/live", "/health/ready", "/metrics"} {
		doJSON(t, handler, http.MethodGet, path, "", nil)
	}
	if buf.Len() != 0 {
		t.Errorf("health/metrics produced audit output:\n%s", buf.String())
	}
}

// TestAuditProductionRuntimeUnconfiguredEmitsOneErrorEvent proves that the
// runtime route with an unconfigured LLM fails closed with 503 and emits
// exactly one runtime error audit event.
func TestAuditProductionRuntimeUnconfiguredEmitsOneErrorEvent(t *testing.T) {
	handler, buf := newAuditHandler(t)
	rec := doJSON(t, handler, http.MethodPost, "/v1/runtime/chat",
		`{"text":"Клиент Иванов Иван, email ivanov@example.com","scope_id":"scope-1"}`,
		map[string]string{"Authorization": "Bearer key-a"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}

	lines := auditLines(buf)
	if len(lines) != 1 {
		t.Fatalf("got %d audit lines, want exactly 1:\n%s", len(lines), buf.String())
	}
	m := auditEvent(t, lines[0])
	if m["operation"] != "runtime" {
		t.Errorf("operation = %v, want runtime", m["operation"])
	}
	if m["result"] != "error" {
		t.Errorf("result = %v, want error", m["result"])
	}
}

// toStrings converts a JSON array of strings to []string.
func toStrings(t *testing.T, v any) []string {
	t.Helper()
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("value %v is not a JSON array", v)
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("array item %v is not a string", item)
		}
		out = append(out, s)
	}
	return out
}

// toBools converts a JSON array of bools to []bool.
func toBools(t *testing.T, v any) []bool {
	t.Helper()
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("value %v is not a JSON array", v)
	}
	out := make([]bool, 0, len(raw))
	for _, item := range raw {
		b, ok := item.(bool)
		if !ok {
			t.Fatalf("array item %v is not a bool", item)
		}
		out = append(out, b)
	}
	return out
}

// containsAll reports whether haystack contains every needle.
func containsAll(haystack []string, needles ...string) bool {
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
