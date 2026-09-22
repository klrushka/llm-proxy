package testcorpus

import (
	"path/filepath"
	"strings"
	"testing"
)

const corpusPath = "../../testdata/pii-corpus.json"

func TestLoadAndValidateFixture(t *testing.T) {
	c, err := Load(corpusPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := Validate(c); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestFixtureVersionAndUnit(t *testing.T) {
	c, err := Load(corpusPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c.Version != "1" {
		t.Errorf("version = %q, want 1", c.Version)
	}
	if c.OffsetUnit != OffsetUnit {
		t.Errorf("offset_unit = %q, want %q", c.OffsetUnit, OffsetUnit)
	}
}

func TestFixtureHasThreeCases(t *testing.T) {
	c, err := Load(corpusPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(c.Cases) != 3 {
		t.Fatalf("len(cases) = %d, want 3", len(c.Cases))
	}
}

func TestFixtureUniqueIDs(t *testing.T) {
	c, err := Load(corpusPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	seen := make(map[string]bool)
	for _, tc := range c.Cases {
		if seen[tc.ID] {
			t.Errorf("duplicate case id %q", tc.ID)
		}
		seen[tc.ID] = true
	}
}

func TestFixtureExactByteOffsets(t *testing.T) {
	c, err := Load(corpusPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	for _, tc := range c.Cases {
		for _, s := range tc.Spans {
			if got := tc.Input[s.Start:s.End]; got != s.Value {
				t.Errorf("case %q span %q: input[%d:%d] = %q, want %q", tc.ID, s.Type, s.Start, s.End, got, s.Value)
			}
		}
	}
}

func TestFixtureExpectedMasks(t *testing.T) {
	c, err := Load(corpusPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := map[string]string{
		"positive-full-name-email": "<FULL_NAME>, email: <EMAIL>",
		"hard-negative-pushkin":    "Пушкин — поэт",
		"positive-phone":           "Телефон: <PHONE>",
	}
	for _, tc := range c.Cases {
		if tc.ExpectedMask != want[tc.ID] {
			t.Errorf("case %q expected_mask = %q, want %q", tc.ID, tc.ExpectedMask, want[tc.ID])
		}
	}
}

func TestFixtureHardNegativeUnchanged(t *testing.T) {
	c, err := Load(corpusPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	for _, tc := range c.Cases {
		if tc.ID != "hard-negative-pushkin" {
			continue
		}
		if tc.ExpectedMask != tc.Input {
			t.Errorf("hard negative expected_mask %q != input %q", tc.ExpectedMask, tc.Input)
		}
		for _, s := range tc.Spans {
			if s.Personal {
				t.Errorf("hard negative span %q marked personal", s.Type)
			}
			if s.MaskToken != "" {
				t.Errorf("hard negative span %q has mask_token %q", s.Type, s.MaskToken)
			}
		}
	}
}

func TestValidateRejectsBadVersion(t *testing.T) {
	c := Corpus{Version: "2", OffsetUnit: OffsetUnit, Cases: nil}
	if err := Validate(c); err == nil {
		t.Fatal("Validate() expected error for bad version")
	}
}

func TestValidateRejectsBadOffsetUnit(t *testing.T) {
	c := Corpus{Version: "1", OffsetUnit: "char", Cases: nil}
	if err := Validate(c); err == nil {
		t.Fatal("Validate() expected error for bad offset_unit")
	}
}

func TestValidateRejectsDuplicateID(t *testing.T) {
	c := Corpus{Version: "1", OffsetUnit: OffsetUnit, Cases: []Case{
		{ID: "a", Input: "x", ExpectedMask: "x"},
		{ID: "a", Input: "y", ExpectedMask: "y"},
	}}
	if err := Validate(c); err == nil {
		t.Fatal("Validate() expected error for duplicate id")
	}
}

func TestValidateRejectsOutOfBounds(t *testing.T) {
	c := Corpus{Version: "1", OffsetUnit: OffsetUnit, Cases: []Case{
		{ID: "a", Input: "abc", ExpectedMask: "abc", Spans: []Span{
			{Type: "X", Start: 0, End: 10, Value: "abc", Personal: false},
		}},
	}}
	if err := Validate(c); err == nil {
		t.Fatal("Validate() expected error for out-of-bounds span")
	}
}

func TestValidateRejectsValueMismatch(t *testing.T) {
	c := Corpus{Version: "1", OffsetUnit: OffsetUnit, Cases: []Case{
		{ID: "a", Input: "abc", ExpectedMask: "abc", Spans: []Span{
			{Type: "X", Start: 0, End: 3, Value: "xyz", Personal: false},
		}},
	}}
	if err := Validate(c); err == nil {
		t.Fatal("Validate() expected error for value mismatch")
	}
}

func TestValidateRejectsOverlappingSpans(t *testing.T) {
	c := Corpus{Version: "1", OffsetUnit: OffsetUnit, Cases: []Case{
		{ID: "a", Input: "abcd", ExpectedMask: "abcd", Spans: []Span{
			{Type: "X", Start: 0, End: 3, Value: "abc", Personal: false},
			{Type: "Y", Start: 2, End: 4, Value: "cd", Personal: false},
		}},
	}}
	if err := Validate(c); err == nil {
		t.Fatal("Validate() expected error for overlapping spans")
	}
}

func TestValidateRejectsPersonalWithoutMaskToken(t *testing.T) {
	c := Corpus{Version: "1", OffsetUnit: OffsetUnit, Cases: []Case{
		{ID: "a", Input: "abc", ExpectedMask: "abc", Spans: []Span{
			{Type: "X", Start: 0, End: 3, Value: "abc", Personal: true},
		}},
	}}
	if err := Validate(c); err == nil {
		t.Fatal("Validate() expected error for personal span without mask_token")
	}
}

func TestValidateRejectsNonPersonalWithMaskToken(t *testing.T) {
	c := Corpus{Version: "1", OffsetUnit: OffsetUnit, Cases: []Case{
		{ID: "a", Input: "abc", ExpectedMask: "abc", Spans: []Span{
			{Type: "X", Start: 0, End: 3, Value: "abc", Personal: false, MaskToken: "<X>"},
		}},
	}}
	if err := Validate(c); err == nil {
		t.Fatal("Validate() expected error for non-personal span with mask_token")
	}
}

func TestValidateRejectsMaskTokenMissingFromExpectedMask(t *testing.T) {
	c := Corpus{Version: "1", OffsetUnit: OffsetUnit, Cases: []Case{
		{ID: "a", Input: "abc", ExpectedMask: "no token here", Spans: []Span{
			{Type: "X", Start: 0, End: 3, Value: "abc", Personal: true, MaskToken: "<X>"},
		}},
	}}
	if err := Validate(c); err == nil {
		t.Fatal("Validate() expected error for mask_token missing from expected_mask")
	}
}

func TestFixturePathResolves(t *testing.T) {
	if _, err := filepath.Abs(corpusPath); err != nil {
		t.Fatalf("Abs() error = %v", err)
	}
}

func TestFixtureIsSynthetic(t *testing.T) {
	c, err := Load(corpusPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	for _, tc := range c.Cases {
		if strings.Contains(tc.Input, "Иванов Иван Иванович") && tc.ID != "positive-full-name-email" {
			t.Errorf("case %q reuses synthetic name outside its fixture", tc.ID)
		}
	}
}
