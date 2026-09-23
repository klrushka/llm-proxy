package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/klrushka/llm-proxy/internal/api"
	"github.com/klrushka/llm-proxy/internal/audit"
	"github.com/klrushka/llm-proxy/internal/config"
	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/metrics"
	"github.com/klrushka/llm-proxy/internal/modelclient"
	"github.com/klrushka/llm-proxy/internal/policy"
	"github.com/klrushka/llm-proxy/internal/process"
	"github.com/klrushka/llm-proxy/internal/tokenization"
	"github.com/klrushka/llm-proxy/internal/vault"
)

// entityJSON builds a single entity object for a worker response. It is a
// local copy of the modelclient test helper so the cmd package does not depend
// on an internal test file.
func entityJSON(label string, start, end int, confidence float64, model string) string {
	return `{"label":` + strconv.Quote(label) +
		`,"start":` + strconv.Itoa(start) +
		`,"end":` + strconv.Itoa(end) +
		`,"confidence":` + strconv.FormatFloat(confidence, 'g', -1, 64) +
		`,"model":` + strconv.Quote(model) + `}`
}

// responseJSON wraps entity objects in a top-level entities array.
func responseJSON(entities ...string) string {
	return `{"entities":[` + strings.Join(entities, ",") + `]}`
}

// newModelDetector builds a real modelclient.Client pointed at srv and a real
// detection registry, then returns the modelDetector adapter under test.
func newModelDetector(t *testing.T, srv *httptest.Server) api.ModelDetector {
	t.Helper()
	client, err := modelclient.New(srv.URL, modelclient.ModeFull, 5*time.Second)
	if err != nil {
		t.Fatalf("modelclient.New() error = %v", err)
	}
	reg, err := detection.New()
	if err != nil {
		t.Fatalf("detection.New() error = %v", err)
	}
	return modelDetector(client, reg)
}

// TestModelDetectorMapsCanonicalEntities proves that canonical rubert and
// gliner entities are mapped to detection candidates with source, type,
// UTF-8 byte offsets and confidence preserved, and that unknown model sources
// and unknown labels are dropped.
func TestModelDetectorMapsCanonicalEntities(t *testing.T) {
	// Synthetic text. "Анна" is 4 code points / 8 UTF-8 bytes; "Иван" is 4
	// code points / 8 UTF-8 bytes.
	const text = "Анна Иван"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, responseJSON(
			entityJSON("FULL_NAME", 0, 4, 0.95, "rubert"),
			entityJSON("ru_pii_person", 5, 9, 0.8, "gliner"),
			entityJSON("FULL_NAME", 0, 4, 0.9, "other"),
			entityJSON("DATE", 0, 4, 0.7, "rubert"),
		))
	}))
	defer srv.Close()

	det := newModelDetector(t, srv)
	got, err := det(context.Background(), text)
	if err != nil {
		t.Fatalf("modelDetector() error = %v", err)
	}

	// Only the two canonical entities survive; the unknown model source and
	// the unknown label are dropped.
	if len(got) != 2 {
		t.Fatalf("len(candidates) = %d, want 2: %+v", len(got), got)
	}

	// rubert FULL_NAME: byte offsets 0..8, confidence 0.95, source rubert.
	rubert := got[0]
	if rubert.Type != detection.TypeFullName {
		t.Errorf("rubert type = %q, want %q", rubert.Type, detection.TypeFullName)
	}
	if rubert.Start != 0 || rubert.End != 8 {
		t.Errorf("rubert offsets = [%d,%d), want [0,8)", rubert.Start, rubert.End)
	}
	if rubert.Confidence != 0.95 {
		t.Errorf("rubert confidence = %v, want 0.95", rubert.Confidence)
	}
	if len(rubert.Sources) != 1 || rubert.Sources[0] != detection.SourceRubert {
		t.Errorf("rubert sources = %v, want [rubert]", rubert.Sources)
	}

	// gliner ru_pii_person alias: byte offsets 9..17, confidence 0.8, source
	// gliner, canonical type FULL_NAME.
	gliner := got[1]
	if gliner.Type != detection.TypeFullName {
		t.Errorf("gliner type = %q, want %q", gliner.Type, detection.TypeFullName)
	}
	if gliner.Start != 9 || gliner.End != 17 {
		t.Errorf("gliner offsets = [%d,%d), want [9,17)", gliner.Start, gliner.End)
	}
	if gliner.Confidence != 0.8 {
		t.Errorf("gliner confidence = %v, want 0.8", gliner.Confidence)
	}
	if len(gliner.Sources) != 1 || gliner.Sources[0] != detection.SourceGliner {
		t.Errorf("gliner sources = %v, want [gliner]", gliner.Sources)
	}
}

