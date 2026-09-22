package scorer

import (
	"math"
	"testing"
)

func approx(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestDetectionMetricsPerfect(t *testing.T) {
	gt := []Span{{Type: "EMAIL", Start: 0, End: 5}, {Type: "PHONE", Start: 10, End: 20}}
	pred := []Span{{Type: "EMAIL", Start: 0, End: 5}, {Type: "PHONE", Start: 10, End: 20}}
	m := DetectionMetrics(gt, nil, pred)

	e := m["EMAIL"]
	if e.TP != 1 || e.FP != 0 || e.FN != 0 {
		t.Fatalf("EMAIL tp/fp/fn = %d/%d/%d, want 1/0/0", e.TP, e.FP, e.FN)
	}
	if !e.PrecisionDefined || !approx(e.Precision, 1.0) {
		t.Errorf("EMAIL precision = %v (defined=%v), want 1.0", e.Precision, e.PrecisionDefined)
	}
	if !e.RecallDefined || !approx(e.Recall, 1.0) {
		t.Errorf("EMAIL recall = %v (defined=%v), want 1.0", e.Recall, e.RecallDefined)
	}
	if !e.F1Defined || !approx(e.F1, 1.0) {
		t.Errorf("EMAIL f1 = %v (defined=%v), want 1.0", e.F1, e.F1Defined)
	}
}

func TestDetectionMetricsMissedAndSpurious(t *testing.T) {
	// One ground-truth EMAIL is missed (FN), one spurious PHONE is predicted
	// (FP), one EMAIL is correct (TP).
	gt := []Span{{Type: "EMAIL", Start: 0, End: 5}, {Type: "EMAIL", Start: 10, End: 15}}
	pred := []Span{{Type: "EMAIL", Start: 0, End: 5}, {Type: "PHONE", Start: 20, End: 30}}

	m := DetectionMetrics(gt, nil, pred)

	e := m["EMAIL"]
	if e.TP != 1 || e.FP != 0 || e.FN != 1 {
		t.Fatalf("EMAIL tp/fp/fn = %d/%d/%d, want 1/0/1", e.TP, e.FP, e.FN)
	}
	if !approx(e.Precision, 1.0) || !approx(e.Recall, 0.5) || !approx(e.F1, 2.0/3.0) {
		t.Errorf("EMAIL p/r/f1 = %v/%v/%v, want 1/0.5/0.666", e.Precision, e.Recall, e.F1)
	}

	p := m["PHONE"]
	if p.TP != 0 || p.FP != 1 || p.FN != 0 {
		t.Fatalf("PHONE tp/fp/fn = %d/%d/%d, want 0/1/0", p.TP, p.FP, p.FN)
	}
	if !approx(p.Precision, 0.0) || !p.PrecisionDefined {
		t.Errorf("PHONE precision = %v (defined=%v), want 0.0 defined", p.Precision, p.PrecisionDefined)
	}
	if p.RecallDefined {
		t.Errorf("PHONE recall should be undefined (no ground truth), got defined")
	}
}

func TestDetectionMetricsHardNegativeIsFalsePositive(t *testing.T) {
	// A prediction exactly matching a non-personal (hard-negative) span must be
	// a false positive, not a true positive.
	gtNeg := []Span{{Type: "LAST_NAME", Start: 0, End: 7}}
	pred := []Span{{Type: "LAST_NAME", Start: 0, End: 7}}

	m := DetectionMetrics(nil, gtNeg, pred)
	l := m["LAST_NAME"]
	if l.TP != 0 || l.FP != 1 || l.FN != 0 {
		t.Fatalf("LAST_NAME tp/fp/fn = %d/%d/%d, want 0/1/0", l.TP, l.FP, l.FN)
	}
	if l.FPOnHardNegative != 1 {
		t.Errorf("FPOnHardNegative = %d, want 1", l.FPOnHardNegative)
	}
	if !l.PrecisionDefined || !approx(l.Precision, 0.0) {
		t.Errorf("LAST_NAME precision = %v (defined=%v), want 0.0 defined", l.Precision, l.PrecisionDefined)
	}
	if l.RecallDefined {
		t.Errorf("LAST_NAME recall should be undefined (no personal ground truth)")
	}
}

func TestDetectionMetricsWrongTypeIsFalsePositive(t *testing.T) {
	// A prediction with the right coordinates but the wrong type must not match
	// the ground-truth personal span.
	gt := []Span{{Type: "EMAIL", Start: 0, End: 5}}
	pred := []Span{{Type: "PHONE", Start: 0, End: 5}}

	m := DetectionMetrics(gt, nil, pred)
	if e := m["EMAIL"]; e.TP != 0 || e.FN != 1 {
		t.Errorf("EMAIL tp/fn = %d/%d, want 0/1", e.TP, e.FN)
	}
	if p := m["PHONE"]; p.TP != 0 || p.FP != 1 {
		t.Errorf("PHONE tp/fp = %d/%d, want 0/1", p.TP, p.FP)
	}
}

func TestDetectionMetricsZeroDenominators(t *testing.T) {
	// A type with no ground truth and no predictions: all metrics undefined.
	m := DetectionMetrics(nil, nil, nil)
	if len(m) != 0 {
		t.Fatalf("expected empty result for empty input, got %d types", len(m))
	}

	// A type present only as a hard negative: precision undefined (no TP+FP
	// unless predicted), recall undefined (no personal ground truth).
	m = DetectionMetrics(nil, []Span{{Type: "LAST_NAME", Start: 0, End: 7}}, nil)
	l := m["LAST_NAME"]
	if l.PrecisionDefined || l.RecallDefined || l.F1Defined {
		t.Errorf("LAST_NAME metrics should all be undefined, got p=%v r=%v f1=%v",
			l.PrecisionDefined, l.RecallDefined, l.F1Defined)
	}
	if l.Support != 0 || l.Predicted != 0 {
		t.Errorf("LAST_NAME support/predicted = %d/%d, want 0/0", l.Support, l.Predicted)
	}
}

func TestDetectionMetricsCaseAware(t *testing.T) {
	// Two cases with identical coordinates must be distinct instances. Case A
	// predicts EMAIL[0,5] (TP), case B does not (FN). Without case-awareness the
	// two ground-truth spans would collapse and the single prediction would
	// falsely match both.
	gt := []Span{
		{Case: "a", Type: "EMAIL", Start: 0, End: 5},
		{Case: "b", Type: "EMAIL", Start: 0, End: 5},
	}
	pred := []Span{{Case: "a", Type: "EMAIL", Start: 0, End: 5}}

	m := DetectionMetrics(gt, nil, pred)
	e := m["EMAIL"]
	if e.TP != 1 || e.FP != 0 || e.FN != 1 {
		t.Fatalf("EMAIL tp/fp/fn = %d/%d/%d, want 1/0/1", e.TP, e.FP, e.FN)
	}
	if e.Support != 2 || e.Predicted != 1 {
		t.Errorf("EMAIL support/predicted = %d/%d, want 2/1", e.Support, e.Predicted)
	}
}

func TestDetectionMetricsF1ZeroWhenBothDefined(t *testing.T) {
	// TP=0 with both FP>0 and FN>0 gives Precision=0 and Recall=0, both defined.
	// F1 is mathematically 0 and must be defined, not excluded from aggregation.
	gt := []Span{{Case: "a", Type: "EMAIL", Start: 0, End: 5}}
	pred := []Span{{Case: "a", Type: "EMAIL", Start: 10, End: 15}}

	m := DetectionMetrics(gt, nil, pred)
	e := m["EMAIL"]
	if e.TP != 0 || e.FP != 1 || e.FN != 1 {
		t.Fatalf("EMAIL tp/fp/fn = %d/%d/%d, want 0/1/1", e.TP, e.FP, e.FN)
	}
	if !e.PrecisionDefined || !approx(e.Precision, 0.0) {
		t.Errorf("EMAIL precision = %v (defined=%v), want 0.0 defined", e.Precision, e.PrecisionDefined)
	}
	if !e.RecallDefined || !approx(e.Recall, 0.0) {
		t.Errorf("EMAIL recall = %v (defined=%v), want 0.0 defined", e.Recall, e.RecallDefined)
	}
	if !e.F1Defined || !approx(e.F1, 0.0) {
		t.Errorf("EMAIL f1 = %v (defined=%v), want 0.0 defined", e.F1, e.F1Defined)
	}
}

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"kitten", "sitting", 3},
		{"", "abc", 3},
		{"abc", "", 3},
		{"Иванов", "Иванов", 0},
		{"Иванов", "Иванова", 1},
	}
	for _, c := range cases {
		if got := Levenshtein(c.a, c.b); got != c.want {
			t.Errorf("Levenshtein(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestMaskingQuality(t *testing.T) {
	cases := []struct {
		name     string
		expected string
		actual   string
		want     float64
	}{
		{"both empty", "", "", 1.0},
		{"identical", "<EMAIL>", "<EMAIL>", 1.0},
		{"empty vs nonempty", "", "abc", 0.0},
	}
	for _, c := range cases {
		got := MaskingQuality(c.expected, c.actual)
		if !approx(got, c.want) {
			t.Errorf("%s: MaskingQuality = %v, want %v", c.name, got, c.want)
		}
	}

	// A missing span must reduce quality below 1.0 (not perfect), but the exact
	// value depends on the edit distance and is not asserted.
	if q := MaskingQuality("<FULL_NAME>, email: <EMAIL>", "Иванов Иван Иванович, email: <EMAIL>"); q >= 1.0 {
		t.Errorf("missing-span MaskingQuality = %v, want < 1.0", q)
	}
}

func TestMaskingQualityRange(t *testing.T) {
	// Quality must always be in 0..1.
	for _, c := range []struct{ a, b string }{
		{"<A>", "<B>"},
		{"<A>", "<A> <B>"},
		{"<A> <B> <C>", "<A> <B>"},
		{"", "<A>"},
	} {
		q := MaskingQuality(c.a, c.b)
		if q < 0 || q > 1 {
			t.Errorf("MaskingQuality(%q, %q) = %v out of range", c.a, c.b, q)
		}
	}
}
