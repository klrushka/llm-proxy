package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/ownership"
	"github.com/klrushka/llm-proxy/internal/policy"
	"github.com/klrushka/llm-proxy/internal/tokenization"
	"github.com/klrushka/llm-proxy/internal/vault"
)

// overlapText is the synthetic fixture for policy projection. RuBERT CITY and
// GLiNER ru_pii_person share the exact span of "Иван", and CITY has the higher
// confidence, so CITY wins the overlap when both are allowed.
const overlapText = "Иван, email: ivan@example.com"

// byteSpan returns the UTF-8 byte span [start,end) of sub within text.
func byteSpan(t *testing.T, text, sub string) (int, int) {
	t.Helper()
	start := strings.Index(text, sub)
	if start < 0 {
		t.Fatalf("substring %q not found in %q", sub, text)
	}
	return start, start + len(sub)
}

// overlapModelDetector returns a model boundary that emits a RuBERT CITY and a
// GLiNER ru_pii_person on the same span, with CITY having the higher
// confidence.
func overlapModelDetector(nameStart, nameEnd int) ModelDetector {
	return func(_ context.Context, _ string) ([]detection.Candidate, error) {
		return []detection.Candidate{
			{Type: detection.TypeAddressCity, Start: nameStart, End: nameEnd, Confidence: 0.95, Sources: []detection.Source{detection.SourceRubert}},
			{Type: detection.TypeFullName, Start: nameStart, End: nameEnd, Confidence: 0.8, Sources: []detection.Source{detection.SourceGliner}},
		}, nil
	}
}

// newPolicyProjectionPipeline builds the real pipeline with the in-memory
// vault, token issuer, the injected model detector and a processing policy
// allowing exactly the given types.
func newPolicyProjectionPipeline(t *testing.T, detector ModelDetector, allowed ...string) *Pipeline {
	t.Helper()
	v, err := vault.NewMemory(time.Hour)
	if err != nil {
		t.Fatalf("vault.NewMemory() error = %v", err)
	}
	g, err := tokenization.New()
	if err != nil {
		t.Fatalf("tokenization.New() error = %v", err)
	}
	p := policy.NewPolicy(allowed)
	return NewPipeline(detector, p, g, v)
}

// entityByType returns the first entity of the given type, or nil.
func entityByType(entities []Entity, typ string) *Entity {
	for i := range entities {
		if entities[i].Type == typ {
			return &entities[i]
		}
	}
	return nil
}

// entityHasReason reports whether reasons contains rc.
func entityHasReason(reasons []string, rc string) bool {
	for _, r := range reasons {
		if r == rc {
			return true
		}
	}
	return false
}

// TestPolicyProjectionAllowedNameAndEmail proves that when FULL_NAME and EMAIL
// are allowed and CITY is disabled, the name and email are personal and
// tokenized, and the disabled CITY is not emitted as a separate top-level
// entity.
func TestPolicyProjectionAllowedNameAndEmail(t *testing.T) {
	nameS, nameE := byteSpan(t, overlapText, "Иван")
	pipe := newPolicyProjectionPipeline(t, overlapModelDetector(nameS, nameE),
		string(detection.TypeFullName), string(detection.TypeEmail))

	resp, err := pipe.Handlers().Detect(context.Background(), DetectRequest{Text: overlapText, IncludeNonPersonal: true})
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	if len(resp.Entities) != 2 {
		t.Fatalf("entities = %d, want 2: %+v", len(resp.Entities), resp.Entities)
	}
	name := entityByType(resp.Entities, string(detection.TypeFullName))
	if name == nil || !name.Personal {
		t.Errorf("FULL_NAME entity = %+v, want personal", name)
	}
	email := entityByType(resp.Entities, string(detection.TypeEmail))
	if email == nil || !email.Personal {
		t.Errorf("EMAIL entity = %+v, want personal", email)
	}
	if entityByType(resp.Entities, string(detection.TypeAddressCity)) != nil {
		t.Errorf("disabled CITY emitted as top-level entity: %+v", resp.Entities)
	}

	tok, err := pipe.Handlers().Tokenize(context.Background(), TokenizeRequest{Text: overlapText, ScopeID: "s1"})
	if err != nil {
		t.Fatalf("Tokenize() error = %v", err)
	}
	if !strings.Contains(tok.TokenizedText, "<FULL_NAME_") || !strings.Contains(tok.TokenizedText, "<EMAIL_") {
		t.Errorf("tokenized_text = %q, want FULL_NAME and EMAIL tokens", tok.TokenizedText)
	}
	for _, leak := range []string{"Иван", "ivan@example.com"} {
		if strings.Contains(tok.TokenizedText, leak) {
			t.Errorf("tokenized_text %q leaks %q", tok.TokenizedText, leak)
		}
	}
}

