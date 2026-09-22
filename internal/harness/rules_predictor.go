package harness

import (
	"context"

	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/merge"
	"github.com/klrushka/llm-proxy/internal/ownership"
	"github.com/klrushka/llm-proxy/internal/rules"
)

// RulesPredictor runs the deterministic Go rules/validators detection pipeline
// (no NER, no network) and returns the personal spans it detects. It is the
// default predictor for the local harness and exercises the real rule-based
// detection path, so the harness reports honest, non-fabricated metrics.
type RulesPredictor struct{}

// Predict runs every rule family over input, merges the candidates, applies
// ownership classification and returns only the personal spans. Non-canonical
// intermediate types (for example the DATE type emitted before contextual
// classification) are filtered out so the output is comparable to the canonical
// corpus ground truth.
func (RulesPredictor) Predict(ctx context.Context, input string) ([]PredictedSpan, error) {
	var candidates []detection.Candidate
	candidates = append(candidates, rules.DetectContacts(input)...)
	candidates = append(candidates, rules.DetectIdentityDocuments(input)...)
	candidates = append(candidates, rules.DetectDates(input)...)
	candidates = append(candidates, rules.DetectPaymentSecrets(input)...)
	candidates = append(candidates, rules.DetectBankCards(input)...)
	candidates = append(candidates, rules.DetectAddressComponents(input)...)
	candidates = append(candidates, rules.DetectINN(input)...)

	reg, err := detection.New()
	if err != nil {
		return nil, err
	}

	merged := merge.Merge(candidates)
	assessed := ownership.Assess(input, merged)

	var out []PredictedSpan
	for _, e := range assessed {
		if !e.Personal {
			continue
		}
		if !reg.Lookup(e.Type) {
			continue
		}
		out = append(out, PredictedSpan{
			Type:     string(e.Type),
			Start:    e.Start,
			End:      e.End,
			Personal: true,
		})
	}
	return out, nil
}
