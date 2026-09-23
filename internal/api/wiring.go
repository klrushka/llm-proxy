// Package api provides the production wiring seam that composes the real
// detection, ownership and tokenization stages into the extended /v1/pii/*
// operations. The model worker is the only external boundary; rules, merge,
// ownership, tokenization and the vault are the real in-process
// implementations.
package api

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/klrushka/llm-proxy/internal/audit"
	"github.com/klrushka/llm-proxy/internal/contextual"
	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/merge"
	"github.com/klrushka/llm-proxy/internal/metrics"
	"github.com/klrushka/llm-proxy/internal/ownership"
	"github.com/klrushka/llm-proxy/internal/policy"
	"github.com/klrushka/llm-proxy/internal/rules"
	"github.com/klrushka/llm-proxy/internal/runtime"
	"github.com/klrushka/llm-proxy/internal/tokenization"
	"github.com/klrushka/llm-proxy/internal/vault"
)

// ErrReviewRequired is the fixed safe sentinel error returned by tokenize when
// a policy-allowed entity is ambiguous and requires review. It never embeds
// original text, entity values, scope, offsets or model output. Callers can
// classify the failure with errors.Is without string matching.
var ErrReviewRequired = errors.New("tokenize: review required")

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
	issuer tokenization.RevocableTokenIssuer
	vault  vault.Vault

	// lifecycle serializes the reversible-tokenization lifecycle so a scope
	// revoke is linearizable with respect to tokenize/detokenize. tokenize and
	// detokenize hold a read lock for the whole issuance/persistence or restore
	// span; revoke holds the write lock across both the vault revoke and the
	// issuer cache cleanup. After a successful revoke returns, no old or
	// concurrently-created mapping of that scope can resolve or resurrect.
	lifecycle sync.RWMutex
}

