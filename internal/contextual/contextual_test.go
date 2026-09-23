package contextual

import (
	"reflect"
	"strings"
	"testing"

	"github.com/klrushka/llm-proxy/internal/detection"
)

func cand(t detection.Type, start, end int, conf float64, sources ...detection.Source) detection.Candidate {
	return detection.Candidate{Type: t, Start: start, End: end, Confidence: conf, Sources: sources}
}

// spanOf returns the UTF-8 byte span [start,end) of sub within text.
func spanOf(t *testing.T, text, sub string) (int, int) {
	t.Helper()
	start := strings.Index(text, sub)
	if start < 0 {
		t.Fatalf("substring %q not found in %q", sub, text)
	}
	return start, start + len(sub)
}

func TestClassifyBirthAndIssueDates(t *testing.T) {
	text := "дата рождения 01.02.1990, паспорт выдан 03.04.2020, встреча 05.05.2025"
	birthS, birthE := spanOf(t, text, "01.02.1990")
	issueS, issueE := spanOf(t, text, "03.04.2020")
	meetS, meetE := spanOf(t, text, "05.05.2025")

	in := []detection.Candidate{
		cand(typeDate, meetS, meetE, 0.9, detection.SourceRubert),
		cand(typeDate, birthS, birthE, 0.9, detection.SourceRubert),
		cand(typeDate, issueS, issueE, 0.9, detection.SourceRubert),
	}
	want := []detection.Candidate{
		cand(detection.TypeBirthDate, birthS, birthE, 0.9, detection.SourceRubert),
		cand(detection.TypePassportIssueDate, issueS, issueE, 0.9, detection.SourceRubert),
	}
	if got := Classify(text, in); !reflect.DeepEqual(got, want) {
		t.Errorf("Classify() = %+v, want %+v", got, want)
	}
}

func TestClassifyBirthDateViaRodilsya(t *testing.T) {
	for _, phrase := range []string{"родился", "родилась"} {
		text := phrase + " 01.02.1990"
		s, e := spanOf(t, text, "01.02.1990")
		in := []detection.Candidate{cand(typeDate, s, e, 0.9, detection.SourceGliner)}
		want := []detection.Candidate{cand(detection.TypeBirthDate, s, e, 0.9, detection.SourceGliner)}
		if got := Classify(text, in); !reflect.DeepEqual(got, want) {
			t.Errorf("Classify(%q) = %+v, want %+v", text, got, want)
		}
	}
}

func TestClassifyIssueDateViaBareVydanWithPassportContext(t *testing.T) {
	// "паспорт" and "выдан" are separated by "был", so only the bare "выдан"
	// phrase applies, and it is confirmed by the local passport context.
	text := "паспорт был выдан 03.04.2020"
	s, e := spanOf(t, text, "03.04.2020")
	in := []detection.Candidate{cand(typeDate, s, e, 0.9, detection.SourceRubert)}
	want := []detection.Candidate{cand(detection.TypePassportIssueDate, s, e, 0.9, detection.SourceRubert)}
	if got := Classify(text, in); !reflect.DeepEqual(got, want) {
		t.Errorf("Classify() = %+v, want %+v", got, want)
	}
}

func TestClassifyBareVydanWithoutPassportContextDropped(t *testing.T) {
	text := "документ выдан 03.04.2020"
	s, e := spanOf(t, text, "03.04.2020")
	in := []detection.Candidate{cand(typeDate, s, e, 0.9, detection.SourceRubert)}
	if got := Classify(text, in); len(got) != 0 {
		t.Errorf("Classify() = %+v, want empty (bare выдан without passport context)", got)
	}
}

func TestClassifyBareVydanPassportOnOtherLineDropped(t *testing.T) {
	// The passport context is on a previous line; the bounded local region
	// never crosses a newline, so the bare "выдан" is not confirmed.
	text := "паспорт\nвыдан 03.04.2020"
	s, e := spanOf(t, text, "03.04.2020")
	in := []detection.Candidate{cand(typeDate, s, e, 0.9, detection.SourceRubert)}
	if got := Classify(text, in); len(got) != 0 {
		t.Errorf("Classify() = %+v, want empty (passport context on other line)", got)
	}
}

func TestClassifyBareVydanPassportInOtherClauseDropped(t *testing.T) {
	// The passport marker is in a different clause separated by ";"; the bare
	// "выдан" must not cross the clause boundary.
	text := "паспорт утерян; диплом выдан 03.04.2020"
	s, e := spanOf(t, text, "03.04.2020")
	in := []detection.Candidate{cand(typeDate, s, e, 0.9, detection.SourceRubert)}
	if got := Classify(text, in); len(got) != 0 {
		t.Errorf("Classify() = %+v, want empty (passport context in other clause)", got)
	}
}

