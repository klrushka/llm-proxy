package merge

import (
	"reflect"
	"testing"

	"github.com/klrushka/llm-proxy/internal/detection"
)

func cand(t detection.Type, start, end int, conf float64, sources ...detection.Source) detection.Candidate {
	return detection.Candidate{Type: t, Start: start, End: end, Confidence: conf, Sources: sources}
}

func entity(c detection.Candidate, comps ...detection.Candidate) Entity {
	return Entity{Candidate: c, Components: comps}
}

func TestExactDuplicateSourceUnionAndMaxConfidence(t *testing.T) {
	in := []detection.Candidate{
		cand(detection.TypePassportNumber, 10, 22, 0.6, detection.SourceRubert),
		cand(detection.TypePassportNumber, 10, 22, 0.9, detection.SourceGliner, detection.SourceRegex),
		cand(detection.TypePassportNumber, 10, 22, 0.8, detection.SourceValidator),
	}
	got := Merge(in)
	want := []Entity{
		entity(cand(detection.TypePassportNumber, 10, 22, 0.9,
			detection.SourceRubert, detection.SourceGliner, detection.SourceRegex, detection.SourceValidator)),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Merge() = %+v, want %+v", got, want)
	}
}

func TestInputOrderIndependence(t *testing.T) {
	base := []detection.Candidate{
		cand(detection.TypePassportNumber, 15, 27, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeFullName, 0, 40, 0.95, detection.SourceRubert),
		cand(detection.TypeFullName, 60, 80, 0.9, detection.SourceRubert),
		cand(detection.TypeFirstName, 60, 68, 0.9, detection.SourceRubert),
		cand(detection.TypeEmail, 30, 45, 0.9, detection.SourceRegex),
		cand(detection.TypeEmail, 30, 45, 0.95, detection.SourceGliner),
		cand(detection.TypePhone, 90, 105, 0.8, detection.SourceGliner),
	}
	reversed := []detection.Candidate{
		cand(detection.TypePhone, 90, 105, 0.8, detection.SourceGliner),
		cand(detection.TypeEmail, 30, 45, 0.95, detection.SourceGliner),
		cand(detection.TypeEmail, 30, 45, 0.9, detection.SourceRegex),
		cand(detection.TypeFirstName, 60, 68, 0.9, detection.SourceRubert),
		cand(detection.TypeFullName, 60, 80, 0.9, detection.SourceRubert),
		cand(detection.TypeFullName, 0, 40, 0.95, detection.SourceRubert),
		cand(detection.TypePassportNumber, 15, 27, 1.0, detection.SourceRegex, detection.SourceValidator),
	}
	shuffled := []detection.Candidate{
		cand(detection.TypeEmail, 30, 45, 0.9, detection.SourceRegex),
		cand(detection.TypeFullName, 0, 40, 0.95, detection.SourceRubert),
		cand(detection.TypePhone, 90, 105, 0.8, detection.SourceGliner),
		cand(detection.TypePassportNumber, 15, 27, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeFirstName, 60, 68, 0.9, detection.SourceRubert),
		cand(detection.TypeEmail, 30, 45, 0.95, detection.SourceGliner),
		cand(detection.TypeFullName, 60, 80, 0.9, detection.SourceRubert),
	}

	want := []Entity{
		entity(cand(detection.TypePassportNumber, 15, 27, 1.0,
			detection.SourceRegex, detection.SourceValidator)),
		entity(cand(detection.TypeEmail, 30, 45, 0.95,
			detection.SourceGliner, detection.SourceRegex)),
		entity(
			cand(detection.TypeFullName, 60, 80, 0.9, detection.SourceRubert),
			cand(detection.TypeFirstName, 60, 68, 0.9, detection.SourceRubert),
		),
		entity(cand(detection.TypePhone, 90, 105, 0.8, detection.SourceGliner)),
	}

	for name, in := range map[string][]detection.Candidate{
		"base":     base,
		"reversed": reversed,
		"shuffled": shuffled,
	} {
		got := Merge(in)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Merge(%s) = %+v, want %+v", name, got, want)
		}
	}
}

func TestValidatedNarrowPassportDropsBroadModelSpan(t *testing.T) {
	in := []detection.Candidate{
		cand(detection.TypePassportNumber, 15, 27, 0.99, detection.SourceRubert),
		cand(detection.TypeFullName, 0, 40, 0.95, detection.SourceGliner),
		cand(detection.TypePassportNumber, 15, 27, 1.0, detection.SourceRegex, detection.SourceValidator),
	}
	got := Merge(in)
	want := []Entity{
		entity(cand(detection.TypePassportNumber, 15, 27, 1.0,
			detection.SourceRubert, detection.SourceRegex, detection.SourceValidator)),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Merge() = %+v, want %+v", got, want)
	}
}

func TestRegexBeatsModelOnlyOverlap(t *testing.T) {
	in := []detection.Candidate{
		cand(detection.TypeEmail, 20, 35, 0.98, detection.SourceRubert),
		cand(detection.TypeEmail, 22, 33, 0.9, detection.SourceRegex),
	}
	got := Merge(in)
	want := []Entity{
		entity(cand(detection.TypeEmail, 22, 33, 0.9, detection.SourceRegex)),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Merge() = %+v, want %+v", got, want)
	}
}

