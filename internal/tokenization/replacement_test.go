package tokenization

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/merge"
	"github.com/klrushka/llm-proxy/internal/ownership"
)

// fakeIssuer issues deterministic tokens keyed by (scope, type, value) so
// repeated values reuse a token, and can be forced to fail.
type fakeIssuer struct {
	err    error
	tokens map[string]string
}

func (f *fakeIssuer) Token(scope string, typ detection.Type, value string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	key := scope + "\x00" + string(typ) + "\x00" + value
	if t, ok := f.tokens[key]; ok {
		return t, nil
	}
	t := "<" + string(typ) + "_" + strconv.Itoa(len(f.tokens)+1) + ">"
	f.tokens[key] = t
	return t, nil
}

func ownEnt(t detection.Type, start, end int, personal bool, comps ...detection.Candidate) ownership.Entity {
	return ownership.Entity{
		Entity: merge.Entity{
			Candidate:  detection.Candidate{Type: t, Start: start, End: end, Confidence: 1.0},
			Components: comps,
		},
		Personal: personal,
	}
}

func TestReplaceRightToLeftCyrillic(t *testing.T) {
	text := "Клиент Иванов Иван, телефон +7 900 123-45-67, email ivanov@example.com"
	nameStart := strings.Index(text, "Иванов Иван")
	phoneStart := strings.Index(text, "+7 900 123-45-67")
	emailStart := strings.Index(text, "ivanov@example.com")
	results := []ownership.Entity{
		ownEnt(detection.TypeFullName, nameStart, nameStart+len("Иванов Иван"), true),
		ownEnt(detection.TypePhone, phoneStart, phoneStart+len("+7 900 123-45-67"), true),
		ownEnt(detection.TypeEmail, emailStart, emailStart+len("ivanov@example.com"), true),
	}

	got, err := Replace(text, "scope-1", results, &fakeIssuer{tokens: map[string]string{}})
	if err != nil {
		t.Fatalf("Replace() error = %v", err)
	}

	// Right-to-left processing issues email first (token 1), phone second
	// (token 2), name last (token 3).
	want := "Клиент <FULL_NAME_3>, телефон <PHONE_2>, email <EMAIL_1>"
	if got.Text != want {
		t.Errorf("Text = %q, want %q", got.Text, want)
	}
	if len(got.Replacements) != 3 {
		t.Fatalf("Replacements = %d, want 3", len(got.Replacements))
	}
	for _, v := range []string{"Иванов Иван", "+7 900 123-45-67", "ivanov@example.com"} {
		if strings.Contains(got.Text, v) {
			t.Errorf("tokenized text still contains %q: %q", v, got.Text)
		}
	}
}

func TestReplacePersonalOnly(t *testing.T) {
	text := "Александр Пушкин — русский поэт. Клиент Иванов Иван."
	poetStart := strings.Index(text, "Александр Пушкин")
	nameStart := strings.Index(text, "Иванов Иван")
	results := []ownership.Entity{
		ownEnt(detection.TypeFullName, poetStart, poetStart+len("Александр Пушкин"), false),
		ownEnt(detection.TypeFullName, nameStart, nameStart+len("Иванов Иван"), true),
	}

	got, err := Replace(text, "scope-1", results, &fakeIssuer{tokens: map[string]string{}})
	if err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if !strings.Contains(got.Text, "Александр Пушкин") {
		t.Errorf("non-personal entity was replaced: %q", got.Text)
	}
	if strings.Contains(got.Text, "Иванов Иван") {
		t.Errorf("personal entity not replaced: %q", got.Text)
	}
	if len(got.Replacements) != 1 {
		t.Fatalf("Replacements = %d, want 1", len(got.Replacements))
	}
}

func TestReplaceRepeatedValueReusesGeneratorToken(t *testing.T) {
	text := "Иванов Иван и Иванов Иван"
	start := strings.Index(text, "Иванов Иван")
	second := strings.Index(text[start+1:], "Иванов Иван") + start + 1
	results := []ownership.Entity{
		ownEnt(detection.TypeFullName, start, start+len("Иванов Иван"), true),
		ownEnt(detection.TypeFullName, second, second+len("Иванов Иван"), true),
	}

	g := newGenerator(&seqReader{blocks: [][]byte{block("11111111111111111111111111111111")}}, mustRegistry(t))
	got, err := Replace(text, "scope-1", results, g)
	if err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if len(got.Replacements) != 2 {
		t.Fatalf("Replacements = %d, want 2", len(got.Replacements))
	}
	if got.Replacements[0].Token != got.Replacements[1].Token {
		t.Errorf("repeated value got different tokens %q and %q", got.Replacements[0].Token, got.Replacements[1].Token)
	}
	if strings.Count(got.Text, got.Replacements[0].Token) != 2 {
		t.Errorf("expected token to appear twice: %q", got.Text)
	}
}