// NewPipeline builds a Pipeline from the injected model-worker boundary,
// static processing policy, token issuer and vault. The rules, contextual, merge and
// ownership stages are the real in-process implementations. The issuer must be
// revocable so a scope revoke clears both the vault and the issuer cache.
func NewPipeline(model ModelDetector, p policy.Policy, issuer tokenization.RevocableTokenIssuer, v vault.Vault) *Pipeline {
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

// RuntimeCoordinator builds the runtime coordinator backed by the real
// pipeline and the injected LLM boundary. The protection boundary tokenizes
// confirmed personal entities through the real detection/ownership pipeline and
// persists mappings to the real vault; the restoration boundary detokenizes the
// LLM output in strict mode without rerunning detection. The LLM boundary is
// the only external boundary in the runtime flow and receives only protected
// text.
func (p *Pipeline) RuntimeCoordinator(llm runtime.LLMClient) *runtime.Coordinator {
	return runtime.New(
		func(ctx context.Context, scope, text string) (string, error) {
			res, err := p.tokenize(ctx, TokenizeRequest{Text: text, ScopeID: scope})
			if err != nil {
				return "", err
			}
			return res.TokenizedText, nil
		},
		llm,
		func(ctx context.Context, scope, protected string) (string, error) {
			res, err := p.detokenize(ctx, DetokenizeRequest{Text: protected, ScopeID: scope, Mode: ModeStrict})
			if err != nil {
				return "", err
			}
			return res.RestoredText, nil
		},
	)
}

// detectEntities runs the full detection pipeline and returns ownership
// results with the static processing policy applied. It never returns plaintext values.
func (p *Pipeline) detectEntities(ctx context.Context, text string) ([]ownership.Entity, error) {
	pol := p.policy
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

	// Split candidates into policy-allowed and policy-disabled before merge so
	// a disabled type can never change operational merge winners, components,
	// or ownership evidence of an allowed entity.
	var allowedCandidates []detection.Candidate
	var disabledCandidates []detection.Candidate
	for _, c := range candidates {
		if pol.AllowsType(string(c.Type)) {
			allowedCandidates = append(allowedCandidates, c)
		} else {
			disabledCandidates = append(disabledCandidates, c)
		}
	}

	// Operational pass: merge and assess only allowed candidates. All are
	// allowed by policy, so ownership is the operational view.
	allowedResults := ownership.Assess(text, merge.Merge(allowedCandidates))

	// Reporting-only pass: merge and assess only disabled candidates so a
	// disabled type cannot change operational winners, components, or ownership
	// evidence. Every disabled reporting-only entity is non-personal metadata
	// with type_disabled_by_policy and no owner id.
	disabledResults := ownership.Assess(text, merge.Merge(disabledCandidates))
	for i := range disabledResults {
		disabledResults[i].Personal = false
		disabledResults[i].OwnerID = ""
		if !hasReason(disabledResults[i].ReasonCodes, ownership.ReasonTypeDisabledByPolicy) {
			disabledResults[i].ReasonCodes = append(disabledResults[i].ReasonCodes, ownership.ReasonTypeDisabledByPolicy)
		}
	}

	// Final results: allowed operational results first, then disabled
	// reporting-only metadata that does not overlap any allowed result. A
	// disabled entity overlapping an allowed operational result is not emitted
	// as a separate top-level entity.
	results := append([]ownership.Entity(nil), allowedResults...)
	for _, r := range disabledResults {
		if overlapsAnyAllowed(r, allowedResults) {
			continue
		}
		results = append(results, r)
	}

	sort.SliceStable(results, func(i, j int) bool {
		return documentOrder(results[i].Candidate, results[j].Candidate)
	})

	// Record only safe allowlisted metadata (types, personal flags, sources,
	// reason codes) and the input token count into the request audit collector. The collector accepts
	// only audit.Entity and never carries plaintext values, offsets, mappings
	// or keys. If no collector is present (e.g. a request that did not pass
	// through the outer audit middleware) this is a no-op.
	if col, ok := audit.CollectorFromContext(ctx); ok {
		col.AddInputTokens(metrics.CountTokens(text))
		for _, r := range results {
			col.AddEntity(auditEntityFromOwnership(r))
		}
	}
	return results, nil
}

// auditEntityFromOwnership maps an ownership result to the safe audit entity
// metadata. It carries only the canonical type, the personal flag, the
// detection sources and the ownership reason codes; it never carries the
// plaintext value, offsets, mappings, ciphertext or keys.
func auditEntityFromOwnership(r ownership.Entity) audit.Entity {
	sources := make([]string, 0, len(r.Sources))
	for _, s := range r.Sources {
		sources = append(sources, string(s))
	}
	reasons := make([]string, 0, len(r.ReasonCodes))
	for _, rc := range r.ReasonCodes {
		reasons = append(reasons, string(rc))
	}
	return audit.Entity{
		Type:        string(r.Type),
		Personal:    r.Personal,
		Sources:     sources,
		ReasonCodes: reasons,
	}
}

// hasReason reports whether reasons contains rc.
func hasReason(reasons []ownership.ReasonCode, rc ownership.ReasonCode) bool {
	for _, r := range reasons {
		if r == rc {
			return true
		}
	}
	return false
}

// overlapsAnyAllowed reports whether r shares any byte with any allowed result.
func overlapsAnyAllowed(r ownership.Entity, allowed []ownership.Entity) bool {
	for _, a := range allowed {
		if r.Start < a.End && a.Start < r.End {
			return true
		}
	}
	return false
}

// documentOrder sorts by Start, then End, then Type.
func documentOrder(a, b detection.Candidate) bool {
	if a.Start != b.Start {
		return a.Start < b.Start
	}
	if a.End != b.End {
		return a.End < b.End
	}
	return a.Type < b.Type
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
	// Fail closed on any policy-allowed ambiguous entity that requires review:
	// it must never be sent to the LLM as plaintext or as a partial tokenized
	// result. A reporting-only entity disabled by policy (type_disabled_by_policy)
	// is explicitly excluded by the consumer and must not block tokenization.
	for _, r := range results {
		if r.ReviewRecommended && !hasReason(r.ReasonCodes, ownership.ReasonTypeDisabledByPolicy) {
			return TokenizeResponse{}, ErrReviewRequired
		}
	}
	// Hold the lifecycle read lock across the whole issuance and persistence
	// span (from the first issuer.Token through every vault.Save inside
	// ReplaceAndPersist) so a concurrent revoke cannot interleave and leave a
	// stale mapping resolvable after it returns.
	p.lifecycle.RLock()
	if err := ctx.Err(); err != nil {
		p.lifecycle.RUnlock()
		return TokenizeResponse{}, err
	}
	res, err := tokenization.ReplaceAndPersist(ctx, req.Text, req.ScopeID, results, p.issuer, p.vault)
	p.lifecycle.RUnlock()
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
	// Hold the lifecycle read lock across the whole restore so a concurrent
	// revoke cannot clear a mapping mid-restore and yield a partial result.
	p.lifecycle.RLock()
	if err := ctx.Err(); err != nil {
		p.lifecycle.RUnlock()
		return DetokenizeResponse{}, err
	}
	res, err := tokenization.Detokenize(ctx, req.Text, req.ScopeID, tokenization.Mode(req.Mode), p.vault)
	p.lifecycle.RUnlock()
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
	// Hold the lifecycle write lock across issuer and vault cleanup so revoke is
	// linearizable with in-flight tokenize/detokenize operations. Clear the
	// issuer first: if that fails, do not report success or remove the vault
	// mapping while a reusable token can still be re-issued.
	p.lifecycle.Lock()
	defer p.lifecycle.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.issuer.RevokeScope(ctx, scopeID); err != nil {
		return err
	}
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