func TestClassifyBareVydanNegatedDropped(t *testing.T) {
	text := "паспорт не выдан 03.04.2020"
	s, e := spanOf(t, text, "03.04.2020")
	in := []detection.Candidate{cand(typeDate, s, e, 0.9, detection.SourceRubert)}
	if got := Classify(text, in); len(got) != 0 {
		t.Errorf("Classify() = %+v, want empty (negated выдан)", got)
	}
}

func TestClassifyNegatedContextPhrasesDropped(t *testing.T) {
	tests := []struct {
		text string
		sub  string
		typ  detection.Type
	}{
		{"не родился 01.02.1990", "01.02.1990", typeDate},
		{"не родилась 01.02.1990", "01.02.1990", typeDate},
		{"не родился в Москве", "Москве", typeLocation},
		{"не проживает в Москве", "Москве", typeLocation},
		{"не зарегистрирован в Москве", "Москве", typeLocation},
	}
	for _, tt := range tests {
		s, e := spanOf(t, tt.text, tt.sub)
		in := []detection.Candidate{cand(tt.typ, s, e, 0.9, detection.SourceRubert)}
		if got := Classify(tt.text, in); len(got) != 0 {
			t.Errorf("Classify(%q) = %+v, want empty (negated context)", tt.text, got)
		}
	}
}

func TestClassifyAuxiliaryNegationDropped(t *testing.T) {
	tests := []struct {
		text string
		sub  string
		typ  detection.Type
	}{
		{"паспорт не был выдан 03.04.2020", "03.04.2020", typeDate},
		{"никогда не был зарегистрирован в Москве", "Москве", typeLocation},
	}
	for _, tt := range tests {
		s, e := spanOf(t, tt.text, tt.sub)
		in := []detection.Candidate{cand(tt.typ, s, e, 0.9, detection.SourceRubert)}
		if got := Classify(tt.text, in); len(got) != 0 {
			t.Errorf("Classify(%q) = %+v, want empty (auxiliary-negated context)", tt.text, got)
		}
	}
}

func TestClassifyNegationInEarlierClauseDoesNotSuppressLaterPositive(t *testing.T) {
	text := "не родился 01.02.1990; дата рождения 05.05.2025"
	s, e := spanOf(t, text, "05.05.2025")
	in := []detection.Candidate{cand(typeDate, s, e, 0.9, detection.SourceRubert)}
	want := []detection.Candidate{cand(detection.TypeBirthDate, s, e, 0.9, detection.SourceRubert)}
	if got := Classify(text, in); !reflect.DeepEqual(got, want) {
		t.Errorf("Classify() = %+v, want %+v (negation must not cross clause)", got, want)
	}
}

func TestClassifyPositiveContextNotNegated(t *testing.T) {
	tests := []struct {
		text string
		sub  string
		typ  detection.Type
		want detection.Type
	}{
		{"родился 01.02.1990", "01.02.1990", typeDate, detection.TypeBirthDate},
		{"родилась 01.02.1990", "01.02.1990", typeDate, detection.TypeBirthDate},
		{"родился в Москве", "Москве", typeLocation, detection.TypeBirthPlace},
		{"проживает в Москве", "Москве", typeLocation, detection.TypeAddress},
		{"зарегистрирован в Москве", "Москве", typeLocation, detection.TypeAddress},
	}
	for _, tt := range tests {
		s, e := spanOf(t, tt.text, tt.sub)
		in := []detection.Candidate{cand(tt.typ, s, e, 0.9, detection.SourceRubert)}
		want := []detection.Candidate{cand(tt.want, s, e, 0.9, detection.SourceRubert)}
		if got := Classify(tt.text, in); !reflect.DeepEqual(got, want) {
			t.Errorf("Classify(%q) = %+v, want %+v", tt.text, got, want)
		}
	}
}

func TestClassifyPassportIssueDateViaVydachi(t *testing.T) {
	text := "дата выдачи 03.04.2020"
	s, e := spanOf(t, text, "03.04.2020")
	in := []detection.Candidate{cand(typeDate, s, e, 0.9, detection.SourceRubert)}
	want := []detection.Candidate{cand(detection.TypePassportIssueDate, s, e, 0.9, detection.SourceRubert)}
	if got := Classify(text, in); !reflect.DeepEqual(got, want) {
		t.Errorf("Classify() = %+v, want %+v", got, want)
	}
}

