// Package api provides the production wiring seam that composes the real
// detection, ownership and tokenization stages into the extended /v1/pii/*
// operations. The model worker is the only external boundary; rules, merge,
// ownership, tokenization and the vault are the real in-process
// implementations.
package api

import (
	"context"

	"github.com/klrushka/llm-proxy/internal/contextual"
	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/merge"
	"github.com/klrushka/llm-proxy/internal/ownership"
	"github.com/klrushka/llm-proxy/internal/policy"
	"github.com/klrushka/llm-proxy/internal/rules"
	"github.com/klrushka/llm-proxy/internal/tokenization"
	"github.com/klrushka/llm-proxy/internal/vault"
)

// ModelDetector is the model-worker boundary. It returns model detection
// candidates for text. A nil or empty result means the model contributed no
// candidates (e.g. fast mode or an unavailable worker). It is the only
// external boundary in the pipeline; rules, merge, ownership, tokenization and
// the vault are the real in-process implementations.
type ModelDetector func(ctx context.Context, text string) ([]detection.Candidate, error)

// Pipeline composes the real detection, ownership and tokenization stages into
// the /v1/pii/* operations. It is the production wiring seam for the extended
// API.
type Pipeline struct {
	model  ModelDetector
	policy policy.Policy
	issuer tokenization.TokenIssuer
	vault  vault.Vault
}

// NewPipeline builds a Pipeline from the injected model-worker boundary,
// consumer policy, token issuer and vault. The rules, contextual, merge and
// ownership stages are the real in-process implementations.
func NewPipeline(model ModelDetector, p policy.Policy, issuer tokenization.TokenIssuer, v vault.Vault) *Pipeline {
	return &Pipeline{model: model, policy: p, issuer: issuer, vault: v}
}

// Handlers returns the PIIHandlers backed by the real pipeline.
func (p *Pipeline) Handlers() PIIHandlers {
	return PIIHandlers{
		Detect:      p.detect,
		Tokenize:    p.tokenize,
		Detokenize:  p.detokenize,
		RevokeScope: p.revokeScope,
	}
}

// detectEntities runs the full detection pipeline and returns ownership
// results with the consumer policy applied. It never returns plaintext values.
func (p *Pipeline) detectEntities(ctx context.Context, text string) ([]ownership.Entity, error) {
	var candidates []detection.Candidate
	if p.model != nil {
		mc, err := p.model(ctx, text)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, mc...)
	}
	candidates = append(candidates, rules.DetectContacts(text)...)
	candidates = append(candidates, rules.DetectIdentityDocuments(text)...)
	candidates = append(candidates, rules.DetectBankCards(text)...)
	candidates = append(candidates, rules.DetectINN(text)...)
	candidates = append(candidates, rules.DetectDates(text)...)
	candidates = append(candidates, rules.DetectAddressComponents(text)...)
	candidates = append(candidates, rules.DetectPaymentSecrets(text)...)

	candidates = contextual.Classify(text, candidates)
	merged := merge.Merge(candidates)
	results := ownership.Assess(text, merged)
	return ownership.ApplyPolicy(results, p.policy), nil
}

func (p *Pipeline) detect(ctx context.Context, req DetectRequest) (DetectResponse, error) {
	results, err := p.detectEntities(ctx, req.Text)
	if err != nil {
		return DetectResponse{}, err
	}
	resp := DetectResponse{
		RequestID:     req.RequestID,
		DetectedTypes: []string{},
		Entities:      []Entity{},
	}
	seen := make(map[string]bool)
	for _, r := range results {
		if !req.IncludeNonPersonal && !r.Personal {
			continue
		}
		e := entityFromOwnership(r)
		resp.Entities = append(resp.Entities, e)
		if e.Personal {
			resp.HasPersonalData = true
		}
		if !seen[string(r.Type)] {
			seen[string(r.Type)] = true
			resp.DetectedTypes = append(resp.DetectedTypes, string(r.Type))
		}
	}
	return resp, nil
}

func (p *Pipeline) tokenize(ctx context.Context, req TokenizeRequest) (TokenizeResponse, error) {
	results, err := p.detectEntities(ctx, req.Text)
	if err != nil {
		return TokenizeResponse{}, err
	}
	res, err := tokenization.ReplaceAndPersist(ctx, req.Text, req.ScopeID, results, p.issuer, p.vault)
	if err != nil {
		return TokenizeResponse{}, err
	}
	resp := TokenizeResponse{
		TokenizedText: res.Text,
		DetectedTypes: []string{},
		Entities:      []Entity{},
		ScopeID:       req.ScopeID,
	}
	seen := make(map[string]bool)
	for _, r := range results {
		e := entityFromOwnership(r)
		resp.Entities = append(resp.Entities, e)
		if !seen[string(r.Type)] {
			seen[string(r.Type)] = true
			resp.DetectedTypes = append(resp.DetectedTypes, string(r.Type))
		}
	}
	return resp, nil
}

func (p *Pipeline) detokenize(ctx context.Context, req DetokenizeRequest) (DetokenizeResponse, error) {
	res, err := tokenization.Detokenize(ctx, req.Text, req.ScopeID, tokenization.Mode(req.Mode), p.vault)
	if err != nil {
		return DetokenizeResponse{}, err
	}
	return DetokenizeResponse{
		RestoredText:       res.Text,
		ResolvedTokenCount: res.ResolvedTokenCount,
		UnresolvedTokens:   res.UnresolvedTokens,
	}, nil
}

func (p *Pipeline) revokeScope(ctx context.Context, scopeID string) error {
	return p.vault.RevokeScope(ctx, scopeID)
}

// entityFromOwnership maps an ownership result to the safe API entity
// metadata. It never carries the plaintext value.
func entityFromOwnership(r ownership.Entity) Entity {
	sources := make([]string, 0, len(r.Sources))
	for _, s := range r.Sources {
		sources = append(sources, string(s))
	}
	reasons := make([]string, 0, len(r.ReasonCodes))
	for _, rc := range r.ReasonCodes {
		reasons = append(reasons, string(rc))
	}
	return Entity{
		Type:              string(r.Type),
		Start:             r.Start,
		End:               r.End,
		Confidence:        r.Confidence,
		Personal:          r.Personal,
		Sources:           sources,
		ReasonCodes:       reasons,
		OwnerID:           r.OwnerID,
		OwnerType:         string(r.OwnerType),
		OwnershipScore:    r.OwnershipScore,
		ReviewRecommended: r.ReviewRecommended,
	}
}
