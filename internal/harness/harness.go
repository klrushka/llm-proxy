// Package harness runs the local quality harness over a synthetic corpus using
// an injected Predictor. It computes per-type precision/recall/F1, normalized
// span-based Levenshtein masking quality and exact-match restoration, and
// emits a machine-readable report. It never substitutes hardcoded success
// numbers for real verification: every metric is computed from the predictor's
// actual output and the real tokenization/detokenization round trip.
package harness

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/merge"
	"github.com/klrushka/llm-proxy/internal/ownership"
	"github.com/klrushka/llm-proxy/internal/scorer"
	"github.com/klrushka/llm-proxy/internal/testcorpus"
	"github.com/klrushka/llm-proxy/internal/tokenization"
	"github.com/klrushka/llm-proxy/internal/vault"
)

// ttl is the in-memory vault TTL used for the restoration round trip. It is
// long enough that no mapping expires during a single harness run.
const ttl = time.Hour

// PredictedSpan is one span predicted by a Predictor. Personal reports whether
// the predictor classified the span as personal data that must be masked.
type PredictedSpan struct {
	Type     string `json:"type"`
	Start    int    `json:"start"`
	End      int    `json:"end"`
	Personal bool   `json:"personal"`
}

// Predictor detects personal PII spans in input. Implementations must be
// deterministic and must not require network access.
type Predictor interface {
	Predict(ctx context.Context, input string) ([]PredictedSpan, error)
}

// CaseResult is the per-case outcome of the harness.
type CaseResult struct {
	ID               string          `json:"id"`
	MaskingQuality   float64         `json:"masking_quality"`
	RestorationExact bool            `json:"restoration_exact"`
	PredictedSpans   []PredictedSpan `json:"predicted_spans"`
}

// Summary is the macro-average of per-type metrics. Because Precision, Recall
// and F1 can each be defined over a different set of types (a type with an
// undefined metric is excluded from that metric), each metric reports its own
// contributing type count rather than a single misleading counter.
type Summary struct {
	Precision      float64 `json:"precision"`
	Recall         float64 `json:"recall"`
	F1             float64 `json:"f1"`
	PrecisionTypes int     `json:"precision_types"`
	RecallTypes    int     `json:"recall_types"`
	F1Types        int     `json:"f1_types"`
}

// Report is the machine-readable harness output.
type Report struct {
	CorpusVersion        string           `json:"corpus_version"`
	OffsetUnit           string           `json:"offset_unit"`
	PerType              []scorer.PerType `json:"per_type"`
	MaskingQuality       float64          `json:"masking_quality"`
	RestorationExactRate float64          `json:"restoration_exact_rate"`
	Summary              Summary          `json:"summary"`
	Cases                []CaseResult     `json:"cases"`
}

// Run executes the harness over every case in corpus using predictor and
// returns the aggregate report. It never fabricates results: masking quality is
// computed from the predictor's actual masked output and restoration is a real
// tokenize -> detokenize round trip through the tokenization and vault
// contracts. A predictor error on any case fails the whole run.
func Run(ctx context.Context, corpus testcorpus.Corpus, p Predictor) (Report, error) {
	var report Report
	report.CorpusVersion = corpus.Version
	report.OffsetUnit = corpus.OffsetUnit

	var allGT, allNeg, allPred []scorer.Span
	var maskSum float64
	var restoreOK int

	for _, tc := range corpus.Cases {
		pred, err := p.Predict(ctx, tc.Input)
		if err != nil {
			return Report{}, fmt.Errorf("harness: case %q: predict: %w", tc.ID, err)
		}

		for _, s := range tc.Spans {
			sp := scorer.Span{Case: tc.ID, Type: s.Type, Start: s.Start, End: s.End}
			if s.Personal {
				allGT = append(allGT, sp)
			} else {
				allNeg = append(allNeg, sp)
			}
		}
		// Only predictions classified as personal are positive predictions for
		// detection metrics; a Personal=false prediction is not a positive and
		// must not create a false positive.
		for _, s := range pred {
			if !s.Personal {
				continue
			}
			allPred = append(allPred, scorer.Span{Case: tc.ID, Type: s.Type, Start: s.Start, End: s.End})
		}

		q := scorer.MaskingQuality(tc.ExpectedMask, canonicalMask(tc.Input, pred))
		maskSum += q

		restored, err := roundTrip(ctx, tc.ID, tc.Input, pred)
		exact := err == nil && restored == tc.Input
		if exact {
			restoreOK++
		}

		report.Cases = append(report.Cases, CaseResult{
			ID:               tc.ID,
			MaskingQuality:   q,
			RestorationExact: exact,
			PredictedSpans:   pred,
		})
	}

	report.PerType = sortedPerType(scorer.DetectionMetrics(allGT, allNeg, allPred))

	n := len(corpus.Cases)
	if n > 0 {
		report.MaskingQuality = maskSum / float64(n)
		report.RestorationExactRate = float64(restoreOK) / float64(n)
	}
	report.Summary = macroAverage(report.PerType)
	return report, nil
}