func TestClassifyRussianTextualBirthDate(t *testing.T) {
	text := "дата рождения 1 января 1990"
	s, e := spanOf(t, text, "1 января 1990")
	in := []detection.Candidate{cand(typeDate, s, e, 0.9, detection.SourceGliner)}
	want := []detection.Candidate{cand(detection.TypeBirthDate, s, e, 0.9, detection.SourceGliner)}
	if got := Classify(text, in); !reflect.DeepEqual(got, want) {
		t.Errorf("Classify() = %+v, want %+v", got, want)
	}
}

func TestClassifyBirthPlace(t *testing.T) {
	text := "место рождения город Тестовск"
	s, e := spanOf(t, text, "Тестовск")
	in := []detection.Candidate{cand(typeLocation, s, e, 0.9, detection.SourceRubert)}
	want := []detection.Candidate{cand(detection.TypeBirthPlace, s, e, 0.9, detection.SourceRubert)}
	if got := Classify(text, in); !reflect.DeepEqual(got, want) {
		t.Errorf("Classify() = %+v, want %+v", got, want)
	}
}

func TestClassifyLocationAddressContext(t *testing.T) {
	text := "адрес проживания: Москва"
	s, e := spanOf(t, text, "Москва")
	in := []detection.Candidate{cand(typeLocation, s, e, 0.9, detection.SourceRubert)}
	want := []detection.Candidate{cand(detection.TypeAddress, s, e, 0.9, detection.SourceRubert)}
	if got := Classify(text, in); !reflect.DeepEqual(got, want) {
		t.Errorf("Classify() = %+v, want %+v", got, want)
	}
}

func TestClassifyBirthPlaceViaRodilsyaV(t *testing.T) {
	for _, phrase := range []string{"родился в", "родилась в"} {
		text := phrase + " городе Тестовск"
		s, e := spanOf(t, text, "Тестовск")
		in := []detection.Candidate{cand(typeLocation, s, e, 0.9, detection.SourceGliner)}
		want := []detection.Candidate{cand(detection.TypeBirthPlace, s, e, 0.9, detection.SourceGliner)}
		if got := Classify(text, in); !reflect.DeepEqual(got, want) {
			t.Errorf("Classify(%q) = %+v, want %+v", text, got, want)
		}
	}
}

func TestClassifyAddressBlockViaProzhivaet(t *testing.T) {
	text := "проживает в Москве, ул. Ленина, д. 5"
	cityS, cityE := spanOf(t, text, "Москве")
	streetS, streetE := spanOf(t, text, "Ленина")
	houseS, houseE := spanOf(t, text, "5")

	in := []detection.Candidate{
		cand(detection.TypeAddressCity, cityS, cityE, 0.9, detection.SourceRubert),
		cand(detection.TypeAddressStreet, streetS, streetE, 0.9, detection.SourceRubert),
		cand(detection.TypeAddressHouse, houseS, houseE, 0.9, detection.SourceRubert),
	}
	want := []detection.Candidate{
		cand(detection.TypeAddressCity, cityS, cityE, 0.9, detection.SourceRubert),
		cand(detection.TypeAddress, cityS, houseE, 0.9, detection.SourceRubert),
		cand(detection.TypeAddressStreet, streetS, streetE, 0.9, detection.SourceRubert),
		cand(detection.TypeAddressHouse, houseS, houseE, 0.9, detection.SourceRubert),
	}
	if got := Classify(text, in); !reflect.DeepEqual(got, want) {
		t.Errorf("Classify() = %+v, want %+v", got, want)
	}
}

func TestClassifyAddressBlockViaZaregistrirovan(t *testing.T) {
	text := "зарегистрирован в Москве, ул. Ленина, д. 5"
	cityS, cityE := spanOf(t, text, "Москве")
	streetS, streetE := spanOf(t, text, "Ленина")
	houseS, houseE := spanOf(t, text, "5")

	in := []detection.Candidate{
		cand(detection.TypeAddressCity, cityS, cityE, 0.9, detection.SourceRubert),
		cand(detection.TypeAddressStreet, streetS, streetE, 0.9, detection.SourceRubert),
		cand(detection.TypeAddressHouse, houseS, houseE, 0.9, detection.SourceRubert),
	}
	want := []detection.Candidate{
		cand(detection.TypeAddressCity, cityS, cityE, 0.9, detection.SourceRubert),
		cand(detection.TypeAddress, cityS, houseE, 0.9, detection.SourceRubert),
		cand(detection.TypeAddressStreet, streetS, streetE, 0.9, detection.SourceRubert),
		cand(detection.TypeAddressHouse, houseS, houseE, 0.9, detection.SourceRubert),
	}
	if got := Classify(text, in); !reflect.DeepEqual(got, want) {
		t.Errorf("Classify() = %+v, want %+v", got, want)
	}
}

