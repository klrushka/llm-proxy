package rules

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/klrushka/llm-proxy/internal/detection"
)

// passportDigitsRe matches a Russian passport series+number in the standard
// 2-2-6 grouping, e.g. "00 00 000000". The span is the digit run only; the
// surrounding "паспорт" context is required separately.
var passportDigitsRe = regexp.MustCompile(`\d{2}[ \t]\d{2}[ \t]\d{6}`)

// divisionDigitsRe matches a passport division code in the standard 3-3
// grouping, e.g. "000-000".
var divisionDigitsRe = regexp.MustCompile(`\d{3}-\d{3}`)

// driverDigitsRe matches a Russian driver license in the common 4-6 grouping,
// e.g. "7777 123456".
var driverDigitsRe = regexp.MustCompile(`\d{4}[ \t]\d{6}`)

// passportContexts are the explicit local context words that classify a
// 2-2-6 digit run as a passport number.
var passportContexts = []string{"паспорт"}

// divisionContexts are the explicit local context words that classify a 3-3
// digit run as a passport division code.
var divisionContexts = []string{"код подразделения"}

// driverContexts are the explicit local context words that classify a 4-6
// digit run as a driver license number. They are stored lowercase; matching is
// case-insensitive.
var driverContexts = []string{"водительское удостоверение", "водительские права", "ву"}

// DetectIdentityDocuments returns passport number, passport division code and
// driver license candidates for text. Offsets are UTF-8 byte offsets, start
// inclusive and end exclusive. Candidates are returned deterministically in
// document order, with Type as a stable tie-breaker only. No plaintext is
// stored on candidates.
//
// Passport and driver license numbers share a ten-digit shape, so each is
// classified only by its own explicit local context: a passport context never
// yields DRIVER_LICENSE_NUMBER and a driver license context never yields
// PASSPORT_NUMBER. Context-free digit runs are rejected.
func DetectIdentityDocuments(text string) []detection.Candidate {
	var out []detection.Candidate
	out = append(out, identityCandidates(text, passportDigitsRe, passportContexts, detection.TypePassportNumber)...)
	out = append(out, identityCandidates(text, divisionDigitsRe, divisionContexts, detection.TypePassportDivisionCode)...)
	out = append(out, identityCandidates(text, driverDigitsRe, driverContexts, detection.TypeDriverLicenseNumber)...)

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

// identityCandidates finds digit-run matches of re, rejects embedded or
// malformed runs, and keeps only those with an explicit local context word
// immediately before the digits.
func identityCandidates(text string, re *regexp.Regexp, contexts []string, t detection.Type) []detection.Candidate {
	var out []detection.Candidate
	for _, loc := range re.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		if !digitBoundaryOK(text, start, end) {
			continue
		}
		if !contextBefore(text, start, contexts) {
			continue
		}
		out = append(out, candidate(t, start, end))
	}
	return out
}

// digitBoundaryOK rejects a digit run embedded in a longer digit run, which
// covers too-long and malformed forms.
func digitBoundaryOK(text string, start, end int) bool {
	if start > 0 && isDigitByte(text[start-1]) {
		return false
	}
	if end < len(text) && isDigitByte(text[end]) {
		return false
	}
	return true
}

// contextBefore reports whether one of contexts appears immediately before
// start (allowing only whitespace or a colon between) and is not part of a
// larger word. Matching is case-insensitive; contexts are stored lowercase.
func contextBefore(text string, start int, contexts []string) bool {
	prefix := strings.TrimRight(text[:start], " \t:")
	lower := strings.ToLower(prefix)
	for _, ctx := range contexts {
		if !strings.HasSuffix(lower, ctx) {
			continue
		}
		ctxStart := len(prefix) - len(ctx)
		if ctxStart > 0 {
			r, _ := utf8.DecodeLastRuneInString(prefix[:ctxStart])
			if unicode.IsLetter(r) {
				continue
			}
		}
		return true
	}
	return false
}

// isDigitByte reports whether b is an ASCII digit.
func isDigitByte(b byte) bool {
	return b >= '0' && b <= '9'
}
