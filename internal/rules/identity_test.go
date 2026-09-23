package rules

import (
	"reflect"
	"strings"
	"testing"

	"github.com/klrushka/llm-proxy/internal/detection"
)

// identitySpan returns the UTF-8 byte offsets of the first occurrence of value
// in text, start inclusive and end exclusive.
func identitySpan(text, value string) (int, int) {
	i := strings.Index(text, value)
	if i < 0 {
		panic("value not found in text: " + value)
	}
	return i, i + len(value)
}

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
			name: "driver license 2-2-6 grouping",
			text: "водительское удостоверение 77 77 123456",
			want: []detection.Candidate{
				{Type: detection.TypeDriverLicenseNumber, Start: 52, End: 64, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
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
		{
			name: "explicit passport 4-6 form",
			text: "паспорт 0000 000000",
			want: []detection.Candidate{
				{Type: detection.TypePassportNumber, Start: 15, End: 26, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "explicit passport серия номер form",
			text: "паспорт: серия 0000 номер 000000",
			want: []detection.Candidate{
				{Type: detection.TypePassportNumber, Start: 27, End: 49, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "explicit passport серия номер standalone",
			text: "серия 0000 номер 000000",
			want: []detection.Candidate{
				{Type: detection.TypePassportNumber, Start: 11, End: 33, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "explicit passport серия № form",
			text: "серия 0000 № 000000",
			want: []detection.Candidate{
				{Type: detection.TypePassportNumber, Start: 11, End: 26, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "explicit passport серия № no space",
			text: "серия 1234 №567890",
			want: []detection.Candidate{
				{Type: detection.TypePassportNumber, Start: 11, End: 25, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "explicit passport серия № spaced",
			text: "серия 1234 № 567890",
			want: []detection.Candidate{
				{Type: detection.TypePassportNumber, Start: 11, End: 26, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "explicit passport паспорт № leading",
			text: "паспорт № 1234 567890",
			want: []detection.Candidate{
				{Type: detection.TypePassportNumber, Start: 19, End: 30, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "explicit passport uppercase marker",
			text: "ПАСПОРТ: СЕРИЯ 0000 НОМЕР 000000",
			want: []detection.Candidate{
				{Type: detection.TypePassportNumber, Start: 27, End: 49, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "citizenship value",
			text: "гражданство: Российская Федерация",
			want: []detection.Candidate{
				{Type: detection.TypeCitizenship, Start: 24, End: 63, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "citizenship abbreviation",
			text: "гражданство: РФ",
			want: []detection.Candidate{
				{Type: detection.TypeCitizenship, Start: 24, End: 28, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "citizenship stops at period",
			text: "гражданство: РФ.",
			want: []detection.Candidate{
				{Type: detection.TypeCitizenship, Start: 24, End: 28, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "citizenship stops at passport marker",
			text: "гражданство: Российская Федерация паспорт 00 00 000000",
			want: []detection.Candidate{
				{Type: detection.TypeCitizenship, Start: 24, End: 63, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
				{Type: detection.TypePassportNumber, Start: 79, End: 91, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "citizenship uppercase marker",
			text: "ГРАЖДАНСТВО: Российская Федерация",
			want: []detection.Candidate{
				{Type: detection.TypeCitizenship, Start: 24, End: 63, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "citizenship without colon",
			text: "Гражданство РФ",
			want: []detection.Candidate{
				{Type: detection.TypeCitizenship, Start: 23, End: 27, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "citizenship with dash separator",
			text: "Гражданство — РФ",
			want: []detection.Candidate{
				{Type: detection.TypeCitizenship, Start: 27, End: 31, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "issuer кем выдан",
			text: "кем выдан: ОВД района",
			want: []detection.Candidate{
				{Type: detection.TypePassportIssuer, Start: 19, End: 38, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "issuer орган выдавший паспорт",
			text: "орган, выдавший паспорт: ОВД",
			want: []detection.Candidate{
				{Type: detection.TypePassportIssuer, Start: 45, End: 51, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "issuer паспорт выдан",
			text: "паспорт выдан: ОВД",
			want: []detection.Candidate{
				{Type: detection.TypePassportIssuer, Start: 27, End: 33, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "issuer stops at код подразделения",
			text: "кем выдан: ОВД района код подразделения 000-000",
			want: []detection.Candidate{
				{Type: detection.TypePassportIssuer, Start: 19, End: 38, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
				{Type: detection.TypePassportDivisionCode, Start: 73, End: 80, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "issuer uppercase marker",
			text: "КЕМ ВЫДАН: ОВД",
			want: []detection.Candidate{
				{Type: detection.TypePassportIssuer, Start: 19, End: 25, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "issuer without colon",
			text: "Кем выдан ОВД района",
			want: []detection.Candidate{
				{Type: detection.TypePassportIssuer, Start: 18, End: 37, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "cyrillic prefix before explicit passport",
			text: "Данные: паспорт 0000 000000",
			want: []detection.Candidate{
				{Type: detection.TypePassportNumber, Start: 29, End: 40, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "cyrillic prefix before citizenship",
			text: "Данные: гражданство: Российская Федерация",
			want: []detection.Candidate{
				{Type: detection.TypeCitizenship, Start: 38, End: 77, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "citizenship stops at semicolon",
			text: "гражданство: РФ; код подразделения: 000-000",
			want: []detection.Candidate{
				{Type: detection.TypeCitizenship, Start: 24, End: 28, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
				{Type: detection.TypePassportDivisionCode, Start: 65, End: 72, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "citizenship stops at comma",
			text: "гражданство: Российская Федерация, паспорт 00 00 000000",
			want: []detection.Candidate{
				{Type: detection.TypeCitizenship, Start: 24, End: 63, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
				{Type: detection.TypePassportNumber, Start: 80, End: 92, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "citizenship stops at newline",
			text: "гражданство: Российская Федерация\nпаспорт 00 00 000000",
			want: []detection.Candidate{
				{Type: detection.TypeCitizenship, Start: 24, End: 63, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
				{Type: detection.TypePassportNumber, Start: 79, End: 91, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "issuer full text with abbreviation and digits",
			text: "кем выдан: ГУ МВД России по г. Москве",
			want: []detection.Candidate{
				{Type: detection.TypePassportIssuer, Start: 19, End: 65, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "issuer stops at semicolon",
			text: "кем выдан: ГУ МВД России по г. Москве; код подразделения: 000-000",
			want: []detection.Candidate{
				{Type: detection.TypePassportIssuer, Start: 19, End: 65, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
				{Type: detection.TypePassportDivisionCode, Start: 102, End: 109, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "issuer stops at newline",
			text: "кем выдан: ГУ МВД России по г. Москве\nдата выдачи: 01.01.2020",
			want: []detection.Candidate{
				{Type: detection.TypePassportIssuer, Start: 19, End: 65, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "issuer stops at дата выдачи",
			text: "кем выдан: ГУ МВД России по г. Москве дата выдачи: 01.01.2020",
			want: []detection.Candidate{
				{Type: detection.TypePassportIssuer, Start: 19, End: 65, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "issuer trailing comma trimmed",
			text: "кем выдан: ОВД, код подразделения: 000-000",
			want: []detection.Candidate{
				{Type: detection.TypePassportIssuer, Start: 19, End: 25, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
				{Type: detection.TypePassportDivisionCode, Start: 62, End: 69, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
			},
		},
		{
			name: "issuer does not capture next field",
			text: "кем выдан: ОВД района паспорт выдан: 01.01.2020",
			want: []detection.Candidate{
				{Type: detection.TypePassportIssuer, Start: 19, End: 38, Confidence: 1.0, Sources: []detection.Source{detection.SourceRegex}},
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
		{"passport malformed grouping", "паспорт 000 000000"},
		{"context part of larger word", "загранпаспорт 00 00 000000"},
		{"division too short", "код подразделения 000-00"},
		{"division malformed", "код подразделения 0000-000"},
		{"driver embedded in longer run", "водительское удостоверение 7777 1234567"},
		{"explicit passport too long series", "паспорт 00000 000000"},
		{"explicit passport too short series", "паспорт 000 000000"},
		{"explicit passport too long number", "паспорт 0000 0000000"},
		{"explicit passport too short number", "паспорт 0000 00000"},
		{"explicit passport embedded series", "паспорт 10000 000000"},
		{"explicit passport marker part of larger word", "загранпаспорт 0000 000000"},
		{"citizenship empty value", "гражданство:"},
		{"citizenship numeric value", "гражданство: 123"},
		{"citizenship symbol value", "гражданство: @@@"},
		{"citizenship too many words", "гражданство: Российская Федерация Российская Федерация Российская"},
		{"citizenship too many words before comma", "гражданство: Российская Федерация Российская Федерация Российская, далее"},
		{"citizenship marker part of larger word", "гражданствоподтверждение: Российская Федерация"},
		{"citizenship marker preceded by digit", "1гражданство: Российская Федерация"},
		{"issuer empty value", "кем выдан:"},
		{"issuer marker part of larger word", "загранпаспорт выдан: ОВД"},
		{"explicit passport value followed by letter", "паспорт 0000 000000A"},
		{"explicit passport value preceded by letter", "паспорт A0000 000000"},
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
