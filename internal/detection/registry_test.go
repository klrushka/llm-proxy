package detection

import (
	"reflect"
	"testing"
)

// expectedCanonicalTypes independently enumerates the 28 canonical PII types
// from the Supported PII type registry requirement. It is test-owned and does
// not reference production defaultTypes, so deleting or misspelling a
// production type cannot keep these tests green.
var expectedCanonicalTypes = []Type{
	Type("FULL_NAME"),
	Type("FIRST_NAME"),
	Type("LAST_NAME"),
	Type("MIDDLE_NAME"),
	Type("BIRTH_DATE"),
	Type("BIRTH_PLACE"),
	Type("PASSPORT_NUMBER"),
	Type("CITIZENSHIP"),
	Type("PASSPORT_ISSUER"),
	Type("PASSPORT_DIVISION_CODE"),
	Type("PASSPORT_ISSUE_DATE"),
	Type("DRIVER_LICENSE_NUMBER"),
	Type("ADDRESS"),
	Type("ADDRESS_COUNTRY"),
	Type("ADDRESS_POSTAL_CODE"),
	Type("ADDRESS_REGION"),
	Type("ADDRESS_CITY"),
	Type("ADDRESS_STREET"),
	Type("ADDRESS_HOUSE"),
	Type("ADDRESS_BUILDING"),
	Type("ADDRESS_APARTMENT"),
	Type("EMAIL"),
	Type("PHONE"),
	Type("INN_PERSON"),
	Type("BANK_CARD_NUMBER"),
	Type("CARD_CVV"),
	Type("CARD_PIN"),
	Type("CARDHOLDER_NAME"),
}

func TestDefaultRegistryHasAllCanonicalTypes(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	for _, name := range expectedCanonicalTypes {
		if !r.Lookup(name) {
			t.Errorf("Lookup(%q) = false, want true", name)
		}
		if !r.Contains(name) {
			t.Errorf("Contains(%q) = false, want true", name)
		}
	}
}

func TestDefaultRegistryHasExactlyCanonicalTypes(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	got := r.Types()
	if len(got) != len(expectedCanonicalTypes) {
		t.Fatalf("len(Types()) = %d, want %d", len(got), len(expectedCanonicalTypes))
	}
	for _, name := range expectedCanonicalTypes {
		if !r.Lookup(name) {
			t.Errorf("Types() missing canonical type %q", name)
		}
	}
}

func TestLookupUnknownType(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	for _, name := range []Type{"BOGUS", "", "FULL_NAME_EXTRA", "full_name"} {
		if r.Lookup(name) {
			t.Errorf("Lookup(%q) = true, want false", name)
		}
	}
}

func TestLookupCaseSensitive(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if r.Lookup("full_name") {
		t.Error("Lookup(\"full_name\") = true, want false (case-sensitive)")
	}
	if r.Lookup("Email") {
		t.Error("Lookup(\"Email\") = true, want false (case-sensitive)")
	}
}

func TestTypesReturnsSortedDefensiveCopy(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	got := r.Types()
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Errorf("Types() not sorted at %d: %q >= %q", i, got[i-1], got[i])
		}
	}

	original := got[0]
	got[0] = "MUTATED"
	if r.Lookup("MUTATED") {
		t.Error("registry changed after mutating Types() result")
	}
	if !r.Lookup(original) {
		t.Errorf("registry lost original type %q after Types() mutation", original)
	}
}

func TestExtraTypeRegistration(t *testing.T) {
	r, err := New(Type("CUSTOM_TYPE"))
	if err != nil {
		t.Fatalf("New(extra) error = %v", err)
	}
	if !r.Lookup("CUSTOM_TYPE") {
		t.Error("Lookup(CUSTOM_TYPE) = false, want true")
	}
	if !r.Lookup(TypeFullName) {
		t.Error("extra registration dropped canonical type FULL_NAME")
	}
}

func TestExtraTypeRegistrationRejectsEmpty(t *testing.T) {
	if _, err := New(Type("")); err == nil {
		t.Fatal("New(empty extra) expected error")
	}
}

func TestExtraTypeRegistrationRejectsDuplicate(t *testing.T) {
	if _, err := New(TypeFullName); err == nil {
		t.Fatal("New(duplicate canonical extra) expected error")
	}
	if _, err := New(Type("CUSTOM"), Type("CUSTOM")); err == nil {
		t.Fatal("New(duplicate extra) expected error")
	}
}

func TestResolveModelLabelRubertCanonical(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	tests := []struct {
		label string
		want  Type
	}{
		{"FIRST_NAME", TypeFirstName},
		{"LAST_NAME", TypeLastName},
		{"MIDDLE_NAME", TypeMiddleName},
		{"EMAIL", TypeEmail},
		{"PHONE", TypePhone},
	}
	for _, tt := range tests {
		got, ok := r.ResolveModelLabel(SourceRubert, tt.label)
		if !ok || got != tt.want {
			t.Errorf("ResolveModelLabel(rubert, %q) = %q,%v, want %q,true", tt.label, got, ok, tt.want)
		}
	}
}

