// Package scorer implements the deterministic local quality metrics for the
// PII protection service: per-type precision/recall/F1 over detected spans, a
// normalized span-based Levenshtein masking-quality score in 0..1, and
// exact-match restoration. It is the scoring core of the local quality harness
// (task 10.2). It holds no plaintext and makes no privacy decisions.
package scorer

// Span is one detected or ground-truth entity span. Case identifies the
// document/case the span belongs to, so identical coordinates from different
// cases never collapse or falsely match. Offsets are UTF-8 byte offsets, start
// inclusive and end exclusive.
type Span struct {
	Case  string
	Type  string
	Start int
	End   int
}

// PerType holds per-type detection metrics.
//
// Matching semantics (see DetectionMetrics): a predicted span matches a
// ground-truth personal span iff they share the same Case, Type, Start and End
// (exact coordinate match within one case). Because exact coordinates within a
// case are unique, matching is one-to-one and deterministic; identical
// coordinates in different cases are distinct instances.
//
// Zero-denominator behavior: Precision is defined only when TP+FP > 0, Recall
// only when TP+FN > 0, and F1 is defined whenever both Precision and Recall are
// defined. When Precision and Recall are both defined and both equal 0, F1 is
// mathematically 0 and is reported as defined. When a metric is undefined it is
// reported as 0 with its Defined flag false; the caller decides how to
// aggregate. Support is the number of ground-truth personal spans and Predicted
// is the number of predicted spans, so a consumer can always see the
// denominator even when a metric is undefined.
type PerType struct {
	Type             string  `json:"type"`
	TP               int     `json:"tp"`
	FP               int     `json:"fp"`
	FN               int     `json:"fn"`
	FPOnHardNegative int     `json:"fp_on_hard_negative"`
	Precision        float64 `json:"precision"`
	Recall           float64 `json:"recall"`
	F1               float64 `json:"f1"`
	PrecisionDefined bool    `json:"precision_defined"`
	RecallDefined    bool    `json:"recall_defined"`
	F1Defined        bool    `json:"f1_defined"`
	Support          int     `json:"support"`
	Predicted        int     `json:"predicted"`
}

// spanKey identifies one exact span within one case.
type spanKey struct {
	caseID string
	start  int
	end    int
}

// DetectionMetrics computes per-type precision/recall/F1 by exact span match.
//
// gtPersonal carries the ground-truth personal spans (the entities that must be
// detected and masked). gtNeg carries the ground-truth non-personal spans
// (hard negatives that must NOT be detected). pred carries the predicted
// positive spans; callers MUST pass only predictions classified as personal.
//
//   - TP: predicted spans that exactly match a ground-truth personal span in
//     the same case.
//   - FP: predicted spans that do not exactly match any ground-truth personal
//     span in the same case. This includes predictions that exactly match a
//     ground-truth non-personal (hard-negative) span, which are additionally
//     counted in FPOnHardNegative.
//   - FN: ground-truth personal spans not exactly matched by any prediction in
//     the same case.
//
// The result is keyed by canonical type name. Types with no ground-truth
// personal spans and no predictions still appear with zero counts so the report
// is complete and machine-readable.
func DetectionMetrics(gtPersonal, gtNeg, pred []Span) map[string]PerType {
	types := make(map[string]struct{})
	for _, s := range gtPersonal {
		types[s.Type] = struct{}{}
	}
	for _, s := range gtNeg {
		types[s.Type] = struct{}{}
	}
	for _, s := range pred {
		types[s.Type] = struct{}{}
	}

	out := make(map[string]PerType, len(types))
	for t := range types {
		gtSet := make(map[spanKey]struct{})
		for _, s := range gtPersonal {
			if s.Type == t {
				gtSet[spanKey{s.Case, s.Start, s.End}] = struct{}{}
			}
		}
		negSet := make(map[spanKey]struct{})
		for _, s := range gtNeg {
			if s.Type == t {
				negSet[spanKey{s.Case, s.Start, s.End}] = struct{}{}
			}
		}
		predSet := make(map[spanKey]struct{})
		for _, s := range pred {
			if s.Type == t {
				predSet[spanKey{s.Case, s.Start, s.End}] = struct{}{}
			}
		}

		pm := PerType{Type: t, Support: len(gtSet), Predicted: len(predSet)}
		for k := range predSet {
			if _, ok := gtSet[k]; ok {
				pm.TP++
			} else {
				pm.FP++
				if _, ok := negSet[k]; ok {
					pm.FPOnHardNegative++
				}
			}
		}
		for k := range gtSet {
			if _, ok := predSet[k]; !ok {
				pm.FN++
			}
		}

		if pm.TP+pm.FP > 0 {
			pm.Precision = float64(pm.TP) / float64(pm.TP+pm.FP)
			pm.PrecisionDefined = true
		}
		if pm.TP+pm.FN > 0 {
			pm.Recall = float64(pm.TP) / float64(pm.TP+pm.FN)
			pm.RecallDefined = true
		}
		if pm.PrecisionDefined && pm.RecallDefined {
			pm.F1Defined = true
			if pm.Precision+pm.Recall > 0 {
				pm.F1 = 2 * pm.Precision * pm.Recall / (pm.Precision + pm.Recall)
			}
			// When both Precision and Recall are 0, F1 is mathematically 0 and
			// remains 0 while still being defined.
		}
		out[t] = pm
	}
	return out
}

// Levenshtein returns the rune-based Levenshtein edit distance between a and b.
func Levenshtein(a, b string) int {
	ar := []rune(a)
	br := []rune(b)
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := 0; j <= len(br); j++ {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 0
			if ar[i-1] != br[j-1] {
				cost = 1
			}
			curr[j] = min(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}

// MaskingQuality returns the normalized span-based Levenshtein masking quality
// in 0..1. It compares the expected masked representation with the actual
// masked representation (each PII span replaced by a canonical <TYPE>
// placeholder) and normalizes the edit distance by the longer of the two, so
// 1.0 means the masks match exactly and 0.0 means they are maximally different.
// Two empty strings are a perfect match (1.0).
func MaskingQuality(expected, actual string) float64 {
	er := len([]rune(expected))
	ar := len([]rune(actual))
	if er == 0 && ar == 0 {
		return 1.0
	}
	maxLen := er
	if ar > maxLen {
		maxLen = ar
	}
	q := 1.0 - float64(Levenshtein(expected, actual))/float64(maxLen)
	if q < 0 {
		return 0
	}
	return q
}
