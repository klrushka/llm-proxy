package rules

import (
	"reflect"
	"testing"

	"github.com/klrushka/llm-proxy/internal/detection"
)

// Synthetic valid card numbers:
//   - 4111111111111111 (Visa, 16 digits)
//   - 5555555555554444 (Mastercard, 16 digits)
//   - 378282246310005  (Amex, 15 digits)
//   - 4222222222222    (13 digits)
//   - 4111111111111111110 (19 digits)

func TestDetectBankCardsPositive(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		start int
		end   int
	}{
		{"contiguous visa", "Карта 4111111111111111", 11, 27},
		{"contiguous mastercard", "4111111111111111", 0, 16},
		{"contiguous amex", "378282246310005", 0, 15},
		{"contiguous 13 digit", "4222222222222", 0, 13},
		{"contiguous 19 digit", "4111111111111111110", 0, 19},
		{"grouped spaces", "4111 1111 1111 1111", 0, 19},
		{"grouped hyphens", "4111-1111-1111-1111", 0, 19},
		{"grouped mixed", "4111 1111-1111 1111", 0, 19},
		{"cyrillic prefix", "Номер карты: 4111 1111 1111 1111", 23, 42},
		{"sentence punctuation excluded", "Карта 4111111111111111.", 11, 27},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectBankCards(tt.text)
			if len(got) != 1 {
				t.Fatalf("DetectBankCards(%q) = %d candidates, want 1", tt.text, len(got))
			}
			c := got[0]
			if c.Type != detection.TypeBankCardNumber {
				t.Errorf("Type = %q, want BANK_CARD_NUMBER", c.Type)
			}
			if c.Start != tt.start || c.End != tt.end {
				t.Errorf("Start/End = %d/%d, want %d/%d", c.Start, c.End, tt.start, tt.end)
			}
			if c.Confidence != 1.0 {
				t.Errorf("Confidence = %v, want 1.0", c.Confidence)
			}
			if !reflect.DeepEqual(c.Sources, []detection.Source{detection.SourceRegex, detection.SourceValidator}) {
				t.Errorf("Sources = %v, want [regex validator]", c.Sources)
			}
		})
	}
}

func TestDetectBankCardsNegative(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"invalid checksum", "4111111111111112"},
		{"mutated last digit", "Карта 4111111111111112"},
		{"too short 12 digits", "411111111111"},
		{"too long 20 digits", "41111111111111111111"},
		{"non digit", "411111111111111a"},
		{"malformed doubled space", "4111  1111 1111 1111"},
		{"malformed doubled hyphen", "4111--1111-1111-1111"},
		{"embedded in longer digit run", "94111111111111111"},
		{"embedded after longer run", "41111111111111111"},
		{"embedded both sides", "941111111111111118"},
		{"grouped embedded longer", "94111 1111 1111 1111"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectBankCards(tt.text); len(got) != 0 {
				t.Errorf("DetectBankCards(%q) = %d candidates, want 0", tt.text, len(got))
			}
		})
	}
}

func TestDetectBankCardsMultiple(t *testing.T) {
	text := "Карта 4111111111111111 и 5555 5555 5555 4444"
	got := DetectBankCards(text)
	if len(got) != 2 {
		t.Fatalf("DetectBankCards(%q) = %d candidates, want 2", text, len(got))
	}
	if got[0].Start > got[1].Start {
		t.Errorf("candidates not in document order: %d > %d", got[0].Start, got[1].Start)
	}
	if got[0].Type != detection.TypeBankCardNumber || got[1].Type != detection.TypeBankCardNumber {
		t.Errorf("types = %q, %q, want BANK_CARD_NUMBER both", got[0].Type, got[1].Type)
	}
}

func TestValidLuhn(t *testing.T) {
	tests := []struct {
		name   string
		digits string
		want   bool
	}{
		{"visa", "4111111111111111", true},
		{"mastercard", "5555555555554444", true},
		{"amex", "378282246310005", true},
		{"13 digit", "4222222222222", true},
		{"19 digit", "4111111111111111110", true},
		{"mutated checksum", "4111111111111112", false},
		{"too short", "411111111111", false},
		{"too long", "41111111111111111111", false},
		{"non digit", "411111111111111a", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidLuhn(tt.digits); got != tt.want {
				t.Errorf("ValidLuhn(%q) = %v, want %v", tt.digits, got, tt.want)
			}
		})
	}
}