// TestPolicyProjectionAllowedCityAndEmail proves that when ADDRESS_CITY and
// EMAIL are allowed and FULL_NAME is disabled, the CITY stays
// ambiguous/nonpersonal/review-recommended, the disabled name does not give it
// linked ownership, and tokenize fails closed because the policy-allowed CITY
// requires review.
func TestPolicyProjectionAllowedCityAndEmail(t *testing.T) {
	nameS, nameE := byteSpan(t, overlapText, "Иван")
	pipe := newPolicyProjectionPipeline(t, overlapModelDetector(nameS, nameE),
		string(detection.TypeAddressCity), string(detection.TypeEmail))

	resp, err := pipe.Handlers().Detect(context.Background(), DetectRequest{Text: overlapText, IncludeNonPersonal: true})
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	if len(resp.Entities) != 2 {
		t.Fatalf("entities = %d, want 2: %+v", len(resp.Entities), resp.Entities)
	}
	city := entityByType(resp.Entities, string(detection.TypeAddressCity))
	if city == nil {
		t.Fatalf("CITY entity missing: %+v", resp.Entities)
	}
	if city.Personal {
		t.Errorf("CITY personal = true, want false (disabled name must not give linked ownership)")
	}
	if !city.ReviewRecommended {
		t.Errorf("CITY ReviewRecommended = false, want true")
	}
	if entityHasReason(city.ReasonCodes, string(ownership.ReasonTypeDisabledByPolicy)) {
		t.Errorf("CITY reasons %v carry type_disabled_by_policy, want ambiguous only", city.ReasonCodes)
	}
	email := entityByType(resp.Entities, string(detection.TypeEmail))
	if email == nil || !email.Personal {
		t.Errorf("EMAIL entity = %+v, want personal", email)
	}
	if entityByType(resp.Entities, string(detection.TypeFullName)) != nil {
		t.Errorf("disabled FULL_NAME emitted as top-level entity: %+v", resp.Entities)
	}

	// The policy-allowed ambiguous CITY requires review, so tokenize must fail
	// closed and must not emit a partial tokenized result.
	tok, err := pipe.Handlers().Tokenize(context.Background(), TokenizeRequest{Text: overlapText, ScopeID: "s1"})
	if !errors.Is(err, ErrReviewRequired) {
		t.Fatalf("Tokenize() error = %v, want ErrReviewRequired", err)
	}
	if tok.TokenizedText != "" {
		t.Errorf("tokenized_text = %q, want empty on fail-closed", tok.TokenizedText)
	}
}

// TestPolicyProjectionAllAllowed proves that when all three types are allowed,
// CITY wins the overlap, FULL_NAME stays a component, CITY and EMAIL get linked
// ownership, and exactly one token is created on the overlap.
func TestPolicyProjectionAllAllowed(t *testing.T) {
	nameS, nameE := byteSpan(t, overlapText, "Иван")
	pipe := newPolicyProjectionPipeline(t, overlapModelDetector(nameS, nameE),
		string(detection.TypeAddressCity), string(detection.TypeFullName), string(detection.TypeEmail))

	resp, err := pipe.Handlers().Detect(context.Background(), DetectRequest{Text: overlapText, IncludeNonPersonal: true})
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	if len(resp.Entities) != 2 {
		t.Fatalf("entities = %d, want 2: %+v", len(resp.Entities), resp.Entities)
	}
	city := entityByType(resp.Entities, string(detection.TypeAddressCity))
	if city == nil || !city.Personal {
		t.Errorf("CITY entity = %+v, want personal", city)
	}
	email := entityByType(resp.Entities, string(detection.TypeEmail))
	if email == nil || !email.Personal {
		t.Errorf("EMAIL entity = %+v, want personal", email)
	}
	if city.OwnerID == "" || city.OwnerID != email.OwnerID {
		t.Errorf("CITY owner_id %q and EMAIL owner_id %q, want shared linked ownership", city.OwnerID, email.OwnerID)
	}
	if entityByType(resp.Entities, string(detection.TypeFullName)) != nil {
		t.Errorf("FULL_NAME emitted as separate top-level entity, want component only: %+v", resp.Entities)
	}

	tok, err := pipe.Handlers().Tokenize(context.Background(), TokenizeRequest{Text: overlapText, ScopeID: "s1"})
	if err != nil {
		t.Fatalf("Tokenize() error = %v", err)
	}
	if strings.Count(tok.TokenizedText, "<ADDRESS_CITY_") != 1 {
		t.Errorf("tokenized_text = %q, want exactly one ADDRESS_CITY token on the overlap", tok.TokenizedText)
	}
	if strings.Count(tok.TokenizedText, "<EMAIL_") != 1 {
		t.Errorf("tokenized_text = %q, want exactly one EMAIL token", tok.TokenizedText)
	}
	for _, leak := range []string{"Иван", "ivan@example.com"} {
		if strings.Contains(tok.TokenizedText, leak) {
			t.Errorf("tokenized_text %q leaks %q", tok.TokenizedText, leak)
		}
	}
}