func TestEqualTierOuterSpanWinsAndInnerComponentsRetained(t *testing.T) {
	in := []detection.Candidate{
		cand(detection.TypeFirstName, 0, 8, 0.9, detection.SourceRubert),
		cand(detection.TypeFullName, 0, 20, 0.9, detection.SourceRubert),
	}
	got := Merge(in)
	want := []Entity{
		entity(
			cand(detection.TypeFullName, 0, 20, 0.9, detection.SourceRubert),
			cand(detection.TypeFirstName, 0, 8, 0.9, detection.SourceRubert),
		),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Merge() = %+v, want %+v", got, want)
	}
}

func TestNestedFullNameAddressComponentsProduceOneEntity(t *testing.T) {
	in := []detection.Candidate{
		cand(detection.TypeFullName, 0, 20, 0.9, detection.SourceRubert),
		cand(detection.TypeFirstName, 0, 8, 0.9, detection.SourceRubert),
		cand(detection.TypeLastName, 9, 20, 0.9, detection.SourceRubert),
		cand(detection.TypeAddress, 30, 80, 0.9, detection.SourceGliner),
		cand(detection.TypeAddressCity, 40, 55, 0.9, detection.SourceGliner),
		cand(detection.TypeAddressStreet, 60, 75, 0.9, detection.SourceGliner),
	}
	got := Merge(in)
	want := []Entity{
		entity(
			cand(detection.TypeFullName, 0, 20, 0.9, detection.SourceRubert),
			cand(detection.TypeFirstName, 0, 8, 0.9, detection.SourceRubert),
			cand(detection.TypeLastName, 9, 20, 0.9, detection.SourceRubert),
		),
		entity(
			cand(detection.TypeAddress, 30, 80, 0.9, detection.SourceGliner),
			cand(detection.TypeAddressCity, 40, 55, 0.9, detection.SourceGliner),
			cand(detection.TypeAddressStreet, 60, 75, 0.9, detection.SourceGliner),
		),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Merge() = %+v, want %+v", got, want)
	}
}

func TestPartialOverlapLoserIsDropped(t *testing.T) {
	in := []detection.Candidate{
		cand(detection.TypeFullName, 0, 20, 0.9, detection.SourceRubert),
		cand(detection.TypeAddress, 15, 40, 0.9, detection.SourceGliner),
	}
	got := Merge(in)
	want := []Entity{
		entity(cand(detection.TypeAddress, 15, 40, 0.9, detection.SourceGliner)),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Merge() = %+v, want %+v", got, want)
	}
}

func TestNonOverlappingEntitiesRemainInDocumentOrder(t *testing.T) {
	in := []detection.Candidate{
		cand(detection.TypePhone, 50, 65, 0.8, detection.SourceGliner),
		cand(detection.TypeFullName, 0, 20, 0.9, detection.SourceRubert),
		cand(detection.TypeEmail, 30, 45, 0.95, detection.SourceRegex),
	}
	got := Merge(in)
	want := []Entity{
		entity(cand(detection.TypeFullName, 0, 20, 0.9, detection.SourceRubert)),
		entity(cand(detection.TypeEmail, 30, 45, 0.95, detection.SourceRegex)),
		entity(cand(detection.TypePhone, 50, 65, 0.8, detection.SourceGliner)),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Merge() = %+v, want %+v", got, want)
	}
}

func TestInvalidSpansIgnored(t *testing.T) {
	in := []detection.Candidate{
		cand(detection.TypeFullName, -1, 5, 0.9, detection.SourceRubert),
		cand(detection.TypeEmail, 10, 10, 0.9, detection.SourceRegex),
		cand(detection.TypePhone, 20, 15, 0.9, detection.SourceGliner),
		cand(detection.TypePassportNumber, 30, 42, 1.0, detection.SourceRegex, detection.SourceValidator),
	}
	got := Merge(in)
	want := []Entity{
		entity(cand(detection.TypePassportNumber, 30, 42, 1.0, detection.SourceRegex, detection.SourceValidator)),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Merge() = %+v, want %+v", got, want)
	}
}

func TestReturnedDataDoesNotAliasOrMutateInput(t *testing.T) {
	src := []detection.Source{detection.SourceRubert, detection.SourceRegex}
	in := []detection.Candidate{
		cand(detection.TypeFullName, 0, 20, 0.9, src...),
		cand(detection.TypeFirstName, 0, 8, 0.9, detection.SourceRubert),
	}
	origIn := make([]detection.Candidate, len(in))
	for i, c := range in {
		origIn[i] = c
		origIn[i].Sources = append([]detection.Source(nil), c.Sources...)
	}

	got := Merge(in)

	if !reflect.DeepEqual(in, origIn) {
		t.Errorf("Merge() mutated input: got %+v, want %+v", in, origIn)
	}

	got[0].Sources[0] = detection.SourceValidator
	got[0].Components[0].Sources[0] = detection.SourceValidator
	if !reflect.DeepEqual(in, origIn) {
		t.Errorf("mutating result aliased input: got %+v, want %+v", in, origIn)
	}
}

func TestBroadLosingNERSpanDroppedByNarrowValidatedWinner(t *testing.T) {
	in := []detection.Candidate{
		cand(detection.TypePassportNumber, 15, 27, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeFullName, 0, 40, 0.95, detection.SourceRubert),
	}
	got := Merge(in)
	want := []Entity{
		entity(cand(detection.TypePassportNumber, 15, 27, 1.0, detection.SourceRegex, detection.SourceValidator)),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Merge() = %+v, want %+v", got, want)
	}
}

func TestEmptyAndNilInput(t *testing.T) {
	if got := Merge(nil); got != nil {
		t.Errorf("Merge(nil) = %+v, want nil", got)
	}
	if got := Merge([]detection.Candidate{}); got != nil {
		t.Errorf("Merge(empty) = %+v, want nil", got)
	}
}