func TestReplaceOuterSpanComponentsNotTokenized(t *testing.T) {
	text := "Клиент Иванов Иван Иванович"
	start := strings.Index(text, "Иванов Иван Иванович")
	comp := detection.Candidate{Type: detection.TypeFirstName, Start: start, End: start + len("Иванов"), Confidence: 0.9}
	results := []ownership.Entity{
		{
			Entity: merge.Entity{
				Candidate:  detection.Candidate{Type: detection.TypeFullName, Start: start, End: start + len("Иванов Иван Иванович"), Confidence: 0.95},
				Components: []detection.Candidate{comp},
			},
			Personal: true,
		},
	}

	got, err := Replace(text, "scope-1", results, &fakeIssuer{tokens: map[string]string{}})
	if err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if len(got.Replacements) != 1 {
		t.Fatalf("Replacements = %d, want 1 (outer only)", len(got.Replacements))
	}
	if strings.Contains(got.Text, "Иванов") {
		t.Errorf("component text leaked: %q", got.Text)
	}
	if strings.Count(got.Text, got.Replacements[0].Token) != 1 {
		t.Errorf("outer token should appear once: %q", got.Text)
	}
}

func TestReplaceDuplicateAndContainedSpans(t *testing.T) {
	text := "Клиент Иванов Иван Иванович"
	start := strings.Index(text, "Иванов Иван Иванович")
	results := []ownership.Entity{
		ownEnt(detection.TypeFullName, start, start+len("Иванов Иван Иванович"), true),
		ownEnt(detection.TypeFullName, start, start+len("Иванов Иван Иванович"), true),
		ownEnt(detection.TypeFirstName, start, start+len("Иванов"), true),
	}

	got, err := Replace(text, "scope-1", results, &fakeIssuer{tokens: map[string]string{}})
	if err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if len(got.Replacements) != 1 {
		t.Fatalf("Replacements = %d, want 1 (outer kept)", len(got.Replacements))
	}
	if strings.Contains(got.Text, "Иванов") {
		t.Errorf("contained span leaked: %q", got.Text)
	}
	if strings.Count(got.Text, got.Replacements[0].Token) != 1 {
		t.Errorf("outer token should appear once: %q", got.Text)
	}
}

func TestReplaceInvalidSpans(t *testing.T) {
	text := "Клиент Иванов Иван"
	start := strings.Index(text, "Иванов Иван")
	cases := []struct {
		name  string
		start int
		end   int
	}{
		{"negative-start", -1, start + len("Иванов Иван")},
		{"reversed", start + len("Иванов Иван"), start},
		{"out-of-range", start, len(text) + 5},
		{"mid-rune", start + 1, start + len("Иванов Иван")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results := []ownership.Entity{ownEnt(detection.TypeFullName, tc.start, tc.end, true)}
			_, err := Replace(text, "scope-1", results, &fakeIssuer{tokens: map[string]string{}})
			if !errors.Is(err, ErrInvalidSpan) {
				t.Errorf("Replace() error = %v, want ErrInvalidSpan", err)
			}
		})
	}
}

func TestReplacePartialOverlapFails(t *testing.T) {
	text := "Клиент Иванов Иван Иванович"
	start := strings.Index(text, "Иванов Иван Иванович")
	results := []ownership.Entity{
		ownEnt(detection.TypeFullName, start, start+len("Иванов Иван"), true),
		ownEnt(detection.TypeFirstName, start+len("Иванов "), start+len("Иванов Иван Иванович"), true),
	}

	_, err := Replace(text, "scope-1", results, &fakeIssuer{tokens: map[string]string{}})
	if !errors.Is(err, ErrPartialOverlap) {
		t.Errorf("Replace() error = %v, want ErrPartialOverlap", err)
	}
}