func TestClassifyNegatedAddressBlockNotBuilt(t *testing.T) {
	for _, phrase := range []string{"не проживает", "не зарегистрирован"} {
		text := phrase + " в Москве, ул. Ленина, д. 5"
		cityS, cityE := spanOf(t, text, "Москве")
		streetS, streetE := spanOf(t, text, "Ленина")
		houseS, houseE := spanOf(t, text, "5")

		in := []detection.Candidate{
			cand(detection.TypeAddressCity, cityS, cityE, 0.9, detection.SourceRubert),
			cand(detection.TypeAddressStreet, streetS, streetE, 0.9, detection.SourceRubert),
			cand(detection.TypeAddressHouse, houseS, houseE, 0.9, detection.SourceRubert),
		}
		want := []detection.Candidate{
			cand(detection.TypeAddressCity, cityS, cityE, 0.9, detection.SourceRubert),
			cand(detection.TypeAddressStreet, streetS, streetE, 0.9, detection.SourceRubert),
			cand(detection.TypeAddressHouse, houseS, houseE, 0.9, detection.SourceRubert),
		}
		if got := Classify(text, in); !reflect.DeepEqual(got, want) {
			t.Errorf("Classify(%q) = %+v, want %+v (no ADDRESS block)", text, got, want)
		}
	}
}

func TestClassifyRegistrationAddressBlock(t *testing.T) {
	text := "адрес регистрации: 101000, г. Москва, ул. Ленина, д. 5, кв. 12"
	postalS, postalE := spanOf(t, text, "101000")
	cityS, cityE := spanOf(t, text, "Москва")
	streetS, streetE := spanOf(t, text, "Ленина")
	houseS, houseE := spanOf(t, text, "5")
	aptS, aptE := spanOf(t, text, "12")

	in := []detection.Candidate{
		cand(detection.TypeAddressPostalCode, postalS, postalE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressCity, cityS, cityE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressStreet, streetS, streetE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressHouse, houseS, houseE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressApartment, aptS, aptE, 1.0, detection.SourceRegex, detection.SourceValidator),
	}
	want := []detection.Candidate{
		cand(detection.TypeAddressPostalCode, postalS, postalE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddress, postalS, aptE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressCity, cityS, cityE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressStreet, streetS, streetE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressHouse, houseS, houseE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressApartment, aptS, aptE, 1.0, detection.SourceRegex, detection.SourceValidator),
	}
	if got := Classify(text, in); !reflect.DeepEqual(got, want) {
		t.Errorf("Classify() = %+v, want %+v", got, want)
	}
}

func TestClassifyAddressBlockWithLocationDeduplicated(t *testing.T) {
	text := "адрес регистрации: 101000, г. Москва, ул. Ленина, д. 5"
	postalS, postalE := spanOf(t, text, "101000")
	cityS, cityE := spanOf(t, text, "Москва")
	streetS, streetE := spanOf(t, text, "Ленина")
	houseS, houseE := spanOf(t, text, "5")

	in := []detection.Candidate{
		cand(typeLocation, cityS, cityE, 0.9, detection.SourceRubert),
		cand(detection.TypeAddressPostalCode, postalS, postalE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressCity, cityS, cityE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressStreet, streetS, streetE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressHouse, houseS, houseE, 1.0, detection.SourceRegex, detection.SourceValidator),
	}
	want := []detection.Candidate{
		cand(detection.TypeAddressPostalCode, postalS, postalE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddress, postalS, houseE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressCity, cityS, cityE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressStreet, streetS, streetE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressHouse, houseS, houseE, 1.0, detection.SourceRegex, detection.SourceValidator),
	}
	if got := Classify(text, in); !reflect.DeepEqual(got, want) {
		t.Errorf("Classify() = %+v, want %+v", got, want)
	}
}

