package rules

import (
	"reflect"
	"testing"

	"github.com/klrushka/llm-proxy/internal/detection"
)

// Synthetic valid personal INN: 123456789047 (both check digits correct).
// Synthetic valid organization INN: 1234567894 (check digit correct).

func TestDetectINNPositive(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []detection.Candidate
	}{
		{
			name: "person inn физлица",
			text: "ИНН физлица 123456789047",
			want: []detection.Candidate{
				{Type: detection.TypeINNPerson, Start: 22, End: 34, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex, detection.SourceValidator}},
			},
		},
		{
			name: "bare инн context",
			text: "ИНН 123456789047",
			want: []detection.Candidate{
				{Type: detection.TypeINNPerson, Start: 7, End: 19, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex, detection.SourceValidator}},
			},
		},
		{
			name: "cyrillic prefix before инн",
			text: "Данные: ИНН физлица 123456789047",
			want: []detection.Candidate{
				{Type: detection.TypeINNPerson, Start: 36, End: 48, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex, detection.SourceValidator}},
			},
		},
		{
			name: "person inn физического лица",
			text: "ИНН физического лица 123456789047",
			want: []detection.Candidate{
				{Type: detection.TypeINNPerson, Start: 39, End: 51, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex, detection.SourceValidator}},
			},
		},
		{
			name: "person inn физ. лица",
			text: "ИНН физ. лица 123456789047",
			want: []detection.Candidate{
				{Type: detection.TypeINNPerson, Start: 24, End: 36, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex, detection.SourceValidator}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectINN(tt.text)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DetectINN(%q) = %#v, want %#v", tt.text, got, tt.want)
			}
		})
	}
}

func TestDetectINNNegative(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"organization context организации", "ИНН организации 123456789047"},
		{"organization context юрлица", "ИНН юрлица 123456789047"},
		{"organization context юридического лица", "ИНН юридического лица 123456789047"},
		{"organization inn not emitted as person", "ИНН организации 1234567894"},
		{"context free", "123456789047"},
		{"mutated first check digit", "ИНН физлица 123456789057"},
		{"mutated second check digit", "ИНН физлица 123456789048"},
		{"invalid checksum", "ИНН физлица 123456789040"},
		{"too short", "ИНН физлица 12345678904"},
		{"too long", "ИНН физлица 1234567890471"},
		{"non digit", "ИНН физлица 12345678904a"},
		{"embedded in longer digit run", "ИНН физлица 9123456789047"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectINN(tt.text); len(got) != 0 {
				t.Errorf("DetectINN(%q) = %d candidates, want 0", tt.text, len(got))
			}
		})
	}
}

func TestValidINNPerson(t *testing.T) {
	tests := []struct {
		name   string
		digits string
		want   bool
	}{
		{"valid", "123456789047", true},
		{"mutated first check digit", "123456789057", false},
		{"mutated second check digit", "123456789048", false},
		{"too short", "12345678904", false},
		{"too long", "1234567890471", false},
		{"non digit", "12345678904a", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidINNPerson(tt.digits); got != tt.want {
				t.Errorf("ValidINNPerson(%q) = %v, want %v", tt.digits, got, tt.want)
			}
		})
	}
}

func TestValidINNOrganization(t *testing.T) {
	tests := []struct {
		name   string
		digits string
		want   bool
	}{
		{"valid", "1234567894", true},
		{"mutated check digit", "1234567895", false},
		{"too short", "123456789", false},
		{"too long", "12345678941", false},
		{"non digit", "123456789a", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidINNOrganization(tt.digits); got != tt.want {
				t.Errorf("ValidINNOrganization(%q) = %v, want %v", tt.digits, got, tt.want)
			}
		})
	}
}
