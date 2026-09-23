package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/klrushka/llm-proxy/internal/api"
	"github.com/klrushka/llm-proxy/internal/config"
	"github.com/klrushka/llm-proxy/internal/contextual"
	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/metrics"
	"github.com/klrushka/llm-proxy/internal/modelclient"
	"github.com/klrushka/llm-proxy/internal/policy"
	"github.com/klrushka/llm-proxy/internal/process"
	"github.com/klrushka/llm-proxy/internal/runtime"
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

// spanOf returns the UTF-8 byte span [start,end) of sub within text.
func spanOf(t *testing.T, text, sub string) (int, int) {
	t.Helper()
	start := strings.Index(text, sub)
	if start < 0 {
		t.Fatalf("substring %q not found in %q", sub, text)
	}
	return start, start + len(sub)
}

// runeSpanOf returns the code-point span [start,end) of sub within text. The
// worker reports code-point offsets, which the model client converts to UTF-8
// byte offsets.
func runeSpanOf(t *testing.T, text, sub string) (int, int) {
	t.Helper()
	bs, be := spanOf(t, text, sub)
	return utf8.RuneCountInString(text[:bs]), utf8.RuneCountInString(text[:be])
}

// newModelDetector builds a real modelclient.Client pointed at srv and a real
// detection registry, then returns the modelDetector adapter under test. The
// server handler is wrapped so POST /plan_windows returns a valid single-window
// plan while /infer is delegated to the test's entity handler, letting the
// production modelDetector (which plans before inferring) drive the existing
// entity-only test servers.
func newModelDetector(t *testing.T, srv *httptest.Server) api.ModelDetector {
	t.Helper()
	if srv.Config != nil && srv.Config.Handler != nil {
		srv.Config.Handler = planAware(srv.Config.Handler)
	}
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

// planAware wraps a handler so the server also answers POST /plan_windows with
// a valid single-window plan covering the whole request text, while /infer is
// delegated to the wrapped handler. This lets the production modelDetector
// (which plans before inferring) drive the existing entity-only test handlers.
func planAware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/plan_windows" {
			var body struct {
				Text string `json:"text"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			end := utf8.RuneCountInString(body.Text)
			fmt.Fprintf(w, `{"model":%q,"total_count":1,"max_window_tokens":510,"windows":[{"start":0,"end":%d,"token_count":1}]}`, "redmadrobot-rnd/rubert-base-pii-ner", end)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// TestModelDetectorMapsCanonicalEntities proves that real rubert and gliner
// entities are mapped to detection candidates with source, type, UTF-8 byte
// offsets and confidence preserved.
func TestModelDetectorMapsCanonicalEntities(t *testing.T) {
	// Synthetic text. "Анна" is 4 code points / 8 UTF-8 bytes; "Иван" is 4
	// code points / 8 UTF-8 bytes.
	const text = "Анна Иван"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, responseJSON(
			entityJSON("FIRST_NAME", 0, 4, 0.95, "rubert"),
			entityJSON("ru_pii_person", 5, 9, 0.8, "gliner"),
		))
	}))
	defer srv.Close()

	det := newModelDetector(t, srv)
	got, err := det(context.Background(), text)
	if err != nil {
		t.Fatalf("modelDetector() error = %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("len(candidates) = %d, want 2: %+v", len(got), got)
	}

	// rubert FIRST_NAME: byte offsets 0..8, confidence 0.95, source rubert.
	rubert := got[0]
	if rubert.Type != detection.TypeFirstName {
		t.Errorf("rubert type = %q, want %q", rubert.Type, detection.TypeFirstName)
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

// TestModelDetectorFailsClosedOnUnknownSource proves that an entity with an
// unknown model source fails the whole inference with a safe ErrInvalidResponse
// and no candidates.
func TestModelDetectorFailsClosedOnUnknownSource(t *testing.T) {
	const text = "Анна"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, responseJSON(
			entityJSON("FULL_NAME", 0, 4, 0.9, "other"),
		))
	}))
	defer srv.Close()

	det := newModelDetector(t, srv)
	got, err := det(context.Background(), text)
	if err == nil {
		t.Fatal("modelDetector() error = nil, want ErrInvalidResponse")
	}
	if !errors.Is(err, modelclient.ErrInvalidResponse) {
		t.Errorf("modelDetector() error = %v, want ErrInvalidResponse", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(candidates) = %d, want 0: %+v", len(got), got)
	}
	if strings.Contains(err.Error(), text) || strings.Contains(err.Error(), "other") {
		t.Errorf("error %q leaks request text or source", err.Error())
	}
}

// TestModelDetectorFailsClosedOnUnknownLabel proves that an entity with a label
// outside the source allowlist fails the whole inference with a safe
// ErrInvalidResponse and no candidates.
func TestModelDetectorFailsClosedOnUnknownLabel(t *testing.T) {
	const text = "Анна"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, responseJSON(
			entityJSON("DATE", 0, 4, 0.7, "rubert"),
		))
	}))
	defer srv.Close()

	det := newModelDetector(t, srv)
	got, err := det(context.Background(), text)
	if err == nil {
		t.Fatal("modelDetector() error = nil, want ErrInvalidResponse")
	}
	if !errors.Is(err, modelclient.ErrInvalidResponse) {
		t.Errorf("modelDetector() error = %v, want ErrInvalidResponse", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(candidates) = %d, want 0: %+v", len(got), got)
	}
	if strings.Contains(err.Error(), text) || strings.Contains(err.Error(), "DATE") {
		t.Errorf("error %q leaks request text or label", err.Error())
	}
}

// TestModelDetectorMapsRubertAddressLabels proves that RuBERT address labels
// resolve to canonical address component types with offsets, confidence and
// source preserved, and that DISTRICT maps to ADDRESS_REGION.
func TestModelDetectorMapsRubertAddressLabels(t *testing.T) {
	const text = "Россия, Москва, ул. Ленина, д. 5"
	countryS, countryE := runeSpanOf(t, text, "Россия")
	regionS, regionE := runeSpanOf(t, text, "Москва")
	cityS, cityE := runeSpanOf(t, text, "Москва")
	streetS, streetE := runeSpanOf(t, text, "Ленина")
	houseS, houseE := runeSpanOf(t, text, "5")
	districtS, districtE := runeSpanOf(t, text, "Ленина")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, responseJSON(
			entityJSON("COUNTRY", countryS, countryE, 0.9, "rubert"),
			entityJSON("REGION", regionS, regionE, 0.8, "rubert"),
			entityJSON("CITY", cityS, cityE, 0.95, "rubert"),
			entityJSON("STREET", streetS, streetE, 0.9, "rubert"),
			entityJSON("HOUSE", houseS, houseE, 0.9, "rubert"),
			entityJSON("DISTRICT", districtS, districtE, 0.7, "rubert"),
		))
	}))
	defer srv.Close()

	det := newModelDetector(t, srv)
	got, err := det(context.Background(), text)
	if err != nil {
		t.Fatalf("modelDetector() error = %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("len(candidates) = %d, want 6: %+v", len(got), got)
	}
	countryBS, countryBE := spanOf(t, text, "Россия")
	regionBS, regionBE := spanOf(t, text, "Москва")
	streetBS, streetBE := spanOf(t, text, "Ленина")
	houseBS, houseBE := spanOf(t, text, "5")
	// ReconstructGlobalOffsets returns entities in a deterministic global order:
	// ascending Start, then End, then Label, then Model, then Confidence. So the
	// same-span CITY/REGION and DISTRICT/STREET pairs are ordered by label.
	want := []detection.Candidate{
		{Type: detection.TypeAddressCountry, Start: countryBS, End: countryBE, Confidence: 0.9, Sources: []detection.Source{detection.SourceRubert}},
		{Type: detection.TypeAddressCity, Start: regionBS, End: regionBE, Confidence: 0.95, Sources: []detection.Source{detection.SourceRubert}},
		{Type: detection.TypeAddressRegion, Start: regionBS, End: regionBE, Confidence: 0.8, Sources: []detection.Source{detection.SourceRubert}},
		{Type: detection.TypeAddressRegion, Start: streetBS, End: streetBE, Confidence: 0.7, Sources: []detection.Source{detection.SourceRubert}},
		{Type: detection.TypeAddressStreet, Start: streetBS, End: streetBE, Confidence: 0.9, Sources: []detection.Source{detection.SourceRubert}},
		{Type: detection.TypeAddressHouse, Start: houseBS, End: houseBE, Confidence: 0.9, Sources: []detection.Source{detection.SourceRubert}},
	}
	for i, c := range want {
		if got[i].Type != c.Type || got[i].Start != c.Start || got[i].End != c.End || got[i].Confidence != c.Confidence {
			t.Errorf("candidate[%d] = %+v, want %+v", i, got[i], c)
		}
		if len(got[i].Sources) != 1 || got[i].Sources[0] != detection.SourceRubert {
			t.Errorf("candidate[%d] sources = %v, want [rubert]", i, got[i].Sources)
		}
	}
}

// TestModelDetectorMapsGlinerIntermediate proves that GLiNER ru_pii_date and
// ru_pii_location reach the pipeline as intermediate DATE and LOCATION
// candidates with offsets, confidence and source preserved.
func TestModelDetectorMapsGlinerIntermediate(t *testing.T) {
	const text = "дата рождения 01.02.1990, место рождения город Тестовск"
	dateS, dateE := runeSpanOf(t, text, "01.02.1990")
	locS, locE := runeSpanOf(t, text, "Тестовск")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, responseJSON(
			entityJSON("ru_pii_date", dateS, dateE, 0.85, "gliner"),
			entityJSON("ru_pii_location", locS, locE, 0.8, "gliner"),
		))
	}))
	defer srv.Close()

	det := newModelDetector(t, srv)
	got, err := det(context.Background(), text)
	if err != nil {
		t.Fatalf("modelDetector() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(candidates) = %d, want 2: %+v", len(got), got)
	}
	dateBS, dateBE := spanOf(t, text, "01.02.1990")
	locBS, locBE := spanOf(t, text, "Тестовск")
	if got[0].Type != detection.TypeDate || got[0].Start != dateBS || got[0].End != dateBE || got[0].Confidence != 0.85 {
		t.Errorf("date candidate = %+v, want DATE [%d,%d) conf 0.85", got[0], dateBS, dateBE)
	}
	if got[1].Type != detection.TypeLocation || got[1].Start != locBS || got[1].End != locBE || got[1].Confidence != 0.8 {
		t.Errorf("location candidate = %+v, want LOCATION [%d,%d) conf 0.8", got[1], locBS, locBE)
	}
	for _, c := range got {
		if len(c.Sources) != 1 || c.Sources[0] != detection.SourceGliner {
			t.Errorf("candidate sources = %v, want [gliner]", c.Sources)
		}
	}
}

// TestModelDetectorConfirmsDocumentLabels proves that RuBERT document labels
// become canonical types only when the Go validators confirm the value with an
// exact span match, and that invalid structural values are dropped rather than
// promoted by the model label alone.
func TestModelDetectorConfirmsDocumentLabels(t *testing.T) {
	// Synthetic values: a valid passport "00 00 000000" under "паспорт"
	// context, a valid personal INN under "инн" context, a Luhn-valid card, a
	// valid driver license under "водительское удостоверение" context, and
	// invalid structural values (a passport-shaped run with no Go context, a
	// non-Luhn card, and an INN with a bad checksum).
	const text = "паспорт 00 00 000000, инн 123456789047, карта 4111 1111 1111 1111, водительское удостоверение 7777 123456, 12 34 567890, карта 4111 1111 1111 1112, инн 123456789012"
	passS, passE := runeSpanOf(t, text, "00 00 000000")
	innS, innE := runeSpanOf(t, text, "123456789047")
	cardS, cardE := runeSpanOf(t, text, "4111 1111 1111 1111")
	dlS, dlE := runeSpanOf(t, text, "7777 123456")
	badPassS, badPassE := runeSpanOf(t, text, "12 34 567890")
	badCardS, badCardE := runeSpanOf(t, text, "4111 1111 1111 1112")
	badInnS, badInnE := runeSpanOf(t, text, "123456789012")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, responseJSON(
			entityJSON("PASSPORT", passS, passE, 0.9, "rubert"),
			entityJSON("INN", innS, innE, 0.9, "rubert"),
			entityJSON("CREDIT_CARD", cardS, cardE, 0.9, "rubert"),
			entityJSON("DRIVER_LICENSE", dlS, dlE, 0.9, "rubert"),
			entityJSON("PASSPORT", badPassS, badPassE, 0.9, "rubert"),
			entityJSON("CREDIT_CARD", badCardS, badCardE, 0.9, "rubert"),
			entityJSON("INN", badInnS, badInnE, 0.9, "rubert"),
		))
	}))
	defer srv.Close()

	det := newModelDetector(t, srv)
	got, err := det(context.Background(), text)
	if err != nil {
		t.Fatalf("modelDetector() error = %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("len(candidates) = %d, want 4: %+v", len(got), got)
	}
	want := []detection.Type{
		detection.TypePassportNumber,
		detection.TypeINNPerson,
		detection.TypeBankCardNumber,
		detection.TypeDriverLicenseNumber,
	}
	for i, typ := range want {
		if got[i].Type != typ {
			t.Errorf("candidate[%d] type = %q, want %q", i, got[i].Type, typ)
		}
		if len(got[i].Sources) != 1 || got[i].Sources[0] != detection.SourceRubert {
			t.Errorf("candidate[%d] sources = %v, want [rubert]", i, got[i].Sources)
		}
	}
}

// TestModelDetectorRejectsPartialAndEnclosingDocumentSpans proves that a model
// document label is confirmed only on an exact UTF-8 byte span match: a
// one-byte partial overlap and an enclosing model span are both rejected.
func TestModelDetectorRejectsPartialAndEnclosingDocumentSpans(t *testing.T) {
	const text = "паспорт 00 00 000000"
	passS, passE := runeSpanOf(t, text, "00 00 000000")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, responseJSON(
			// Partial overlap: model span covers only the first digit.
			entityJSON("PASSPORT", passS, passS+1, 0.9, "rubert"),
			// Enclosing span: model span covers the whole phrase.
			entityJSON("PASSPORT", 0, passE, 0.9, "rubert"),
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

// TestModelDetectorRejectsGlinerStructuralLabel proves that a structurally
// valid-looking PASSPORT label from gliner fails the whole inference with a
// safe ErrInvalidResponse and no candidates, because the label is not valid for
// the gliner source.
func TestModelDetectorRejectsGlinerStructuralLabel(t *testing.T) {
	const text = "паспорт 00 00 000000"
	passS, passE := runeSpanOf(t, text, "00 00 000000")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, responseJSON(
			entityJSON("PASSPORT", passS, passE, 0.9, "gliner"),
		))
	}))
	defer srv.Close()

	det := newModelDetector(t, srv)
	got, err := det(context.Background(), text)
	if err == nil {
		t.Fatal("modelDetector() error = nil, want ErrInvalidResponse")
	}
	if !errors.Is(err, modelclient.ErrInvalidResponse) {
		t.Errorf("modelDetector() error = %v, want ErrInvalidResponse", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(candidates) = %d, want 0: %+v", len(got), got)
	}
	if strings.Contains(err.Error(), text) || strings.Contains(err.Error(), "PASSPORT") {
		t.Errorf("error %q leaks request text or label", err.Error())
	}
}

// TestModelDetectorConfirmsRubertPassportLabel proves that a valid RuBERT
// PASSPORT structural label still passes only after exact Go-validator
// confirmation, producing a single passport candidate from the rubert source.
func TestModelDetectorConfirmsRubertPassportLabel(t *testing.T) {
	const text = "паспорт 00 00 000000"
	passS, passE := runeSpanOf(t, text, "00 00 000000")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, responseJSON(
			entityJSON("PASSPORT", passS, passE, 0.9, "rubert"),
		))
	}))
	defer srv.Close()

	det := newModelDetector(t, srv)
	got, err := det(context.Background(), text)
	if err != nil {
		t.Fatalf("modelDetector() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(candidates) = %d, want 1: %+v", len(got), got)
	}
	if got[0].Type != detection.TypePassportNumber {
		t.Errorf("candidate type = %q, want %q", got[0].Type, detection.TypePassportNumber)
	}
	if len(got[0].Sources) != 1 || got[0].Sources[0] != detection.SourceRubert {
		t.Errorf("candidate sources = %v, want [rubert]", got[0].Sources)
	}
}

// TestModelDetectorRejectsGenericGlinerLabel proves that an unexpected generic
// gliner ru_pii label fails closed with a safe error matching
// modelclient.ErrInvalidResponse, an empty result, and no request text in the
// error.
func TestModelDetectorRejectsGenericGlinerLabel(t *testing.T) {
	const text = "Анна"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, responseJSON(
			entityJSON("ru_pii", 0, 4, 0.9, "gliner"),
		))
	}))
	defer srv.Close()

	det := newModelDetector(t, srv)
	got, err := det(context.Background(), text)
	if err == nil {
		t.Fatal("modelDetector() error = nil, want ErrInvalidResponse")
	}
	if !errors.Is(err, modelclient.ErrInvalidResponse) {
		t.Errorf("modelDetector() error = %v, want ErrInvalidResponse", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(candidates) = %d, want 0: %+v", len(got), got)
	}
	if strings.Contains(err.Error(), text) {
		t.Errorf("error %q leaks request text %q", err.Error(), text)
	}
}

// TestModelDetectorDropsUnknownLabel proves that a truly unknown label fails
// closed with a safe ErrInvalidResponse and no candidates.
func TestModelDetectorDropsUnknownLabel(t *testing.T) {
	const text = "Анна"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, responseJSON(
			entityJSON("BOGUS", 0, 4, 0.9, "rubert"),
		))
	}))
	defer srv.Close()

	det := newModelDetector(t, srv)
	got, err := det(context.Background(), text)
	if err == nil {
		t.Fatal("modelDetector() error = nil, want ErrInvalidResponse")
	}
	if !errors.Is(err, modelclient.ErrInvalidResponse) {
		t.Errorf("modelDetector() error = %v, want ErrInvalidResponse", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(candidates) = %d, want 0: %+v", len(got), got)
	}
	if strings.Contains(err.Error(), text) || strings.Contains(err.Error(), "BOGUS") {
		t.Errorf("error %q leaks request text or label", err.Error())
	}
}

// TestModelDetectorIntermediateClassifiedByContextual proves that GLiNER
// DATE/LOCATION candidates produced by the model boundary are promoted by
// contextual classification only with sufficient local context, and that
// ambiguous candidates are dropped rather than leaked as canonical types.
func TestModelDetectorIntermediateClassifiedByContextual(t *testing.T) {
	const text = "дата рождения 01.02.1990, встреча 05.05.2025, место рождения город Тестовск"
	birthS, birthE := runeSpanOf(t, text, "01.02.1990")
	meetS, meetE := runeSpanOf(t, text, "05.05.2025")
	locS, locE := runeSpanOf(t, text, "Тестовск")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, responseJSON(
			entityJSON("ru_pii_date", birthS, birthE, 0.85, "gliner"),
			entityJSON("ru_pii_date", meetS, meetE, 0.8, "gliner"),
			entityJSON("ru_pii_location", locS, locE, 0.8, "gliner"),
		))
	}))
	defer srv.Close()

	det := newModelDetector(t, srv)
	got, err := det(context.Background(), text)
	if err != nil {
		t.Fatalf("modelDetector() error = %v", err)
	}
	classified := contextual.Classify(text, got)
	if len(classified) != 2 {
		t.Fatalf("len(classified) = %d, want 2: %+v", len(classified), classified)
	}
	if classified[0].Type != detection.TypeBirthDate {
		t.Errorf("classified[0] type = %q, want %q", classified[0].Type, detection.TypeBirthDate)
	}
	if classified[1].Type != detection.TypeBirthPlace {
		t.Errorf("classified[1] type = %q, want %q", classified[1].Type, detection.TypeBirthPlace)
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
	p := policy.NewPolicy(allowed)
	pipe := api.NewPipeline(nil, p, g, v)
	return pipe, pipe.Handlers()
}

// newPipelineWithModel builds the real pipeline with the in-memory vault, token
// issuer, a processing policy allowing exactly the given types, and the injected
// model detector.
func newPipelineWithModel(t *testing.T, detector api.ModelDetector, allowedTypes ...string) *api.Pipeline {
	t.Helper()
	v, err := vault.NewMemory(time.Hour)
	if err != nil {
		t.Fatalf("vault.NewMemory() error = %v", err)
	}
	g, err := tokenization.New()
	if err != nil {
		t.Fatalf("tokenization.New() error = %v", err)
	}
	p := policy.NewPolicy(allowedTypes)
	return api.NewPipeline(detector, p, g, v)
}

// TestRuntimeMasksRubertAddressContext proves that a synthetic address from real
// RuBERT CITY/STREET/HOUSE labels under explicit "адрес регистрации" context is
// masked before the LLM boundary, and that a negated "не проживает" context does
// not make the address personal.
func TestRuntimeMasksRubertAddressContext(t *testing.T) {
	const text = "адрес регистрации: Москва, ул. Ленина, д. 5"
	cityS, cityE := runeSpanOf(t, text, "Москва")
	streetS, streetE := runeSpanOf(t, text, "Ленина")
	houseS, houseE := runeSpanOf(t, text, "5")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, responseJSON(
			entityJSON("CITY", cityS, cityE, 0.95, "rubert"),
			entityJSON("STREET", streetS, streetE, 0.9, "rubert"),
			entityJSON("HOUSE", houseS, houseE, 0.9, "rubert"),
		))
	}))
	defer srv.Close()

	det := newModelDetector(t, srv)
	pipe := newPipelineWithModel(t, det, string(detection.TypeAddress))

	var llmCalls int
	var llmInput string
	coord := pipe.RuntimeCoordinator(func(_ context.Context, protected string) (string, error) {
		llmCalls++
		llmInput = protected
		return protected, nil
	})

	got, err := coord.Run(context.Background(), "scope-addr", text)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if llmCalls != 1 {
		t.Fatalf("llm calls = %d, want 1", llmCalls)
	}
	if !strings.Contains(llmInput, "<ADDRESS_") {
		t.Errorf("llm input %q missing <ADDRESS_ token", llmInput)
	}
	for _, leak := range []string{"Москва", "Ленина", "д. 5", text} {
		if strings.Contains(llmInput, leak) {
			t.Errorf("llm input %q leaks %q", llmInput, leak)
		}
	}
	if !strings.Contains(got, "Москва") || !strings.Contains(got, "Ленина") || !strings.Contains(got, "д. 5") {
		t.Errorf("restored result %q missing original address", got)
	}

	// Negated context must not make the address personal.
	const negText = "не проживает в Москве, ул. Ленина, д. 5"
	negCityS, negCityE := runeSpanOf(t, negText, "Москве")
	negStreetS, negStreetE := runeSpanOf(t, negText, "Ленина")
	negHouseS, negHouseE := runeSpanOf(t, negText, "5")
	negSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, responseJSON(
			entityJSON("CITY", negCityS, negCityE, 0.95, "rubert"),
			entityJSON("STREET", negStreetS, negStreetE, 0.9, "rubert"),
			entityJSON("HOUSE", negHouseS, negHouseE, 0.9, "rubert"),
		))
	}))
	defer negSrv.Close()

	negDet := newModelDetector(t, negSrv)
	negPipe := newPipelineWithModel(t, negDet, string(detection.TypeAddress))
	resp, err := negPipe.Handlers().Detect(context.Background(), api.DetectRequest{
		Text: negText, IncludeNonPersonal: true,
	})
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	if resp.HasPersonalData {
		t.Errorf("HasPersonalData = true, want false for negated context")
	}
	for _, e := range resp.Entities {
		if e.Personal {
			t.Errorf("entity %s personal = true, want false for negated context", e.Type)
		}
		if !e.ReviewRecommended {
			t.Errorf("entity %s ReviewRecommended = false, want true", e.Type)
		}
	}
}

// TestRuntimePolicyProjection proves that processing policy projection splits
// overlapping candidates before merge: a disabled type never changes the
// operational winner, components, or ownership evidence of an allowed entity.
// When FULL_NAME is disabled, the allowed CITY stays ambiguous and
// review-recommended, so the runtime fails closed before the LLM; when CITY is
// disabled, the allowed FULL_NAME is masked and restored.
func TestRuntimePolicyProjection(t *testing.T) {
	const text = "Иван, email: ivan@example.com"
	nameS, nameE := runeSpanOf(t, text, "Иван")

	// overlapSrv returns a RuBERT CITY and a GLiNER ru_pii_person on the same
	// span, with CITY having the higher confidence.
	overlapSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, responseJSON(
			entityJSON("CITY", nameS, nameE, 0.95, "rubert"),
			entityJSON("ru_pii_person", nameS, nameE, 0.8, "gliner"),
		))
	}))
	defer overlapSrv.Close()

	t.Run("allowed ambiguous city fails closed before llm", func(t *testing.T) {
		det := newModelDetector(t, overlapSrv)
		pipe := newPipelineWithModel(t, det,
			string(detection.TypeAddressCity), string(detection.TypeEmail))

		var llmCalls int
		coord := pipe.RuntimeCoordinator(func(_ context.Context, protected string) (string, error) {
			llmCalls++
			return protected, nil
		})

		got, err := coord.Run(context.Background(), "scope-city", text)
		if !errors.Is(err, runtime.ErrProtectFailed) {
			t.Fatalf("Run() error = %v, want ErrProtectFailed", err)
		}
		if got != "" {
			t.Errorf("Run() result = %q, want empty on fail-closed", got)
		}
		if llmCalls != 0 {
			t.Fatalf("llm calls = %d, want 0 (chain must stop before the LLM)", llmCalls)
		}
	})

	t.Run("disabled city does not suppress allowed full name", func(t *testing.T) {
		det := newModelDetector(t, overlapSrv)
		pipe := newPipelineWithModel(t, det,
			string(detection.TypeFullName), string(detection.TypeEmail))

		var llmCalls int
		var llmInput string
		coord := pipe.RuntimeCoordinator(func(_ context.Context, protected string) (string, error) {
			llmCalls++
			llmInput = protected
			return protected, nil
		})

		got, err := coord.Run(context.Background(), "scope-name", text)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if llmCalls != 1 {
			t.Fatalf("llm calls = %d, want 1", llmCalls)
		}
		// The allowed FULL_NAME wins the overlap and is masked together with
		// the email before the LLM boundary.
		for _, leak := range []string{"Иван", "ivan@example.com"} {
			if strings.Contains(llmInput, leak) {
				t.Errorf("llm input %q leaks %q", llmInput, leak)
			}
		}
		if !strings.Contains(llmInput, "<FULL_NAME_") || !strings.Contains(llmInput, "<EMAIL_") {
			t.Errorf("llm input %q missing FULL_NAME and EMAIL tokens", llmInput)
		}
		if !strings.Contains(got, "Иван") || !strings.Contains(got, "ivan@example.com") {
			t.Errorf("restored result %q missing original values", got)
		}
	})
}

// TestRuntimeGenericGlinerFailsBeforeLLM proves that an unexpected generic
// gliner ru_pii label fails protection before the LLM boundary is reached.
func TestRuntimeGenericGlinerFailsBeforeLLM(t *testing.T) {
	const text = "Анна"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, responseJSON(
			entityJSON("ru_pii", 0, 4, 0.9, "gliner"),
		))
	}))
	defer srv.Close()

	det := newModelDetector(t, srv)
	pipe := newPipelineWithModel(t, det, string(detection.TypeFullName))

	var llmCalls int
	coord := pipe.RuntimeCoordinator(func(_ context.Context, protected string) (string, error) {
		llmCalls++
		return protected, nil
	})

	got, err := coord.Run(context.Background(), "scope-generic", text)
	if err == nil {
		t.Fatal("Run() error = nil, want error")
	}
	if got != "" {
		t.Errorf("Run() result = %q, want empty", got)
	}
	if llmCalls != 0 {
		t.Errorf("llm calls = %d, want 0", llmCalls)
	}
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
	regMetrics := metrics.New(metrics.Options{})

	cfg := config.Config{LLM: config.LLMConfig{Timeout: config.DefaultLLMTimeout}}
	mux, err := buildRouter(cfg, pipe, handlers, op, regMetrics)
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

// newRawModelDetector builds a real modelclient.Client pointed at srv and a
// real detection registry, then returns the modelDetector adapter under test,
// without wrapping the server handler. It is used by tests that control the
// /plan_windows response themselves.
func newRawModelDetector(t *testing.T, srv *httptest.Server) api.ModelDetector {
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

// TestModelDetectorPlansThenInfersMultipleWindows proves that the production
// modelDetector requests a window plan and then issues multiple /infer calls
// (one per window) before mapping the reconstructed entities to candidates.
func TestModelDetectorPlansThenInfersMultipleWindows(t *testing.T) {
	var planCalls, inferCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/plan_windows" {
			atomic.AddInt32(&planCalls, 1)
			fmt.Fprint(w, `{"model":"redmadrobot-rnd/rubert-base-pii-ner","total_count":2,"max_window_tokens":510,"windows":[{"start":0,"end":6,"token_count":3},{"start":4,"end":10,"token_count":3}]}`)
			return
		}
		atomic.AddInt32(&inferCalls, 1)
		fmt.Fprint(w, responseJSON(entityJSON("FIRST_NAME", 0, 5, 0.9, "rubert")))
	}))
	defer srv.Close()

	det := newRawModelDetector(t, srv)
	got, err := det(context.Background(), "abcdefghij")
	if err != nil {
		t.Fatalf("modelDetector() error = %v", err)
	}
	if atomic.LoadInt32(&planCalls) != 1 {
		t.Fatalf("plan calls = %d, want 1", planCalls)
	}
	if atomic.LoadInt32(&inferCalls) < 2 {
		t.Fatalf("infer calls = %d, want >= 2", inferCalls)
	}
	if len(got) == 0 {
		t.Fatal("no candidates returned")
	}
}

// TestModelDetectorRejectsUnknownSourceFromAnyWindow proves that an entity with
// an unknown model source in any window fails the whole inference with a safe
// ErrInvalidResponse and no candidates.
func TestModelDetectorRejectsUnknownSourceFromAnyWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/plan_windows" {
			fmt.Fprint(w, `{"model":"redmadrobot-rnd/rubert-base-pii-ner","total_count":2,"max_window_tokens":510,"windows":[{"start":0,"end":6,"token_count":3},{"start":4,"end":10,"token_count":3}]}`)
			return
		}
		fmt.Fprint(w, responseJSON(entityJSON("FULL_NAME", 0, 5, 0.9, "other")))
	}))
	defer srv.Close()

	det := newRawModelDetector(t, srv)
	got, err := det(context.Background(), "abcdefghij")
	if err == nil {
		t.Fatal("modelDetector() error = nil, want ErrInvalidResponse")
	}
	if !errors.Is(err, modelclient.ErrInvalidResponse) {
		t.Errorf("modelDetector() error = %v, want ErrInvalidResponse", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(candidates) = %d, want 0: %+v", len(got), got)
	}
	if strings.Contains(err.Error(), "other") {
		t.Errorf("error %q leaks source", err.Error())
	}
}

// TestModelDetectorRejectsUnknownLabelFromAnyWindow proves that an entity with
// a label outside the source allowlist in any window fails the whole inference
// with a safe ErrInvalidResponse and no candidates.
func TestModelDetectorRejectsUnknownLabelFromAnyWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/plan_windows" {
			fmt.Fprint(w, `{"model":"redmadrobot-rnd/rubert-base-pii-ner","total_count":2,"max_window_tokens":510,"windows":[{"start":0,"end":6,"token_count":3},{"start":4,"end":10,"token_count":3}]}`)
			return
		}
		fmt.Fprint(w, responseJSON(entityJSON("BOGUS", 0, 5, 0.9, "rubert")))
	}))
	defer srv.Close()

	det := newRawModelDetector(t, srv)
	got, err := det(context.Background(), "abcdefghij")
	if err == nil {
		t.Fatal("modelDetector() error = nil, want ErrInvalidResponse")
	}
	if !errors.Is(err, modelclient.ErrInvalidResponse) {
		t.Errorf("modelDetector() error = %v, want ErrInvalidResponse", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(candidates) = %d, want 0: %+v", len(got), got)
	}
	if strings.Contains(err.Error(), "BOGUS") {
		t.Errorf("error %q leaks label", err.Error())
	}
}

// --- Public API integration through the real router ---

// doJSON sends a JSON request to handler with optional headers and returns the
// recorder.
func doJSON(t *testing.T, handler http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
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
	return rec
}

// newPublicHandler builds the real router without incoming authorization.
func newPublicHandler(t *testing.T) http.Handler {
	t.Helper()
	pipe, handlers := newTestPipeline(t)
	op := process.NewOperation(process.NewStore(), func(ctx context.Context, payload string) (string, error) {
		res, err := handlers.Tokenize(ctx, api.TokenizeRequest{Text: payload, ScopeID: processScope})
		if err != nil {
			return "", err
		}
		return res.TokenizedText, nil
	})
	regMetrics := metrics.New(metrics.Options{})
	cfg := config.Config{LLM: config.LLMConfig{Timeout: config.DefaultLLMTimeout}}
	handler, err := buildRouter(cfg, pipe, handlers, op, regMetrics)
	if err != nil {
		t.Fatalf("buildRouter() error = %v", err)
	}
	return handler
}

// TestPublicRouterIntegration proves functional routes are available without
// an Authorization header and are not gated by a checker/production profile.
func TestPublicRouterIntegration(t *testing.T) {
	handler := newPublicHandler(t)

	rec := doJSON(t, handler, http.MethodPost, "/process",
		`{"payload":"Клиент Иванов Иван, email ivanov@example.com","payload_id":"id-1"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /process status = %d, want %d; body = %q", rec.Code, http.StatusOK, rec.Body.String())
	}

	for _, path := range []string{"/health/live", "/health/ready"} {
		rec := doJSON(t, handler, http.MethodGet, path, "", nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want %d", path, rec.Code, http.StatusOK)
		}
	}

	routes := []struct {
		method   string
		path     string
		body     string
		wantCode int
	}{
		{http.MethodPost, "/v1/pii/detect", `{"text":"email ivanov@example.com"}`, http.StatusOK},
		{http.MethodPost, "/v1/pii/tokenize", `{"text":"email ivanov@example.com","scope_id":"s1"}`, http.StatusOK},
		{http.MethodDelete, "/v1/pii/scopes/s2", "", http.StatusOK},
		{http.MethodPost, "/v1/runtime/chat", `{"text":"hello","scope_id":"s3"}`, http.StatusServiceUnavailable},
		{http.MethodGet, "/metrics", "", http.StatusOK},
	}
	for _, rt := range routes {
		rec := doJSON(t, handler, rt.method, rt.path, rt.body, nil)
		if rec.Code != rt.wantCode {
			t.Errorf("%s %s status = %d, want %d; body = %q", rt.method, rt.path, rec.Code, rt.wantCode, rec.Body.String())
		}
	}
}

// TestPublicRouterIgnoresIdentityHeaders proves legacy identity headers neither
// grant nor deny access and cannot alter the static processing policy.
func TestPublicRouterIgnoresIdentityHeaders(t *testing.T) {
	handler := newPublicHandler(t)
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"no headers", nil},
		{"arbitrary authorization", map[string]string{"Authorization": "Bearer wrong-key"}},
		{"spoofed system id", map[string]string{"X-System-ID": "sys-a"}},
	}
	for _, tc := range cases {
		rec := doJSON(t, handler, http.MethodPost, "/v1/pii/tokenize",
			`{"text":"email ivanov@example.com","scope_id":"s1"}`, tc.headers)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want %d; body = %q", tc.name, rec.Code, http.StatusOK, rec.Body.String())
		}
	}
}

