package rules

import (
	"reflect"
	"testing"

	"github.com/klrushka/llm-proxy/internal/detection"
)

func TestDetectIdentityDocumentsPositive(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []detection.Candidate
	}{
		{
			name: "passport number",
			text: "паспорт 00 00 000000",
			want: []detection.Candidate{
				{Type: detection.TypePassportNumber, Start: 15, End: 27, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "passport number uppercase context",
			text: "Паспорт: 00 00 000000",
			want: []detection.Candidate{
				{Type: detection.TypePassportNumber, Start: 16, End: 28, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "division code",
			text: "код подразделения 000-000",
			want: []detection.Candidate{
				{Type: detection.TypePassportDivisionCode, Start: 34, End: 41, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "driver license full context",
			text: "водительское удостоверение 7777 123456",
			want: []detection.Candidate{
				{Type: detection.TypeDriverLicenseNumber, Start: 52, End: 63, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "driver license abbreviation context",
			text: "ВУ: 7777 123456",
			want: []detection.Candidate{
				{Type: detection.TypeDriverLicenseNumber, Start: 6, End: 17, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "passport number plus division code",
			text: "паспорт 00 00 000000, код подразделения 000-000",
			want: []detection.Candidate{
				{Type: detection.TypePassportNumber, Start: 15, End: 27, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
				{Type: detection.TypePassportDivisionCode, Start: 63, End: 70, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "cyrillic prefix before passport",
			text: "Данные: паспорт 00 00 000000",
			want: []detection.Candidate{
				{Type: detection.TypePassportNumber, Start: 29, End: 41, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "driver license context yields only driver type",
			text: "водительское удостоверение 7777 123456",
			want: []detection.Candidate{
				{Type: detection.TypeDriverLicenseNumber, Start: 52, End: 63, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "passport context yields only passport type",
			text: "паспорт 00 00 000000",
			want: []detection.Candidate{
				{Type: detection.TypePassportNumber, Start: 15, End: 27, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectIdentityDocuments(tt.text)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DetectIdentityDocuments(%q) = %#v, want %#v", tt.text, got, tt.want)
			}
		})
	}
}

func TestDetectIdentityDocumentsNegative(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"passport without context", "00 00 000000"},
		{"driver without context", "0000 000000"},
		{"division without context", "000-000"},
		{"passport too long", "паспорт 00 00 0000000"},
		{"passport too short", "паспорт 00 00 00000"},
		{"passport malformed grouping", "паспорт 0000 000000"},
		{"driver license unsupported grouping", "водительское удостоверение 77 77 123456"},
		{"context part of larger word", "загранпаспорт 00 00 000000"},
		{"division too short", "код подразделения 000-00"},
		{"division malformed", "код подразделения 0000-000"},
		{"driver embedded in longer run", "водительское удостоверение 7777 1234567"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectIdentityDocuments(tt.text); len(got) != 0 {
				t.Errorf("DetectIdentityDocuments(%q) = %d candidates, want 0", tt.text, len(got))
			}
		})
	}
}

func TestDetectIdentityDocumentsDocumentOrder(t *testing.T) {
	text := "паспорт 00 00 000000, код подразделения 000-000"
	got := DetectIdentityDocuments(text)
	if len(got) != 2 {
		t.Fatalf("DetectIdentityDocuments(%q) = %d candidates, want 2", text, len(got))
	}
	if got[0].Type != detection.TypePassportNumber || got[1].Type != detection.TypePassportDivisionCode {
		t.Errorf("order = %q then %q, want PASSPORT_NUMBER then PASSPORT_DIVISION_CODE", got[0].Type, got[1].Type)
	}
	if got[0].Start > got[1].Start {
		t.Errorf("candidates not in document order: %d > %d", got[0].Start, got[1].Start)
	}
}
