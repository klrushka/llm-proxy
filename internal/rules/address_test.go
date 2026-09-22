package rules

import (
	"reflect"
	"strings"
	"testing"

	"github.com/klrushka/llm-proxy/internal/detection"
)

// span returns the UTF-8 byte span [start,end) of needle within text.
func span(text, needle string) (int, int) {
	i := strings.Index(text, needle)
	if i < 0 {
		panic("needle not found: " + needle)
	}
	return i, i + len(needle)
}

func TestDetectAddressComponentsCompleteBlock(t *testing.T) {
	text := "Россия, 101000, г. Москва, ул. Ленина, д. 5, корп. 2, кв. 12"
	want := []detection.Candidate{
		{Type: detection.TypeAddressCountry, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex, detection.SourceValidator}},
		{Type: detection.TypeAddressPostalCode, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex, detection.SourceValidator}},
		{Type: detection.TypeAddressCity, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex, detection.SourceValidator}},
		{Type: detection.TypeAddressStreet, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex, detection.SourceValidator}},
		{Type: detection.TypeAddressHouse, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex, detection.SourceValidator}},
		{Type: detection.TypeAddressBuilding, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex, detection.SourceValidator}},
		{Type: detection.TypeAddressApartment, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex, detection.SourceValidator}},
	}
	for _, needle := range []string{"Россия", "101000", "Москва", "Ленина", "5", "2", "12"} {
		s, e := span(text, needle)
		for i := range want {
			if want[i].Type == typeForNeedle(needle) {
				want[i].Start, want[i].End = s, e
			}
		}
	}

	got := DetectAddressComponents(text)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DetectAddressComponents(%q)\n got %v\nwant %v", text, got, want)
	}
}

func typeForNeedle(needle string) detection.Type {
	switch needle {
	case "Россия":
		return detection.TypeAddressCountry
	case "101000":
		return detection.TypeAddressPostalCode
	case "Москва":
		return detection.TypeAddressCity
	case "Ленина":
		return detection.TypeAddressStreet
	case "5":
		return detection.TypeAddressHouse
	case "2":
		return detection.TypeAddressBuilding
	case "12":
		return detection.TypeAddressApartment
	}
	return ""
}

func TestDetectAddressComponentsMarkerFamilies(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		needle string
		typ    detection.Type
	}{
		{"country Россия", "Россия", "Россия", detection.TypeAddressCountry},
		{"country РФ", "РФ", "РФ", detection.TypeAddressCountry},
		{"country Российская Федерация", "Российская Федерация", "Российская Федерация", detection.TypeAddressCountry},
		{"region область", "Московская область", "Московская область", detection.TypeAddressRegion},
		{"region край", "Краснодарский край", "Краснодарский край", detection.TypeAddressRegion},
		{"region республика", "Республика Татарстан", "Республика Татарстан", detection.TypeAddressRegion},
		{"city г.", "г. Москва", "Москва", detection.TypeAddressCity},
		{"city город", "город Москва", "Москва", detection.TypeAddressCity},
		{"street ул.", "ул. Ленина", "Ленина", detection.TypeAddressStreet},
		{"street улица", "улица Ленина", "Ленина", detection.TypeAddressStreet},
		{"street проспект", "проспект Мира", "Мира", detection.TypeAddressStreet},
		{"street пр-т", "пр-т Мира", "Мира", detection.TypeAddressStreet},
		{"house д.", "д. 5", "5", detection.TypeAddressHouse},
		{"house дом", "дом 5", "5", detection.TypeAddressHouse},
		{"building корп.", "корп. 2", "2", detection.TypeAddressBuilding},
		{"building корпус", "корпус 2", "2", detection.TypeAddressBuilding},
		{"building стр.", "стр. 1", "1", detection.TypeAddressBuilding},
		{"building строение", "строение 1", "1", detection.TypeAddressBuilding},
		{"apartment кв.", "кв. 12", "12", detection.TypeAddressApartment},
		{"apartment квартира", "квартира 12", "12", detection.TypeAddressApartment},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectAddressComponents(tt.text)
			if len(got) != 1 {
				t.Fatalf("DetectAddressComponents(%q) = %d candidates, want 1", tt.text, len(got))
			}
			c := got[0]
			if c.Type != tt.typ {
				t.Errorf("Type = %q, want %q", c.Type, tt.typ)
			}
			s, e := span(tt.text, tt.needle)
			if c.Start != s || c.End != e {
				t.Errorf("Start/End = %d/%d, want %d/%d", c.Start, c.End, s, e)
			}
		})
	}
}

func TestDetectAddressComponentsPostalContext(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		needle string
	}{
		{"индекс", "индекс 101000", "101000"},
		{"почтовый индекс", "почтовый индекс 101000", "101000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectAddressComponents(tt.text)
			if len(got) != 1 {
				t.Fatalf("DetectAddressComponents(%q) = %d candidates, want 1", tt.text, len(got))
			}
			c := got[0]
			if c.Type != detection.TypeAddressPostalCode {
				t.Errorf("Type = %q, want ADDRESS_POSTAL_CODE", c.Type)
			}
			s, e := span(tt.text, tt.needle)
			if c.Start != s || c.End != e {
				t.Errorf("Start/End = %d/%d, want %d/%d", c.Start, c.End, s, e)
			}
		})
	}
}