func TestReplaceInputOrderIndependence(t *testing.T) {
	text := "Клиент Иванов Иван, телефон +7 900 123-45-67"
	nameStart := strings.Index(text, "Иванов Иван")
	phoneStart := strings.Index(text, "+7 900 123-45-67")
	base := []ownership.Entity{
		ownEnt(detection.TypeFullName, nameStart, nameStart+len("Иванов Иван"), true),
		ownEnt(detection.TypePhone, phoneStart, phoneStart+len("+7 900 123-45-67"), true),
	}
	reversed := []ownership.Entity{
		ownEnt(detection.TypePhone, phoneStart, phoneStart+len("+7 900 123-45-67"), true),
		ownEnt(detection.TypeFullName, nameStart, nameStart+len("Иванов Иван"), true),
	}

	a, err := Replace(text, "scope-1", base, &fakeIssuer{tokens: map[string]string{}})
	if err != nil {
		t.Fatalf("Replace(base) error = %v", err)
	}
	b, err := Replace(text, "scope-1", reversed, &fakeIssuer{tokens: map[string]string{}})
	if err != nil {
		t.Fatalf("Replace(reversed) error = %v", err)
	}
	if a.Text != b.Text {
		t.Errorf("input order changed text:\n got %q\nwant %q", b.Text, a.Text)
	}
	if !reflect.DeepEqual(a.Replacements, b.Replacements) {
		t.Errorf("input order changed replacements:\n got %+v\nwant %+v", b.Replacements, a.Replacements)
	}
}

func TestReplaceNoAliasing(t *testing.T) {
	text := "Клиент Иванов Иван Иванович"
	start := strings.Index(text, "Иванов Иван Иванович")
	src := []detection.Source{detection.SourceRubert, detection.SourceRegex}
	comp := detection.Candidate{Type: detection.TypeFirstName, Start: start, End: start + len("Иванов"), Confidence: 0.9, Sources: []detection.Source{detection.SourceRubert}}
	in := []ownership.Entity{
		{
			Entity: merge.Entity{
				Candidate:  detection.Candidate{Type: detection.TypeFullName, Start: start, End: start + len("Иванов Иван Иванович"), Confidence: 0.95, Sources: src},
				Components: []detection.Candidate{comp},
			},
			Personal:    true,
			ReasonCodes: []ownership.ReasonCode{ownership.ReasonClientContext},
		},
	}
	orig := make([]ownership.Entity, len(in))
	for i, e := range in {
		orig[i] = e
		orig[i].Sources = append([]detection.Source(nil), e.Sources...)
		if e.Components != nil {
			orig[i].Components = make([]detection.Candidate, len(e.Components))
			for j, c := range e.Components {
				orig[i].Components[j] = c
				orig[i].Components[j].Sources = append([]detection.Source(nil), c.Sources...)
			}
		}
		orig[i].ReasonCodes = append([]ownership.ReasonCode(nil), e.ReasonCodes...)
	}

	got, err := Replace(text, "scope-1", in, &fakeIssuer{tokens: map[string]string{}})
	if err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if !reflect.DeepEqual(in, orig) {
		t.Errorf("Replace() mutated input: got %+v, want %+v", in, orig)
	}

	got.Replacements[0].Entity.Sources[0] = detection.SourceValidator
	got.Replacements[0].Entity.Components[0].Sources[0] = detection.SourceValidator
	got.Replacements[0].Entity.ReasonCodes[0] = ownership.ReasonAmbiguous
	if !reflect.DeepEqual(in, orig) {
		t.Errorf("mutating result aliased input: got %+v, want %+v", in, orig)
	}
}

func TestReplaceIssuerFailureNoPartialResult(t *testing.T) {
	text := "Клиент Иванов Иван, телефон +7 900 123-45-67"
	nameStart := strings.Index(text, "Иванов Иван")
	phoneStart := strings.Index(text, "+7 900 123-45-67")
	results := []ownership.Entity{
		ownEnt(detection.TypeFullName, nameStart, nameStart+len("Иванов Иван"), true),
		ownEnt(detection.TypePhone, phoneStart, phoneStart+len("+7 900 123-45-67"), true),
	}

	got, err := Replace(text, "scope-1", results, &fakeIssuer{tokens: map[string]string{}, err: errors.New("boom")})
	if err == nil {
		t.Fatalf("Replace() error = nil, want issuer error")
	}
	if got.Text != "" {
		t.Errorf("partial tokenized text returned on issuer failure: %q", got.Text)
	}
	if len(got.Replacements) != 0 {
		t.Errorf("partial replacements returned on issuer failure: %+v", got.Replacements)
	}
}

func TestReplaceEmptyAndNilResults(t *testing.T) {
	text := "Клиент Иванов Иван"
	iss := &fakeIssuer{tokens: map[string]string{}}
	for _, results := range [][]ownership.Entity{nil, {}} {
		got, err := Replace(text, "scope-1", results, iss)
		if err != nil {
			t.Fatalf("Replace() error = %v", err)
		}
		if got.Text != text {
			t.Errorf("Text = %q, want %q", got.Text, text)
		}
		if len(got.Replacements) != 0 {
			t.Errorf("Replacements = %+v, want none", got.Replacements)
		}
	}
}