func TestResolveModelLabelGlinerAliases(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	tests := []struct {
		label string
		want  Type
	}{
		{"ru_pii_person", TypeFullName},
		{"ru_pii_phone", TypePhone},
		{"ru_pii_email", TypeEmail},
	}
	for _, tt := range tests {
		got, ok := r.ResolveModelLabel(SourceGliner, tt.label)
		if !ok || got != tt.want {
			t.Errorf("ResolveModelLabel(gliner, %q) = %q,%v, want %q,true", tt.label, got, ok, tt.want)
		}
	}
}

func TestResolveModelLabelGlinerIntermediate(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	tests := []struct {
		label string
		want  Type
	}{
		{"ru_pii_location", TypeLocation},
		{"ru_pii_date", TypeDate},
	}
	for _, tt := range tests {
		got, ok := r.ResolveModelLabel(SourceGliner, tt.label)
		if !ok || got != tt.want {
			t.Errorf("ResolveModelLabel(gliner, %q) = %q,%v, want %q,true", tt.label, got, ok, tt.want)
		}
	}
}

func TestResolveModelLabelRubertAddressLabels(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	tests := []struct {
		label string
		want  Type
	}{
		{"COUNTRY", TypeAddressCountry},
		{"REGION", TypeAddressRegion},
		{"DISTRICT", TypeAddressRegion},
		{"CITY", TypeAddressCity},
		{"STREET", TypeAddressStreet},
		{"HOUSE", TypeAddressHouse},
	}
	for _, tt := range tests {
		got, ok := r.ResolveModelLabel(SourceRubert, tt.label)
		if !ok || got != tt.want {
			t.Errorf("ResolveModelLabel(rubert, %q) = %q,%v, want %q,true", tt.label, got, ok, tt.want)
		}
	}
}

func TestResolveModelLabelRejectsIntermediateAndUnknown(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	rejected := []string{
		"ru_pii",
		"DATE",
		"LOCATION",
		"BOGUS",
		"",
		"PASSPORT",
		"INN",
		"CREDIT_CARD",
		"DRIVER_LICENSE",
	}
	for _, label := range rejected {
		if got, ok := r.ResolveModelLabel(SourceGliner, label); ok {
			t.Errorf("ResolveModelLabel(gliner, %q) = %q,true, want false", label, got)
		}
		if got, ok := r.ResolveModelLabel(SourceRubert, label); ok {
			t.Errorf("ResolveModelLabel(rubert, %q) = %q,true, want false", label, got)
		}
	}
}

// TestResolveModelLabelRejectsCrossSourceCanonical proves that a canonical
// label from a source that does not emit it is rejected fail-closed, and that
// structural canonical aliases never resolve directly.
func TestResolveModelLabelRejectsCrossSourceCanonical(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	// RuBERT does not emit FULL_NAME; GLiNER does not emit FIRST_NAME.
	if got, ok := r.ResolveModelLabel(SourceRubert, "FULL_NAME"); ok {
		t.Errorf("ResolveModelLabel(rubert, FULL_NAME) = %q,true, want false", got)
	}
	if got, ok := r.ResolveModelLabel(SourceGliner, "FIRST_NAME"); ok {
		t.Errorf("ResolveModelLabel(gliner, FIRST_NAME) = %q,true, want false", got)
	}
	// Structural canonical aliases must never resolve directly for either
	// source; they require Go validation.
	for _, label := range []string{"PASSPORT_NUMBER", "INN_PERSON", "BANK_CARD_NUMBER", "DRIVER_LICENSE_NUMBER"} {
		if got, ok := r.ResolveModelLabel(SourceRubert, label); ok {
			t.Errorf("ResolveModelLabel(rubert, %q) = %q,true, want false", label, got)
		}
		if got, ok := r.ResolveModelLabel(SourceGliner, label); ok {
			t.Errorf("ResolveModelLabel(gliner, %q) = %q,true, want false", label, got)
		}
	}
}

func TestResolveModelLabelRejectsUnknownSource(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	for _, model := range []Source{"other", "", "regex", "validator"} {
		if got, ok := r.ResolveModelLabel(model, "FULL_NAME"); ok {
			t.Errorf("ResolveModelLabel(%q, FULL_NAME) = %q,true, want false", model, got)
		}
	}
}

func TestTypesReflectsExtraRegistration(t *testing.T) {
	r, err := New(Type("CUSTOM_TYPE"))
	if err != nil {
		t.Fatalf("New(extra) error = %v", err)
	}
	got := r.Types()
	want := append(append([]Type(nil), expectedCanonicalTypes...), Type("CUSTOM_TYPE"))
	sortTypes(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Types() = %v, want %v", got, want)
	}
}

func sortTypes(in []Type) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}