// TestPublicRouterUsesFullTypeSet proves all canonical types are processed by a
// single static policy rather than selected per consumer.
func TestPublicRouterUsesFullTypeSet(t *testing.T) {
	handler := newPublicHandler(t)
	const text = "Клиент Иванов Иван, email ivanov@example.com, телефон +7 900 123-45-67"
	rec := doJSON(t, handler, http.MethodPost, "/v1/pii/tokenize",
		`{"text":"`+text+`","scope_id":"s1"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("tokenize status = %d, want %d; body = %q", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp api.TokenizeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error = %v", err)
	}
	if !strings.Contains(resp.TokenizedText, "<EMAIL_") {
		t.Errorf("tokenized text %q missing EMAIL token", resp.TokenizedText)
	}
	if !strings.Contains(resp.TokenizedText, "<PHONE_") {
		t.Errorf("tokenized text %q missing PHONE token", resp.TokenizedText)
	}
}

// TestPublicScopeRoundTrip proves the caller scope is the sole namespace and
// detokenization requires no identity or demasking capability.
func TestPublicScopeRoundTrip(t *testing.T) {
	handler := newPublicHandler(t)
	const scope = "shared-scope"
	rec := doJSON(t, handler, http.MethodPost, "/v1/pii/tokenize",
		`{"text":"email ivanov@example.com","scope_id":"`+scope+`"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("tokenize status = %d, want %d; body = %q", rec.Code, http.StatusOK, rec.Body.String())
	}
	var tokResp api.TokenizeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &tokResp); err != nil {
		t.Fatalf("decode error = %v", err)
	}
	rec2 := doJSON(t, handler, http.MethodPost, "/v1/pii/detokenize",
		`{"text":"`+tokResp.TokenizedText+`","scope_id":"`+scope+`","mode":"strict"}`, nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("detokenize status = %d, want %d; body = %q", rec2.Code, http.StatusOK, rec2.Body.String())
	}
	var detResp api.DetokenizeResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &detResp); err != nil {
		t.Fatalf("detokenize decode error = %v", err)
	}
	if !strings.Contains(detResp.RestoredText, "ivanov@example.com") {
		t.Errorf("restored text %q missing original email", detResp.RestoredText)
	}
}

