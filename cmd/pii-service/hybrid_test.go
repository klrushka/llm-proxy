package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/klrushka/llm-proxy/internal/api"
	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/process"
)

func TestHybridUsesBothModelSourcesWhenAvailable(t *testing.T) {
	const text = "Клиент Иван Петров"
	start := strings.Index(text, "Иван Петров")
	called := 0
	pipe := newPipelineWithModel(t, func(context.Context, string) ([]detection.Candidate, error) {
		called++
		return []detection.Candidate{{Type: detection.TypeFullName, Start: start, End: start + len("Иван Петров"), Confidence: .95, Sources: []detection.Source{detection.SourceRubert, detection.SourceGliner}}}, nil
	}, string(detection.TypeFullName))
	var outcomes []string
	mask := hybridProcessMask(pipe, func(s string) { outcomes = append(outcomes, s) })
	result, err := mask(context.Background(), text)
	if err != nil || result == "" || result == text || called != 1 || len(outcomes) != 0 {
		t.Fatalf("full-model primary not used: err=%v masked=%t calls=%d fallback=%v", err, result != "" && result != text, called, outcomes)
	}
}

func TestHybridFallbackMasksAndRestoresInSameScope(t *testing.T) {
	const text = "Клиент, email test@example.com"
	pipe := newPipelineWithModel(t, func(context.Context, string) ([]detection.Candidate, error) {
		return nil, api.ErrModelUnavailable
	}, string(detection.TypeEmail))
	var outcomes []string
	op := process.NewOperation(process.NewStore(), hybridProcessMask(pipe, func(s string) { outcomes = append(outcomes, s) }))
	req := process.Request{Payload: text, PayloadID: "synthetic-hybrid-id"}
	first, err := op.Handle(context.Background(), req)
	if err != nil || first.Result == "" || first.Result == text {
		t.Fatalf("rules fallback failed to mask: err=%v masked=%t", err, first.Result != text && first.Result != "")
	}
	second, err := op.Handle(context.Background(), req)
	if err != nil || second.Result != first.Result || len(outcomes) != 1 || outcomes[0] != "success" {
		t.Fatalf("same-ID fallback not stable: err=%v outcomes=%v", err, outcomes)
	}
	restored, err := pipe.Handlers().Detokenize(context.Background(), api.DetokenizeRequest{Text: first.Result, ScopeID: processScope, Mode: api.ModeStrict})
	if err != nil || restored.RestoredText != text {
		t.Fatalf("shared-scope restore failed: %v", err)
	}
}

func TestHybridFallbackPreservesReviewAndGenericFailure(t *testing.T) {
	const ambiguous = "родился 01.02.1990"
	pipe := newPipelineWithModel(t, func(context.Context, string) ([]detection.Candidate, error) {
		return nil, api.ErrModelUnavailable
	}, string(detection.TypeBirthDate))
	var outcomes []string
	op := process.NewOperation(process.NewStore(), hybridProcessMask(pipe, func(s string) { outcomes = append(outcomes, s) }))
	req := process.Request{Payload: ambiguous, PayloadID: "synthetic-review-id"}
	for i := 0; i < 2; i++ {
		resp, err := op.Handle(context.Background(), req)
		if !errors.Is(err, process.ErrReviewRequired) || resp.Result != "" {
			t.Fatalf("review attempt %d: err=%v result_empty=%t", i, err, resp.Result == "")
		}
	}
	if len(outcomes) != 1 || outcomes[0] != "review" {
		t.Fatalf("review fallback outcomes = %v", outcomes)
	}
	generic := newPipelineWithModel(t, func(context.Context, string) ([]detection.Candidate, error) {
		return nil, errors.New("synthetic generic failure")
	}, string(detection.TypeEmail))
	called := false
	mask := hybridProcessMask(generic, func(string) { called = true })
	if result, err := mask(context.Background(), "email test@example.com"); !errors.Is(err, process.ErrMaskingFailed) || result != "" || called {
		t.Fatalf("generic failure incorrectly fell back: err=%v result_empty=%t called=%t", err, result == "", called)
	}
}
