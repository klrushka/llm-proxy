package rules

import (
	"regexp"
	"sort"

	"github.com/klrushka/llm-proxy/internal/detection"
)

// innPersonRe matches a contiguous 12-digit run, the personal INN form. The
// span is the digit run only; the surrounding INN/person context is required
// separately.
var innPersonRe = regexp.MustCompile(`\d{12}`)

// innPersonContexts are the explicit local context words that classify a
// 12-digit run as a personal INN. They are stored lowercase; matching is
// case-insensitive.
var innPersonContexts = []string{
	"инн",
	"инн физлица",
	"инн физического лица",
	"инн физ. лица",
	"инн человека",
	"инн гражданина",
}

// innOrgContexts are the explicit local context words that denote an
// organization or legal entity. A 12-digit run under such context is never
// emitted as INN_PERSON. They are stored lowercase; matching is
// case-insensitive.
var innOrgContexts = []string{
	"инн организации",
	"инн юрлица",
	"инн юридического лица",
	"инн компании",
	"инн фирмы",
	"инн ооо",
	"инн оао",
	"инн зао",
}

// innPersonWeights11 and innPersonWeights12 are the official check-digit
// weights for the 11th and 12th digits of a personal INN.
var innPersonWeights11 = [10]int{7, 2, 4, 10, 3, 5, 9, 4, 6, 8}
var innPersonWeights12 = [11]int{3, 7, 2, 4, 10, 3, 5, 9, 4, 6, 8}

// innOrgWeights are the official check-digit weights for the 10th digit of an
// organization INN.
var innOrgWeights = [9]int{2, 4, 10, 3, 5, 9, 4, 6, 8}

// DetectINN returns personal INN candidates for text. Only a contiguous
// 12-digit run with explicit INN/person context and both official check digits
// correct is emitted as INN_PERSON. A run whose local context explicitly
// denotes an organization or legal entity is rejected. The 10-digit
// organization form is validated by its checksum via ValidINNOrganization but
// never emitted because the canonical registry has no organization-INN type.
// Offsets are UTF-8 byte offsets, start inclusive and end exclusive. No
// plaintext is stored on candidates.
func DetectINN(text string) []detection.Candidate {
	var out []detection.Candidate
	for _, loc := range innPersonRe.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		if !digitBoundaryOK(text, start, end) {
			continue
		}
		if innOrgContextBefore(text, start) {
			continue
		}
		if !innPersonContextBefore(text, start) {
			continue
		}
		if !ValidINNPerson(text[start:end]) {
			continue
		}
		out = append(out, validatedCandidate(detection.TypeINNPerson, start, end))
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

// ValidINNPerson reports whether digits is a 12-digit personal INN with both
// official check digits correct.
func ValidINNPerson(digits string) bool {
	if len(digits) != 12 {
		return false
	}
	for i := 0; i < len(digits); i++ {
		if !isDigitByte(digits[i]) {
			return false
		}
	}
	if checkDigit(digits, innPersonWeights11[:]) != int(digits[10]-'0') {
		return false
	}
	return checkDigit(digits, innPersonWeights12[:]) == int(digits[11]-'0')
}

// ValidINNOrganization reports whether digits is a 10-digit organization INN
// with its official check digit correct. It recognizes the organization form
// without emitting it as a personal INN.
func ValidINNOrganization(digits string) bool {
	if len(digits) != 10 {
		return false
	}
	for i := 0; i < len(digits); i++ {
		if !isDigitByte(digits[i]) {
			return false
		}
	}
	return checkDigit(digits, innOrgWeights[:]) == int(digits[9]-'0')
}

// checkDigit computes the official INN check digit for digits using weights,
// applying mod 11 then mod 10.
func checkDigit(digits string, weights []int) int {
	sum := 0
	for i, w := range weights {
		sum += int(digits[i]-'0') * w
	}
	return sum % 11 % 10
}

// innPersonContextBefore reports whether a personal INN context appears
// immediately before start (allowing only whitespace or a colon between).
func innPersonContextBefore(text string, start int) bool {
	return contextBefore(text, start, innPersonContexts)
}

// innOrgContextBefore reports whether an organization context appears
// immediately before start (allowing only whitespace or a colon between).
func innOrgContextBefore(text string, start int) bool {
	return contextBefore(text, start, innOrgContexts)
}

// validatedCandidate builds a regex+validator candidate.
func validatedCandidate(t detection.Type, start, end int) detection.Candidate {
	return detection.Candidate{
		Type:       t,
		Start:      start,
		End:        end,
		Confidence: 1.0,
		Sources:    []detection.Source{detection.SourceRegex, detection.SourceValidator},
	}
}