// TestPublicPipelineUsesAllCanonicalTypes proves the static public policy is
// built from the registry rather than a consumer-specific enabled type list.
func TestPublicPipelineUsesAllCanonicalTypes(t *testing.T) {
	reg, err := detection.New()
	if err != nil {
		t.Fatalf("detection.New() error = %v", err)
	}
	allowed := make([]string, 0, len(reg.Types()))
	for _, typ := range reg.Types() {
		allowed = append(allowed, string(typ))
	}
	p := policy.NewPolicy(allowed)
	for _, typ := range reg.Types() {
		if !p.AllowsType(string(typ)) {
			t.Errorf("public policy disallows %s", typ)
		}
	}
}

// TestPipelineUsesCallerScope proves the pipeline always uses the raw caller
// scope without deriving an identity-dependent namespace.
func TestPipelineUsesCallerScope(t *testing.T) {
	_, handlers := newTestPipeline(t)
	const scope = "raw-scope-fallback"
	res, err := handlers.Tokenize(context.Background(), api.TokenizeRequest{
		Text: "email ivanov@example.com", ScopeID: scope,
	})
	if err != nil {
		t.Fatalf("Tokenize() error = %v", err)
	}
	if !strings.Contains(res.TokenizedText, "<EMAIL_") {
		t.Fatalf("tokenized text %q missing EMAIL token", res.TokenizedText)
	}
	det, err := handlers.Detokenize(context.Background(), api.DetokenizeRequest{
		Text: res.TokenizedText, ScopeID: scope, Mode: api.ModeStrict,
	})
	if err != nil {
		t.Fatalf("Detokenize() error = %v", err)
	}
	if !strings.Contains(det.RestoredText, "ivanov@example.com") {
		t.Errorf("restored text %q missing original email", det.RestoredText)
	}
}