// TestPolicyProjectionOnlyEmailAllowed proves that when only EMAIL is allowed,
// only the email is tokenized, and the reporting pass emits one disabled
// merge-winner for the overlap with personal=false, type_disabled_by_policy and
// an empty owner id.
func TestPolicyProjectionOnlyEmailAllowed(t *testing.T) {
	nameS, nameE := byteSpan(t, overlapText, "Иван")
	pipe := newPolicyProjectionPipeline(t, overlapModelDetector(nameS, nameE),
		string(detection.TypeEmail))

	resp, err := pipe.Handlers().Detect(context.Background(), DetectRequest{Text: overlapText, IncludeNonPersonal: true})
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	if len(resp.Entities) != 2 {
		t.Fatalf("entities = %d, want 2: %+v", len(resp.Entities), resp.Entities)
	}
	email := entityByType(resp.Entities, string(detection.TypeEmail))
	if email == nil || !email.Personal {
		t.Errorf("EMAIL entity = %+v, want personal", email)
	}
	city := entityByType(resp.Entities, string(detection.TypeAddressCity))
	if city == nil {
		t.Fatalf("disabled CITY reporting entity missing: %+v", resp.Entities)
	}
	if city.Personal {
		t.Errorf("disabled CITY personal = true, want false")
	}
	if !entityHasReason(city.ReasonCodes, string(ownership.ReasonTypeDisabledByPolicy)) {
		t.Errorf("disabled CITY reasons %v missing type_disabled_by_policy", city.ReasonCodes)
	}
	if city.OwnerID != "" {
		t.Errorf("disabled CITY owner_id = %q, want empty", city.OwnerID)
	}
	if entityByType(resp.Entities, string(detection.TypeFullName)) != nil {
		t.Errorf("FULL_NAME emitted as separate top-level entity, want component of disabled CITY only: %+v", resp.Entities)
	}

	tok, err := pipe.Handlers().Tokenize(context.Background(), TokenizeRequest{Text: overlapText, ScopeID: "s1"})
	if err != nil {
		t.Fatalf("Tokenize() error = %v", err)
	}
	if !strings.Contains(tok.TokenizedText, "<EMAIL_") {
		t.Errorf("tokenized_text = %q, want EMAIL token", tok.TokenizedText)
	}
	if strings.Contains(tok.TokenizedText, "ivan@example.com") {
		t.Errorf("tokenized_text %q leaks email", tok.TokenizedText)
	}
	if !strings.Contains(tok.TokenizedText, "Иван") {
		t.Errorf("tokenized_text %q masked the disabled CITY name", tok.TokenizedText)
	}
}

// TestPolicyProjectionNonOverlapOwnerIDIsolation proves that a disabled
// reporting-only entity on a separate line does not receive an owner id and
// does not collide with the owner id of an allowed personal entity.
func TestPolicyProjectionNonOverlapOwnerIDIsolation(t *testing.T) {
	const text = "email: ivan@example.com\nИван"
	nameS, nameE := byteSpan(t, text, "Иван")
	pipe := newPolicyProjectionPipeline(t, overlapModelDetector(nameS, nameE),
		string(detection.TypeEmail))

	resp, err := pipe.Handlers().Detect(context.Background(), DetectRequest{Text: text, IncludeNonPersonal: true})
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	if len(resp.Entities) != 2 {
		t.Fatalf("entities = %d, want 2: %+v", len(resp.Entities), resp.Entities)
	}
	email := entityByType(resp.Entities, string(detection.TypeEmail))
	if email == nil || !email.Personal {
		t.Errorf("EMAIL entity = %+v, want personal", email)
	}
	if email.OwnerID == "" {
		t.Errorf("EMAIL owner_id = %q, want a deterministic owner id", email.OwnerID)
	}
	name := entityByType(resp.Entities, string(detection.TypeAddressCity))
	if name == nil {
		t.Fatalf("disabled CITY reporting entity missing: %+v", resp.Entities)
	}
	if name.Personal {
		t.Errorf("disabled CITY personal = true, want false")
	}
	if !entityHasReason(name.ReasonCodes, string(ownership.ReasonTypeDisabledByPolicy)) {
		t.Errorf("disabled CITY reasons %v missing type_disabled_by_policy", name.ReasonCodes)
	}
	if name.OwnerID != "" {
		t.Errorf("disabled CITY owner_id = %q, want empty", name.OwnerID)
	}
	if name.OwnerID == email.OwnerID {
		t.Errorf("disabled CITY owner_id %q collides with EMAIL owner_id", name.OwnerID)
	}
	if entityByType(resp.Entities, string(detection.TypeFullName)) != nil {
		t.Errorf("FULL_NAME emitted as separate top-level entity, want component of disabled CITY only: %+v", resp.Entities)
	}
}
