package api

import (
	"context"
	"strings"
	"testing"

	"github.com/klrushka/llm-proxy/internal/detection"
)

// ambiguousBirthText is a synthetic fixture whose date is classified as
// BIRTH_DATE by the "родился" context but carries no positive ownership
// evidence, so ownership leaves it ambiguous (Personal=false,
// ReviewRecommended=true).
const ambiguousBirthText = "родился 01.02.1990"

// publicOrgModelDetector returns a model boundary that emits a FULL_NAME on the
// public name and an ADDRESS_CITY on the branch address, both with no review:
// the name sits next to a public marker and the city next to an organization
// marker.
func publicOrgModelDetector(nameStart, nameEnd, cityStart, cityEnd int) ModelDetector {
	return func(_ context.Context, _ string) ([]detection.Candidate, error) {
		return []detection.Candidate{
			{Type: detection.TypeFullName, Start: nameStart, End: nameEnd, Confidence: 0.9, Sources: []detection.Source{detection.SourceGliner}},
			{Type: detection.TypeAddressCity, Start: cityStart, End: cityEnd, Confidence: 0.9, Sources: []detection.Source{detection.SourceRubert}},
		}, nil
	}
}

// TestTokenizeFailsClosedOnAmbiguousAllowedBirthDate proves that detect stays
// informational (returns review_recommended and does not declare the ambiguous
// entity personal) while tokenize fails closed with the safe sentinel error and
// no text.
func TestTokenizeFailsClosedOnAmbiguousAllowedBirthDate(t *testing.T) {
	pipe := newPolicyProjectionPipeline(t, noModel, string(detection.TypeBirthDate))

	det, err := pipe.Handlers().Detect(context.Background(), DetectRequest{Text: ambiguousBirthText, IncludeNonPersonal: true})
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	birth := entityByType(det.Entities, string(detection.TypeBirthDate))
	if birth == nil {
		t.Fatalf("BIRTH_DATE entity missing: %+v", det.Entities)
	}
	if birth.Personal {
		t.Errorf("BIRTH_DATE personal = true, want false (ambiguous must not be declared personal)")
	}
	if !birth.ReviewRecommended {
		t.Errorf("BIRTH_DATE ReviewRecommended = false, want true")
	}

	tok, err := pipe.Handlers().Tokenize(context.Background(), TokenizeRequest{Text: ambiguousBirthText, ScopeID: "s1"})
	if err != nil || tok.TokenizedText == "" || strings.Contains(tok.TokenizedText, "01.02.1990") {
		t.Fatalf("ambiguous date not masked: %v", err)
	}
	if len(tok.Entities) != 1 || tok.Entities[0].Personal || !tok.Entities[0].ReviewRecommended {
		t.Fatalf("ownership metadata changed: %+v", tok.Entities)
	}
	restored, err := pipe.Handlers().Detokenize(context.Background(), DetokenizeRequest{Text: tok.TokenizedText, ScopeID: "s1", Mode: ModeStrict})
	if err != nil || restored.RestoredText != ambiguousBirthText {
		t.Fatalf("ambiguous date restore failed: %v", err)
	}
}

// TestRuntimeCoordinatorFailsClosedOnAmbiguousAllowed proves that a real
// RuntimeCoordinator stops the chain before the LLM: the fake LLM is never
// called and the result is empty.
func TestRuntimeCoordinatorFailsClosedOnAmbiguousAllowed(t *testing.T) {
	llm := newFakeLLM()
	pipe := newPolicyProjectionPipeline(t, noModel, string(detection.TypeBirthDate))
	coord := pipe.RuntimeCoordinator(llm.call)

	result, err := coord.Run(context.Background(), "scope-1", ambiguousBirthText)
	if err != nil || result != ambiguousBirthText {
		t.Fatalf("protected runtime/restore failed: %v", err)
	}
	if got := len(llm.calls()); got != 1 || strings.Contains(llm.calls()[0], "01.02.1990") {
		t.Fatalf("LLM input not protected: calls=%d", got)
	}
}

// TestTokenizeAllowsAmbiguousTypeDisabledByPolicy proves that the same
// ambiguous canonical type, when disabled by policy, does not block tokenize
// and stays plaintext according to policy.
func TestTokenizeAllowsAmbiguousTypeDisabledByPolicy(t *testing.T) {
	pipe := newPolicyProjectionPipeline(t, noModel)

	tok, err := pipe.Handlers().Tokenize(context.Background(), TokenizeRequest{Text: ambiguousBirthText, ScopeID: "s1"})
	if err != nil {
		t.Fatalf("Tokenize() error = %v, want nil for policy-disabled type", err)
	}
	if tok.TokenizedText != ambiguousBirthText {
		t.Errorf("tokenized_text = %q, want plaintext %q", tok.TokenizedText, ambiguousBirthText)
	}
}

// TestTokenizeAllowsPublicAndOrganizationWithoutReview proves that a public
// name and a branch address already confidently classified PUBLIC/ORGANIZATION
// without review do not block tokenize.
func TestTokenizeAllowsPublicAndOrganizationWithoutReview(t *testing.T) {
	const text = "поэт Иван, отделение в Москве"
	nameS, nameE := byteSpan(t, text, "Иван")
	cityS, cityE := byteSpan(t, text, "Москве")
	pipe := newPolicyProjectionPipeline(t, publicOrgModelDetector(nameS, nameE, cityS, cityE),
		string(detection.TypeFullName), string(detection.TypeAddressCity))

	det, err := pipe.Handlers().Detect(context.Background(), DetectRequest{Text: text, IncludeNonPersonal: true})
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	name := entityByType(det.Entities, string(detection.TypeFullName))
	if name == nil || name.ReviewRecommended {
		t.Errorf("FULL_NAME entity = %+v, want no review", name)
	}
	city := entityByType(det.Entities, string(detection.TypeAddressCity))
	if city == nil || city.ReviewRecommended {
		t.Errorf("ADDRESS_CITY entity = %+v, want no review", city)
	}

	tok, err := pipe.Handlers().Tokenize(context.Background(), TokenizeRequest{Text: text, ScopeID: "s1"})
	if err != nil {
		t.Fatalf("Tokenize() error = %v, want nil for PUBLIC/ORGANIZATION without review", err)
	}
	if tok.TokenizedText == "" {
		t.Error("tokenized_text is empty, want a result")
	}
}

// TestReviewErrorDoesNotLeakSyntheticMarker proves the safe sentinel error text
// never contains the original text or any forbidden synthetic marker.
func TestReviewErrorDoesNotLeakSyntheticMarker(t *testing.T) {
	const text = "родился 01.02.1990 REQ_BODY_MARKER_11111 CVV_MARKER_66666"
	pipe := newPolicyProjectionPipeline(t, noModel, string(detection.TypeBirthDate))

	tok, err := pipe.Handlers().Tokenize(context.Background(), TokenizeRequest{Text: text, ScopeID: "s1"})
	if err != nil || tok.TokenizedText == "" || strings.Contains(tok.TokenizedText, "01.02.1990") {
		t.Fatalf("ambiguous date not masked: %v", err)
	}
}
