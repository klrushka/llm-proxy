package harness

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/klrushka/llm-proxy/internal/testcorpus"
)

// fakePredictor returns a fixed set of predicted spans for every input.
type fakePredictor struct {
	spans []PredictedSpan
	err   error
}

func (f fakePredictor) Predict(_ context.Context, _ string) ([]PredictedSpan, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.spans, nil
}

// mapPredictor returns the spans registered for the exact input string.
type mapPredictor struct {
	byInput map[string][]PredictedSpan
}

func (m mapPredictor) Predict(_ context.Context, input string) ([]PredictedSpan, error) {
	return m.byInput[input], nil
}

func corpusFor(t *testing.T, cases []testcorpus.Case) testcorpus.Corpus {
	t.Helper()
	return testcorpus.Corpus{Version: "1", OffsetUnit: "utf8_byte", Cases: cases}
}

// byteSpan returns the UTF-8 byte offsets [start, end) of the first occurrence
// of needle in haystack, failing the test if it is not found. Computing offsets
// from the string avoids fragile hand-written byte numbers.
func byteSpan(t *testing.T, haystack, needle string) (int, int) {
	t.Helper()
	start := strings.Index(haystack, needle)
	if start < 0 {
		t.Fatalf("needle %q not found in %q", needle, haystack)
	}
	return start, start + len(needle)
}

func TestRunPerfectPredictor(t *testing.T) {
	input := "email: a@b.com"
	email := "a@b.com"
	es, ee := byteSpan(t, input, email)

	cases := []testcorpus.Case{
		{
			ID: "c1", Input: input, ExpectedMask: "email: <EMAIL>",
			Spans: []testcorpus.Span{{Type: "EMAIL", Start: es, End: ee, Value: email, Personal: true, MaskToken: "<EMAIL>"}},
		},
	}
	corpus := corpusFor(t, cases)
	p := fakePredictor{spans: []PredictedSpan{{Type: "EMAIL", Start: es, End: ee, Personal: true}}}

	rep, err := Run(context.Background(), corpus, p)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if rep.MaskingQuality != 1.0 {
		t.Errorf("MaskingQuality = %v, want 1.0", rep.MaskingQuality)
	}
	if rep.RestorationExactRate != 1.0 {
		t.Errorf("RestorationExactRate = %v, want 1.0", rep.RestorationExactRate)
	}
	if rep.Summary.F1Types != 1 || rep.Summary.F1 != 1.0 {
		t.Errorf("Summary = %+v, want 1 type with F1 1.0", rep.Summary)
	}
	if len(rep.Cases) != 1 || !rep.Cases[0].RestorationExact {
		t.Errorf("case restoration = %+v, want exact", rep.Cases)
	}
}

func TestRunErroneousPredictor(t *testing.T) {
	// Ground truth has EMAIL and PHONE. The predictor misses EMAIL (FN),
	// predicts a spurious PHONE at the email coordinates (FP), and predicts a
	// hard-negative LAST_NAME that must not be detected (FP on hard negative).
	c1Input := "email: a@b.com phone: +7 900 123-45-67"
	email := "a@b.com"
	phone := "+7 900 123-45-67"
	es, ee := byteSpan(t, c1Input, email)
	ps, pe := byteSpan(t, c1Input, phone)

	c2Input := "Пушкин — поэт"
	pushkin := "Пушкин"
	ls, le := byteSpan(t, c2Input, pushkin)

	cases := []testcorpus.Case{
		{
			ID: "c1", Input: c1Input,
			ExpectedMask: "email: <EMAIL> phone: <PHONE>",
			Spans: []testcorpus.Span{
				{Type: "EMAIL", Start: es, End: ee, Value: email, Personal: true, MaskToken: "<EMAIL>"},
				{Type: "PHONE", Start: ps, End: pe, Value: phone, Personal: true, MaskToken: "<PHONE>"},
			},
		},
		{
			ID: "c2", Input: c2Input, ExpectedMask: "Пушкин — поэт",
			Spans: []testcorpus.Span{{Type: "LAST_NAME", Start: ls, End: le, Value: pushkin, Personal: false}},
		},
	}
	corpus := corpusFor(t, cases)
	p := mapPredictor{byInput: map[string][]PredictedSpan{
		c1Input: {{Type: "PHONE", Start: es, End: ee, Personal: true}},     // spurious, wrong offset/type
		c2Input: {{Type: "LAST_NAME", Start: ls, End: le, Personal: true}}, // hard negative wrongly detected
	}}

	rep, err := Run(context.Background(), corpus, p)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	byType := map[string]struct {
		tp, fp, fn int
	}{}
	for _, pt := range rep.PerType {
		byType[pt.Type] = struct{ tp, fp, fn int }{pt.TP, pt.FP, pt.FN}
	}

	if e := byType["EMAIL"]; e.tp != 0 || e.fp != 0 || e.fn != 1 {
		t.Errorf("EMAIL tp/fp/fn = %d/%d/%d, want 0/0/1", e.tp, e.fp, e.fn)
	}
	if p := byType["PHONE"]; p.tp != 0 || p.fp != 1 || p.fn != 1 {
		t.Errorf("PHONE tp/fp/fn = %d/%d/%d, want 0/1/1", p.tp, p.fp, p.fn)
	}
	if l := byType["LAST_NAME"]; l.tp != 0 || l.fp != 1 || l.fn != 0 {
		t.Errorf("LAST_NAME tp/fp/fn = %d/%d/%d, want 0/1/0", l.tp, l.fp, l.fn)
	}

	// Masking quality must be below 1.0 because the EMAIL span is not masked.
	if rep.MaskingQuality >= 1.0 {
		t.Errorf("MaskingQuality = %v, want < 1.0 for erroneous predictions", rep.MaskingQuality)
	}
	// Restoration is still exact because the round trip is lossless for the
	// spans that were actually masked.
	if rep.RestorationExactRate != 1.0 {
		t.Errorf("RestorationExactRate = %v, want 1.0", rep.RestorationExactRate)
	}
}

