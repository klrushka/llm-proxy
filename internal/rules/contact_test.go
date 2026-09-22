package rules

import (
	"reflect"
	"testing"

	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/testcorpus"
)

func TestDetectContactsEmailPositive(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		start int
		end   int
	}{
		{"corpus form", "email: ivanov@example.com", 7, 25},
		{"plain", "ivanov@example.com", 0, 18},
		{"uppercase", "IVANOV@EXAMPLE.COM", 0, 18},
		{"mixed case", "Ivanov@Example.Com", 0, 18},
		{"local dots", "ivan.ov@example.com", 0, 19},
		{"local plus", "ivan+tag@example.com", 0, 20},
		{"local percent", "ivan%tag@example.com", 0, 20},
		{"local underscore", "ivan_ov@example.com", 0, 19},
		{"local hyphen", "ivan-ov@example.com", 0, 19},
		{"multi label domain", "ivan@sub.example.com", 0, 20},
		{"inner after whitespace", "user name@example.com", 5, 21},
		{"trailing period excluded", "user@example.com.", 0, 16},
		{"sentence punctuation excluded", "Contact ivanov@example.com!", 8, 26},
		{"comma excluded", "ivanov@example.com,", 0, 18},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectContacts(tt.text)
			if len(got) != 1 {
				t.Fatalf("DetectContacts(%q) = %d candidates, want 1", tt.text, len(got))
			}
			c := got[0]
			if c.Type != detection.TypeEmail {
				t.Errorf("Type = %q, want EMAIL", c.Type)
			}
			if c.Start != tt.start || c.End != tt.end {
				t.Errorf("Start/End = %d/%d, want %d/%d", c.Start, c.End, tt.start, tt.end)
			}
			if c.Confidence != 1.0 {
				t.Errorf("Confidence = %v, want 1.0", c.Confidence)
			}
			if !reflect.DeepEqual(c.Sources, []detection.Source{detection.SourceRegex}) {
				t.Errorf("Sources = %v, want [regex]", c.Sources)
			}
		})
	}
}

func TestDetectContactsEmailNegative(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"no tld", "user@localhost"},
		{"no local part", "@example.com"},
		{"no domain", "user@"},
		{"empty domain label", "user@.com"},
		{"leading dot domain", "user@.example.com"},
		{"consecutive dots local", "user..name@example.com"},
		{"consecutive dots domain", "user@example..com"},
		{"repeated trailing dots", "user@example.com.."},
		{"label leading hyphen", "user@-example.com"},
		{"label trailing hyphen", "user@example-.com"},
		{"local leading hyphen", "-user@example.com"},
		{"local trailing hyphen", "user-@example.com"},
		{"whitespace before at", "user @example.com"},
		{"embedded trailing underscore", "user@example.com_"},
		{"embedded trailing percent", "user@example.com%"},
		{"embedded trailing plus", "user@example.com+"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectContacts(tt.text); len(got) != 0 {
				t.Errorf("DetectContacts(%q) = %d candidates, want 0", tt.text, len(got))
			}
		})
	}
}

func TestDetectContactsPhonePositive(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		start int
		end   int
	}{
		{"corpus form", "Телефон: +7 900 123-45-67", 16, 32},
		{"plus7 compact", "+79001234567", 0, 12},
		{"plus7 spaces", "+7 900 123 45 67", 0, 16},
		{"plus7 hyphens", "+7-900-123-45-67", 0, 16},
		{"plus7 parens", "+7(900)123-45-67", 0, 16},
		{"plus7 parens spaces", "+7 (900) 123-45-67", 0, 18},
		{"domestic 8 compact", "89001234567", 0, 11},
		{"domestic 8 spaces", "8 900 123-45-67", 0, 15},
		{"domestic 8 parens", "8(900)123-45-67", 0, 15},
		{"sentence punctuation excluded", "Телефон: +7 900 123-45-67.", 16, 32},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectContacts(tt.text)
			if len(got) != 1 {
				t.Fatalf("DetectContacts(%q) = %d candidates, want 1", tt.text, len(got))
			}
			c := got[0]
			if c.Type != detection.TypePhone {
				t.Errorf("Type = %q, want PHONE", c.Type)
			}
			if c.Start != tt.start || c.End != tt.end {
				t.Errorf("Start/End = %d/%d, want %d/%d", c.Start, c.End, tt.start, tt.end)
			}
			if c.Confidence != 1.0 {
				t.Errorf("Confidence = %v, want 1.0", c.Confidence)
			}
			if !reflect.DeepEqual(c.Sources, []detection.Source{detection.SourceRegex}) {
				t.Errorf("Sources = %v, want [regex]", c.Sources)
			}
		})
	}
}

func TestDetectContactsPhoneNegative(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"too short", "+7 900 123-45"},
		{"too long", "+7 900 123-45-678"},
		{"missing prefix", "900 123-45-67"},
		{"unbalanced open paren", "+7(900 123-45-67"},
		{"unbalanced close paren", "+7 900)123-45-67"},
		{"letters inside", "+7 900 123-45-6a"},
		{"embedded in longer digit run", "123+7 900 123-45-67"},
		{"embedded after longer digit run", "+7 900 123-45-67456"},
		{"embedded both sides", "9+7 900 123-45-678"},
		{"bare digits no prefix", "9001234567"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectContacts(tt.text); len(got) != 0 {
				t.Errorf("DetectContacts(%q) = %d candidates, want 0", tt.text, len(got))
			}
		})
	}
}

func TestDetectContactsCyrillicByteOffsets(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		start int
		end   int
	}{
		{"cyrillic before email", "Почта: ivanov@example.com", 12, 30},
		{"cyrillic before phone", "Телефон: +7 900 123-45-67", 16, 32},
		{"cyrillic after email", "ivanov@example.com — почта", 0, 18},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectContacts(tt.text)
			if len(got) != 1 {
				t.Fatalf("DetectContacts(%q) = %d candidates, want 1", tt.text, len(got))
			}
			c := got[0]
			if c.Start != tt.start || c.End != tt.end {
				t.Errorf("Start/End = %d/%d, want %d/%d", c.Start, c.End, tt.start, tt.end)
			}
		})
	}
}

func TestDetectContactsDocumentOrderAndTieBreak(t *testing.T) {
	text := "ivanov@example.com +7 900 123-45-67"
	got := DetectContacts(text)
	if len(got) != 2 {
		t.Fatalf("DetectContacts(%q) = %d candidates, want 2", text, len(got))
	}
	if got[0].Type != detection.TypeEmail || got[1].Type != detection.TypePhone {
		t.Errorf("order = %q then %q, want EMAIL then PHONE", got[0].Type, got[1].Type)
	}
	if got[0].Start > got[1].Start {
		t.Errorf("candidates not in document order: %d > %d", got[0].Start, got[1].Start)
	}
}

func TestDetectContactsCorpusFixture(t *testing.T) {
	c, err := testcorpus.Load("../../testdata/pii-corpus.json")
	if err != nil {
		t.Fatalf("testcorpus.Load() error = %v", err)
	}
	if err := testcorpus.Validate(c); err != nil {
		t.Fatalf("testcorpus.Validate() error = %v", err)
	}

	for _, tc := range c.Cases {
		got := DetectContacts(tc.Input)
		for _, want := range tc.Spans {
			if want.Type != "EMAIL" && want.Type != "PHONE" {
				continue
			}
			found := false
			for _, c := range got {
				if c.Type == detection.Type(want.Type) && c.Start == want.Start && c.End == want.End {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("case %q: missing %s span [%d,%d)", tc.ID, want.Type, want.Start, want.End)
			}
		}
	}
}
