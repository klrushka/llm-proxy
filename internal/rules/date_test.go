package rules

import (
	"reflect"
	"strings"
	"testing"

	"github.com/klrushka/llm-proxy/internal/detection"
)

func dateCandidate(start, end int) detection.Candidate {
	return detection.Candidate{
		Type:       TypeDate,
		Start:      start,
		End:        end,
		Confidence: 1.0,
		Sources:    []detection.Source{detection.SourceRegex, detection.SourceValidator},
	}
}

// spanOf returns the UTF-8 byte span [start,end) of sub within text. It fails
// the test if sub is not present, so expected offsets are derived from the
// actual byte positions rather than fragile rune-count arithmetic.
func spanOf(t *testing.T, text, sub string) (int, int) {
	t.Helper()
	start := strings.Index(text, sub)
	if start < 0 {
		t.Fatalf("substring %q not found in %q", sub, text)
	}
	return start, start + len(sub)
}

func TestDetectDatesNumericPositive(t *testing.T) {
	tests := []struct {
		name string
		text string
		subs []string
	}{
		{"dd mm yyyy dots", "01.02.1990", []string{"01.02.1990"}},
		{"dd mm yyyy hyphens", "01-02-1990", []string{"01-02-1990"}},
		{"dd mm yyyy slashes", "01/02/1990", []string{"01/02/1990"}},
		{"iso yyyy mm dd", "1990-02-01", []string{"1990-02-01"}},
		{"leap day leap year", "29.02.2024", []string{"29.02.2024"}},
		{"leap day iso leap year", "2024-02-29", []string{"2024-02-29"}},
		{"mm dd yyyy dots", "12.31.2020", []string{"12.31.2020"}},
		{"yyyy dd mm dots", "2020.31.12", []string{"2020.31.12"}},
		{"yyyy mm dd dots", "2020.12.31", []string{"2020.12.31"}},
		{"mm dd yyyy slashes", "12/31/2020", []string{"12/31/2020"}},
		{"yyyy dd mm hyphens", "2020-31-12", []string{"2020-31-12"}},
		{"cyrillic prefix offsets", "дата рождения 01.02.1990", []string{"01.02.1990"}},
		{"multiple dates", "01.02.1990 и 15.03.1985", []string{"01.02.1990", "15.03.1985"}},
		{"guillemets punctuation", "«01.02.1990»", []string{"01.02.1990"}},
		{"em dash punctuation", "01.02.1990 — дата", []string{"01.02.1990"}},
		{"en dash punctuation", "01.02.1990 – дата", []string{"01.02.1990"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var want []detection.Candidate
			for _, sub := range tt.subs {
				start, end := spanOf(t, tt.text, sub)
				want = append(want, dateCandidate(start, end))
			}
			if got := DetectDates(tt.text); !reflect.DeepEqual(got, want) {
				t.Errorf("DetectDates(%q) = %#v, want %#v", tt.text, got, want)
			}
		})
	}
}

func TestDetectDatesTextualPositive(t *testing.T) {
	tests := []struct {
		name string
		text string
		subs []string
	}{
		{"january", "1 января 1990", []string{"1 января 1990"}},
		{"march with года", "15 марта 1985 года", []string{"15 марта 1985 года"}},
		{"uppercase month", "15 МАРТА 1985", []string{"15 МАРТА 1985"}},
		{"mixed case month", "15 Марта 1985", []string{"15 Марта 1985"}},
		{"uppercase года", "15 марта 1985 ГОДА", []string{"15 марта 1985 ГОДА"}},
		{"cyrillic prefix offsets", "родился 1 января 1990", []string{"1 января 1990"}},
		{"multiple textual dates", "1 января 1990 и 15 марта 1985", []string{"1 января 1990", "15 марта 1985"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var want []detection.Candidate
			for _, sub := range tt.subs {
				start, end := spanOf(t, tt.text, sub)
				want = append(want, dateCandidate(start, end))
			}
			if got := DetectDates(tt.text); !reflect.DeepEqual(got, want) {
				t.Errorf("DetectDates(%q) = %#v, want %#v", tt.text, got, want)
			}
		})
	}
}