func TestClassifyAmbiguousDropped(t *testing.T) {
	text := "встреча 05.05.2025 в городе Тестовск"
	dateS, dateE := spanOf(t, text, "05.05.2025")
	locS, locE := spanOf(t, text, "Тестовск")
	in := []detection.Candidate{
		cand(typeDate, dateS, dateE, 0.9, detection.SourceRubert),
		cand(typeLocation, locS, locE, 0.9, detection.SourceRubert),
	}
	if got := Classify(text, in); len(got) != 0 {
		t.Errorf("Classify() = %+v, want empty (ambiguous dropped)", got)
	}
}

func TestClassifyContextOutsideWindow(t *testing.T) {
	filler := strings.Repeat("а", 60)
	text := "дата рождения " + filler + " 01.02.1990"
	s, e := spanOf(t, text, "01.02.1990")
	in := []detection.Candidate{cand(typeDate, s, e, 0.9, detection.SourceRubert)}
	if got := Classify(text, in); len(got) != 0 {
		t.Errorf("Classify() = %+v, want empty (context outside window)", got)
	}
}

func TestClassifyNoAddressBlockWithoutContext(t *testing.T) {
	text := "г. Москва, ул. Ленина, д. 5"
	cityS, cityE := spanOf(t, text, "Москва")
	streetS, streetE := spanOf(t, text, "Ленина")
	houseS, houseE := spanOf(t, text, "5")
	in := []detection.Candidate{
		cand(detection.TypeAddressCity, cityS, cityE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressStreet, streetS, streetE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressHouse, houseS, houseE, 1.0, detection.SourceRegex, detection.SourceValidator),
	}
	want := []detection.Candidate{
		cand(detection.TypeAddressCity, cityS, cityE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressStreet, streetS, streetE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressHouse, houseS, houseE, 1.0, detection.SourceRegex, detection.SourceValidator),
	}
	if got := Classify(text, in); !reflect.DeepEqual(got, want) {
		t.Errorf("Classify() = %+v, want %+v", got, want)
	}
}

func TestClassifyCanonicalPassthroughDeterministicNoAlias(t *testing.T) {
	text := "Иван Иванов, дата рождения 01.02.1990, +7 900 123-45-67"
	nameS, nameE := spanOf(t, text, "Иван Иванов")
	dateS, dateE := spanOf(t, text, "01.02.1990")
	phoneS, phoneE := spanOf(t, text, "+7 900 123-45-67")

	src := []detection.Source{detection.SourceRubert, detection.SourceRegex}
	in := []detection.Candidate{
		cand(detection.TypeFullName, nameS, nameE, 0.95, detection.SourceRubert),
		cand(typeDate, dateS, dateE, 0.9, detection.SourceRubert),
		cand(detection.TypePhone, phoneS, phoneE, 0.9, src...),
	}
	origIn := make([]detection.Candidate, len(in))
	for i, c := range in {
		origIn[i] = c
		origIn[i].Sources = append([]detection.Source(nil), c.Sources...)
	}

	want := []detection.Candidate{
		cand(detection.TypeFullName, nameS, nameE, 0.95, detection.SourceRubert),
		cand(detection.TypeBirthDate, dateS, dateE, 0.9, detection.SourceRubert),
		cand(detection.TypePhone, phoneS, phoneE, 0.9, src...),
	}

	got := Classify(text, in)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Classify() = %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(in, origIn) {
		t.Errorf("Classify() mutated input: got %+v, want %+v", in, origIn)
	}

	if again := Classify(text, in); !reflect.DeepEqual(again, want) {
		t.Errorf("Classify() not deterministic: %+v, want %+v", again, want)
	}

	got[0].Sources[0] = detection.SourceValidator
	got[2].Sources[0] = detection.SourceValidator
	if !reflect.DeepEqual(in, origIn) {
		t.Errorf("mutating result aliased input: got %+v, want %+v", in, origIn)
	}
}

func TestClassifyEmptyAndNilInput(t *testing.T) {
	if got := Classify("", nil); got != nil {
		t.Errorf("Classify(nil) = %+v, want nil", got)
	}
	if got := Classify("", []detection.Candidate{}); got != nil {
		t.Errorf("Classify(empty) = %+v, want nil", got)
	}
}