func TestReplaceNilIssuer(t *testing.T) {
	text := "Клиент Иванов Иван"
	start := strings.Index(text, "Иванов Иван")
	results := []ownership.Entity{ownEnt(detection.TypeFullName, start, start+len("Иванов Иван"), true)}
	_, err := Replace(text, "scope-1", results, nil)
	if !errors.Is(err, ErrNilIssuer) {
		t.Errorf("Replace() error = %v, want ErrNilIssuer", err)
	}
}

func TestReplaceExactDuplicateWinnerDeterministic(t *testing.T) {
	text := "Клиент Иванов Иван"
	start := strings.Index(text, "Иванов Иван")
	end := start + len("Иванов Иван")

	// Two exact-coordinate personal duplicates with different types and
	// metadata. The winner must be chosen by the total tie-break order
	// (Type ascending), independent of caller input order.
	a := ownership.Entity{
		Entity: merge.Entity{
			Candidate: detection.Candidate{
				Type:       detection.TypeFullName,
				Start:      start,
				End:        end,
				Confidence: 0.9,
				Sources:    []detection.Source{detection.SourceRubert},
			},
		},
		Personal:       true,
		OwnerType:      ownership.OwnerTypePerson,
		OwnerID:        "person-1",
		OwnershipScore: 0.8,
		ReasonCodes:    []ownership.ReasonCode{ownership.ReasonClientContext},
	}
	b := ownership.Entity{
		Entity: merge.Entity{
			Candidate: detection.Candidate{
				Type:       detection.TypeFirstName,
				Start:      start,
				End:        end,
				Confidence: 0.95,
				Sources:    []detection.Source{detection.SourceRegex},
			},
		},
		Personal:       true,
		OwnerType:      ownership.OwnerTypePerson,
		OwnerID:        "person-1",
		OwnershipScore: 0.9,
		ReasonCodes:    []ownership.ReasonCode{ownership.ReasonPositiveContext},
	}

	// Type ascending: FIRST_NAME < FULL_NAME, so b must win regardless of order.
	for _, tc := range []struct {
		name string
		in   []ownership.Entity
	}{
		{"a-first", []ownership.Entity{a, b}},
		{"b-first", []ownership.Entity{b, a}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Replace(text, "scope-1", tc.in, &fakeIssuer{tokens: map[string]string{}})
			if err != nil {
				t.Fatalf("Replace() error = %v", err)
			}
			if len(got.Replacements) != 1 {
				t.Fatalf("Replacements = %d, want 1", len(got.Replacements))
			}
			r := got.Replacements[0]
			if r.Entity.Type != detection.TypeFirstName {
				t.Errorf("winner type = %s, want FIRST_NAME", r.Entity.Type)
			}
			if r.Entity.Confidence != 0.95 {
				t.Errorf("winner confidence = %v, want 0.95", r.Entity.Confidence)
			}
			if r.Entity.OwnershipScore != 0.9 {
				t.Errorf("winner ownership score = %v, want 0.9", r.Entity.OwnershipScore)
			}
			if !reflect.DeepEqual(r.Entity.Sources, []detection.Source{detection.SourceRegex}) {
				t.Errorf("winner sources = %v, want [regex]", r.Entity.Sources)
			}
			if !reflect.DeepEqual(r.Entity.ReasonCodes, []ownership.ReasonCode{ownership.ReasonPositiveContext}) {
				t.Errorf("winner reasons = %v, want [positive_context]", r.Entity.ReasonCodes)
			}
			if !strings.Contains(got.Text, r.Token) {
				t.Errorf("tokenized text missing winner token %q: %q", r.Token, got.Text)
			}
		})
	}

	// Both orders must produce identical tokenized text and metadata.
	first, err := Replace(text, "scope-1", []ownership.Entity{a, b}, &fakeIssuer{tokens: map[string]string{}})
	if err != nil {
		t.Fatalf("Replace(a-first) error = %v", err)
	}
	second, err := Replace(text, "scope-1", []ownership.Entity{b, a}, &fakeIssuer{tokens: map[string]string{}})
	if err != nil {
		t.Fatalf("Replace(b-first) error = %v", err)
	}
	if first.Text != second.Text {
		t.Errorf("input order changed text:\n got %q\nwant %q", second.Text, first.Text)
	}
	if !reflect.DeepEqual(first.Replacements, second.Replacements) {
		t.Errorf("input order changed replacements:\n got %+v\nwant %+v", second.Replacements, first.Replacements)
	}
}