// canonicalMask replaces each predicted personal span in input with a canonical
// <TYPE> placeholder, applying replacements right to left so earlier byte
// offsets stay valid. Non-personal predicted spans are left unchanged. The
// result is compared against the corpus expected_mask for masking quality.
func canonicalMask(input string, spans []PredictedSpan) string {
	var personal []PredictedSpan
	for _, s := range spans {
		if s.Personal {
			personal = append(personal, s)
		}
	}
	sort.SliceStable(personal, func(i, j int) bool {
		return personal[i].Start > personal[j].Start
	})
	buf := []byte(input)
	for _, s := range personal {
		if s.Start < 0 || s.End <= s.Start || s.End > len(buf) {
			continue
		}
		tok := "<" + s.Type + ">"
		out := make([]byte, 0, len(buf)-(s.End-s.Start)+len(tok))
		out = append(out, buf[:s.Start]...)
		out = append(out, tok...)
		out = append(out, buf[s.End:]...)
		buf = out
	}
	return string(buf)
}

// roundTrip runs a real tokenize -> detokenize cycle for the predicted personal
// spans and returns the restored text. It uses the actual tokenization and
// vault contracts: personal spans are replaced by scoped opaque tokens, the
// mappings are saved to an in-memory vault, and strict detokenization restores
// the original. A failure at any stage returns an error and the caller records
// restoration as not exact.
func roundTrip(ctx context.Context, caseID, input string, spans []PredictedSpan) (string, error) {
	scope := "harness-" + caseID

	var entities []ownership.Entity
	for _, s := range spans {
		if !s.Personal {
			continue
		}
		entities = append(entities, ownership.Entity{
			Entity: merge.Entity{
				Candidate: detection.Candidate{
					Type:       detection.Type(s.Type),
					Start:      s.Start,
					End:        s.End,
					Confidence: 1.0,
					Sources:    []detection.Source{detection.SourceRegex},
				},
			},
			Personal:  true,
			OwnerType: ownership.OwnerTypePerson,
		})
	}

	gen, err := tokenization.New()
	if err != nil {
		return "", err
	}
	store, err := vault.NewMemory(ttl)
	if err != nil {
		return "", err
	}

	res, err := tokenization.Replace(input, scope, entities, gen)
	if err != nil {
		return "", err
	}
	for _, r := range res.Replacements {
		val := input[r.Entity.Start:r.Entity.End]
		if err := store.Save(ctx, scope, r.Token, val); err != nil {
			return "", err
		}
	}
	dr, err := tokenization.Detokenize(ctx, res.Text, scope, tokenization.ModeStrict, store)
	if err != nil {
		return "", err
	}
	return dr.Text, nil
}

// sortedPerType returns the per-type metrics sorted by type name for a stable,
// machine-readable report.
func sortedPerType(m map[string]scorer.PerType) []scorer.PerType {
	out := make([]scorer.PerType, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// macroAverage averages each metric over the types where that metric is
// defined. Because Precision, Recall and F1 can be defined over different sets
// of types, each metric accumulates its own sum and count.
func macroAverage(perType []scorer.PerType) Summary {
	var s Summary
	var pSum, rSum, fSum float64
	var pN, rN, fN int
	for _, pt := range perType {
		if pt.PrecisionDefined {
			pSum += pt.Precision
			pN++
		}
		if pt.RecallDefined {
			rSum += pt.Recall
			rN++
		}
		if pt.F1Defined {
			fSum += pt.F1
			fN++
		}
	}
	if pN > 0 {
		s.Precision = pSum / float64(pN)
		s.PrecisionTypes = pN
	}
	if rN > 0 {
		s.Recall = rSum / float64(rN)
		s.RecallTypes = rN
	}
	if fN > 0 {
		s.F1 = fSum / float64(fN)
		s.F1Types = fN
	}
	return s
}