func TestDetectDatesNegative(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"invalid day 32", "32.01.1990"},
		{"invalid day zero", "00.01.1990"},
		{"invalid month zero", "01.00.1990"},
		{"invalid year zero", "01.01.0000"},
		{"february 30", "30.02.2020"},
		{"february 29 non leap", "29.02.2023"},
		{"february 29 non leap iso", "2023-02-29"},
		{"april 31", "31.04.2020"},
		{"invalid in all orders month 13", "13.13.2020"},
		{"invalid in all orders feb 31", "31.02.2020"},
		{"invalid in all orders day 32", "32.32.2020"},
		{"invalid in all orders yyyy dd mm", "2020.31.02"},
		{"mixed separators", "01.02-1990"},
		{"mixed separators iso", "1990-02.01"},
		{"embedded in longer digits", "101.02.1990"},
		{"embedded trailing digits", "01.02.19901"},
		{"embedded in alphanumeric word", "дата01.02.1990"},
		{"embedded trailing letter", "01.02.1990г"},
		{"malformed short year", "01.02.90"},
		{"malformed short day", "1.2.90"},
		{"unsupported text month nominative", "1 январь 1990"},
		{"unsupported text month", "1 фебраля 1990"},
		{"text month missing year", "1 января"},
		{"text month missing day", "января 1990"},
		{"text month non numeric day", "первое января 1990"},
		{"text month invalid day", "32 января 1990"},
		{"text month invalid year", "1 января 0000"},
		{"text month embedded", "x1 января 1990"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectDates(tt.text); len(got) != 0 {
				t.Errorf("DetectDates(%q) = %d candidates, want 0", tt.text, len(got))
			}
		})
	}
}

func TestDetectDatesNoDuplicates(t *testing.T) {
	text := "01.02.1990 01.02.1990"
	got := DetectDates(text)
	if len(got) != 2 {
		t.Fatalf("DetectDates(%q) = %d candidates, want 2", text, len(got))
	}
	if sameSpan(got[0], got[1]) {
		t.Errorf("duplicate candidates not deduplicated: %#v", got)
	}
}

func TestDetectDatesSourcesAndConfidence(t *testing.T) {
	got := DetectDates("01.02.1990")
	if len(got) != 1 {
		t.Fatalf("DetectDates = %d candidates, want 1", len(got))
	}
	c := got[0]
	if c.Confidence != 1.0 {
		t.Errorf("Confidence = %v, want 1.0", c.Confidence)
	}
	want := []detection.Source{detection.SourceRegex, detection.SourceValidator}
	if !reflect.DeepEqual(c.Sources, want) {
		t.Errorf("Sources = %v, want %v", c.Sources, want)
	}
	if c.Type != TypeDate {
		t.Errorf("Type = %q, want %q", c.Type, TypeDate)
	}
}

func TestTypeDateNotInCanonicalRegistry(t *testing.T) {
	r, err := detection.New()
	if err != nil {
		t.Fatalf("detection.New() error = %v", err)
	}
	if r.Lookup(TypeDate) {
		t.Errorf("Lookup(%q) = true, want false (DATE must not be canonical)", TypeDate)
	}
	for _, name := range r.Types() {
		if name == TypeDate {
			t.Errorf("canonical registry contains intermediate type %q", TypeDate)
		}
	}
}

func TestDetectDatesDocumentOrder(t *testing.T) {
	text := "15 марта 1985 года 01.02.1990 1990-02-01"
	got := DetectDates(text)
	for i := 1; i < len(got); i++ {
		if got[i-1].Start > got[i].Start {
			t.Errorf("candidates not in document order: %#v", got)
		}
	}
}