func TestDetectAddressComponentsPostalInBlock(t *testing.T) {
	text := "г. Москва, 101000, ул. Ленина"
	got := DetectAddressComponents(text)
	if len(got) != 3 {
		t.Fatalf("DetectAddressComponents(%q) = %d candidates, want 3", text, len(got))
	}
	s, e := span(text, "101000")
	if got[1].Type != detection.TypeAddressPostalCode || got[1].Start != s || got[1].End != e {
		t.Errorf("postal = %v, want ADDRESS_POSTAL_CODE [%d,%d)", got[1], s, e)
	}
}

func TestDetectAddressComponentsPostalCrossLineNegatives(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"marker on preceding line", "г. Москва\n123456"},
		{"marker on following line", "123456\nул. Ленина"},
		{"marker on both adjacent lines", "г. Москва\n123456\nул. Ленина"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, c := range DetectAddressComponents(tt.text) {
				if c.Type == detection.TypeAddressPostalCode {
					t.Errorf("DetectAddressComponents(%q) emitted postal candidate %v, want none", tt.text, c)
				}
			}
		})
	}
}

func TestDetectAddressComponentsNoCommaValueSpans(t *testing.T) {
	text := "ул. Ленина д. 5"
	got := DetectAddressComponents(text)
	if len(got) != 2 {
		t.Fatalf("DetectAddressComponents(%q) = %d candidates, want 2", text, len(got))
	}
	streetS, streetE := span(text, "Ленина")
	if got[0].Type != detection.TypeAddressStreet || got[0].Start != streetS || got[0].End != streetE {
		t.Errorf("street = %v, want ADDRESS_STREET [%d,%d)", got[0], streetS, streetE)
	}
	houseS, houseE := span(text, "5")
	if got[1].Type != detection.TypeAddressHouse || got[1].Start != houseS || got[1].End != houseE {
		t.Errorf("house = %v, want ADDRESS_HOUSE [%d,%d)", got[1], houseS, houseE)
	}
}

func TestDetectAddressComponentsPostalNegatives(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"unrelated six digits", "123456"},
		{"unrelated six digits in prose", "номер 123456"},
		{"seven digits", "1234567"},
		{"five digits", "12345"},
		{"all zero", "000000"},
		{"all zero with context", "индекс 000000"},
		{"embedded in longer run", "123456789"},
		{"embedded after digits", "9123456"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectAddressComponents(tt.text); len(got) != 0 {
				t.Errorf("DetectAddressComponents(%q) = %d candidates, want 0", tt.text, len(got))
			}
		})
	}
}

func TestDetectAddressComponentsCyrillicByteOffsets(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		needle string
		typ    detection.Type
	}{
		{"cyrillic prefix city", "Адрес: г. Москва", "Москва", detection.TypeAddressCity},
		{"cyrillic prefix postal", "Почтовый индекс 101000", "101000", detection.TypeAddressPostalCode},
		{"cyrillic prefix street", "Регистрация: ул. Ленина", "Ленина", detection.TypeAddressStreet},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectAddressComponents(tt.text)
			if len(got) != 1 {
				t.Fatalf("DetectAddressComponents(%q) = %d candidates, want 1", tt.text, len(got))
			}
			c := got[0]
			if c.Type != tt.typ {
				t.Errorf("Type = %q, want %q", c.Type, tt.typ)
			}
			s, e := span(tt.text, tt.needle)
			if c.Start != s || c.End != e {
				t.Errorf("Start/End = %d/%d, want %d/%d", c.Start, c.End, s, e)
			}
		})
	}
}

func TestDetectAddressComponentsDeterministicOrder(t *testing.T) {
	text := "Россия, 101000, г. Москва, ул. Ленина, д. 5, корп. 2, кв. 12"
	got := DetectAddressComponents(text)
	if len(got) != 7 {
		t.Fatalf("DetectAddressComponents(%q) = %d candidates, want 7", text, len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Start < got[i-1].Start {
			t.Errorf("candidates not in document order at %d: %v before %v", i, got[i-1], got[i])
		}
	}
}

func TestDetectAddressComponentsSourceConfidence(t *testing.T) {
	text := "г. Москва, ул. Ленина, д. 5"
	wantSources := []detection.Source{detection.SourceRegex, detection.SourceValidator}
	for _, c := range DetectAddressComponents(text) {
		if c.Confidence != 1.0 {
			t.Errorf("Confidence = %v, want 1.0", c.Confidence)
		}
		if !reflect.DeepEqual(c.Sources, wantSources) {
			t.Errorf("Sources = %v, want %v", c.Sources, wantSources)
		}
	}
}

func TestDetectAddressComponentsNoDuplicateSpans(t *testing.T) {
	text := "Россия, 101000, г. Москва, ул. Ленина, д. 5, корп. 2, кв. 12"
	got := DetectAddressComponents(text)
	seen := make(map[string]bool)
	for _, c := range got {
		key := string(c.Type) + ":" + itoa(c.Start) + ":" + itoa(c.End)
		if seen[key] {
			t.Errorf("duplicate span %v", c)
		}
		seen[key] = true
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