// TestNewServerHasNonZeroTimeouts proves the production server helper sets all
// four timeouts to non-zero values.
func TestNewServerHasNonZeroTimeouts(t *testing.T) {
	const writeTimeout = 100 * time.Second
	srv := newServer(":0", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}), writeTimeout)
	if srv.ReadHeaderTimeout <= 0 {
		t.Errorf("ReadHeaderTimeout = %v, want > 0", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout <= 0 {
		t.Errorf("ReadTimeout = %v, want > 0", srv.ReadTimeout)
	}
	if srv.WriteTimeout <= 0 {
		t.Errorf("WriteTimeout = %v, want > 0", srv.WriteTimeout)
	}
	if srv.WriteTimeout != writeTimeout {
		t.Errorf("WriteTimeout = %v, want configured budget %v", srv.WriteTimeout, writeTimeout)
	}
	if srv.IdleTimeout <= 0 {
		t.Errorf("IdleTimeout = %v, want > 0", srv.IdleTimeout)
	}
}

func TestServerWriteTimeoutCoversReadAndUpstreamBudgets(t *testing.T) {
	cfg := config.Config{
		ModelClientTimeout: 30 * time.Second,
		LLM:                config.LLMConfig{Timeout: 60 * time.Second},
	}
	want := serverReadTimeout + cfg.ModelClientTimeout + cfg.LLM.Timeout + serverResponseMargin
	if got := serverWriteTimeout(cfg); got != want {
		t.Fatalf("serverWriteTimeout() = %v, want %v", got, want)
	}
	if got := serverWriteTimeout(cfg); got <= serverReadTimeout+cfg.ModelClientTimeout+cfg.LLM.Timeout {
		t.Fatalf("serverWriteTimeout() = %v, want response margin above full budget", got)
	}
}

func TestAdmissionKeepsOperationalEndpointsAvailableWhenFull(t *testing.T) {
	release := make(chan struct{})
	occupied := make(chan struct{})
	handler := newAdmissionMiddleware(1)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/pii/detect" {
			close(occupied)
			<-release
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	done := make(chan struct{})
	go func() {
		defer close(done)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/pii/detect", nil)
		handler.ServeHTTP(rec, req)
	}()
	<-occupied

	for _, path := range []string{"/health/live", "/health/ready", "/metrics"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Errorf("GET %s status = %d, want %d while data plane is full", path, rec.Code, http.StatusNoContent)
		}
	}

	close(release)
	<-done
}

