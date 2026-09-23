package rules

import (
	"reflect"
	"strings"
	"testing"

	"github.com/klrushka/llm-proxy/internal/detection"
)

// paymentSpan returns the UTF-8 byte offsets of the first occurrence of value
// in text, start inclusive and end exclusive.
func paymentSpan(text, value string) (int, int) {
	i := strings.Index(text, value)
	if i < 0 {
		panic("value not found in text: " + value)
	}
	return i, i + len(value)
}

// filterCardholderCandidates returns only the CARDHOLDER_NAME candidates.
func filterCardholderCandidates(in []detection.Candidate) []detection.Candidate {
	var out []detection.Candidate
	for _, c := range in {
		if c.Type == detection.TypeCardholderName {
			out = append(out, c)
		}
	}
	return out
}

func TestDetectPaymentSecretsCVVPositive(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		value string
	}{
		{"cvv context", "CVV: 123", "123"},
		{"cvv lowercase context", "cvv 123", "123"},
		{"cvc context", "CVC: 123", "123"},
		{"cvc lowercase context", "cvc 123", "123"},
		{"код безопасности context", "код безопасности: 123", "123"},
		{"код безопасности uppercase", "КОД БЕЗОПАСНОСТИ 123", "123"},
		{"cvv-код context", "CVV-код: 123", "123"},
		{"cvv-код lowercase context", "cvv-код: 123", "123"},
		{"cvv-код uppercase context", "CVV-КОД: 123", "123"},
		{"tab separator", "CVV:\t123", "123"},
		{"cyrillic prefix", "Данные: CVV: 123", "123"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end := paymentSpan(tt.text, tt.value)
			got := DetectPaymentSecrets(tt.text)
			if len(got) != 1 {
				t.Fatalf("DetectPaymentSecrets(%q) = %d candidates, want 1", tt.text, len(got))
			}
			c := got[0]
			if c.Type != detection.TypeCardCVV {
				t.Errorf("Type = %q, want CARD_CVV", c.Type)
			}
			if c.Start != start || c.End != end {
				t.Errorf("Start/End = %d/%d, want %d/%d", c.Start, c.End, start, end)
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

func TestDetectPaymentSecretsPINPositive(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		value string
	}{
		{"pin context", "PIN: 4567", "4567"},
		{"pin lowercase context", "pin 4567", "4567"},
		{"пин context", "ПИН: 4567", "4567"},
		{"пин lowercase context", "пин 4567", "4567"},
		{"пин-код context", "ПИН-код: 4567", "4567"},
		{"пин-код lowercase context", "пин-код 4567", "4567"},
		{"tab separator", "PIN:\t4567", "4567"},
		{"cyrillic prefix", "Данные: ПИН-код: 4567", "4567"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end := paymentSpan(tt.text, tt.value)
			got := DetectPaymentSecrets(tt.text)
			if len(got) != 1 {
				t.Fatalf("DetectPaymentSecrets(%q) = %d candidates, want 1", tt.text, len(got))
			}
			c := got[0]
			if c.Type != detection.TypeCardPIN {
				t.Errorf("Type = %q, want CARD_PIN", c.Type)
			}
			if c.Start != start || c.End != end {
				t.Errorf("Start/End = %d/%d, want %d/%d", c.Start, c.End, start, end)
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

func TestDetectPaymentSecretsCardholderPositive(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		value string
	}{
		{"держатель карты cyrillic title case", "Держатель карты: Иван Иванов", "Иван Иванов"},
		{"имя держателя cyrillic title case", "Имя держателя: Анна-Мария Петрова", "Анна-Мария Петрова"},
		{"владелец карты cyrillic all caps", "Владелец карты: ИВАН ИВАНОВ", "ИВАН ИВАНОВ"},
		{"имя на карте cyrillic title case", "Имя на карте: Пётр Сидоров", "Пётр Сидоров"},
		{"cardholder latin title case", "Cardholder: Ivan Ivanov", "Ivan Ivanov"},
		{"cardholder name latin title case", "Cardholder name: Jean-Pierre Dupont", "Jean-Pierre Dupont"},
		{"cardholder latin all caps", "CARDHOLDER: IVAN IVANOV", "IVAN IVANOV"},
		{"cardholder name latin all caps", "CARDHOLDER NAME: IVAN IVANOV", "IVAN IVANOV"},
		{"marker uppercase cyrillic", "ДЕРЖАТЕЛЬ КАРТЫ: Иван Иванов", "Иван Иванов"},
		{"three word name", "Cardholder: Ivan Ivanov Petrov", "Ivan Ivanov Petrov"},
		{"four word name", "Cardholder: Ivan Ivanov Petrov Sidorov", "Ivan Ivanov Petrov Sidorov"},
		{"cyrillic prefix", "Данные: Cardholder: Ivan Ivanov", "Ivan Ivanov"},
		{"lowercase cyrillic", "cardholder: иван иванов", "иван иванов"},
		{"lowercase latin", "cardholder: ivan ivanov", "ivan ivanov"},
		{"mixed case cyrillic", "Cardholder: ИвАН Иванов", "ИвАН Иванов"},
		{"mixed case latin", "Cardholder: IvAn IvAnOv", "IvAn IvAnOv"},
		{"lowercase cyrillic hyphen", "Держатель карты: анна-мария петрова", "анна-мария петрова"},
		{"mixed case latin hyphen", "Cardholder: Jean-Pierre dupont", "Jean-Pierre dupont"},
		{"three word upper case", "Cardholder: IVAN IVANOV PETROV", "IVAN IVANOV PETROV"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end := paymentSpan(tt.text, tt.value)
			got := DetectPaymentSecrets(tt.text)
			if len(got) != 1 {
				t.Fatalf("DetectPaymentSecrets(%q) = %d candidates, want 1", tt.text, len(got))
			}
			c := got[0]
			if c.Type != detection.TypeCardholderName {
				t.Errorf("Type = %q, want CARDHOLDER_NAME", c.Type)
			}
			if c.Start != start || c.End != end {
				t.Errorf("Start/End = %d/%d, want %d/%d", c.Start, c.End, start, end)
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

func TestDetectPaymentSecretsCardholderStopsAtMarker(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		value string
	}{
		{"stops at comma", "Cardholder: Ivan Ivanov, CVV: 123", "Ivan Ivanov"},
		{"stops at semicolon", "Cardholder: Ivan Ivanov; PIN: 4567", "Ivan Ivanov"},
		{"stops at newline", "Cardholder: Ivan Ivanov\nPIN: 4567", "Ivan Ivanov"},
		{"stops at payment marker", "Cardholder: Ivan Ivanov CVV 123", "Ivan Ivanov"},
		{"stops at cyrillic payment marker", "Держатель карты: Иван Иванов код безопасности 123", "Иван Иванов"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end := paymentSpan(tt.text, tt.value)
			got := filterCardholderCandidates(DetectPaymentSecrets(tt.text))
			if len(got) != 1 {
				t.Fatalf("cardholder candidates = %d, want 1", len(got))
			}
			if got[0].Start != start || got[0].End != end {
				t.Errorf("Start/End = %d/%d, want %d/%d", got[0].Start, got[0].End, start, end)
			}
		})
	}
}

func TestDetectPaymentSecretsNegative(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"plain three digits not cvv", "кабинет 123"},
		{"cvv embedded in longer run", "CVV: 1234"},
		{"cvv malformed length", "CVV: 12"},
		{"cvv digit embedded", "CVV: 9123"},
		{"cvv context part of larger word", "XCVV: 123"},
		{"plain four digits not pin", "код подтверждения 4567"},
		{"pin embedded in longer run", "PIN: 45678"},
		{"pin malformed length", "PIN: 456"},
		{"pin digit embedded", "PIN: 94567"},
		{"pin context part of larger word", "XPIN: 4567"},
		{"plain name without cardholder context", "Иван Иванов"},
		{"single word name", "Cardholder: Ivan"},
		{"marker part of larger word", "mycardholder Ivan Ivanov"},
		{"no separator after marker", "cardholderivan ivanov"},
		{"arbitrary phrase masked as name", "Cardholder: ivan ivanov lives nearby the park"},
		{"five continuous name words", "Cardholder: ivan ivanov petrov sidorov ivanov"},
		{"lowercase three word phrase", "Cardholder: ivan ivanov petrov"},
		{"lowercase four word phrase", "Cardholder: ivan ivanov lives nearby"},
		{"mixed case three word phrase", "Cardholder: ИвАН Иванов Петров"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectPaymentSecrets(tt.text); len(got) != 0 {
				t.Errorf("DetectPaymentSecrets(%q) = %d candidates, want 0", tt.text, len(got))
			}
		})
	}
}

func TestDetectPaymentSecretsCardholderTrimmedToSingleWord(t *testing.T) {
	text := "Cardholder: Ivan CVV 123"
	got := DetectPaymentSecrets(text)
	for _, c := range got {
		if c.Type == detection.TypeCardholderName {
			t.Errorf("DetectPaymentSecrets(%q) emitted CARDHOLDER_NAME %#v, want none", text, c)
		}
	}
	cvvStart, cvvEnd := paymentSpan(text, "123")
	if len(got) != 1 {
		t.Fatalf("DetectPaymentSecrets(%q) = %d candidates, want exactly 1 (CARD_CVV)", text, len(got))
	}
	c := got[0]
	if c.Type != detection.TypeCardCVV {
		t.Errorf("Type = %q, want CARD_CVV", c.Type)
	}
	if c.Start != cvvStart || c.End != cvvEnd {
		t.Errorf("Start/End = %d/%d, want %d/%d", c.Start, c.End, cvvStart, cvvEnd)
	}
}

func TestDetectPaymentSecretsMultipleAndOrder(t *testing.T) {
	text := "CVV: 123, PIN: 4567, Cardholder: Ivan Ivanov"
	got := DetectPaymentSecrets(text)
	if len(got) != 3 {
		t.Fatalf("DetectPaymentSecrets(%q) = %d candidates, want 3", text, len(got))
	}
	wantTypes := []detection.Type{detection.TypeCardCVV, detection.TypeCardPIN, detection.TypeCardholderName}
	for i, c := range got {
		if c.Type != wantTypes[i] {
			t.Errorf("candidate %d Type = %q, want %q", i, c.Type, wantTypes[i])
		}
		if i > 0 && got[i-1].Start > c.Start {
			t.Errorf("candidates not in document order: %d > %d", got[i-1].Start, c.Start)
		}
	}
}

func TestDetectPaymentSecretsScalarDedupe(t *testing.T) {
	text := "CVV: 123 CVV: 123"
	got := DetectPaymentSecrets(text)
	if len(got) != 2 {
		t.Fatalf("DetectPaymentSecrets(%q) = %d candidates, want 2", text, len(got))
	}
	if sameSpan(got[0], got[1]) {
		t.Errorf("duplicate candidates not deduplicated: %#v", got)
	}
}

func TestDetectPaymentSecretsUTF8PrefixOffsets(t *testing.T) {
	text := "Привет мир, CVV: 123"
	start, end := paymentSpan(text, "123")
	got := DetectPaymentSecrets(text)
	if len(got) != 1 {
		t.Fatalf("DetectPaymentSecrets(%q) = %d candidates, want 1", text, len(got))
	}
	if got[0].Start != start || got[0].End != end {
		t.Errorf("Start/End = %d/%d, want %d/%d", got[0].Start, got[0].End, start, end)
	}
}