func TestRunPersonalFalsePredictionNotFP(t *testing.T) {
	// A Personal=false prediction is not a positive prediction and must not
	// create a false positive, even if it matches a hard-negative span.
	input := "Пушкин — поэт"
	pushkin := "Пушкин"
	ls, le := byteSpan(t, input, pushkin)

	cases := []testcorpus.Case{
		{
			ID: "c1", Input: input, ExpectedMask: "Пушкин — поэт",
			Spans: []testcorpus.Span{{Type: "LAST_NAME", Start: ls, End: le, Value: pushkin, Personal: false}},
		},
	}
	corpus := corpusFor(t, cases)
	p := fakePredictor{spans: []PredictedSpan{{Type: "LAST_NAME", Start: ls, End: le, Personal: false}}}

	rep, err := Run(context.Background(), corpus, p)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for _, pt := range rep.PerType {
		if pt.Type != "LAST_NAME" {
			continue
		}
		if pt.TP != 0 || pt.FP != 0 || pt.FN != 0 {
			t.Errorf("LAST_NAME tp/fp/fn = %d/%d/%d, want 0/0/0 (Personal=false is not a positive)", pt.TP, pt.FP, pt.FN)
		}
	}
}

func TestRunPredictorErrorFailsRun(t *testing.T) {
	corpus := corpusFor(t, []testcorpus.Case{
		{ID: "c1", Input: "x", ExpectedMask: "x"},
	})
	p := fakePredictor{err: errors.New("boom")}
	if _, err := Run(context.Background(), corpus, p); err == nil {
		t.Fatal("Run() expected error when predictor fails")
	}
}

func TestRunEmptyCorpus(t *testing.T) {
	corpus := corpusFor(t, nil)
	rep, err := Run(context.Background(), corpus, fakePredictor{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if rep.MaskingQuality != 0 || rep.RestorationExactRate != 0 {
		t.Errorf("empty corpus quality/restore = %v/%v, want 0/0", rep.MaskingQuality, rep.RestorationExactRate)
	}
	if rep.Summary.F1Types != 0 || rep.Summary.PrecisionTypes != 0 || rep.Summary.RecallTypes != 0 {
		t.Errorf("empty corpus summary types = %+v, want all 0", rep.Summary)
	}
}

func TestCanonicalMask(t *testing.T) {
	input := "Иванов Иван Иванович, email: ivanov@example.com"
	email := "ivanov@example.com"
	es, ee := byteSpan(t, input, email)
	fullName := "Иванов Иван Иванович"
	fs, fe := byteSpan(t, input, fullName)

	spans := []PredictedSpan{
		{Type: "EMAIL", Start: es, End: ee, Personal: true},
		{Type: "FULL_NAME", Start: fs, End: fe, Personal: false}, // non-personal: unchanged
	}
	got := canonicalMask(input, spans)
	want := "Иванов Иван Иванович, email: <EMAIL>"
	if got != want {
		t.Errorf("canonicalMask = %q, want %q", got, want)
	}
}

func TestRoundTripRestoresOriginal(t *testing.T) {
	input := "Телефон: +7 900 123-45-67"
	phone := "+7 900 123-45-67"
	ps, pe := byteSpan(t, input, phone)
	spans := []PredictedSpan{{Type: "PHONE", Start: ps, End: pe, Personal: true}}
	restored, err := roundTrip(context.Background(), "rt", input, spans)
	if err != nil {
		t.Fatalf("roundTrip() error = %v", err)
	}
	if restored != input {
		t.Errorf("roundTrip = %q, want %q", restored, input)
	}
}

func TestRoundTripInvalidSpanFails(t *testing.T) {
	input := "abc"
	spans := []PredictedSpan{{Type: "EMAIL", Start: 0, End: 99, Personal: true}}
	if _, err := roundTrip(context.Background(), "rt", input, spans); err == nil {
		t.Fatal("roundTrip() expected error for out-of-bounds span")
	}
}

func TestRulesPredictorOnCorpus(t *testing.T) {
	corpus, err := testcorpus.Load("../../testdata/pii-corpus.json")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	rep, err := Run(context.Background(), corpus, RulesPredictor{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// The rules pipeline detects EMAIL and PHONE exactly, so those types must
	// have perfect precision/recall. FULL_NAME is not covered by rules, so its
	// recall must be 0 (an honest miss, not a fabricated success).
	byType := map[string]struct {
		tp, fn int
	}{}
	for _, pt := range rep.PerType {
		byType[pt.Type] = struct{ tp, fn int }{pt.TP, pt.FN}
	}
	if e := byType["EMAIL"]; e.tp != 1 || e.fn != 0 {
		t.Errorf("EMAIL tp/fn = %d/%d, want 1/0", e.tp, e.fn)
	}
	if p := byType["PHONE"]; p.tp != 1 || p.fn != 0 {
		t.Errorf("PHONE tp/fn = %d/%d, want 1/0", p.tp, p.fn)
	}
	if f := byType["FULL_NAME"]; f.tp != 0 || f.fn != 1 {
		t.Errorf("FULL_NAME tp/fn = %d/%d, want 0/1 (rules do not detect names)", f.tp, f.fn)
	}
	// The hard-negative LAST_NAME must not be detected.
	if l := byType["LAST_NAME"]; l.tp != 0 || l.fn != 0 {
		t.Errorf("LAST_NAME tp/fn = %d/%d, want 0/0", l.tp, l.fn)
	}
}