// TestAdmissionRejectsWhenFull proves that when all permits are held the next
// request is rejected immediately with 429, Retry-After: 1, a fixed safe JSON
// body, and the downstream handler is not invoked.
func TestAdmissionRejectsWhenFull(t *testing.T) {
	const canary = "ADMISSION_CANARY_77777"
	var downstreamCalled atomic.Bool
	release := make(chan struct{})
	handler := newAdmissionMiddleware(1)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downstreamCalled.Store(true)
		<-release
		w.WriteHeader(http.StatusOK)
	}))

	// First request acquires the single permit and blocks.
	done := make(chan struct{})
	go func() {
		defer close(done)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		handler.ServeHTTP(rec, req)
	}()

	// Wait until the first request holds the permit.
	deadline := time.Now().Add(2 * time.Second)
	for !downstreamCalled.Load() {
		if time.Now().After(deadline) {
			t.Fatal("first request never reached downstream")
		}
		time.Sleep(time.Millisecond)
	}

	// Second request must be rejected immediately without reaching downstream.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", strings.NewReader(canary))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want %q", got, "1")
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := rec.Body.String(); got != `{"error":"service overloaded"}` {
		t.Errorf("body = %q, want fixed safe body", got)
	}
	if strings.Contains(rec.Body.String(), canary) {
		t.Errorf("body leaks canary: %q", rec.Body.String())
	}

	// Release the first request and confirm the permit is reused.
	close(release)
	<-done
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Errorf("status after release = %d, want %d (permit not reused)", rec2.Code, http.StatusOK)
	}
}

// TestAdmissionPermitReleasedOnPanic proves a permit is released even when the
// downstream handler panics, so a later request is admitted.
func TestAdmissionPermitReleasedOnPanic(t *testing.T) {
	var calls atomic.Int32
	handler := newAdmissionMiddleware(1)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			panic("boom")
		}
		w.WriteHeader(http.StatusOK)
	}))

	func() {
		defer func() { _ = recover() }()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		handler.ServeHTTP(rec, req)
	}()

	// The permit must be released after the panic, so the second call is
	// admitted and reaches the downstream handler.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status after panic = %d, want %d (permit not released)", rec.Code, http.StatusOK)
	}
}