func TestClassifyInvalidCandidatesIgnored(t *testing.T) {
	text := "дата рождения 01.02.1990"
	birthS, birthE := spanOf(t, text, "01.02.1990")
	in := []detection.Candidate{
		cand(typeDate, -1, 5, 0.9, detection.SourceRubert),          // negative start
		cand(typeDate, 10, 10, 0.9, detection.SourceRubert),         // reversed (end == start)
		cand(typeDate, 10, 5, 0.9, detection.SourceRubert),          // reversed (end < start)
		cand(typeDate, 0, len(text)+5, 0.9, detection.SourceRubert), // out of range
		cand(typeDate, 1, 5, 0.9, detection.SourceRubert),           // mid-rune start
		cand(typeDate, 0, 3, 0.9, detection.SourceRubert),           // mid-rune end
		cand(typeDate, birthS, birthE, 0.9, detection.SourceRubert), // valid
	}
	want := []detection.Candidate{
		cand(detection.TypeBirthDate, birthS, birthE, 0.9, detection.SourceRubert),
	}
	if got := Classify(text, in); !reflect.DeepEqual(got, want) {
		t.Errorf("Classify() = %+v, want %+v", got, want)
	}
}

func TestClassifyInvalidAddressComponentsIgnored(t *testing.T) {
	text := "адрес регистрации: 101000, г. Москва, ул. Ленина, д. 5"
	postalS, postalE := spanOf(t, text, "101000")
	cityS, cityE := spanOf(t, text, "Москва")
	streetS, streetE := spanOf(t, text, "Ленина")
	houseS, houseE := spanOf(t, text, "5")
	in := []detection.Candidate{
		cand(detection.TypeAddressPostalCode, -5, postalE, 1.0, detection.SourceRegex), // invalid
		cand(detection.TypeAddressPostalCode, postalS, postalE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressCity, cityS, cityE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressStreet, streetS, streetE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressHouse, houseS, houseE, 1.0, detection.SourceRegex, detection.SourceValidator),
	}
	want := []detection.Candidate{
		cand(detection.TypeAddressPostalCode, postalS, postalE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddress, postalS, houseE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressCity, cityS, cityE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressStreet, streetS, streetE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressHouse, houseS, houseE, 1.0, detection.SourceRegex, detection.SourceValidator),
	}
	if got := Classify(text, in); !reflect.DeepEqual(got, want) {
		t.Errorf("Classify() = %+v, want %+v", got, want)
	}
}

func TestClassifyInputOrderIndependence(t *testing.T) {
	text := "дата рождения 01.02.1990, паспорт выдан 03.04.2020, адрес регистрации: 101000, г. Москва, ул. Ленина, д. 5"
	birthS, birthE := spanOf(t, text, "01.02.1990")
	issueS, issueE := spanOf(t, text, "03.04.2020")
	postalS, postalE := spanOf(t, text, "101000")
	cityS, cityE := spanOf(t, text, "Москва")
	streetS, streetE := spanOf(t, text, "Ленина")
	houseS, houseE := spanOf(t, text, "5")

	base := []detection.Candidate{
		cand(typeDate, birthS, birthE, 0.9, detection.SourceRubert),
		cand(typeDate, birthS, birthE, 0.95, detection.SourceGliner),
		cand(typeDate, issueS, issueE, 0.9, detection.SourceRubert),
		cand(typeLocation, cityS, cityE, 0.9, detection.SourceRubert),
		cand(detection.TypeAddressPostalCode, postalS, postalE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressCity, cityS, cityE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressStreet, streetS, streetE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressHouse, houseS, houseE, 1.0, detection.SourceRegex, detection.SourceValidator),
	}

	want := []detection.Candidate{
		cand(detection.TypeBirthDate, birthS, birthE, 0.95, detection.SourceRubert, detection.SourceGliner),
		cand(detection.TypePassportIssueDate, issueS, issueE, 0.9, detection.SourceRubert),
		cand(detection.TypeAddressPostalCode, postalS, postalE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddress, postalS, houseE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressCity, cityS, cityE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressStreet, streetS, streetE, 1.0, detection.SourceRegex, detection.SourceValidator),
		cand(detection.TypeAddressHouse, houseS, houseE, 1.0, detection.SourceRegex, detection.SourceValidator),
	}

	reversed := make([]detection.Candidate, len(base))
	for i, c := range base {
		reversed[len(base)-1-i] = c
	}
	shuffled := []detection.Candidate{
		base[5], base[0], base[7], base[2], base[4], base[1], base[6], base[3],
	}

	for name, in := range map[string][]detection.Candidate{
		"base":     base,
		"reversed": reversed,
		"shuffled": shuffled,
	} {
		if got := Classify(text, in); !reflect.DeepEqual(got, want) {
			t.Errorf("Classify(%s) = %+v, want %+v", name, got, want)
		}
	}
}
