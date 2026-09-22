package rules

import (
	"regexp"
	"sort"

	"github.com/klrushka/llm-proxy/internal/detection"
)

// cardRe matches a bank card number in contiguous 13-19 digit form or in the
// common single-space or single-hyphen grouping of 4-digit groups. The span is
// the full formatted number including separators; validation normalizes
// separators and applies Luhn. Exactly one separator is required between digit
// groups, so doubled or otherwise malformed separators do not match.
var cardRe = regexp.MustCompile(`\d{13,19}|\d{4}(?:[ \t-]\d{4}){2,3}(?:[ \t-]\d{1,4})?`)

// DetectBankCards returns bank card number candidates for text. Only a 13-19
// digit run (contiguous or grouped by single spaces or hyphens) whose
// normalized digits pass Luhn is emitted as BANK_CARD_NUMBER. Offsets are
// UTF-8 byte offsets, start inclusive and end exclusive, covering the original
// formatted span. Candidates are returned deterministically in document order.
// No plaintext is stored on candidates.
func DetectBankCards(text string) []detection.Candidate {
	var out []detection.Candidate
	for _, loc := range cardRe.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		if !cardBoundaryOK(text, start, end) {
			continue
		}
		if !ValidLuhn(normalizeCardDigits(text[start:end])) {
			continue
		}
		out = append(out, validatedCandidate(detection.TypeBankCardNumber, start, end))
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		if out[i].End != out[j].End {
			return out[i].End < out[j].End
		}
		return out[i].Type < out[j].Type
	})
	return out
}

// ValidLuhn reports whether digits is a 13-19 digit card number passing the
// Luhn checksum. digits must contain only ASCII digits; separators are not
// accepted here.
func ValidLuhn(digits string) bool {
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	for i := 0; i < len(digits); i++ {
		if !isDigitByte(digits[i]) {
			return false
		}
	}
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// normalizeCardDigits strips single spaces, tabs and hyphens from a formatted
// card span, returning only the digits. It is used only for Luhn validation;
// the original span is preserved on the candidate.
func normalizeCardDigits(s string) string {
	digits := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if isDigitByte(s[i]) {
			digits = append(digits, s[i])
		}
	}
	return string(digits)
}

// cardBoundaryOK rejects a match embedded in a longer card-like run. Ordinary
// prose whitespace before or after a card is allowed, but a match is rejected
// when an adjacent digit, or an adjacent space/tab/hyphen that connects to a
// digit or another separator outside the match, extends the run. This prevents
// prefix/suffix matches inside repeated-separator or longer grouped runs.
func cardBoundaryOK(text string, start, end int) bool {
	if start > 0 {
		b := text[start-1]
		if isDigitByte(b) {
			return false
		}
		if isCardSeparator(b) && start > 1 && isCardTokenByte(text[start-2]) {
			return false
		}
	}
	if end < len(text) {
		b := text[end]
		if isDigitByte(b) {
			return false
		}
		if isCardSeparator(b) && end+1 < len(text) && isCardTokenByte(text[end+1]) {
			return false
		}
	}
	return true
}

// isCardSeparator reports whether b is a single card grouping separator.
func isCardSeparator(b byte) bool {
	return b == ' ' || b == '\t' || b == '-'
}

// isCardTokenByte reports whether b can continue a card-like run: a digit or a
// grouping separator. Used only for boundary rejection, not for parsing.
func isCardTokenByte(b byte) bool {
	return isDigitByte(b) || isCardSeparator(b)
}