// TestModelDetectorDropsUnknownSourceAndLabel proves that an entity with an
// unknown model source and an entity with an unknown label are both dropped,
// leaving no candidates.
func TestModelDetectorDropsUnknownSourceAndLabel(t *testing.T) {
	const text = "Анна"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, responseJSON(
			entityJSON("FULL_NAME", 0, 4, 0.9, "other"),
			entityJSON("DATE", 0, 4, 0.7, "rubert"),
		))
	}))
	defer srv.Close()

	det := newModelDetector(t, srv)
	got, err := det(context.Background(), text)
	if err != nil {
		t.Fatalf("modelDetector() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(candidates) = %d, want 0: %+v", len(got), got)
	}
}

// newTestPipeline builds the real pipeline with the in-memory vault and a
// rules-only detector (no model worker), mirroring the fast-mode wiring.
func newTestPipeline(t *testing.T) (*api.Pipeline, api.PIIHandlers) {
	t.Helper()
	v, err := vault.NewMemory(time.Hour)
	if err != nil {
		t.Fatalf("vault.NewMemory() error = %v", err)
	}
	g, err := tokenization.New()
	if err != nil {
		t.Fatalf("tokenization.New() error = %v", err)
	}
	reg, err := detection.New()
	if err != nil {
		t.Fatalf("detection.New() error = %v", err)
	}
	allowed := make([]string, 0, len(reg.Types()))
	for _, typ := range reg.Types() {
		allowed = append(allowed, string(typ))
	}
	p := policy.NewPolicy(policy.DefaultConsumerID, allowed)
	pipe := api.NewPipeline(nil, p, g, v)
	return pipe, pipe.Handlers()
}

// TestBuildRouterUnconfiguredLLMNoPanic proves that constructing the router
// with an absent LLM configuration group does not panic and leaves
// /v1/runtime/chat fail-closed with 503 while /process still works. This is a
// regression test for the defect where a nil WithRuntime option was passed to
// NewRouter, which calls every option unconditionally.
func TestBuildRouterUnconfiguredLLMNoPanic(t *testing.T) {
	pipe, handlers := newTestPipeline(t)
	op := process.NewOperation(process.NewStore(), func(ctx context.Context, payload string) (string, error) {
		res, err := handlers.Tokenize(ctx, api.TokenizeRequest{Text: payload, ScopeID: processScope})
		if err != nil {
			return "", err
		}
		return res.TokenizedText, nil
	})
	regMetrics := metrics.NewRegistry(time.Minute)
	logger := audit.New(httptest.NewRecorder())

	cfg := config.Config{LLM: config.LLMConfig{Timeout: config.DefaultLLMTimeout}}
	mux, err := buildRouter(cfg, pipe, handlers, op, regMetrics, logger)
	if err != nil {
		t.Fatalf("buildRouter() error = %v", err)
	}

	// /v1/runtime/chat must fail closed with 503, not panic.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runtime/chat", strings.NewReader(`{"text":"x","scope_id":"s1"}`))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("runtime status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	// /process must still work.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/process", strings.NewReader(`{"payload":"Клиент Иванов Иван, телефон +7 900 123-45-67, email ivanov@example.com","payload_id":"id-1"}`))
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("process status = %d, want %d; body = %q", rec2.Code, http.StatusOK, rec2.Body.String())
	}
}
