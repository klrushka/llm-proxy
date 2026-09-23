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

// Russian foreign passport: two series digits and seven number digits.
var foreignPassportDigitsRe = regexp.MustCompile(`\d{2}[ \t]?\d{7}`)

// divisionDigitsRe matches a passport division code in the standard 3-3
// grouping, e.g. "000-000".
var divisionDigitsRe = regexp.MustCompile(`\d{3}-\d{3}`)

// driverDigitsRe matches a Russian driver license in the common 4-6 grouping,
// e.g. "7777 123456".
var driverDigitsRe = regexp.MustCompile(`\d{4}[ \t]\d{6}`)

// driverDigits2Re matches a Russian driver license in the 2-2-6 grouping,
// e.g. "77 77 123456", classified only by an explicit driver context.
var driverDigits2Re = regexp.MustCompile(`\d{2}[ \t]\d{2}[ \t]\d{6}`)

// passportContexts are the explicit local context words that classify a
// 2-2-6 digit run as a passport number.
var passportContexts = []string{"паспорт"}

var foreignPassportContexts = []string{"загранпаспорт", "заграничный паспорт"}

// divisionContexts are the explicit local context words that classify a 3-3
// digit run as a passport division code.
var divisionContexts = []string{"код подразделения"}

// driverContexts are the explicit local context words that classify a 4-6
// digit run as a driver license number. They are stored lowercase; matching is
// case-insensitive.
var driverContexts = []string{"водительское удостоверение", "водительские права", "ву"}

// explicitPassportRe matches common explicit passport number forms where the
// series and number are written as 4-6 digit groups, e.g. "паспорт 0000 000000",
// "паспорт: серия 0000 номер 000000", "серия 0000 номер 000000", "серия 1234
// №567890", "серия 1234 № 567890" and "паспорт № 1234 567890". A leading "№"
// after the marker is part of the marker; an internal "№"/"номер" between the
// digit groups is part of the value. The captured value span covers both digit
// groups and the internal separator, but not the outer marker. Matching is
// case-insensitive.
var explicitPassportRe = regexp.MustCompile(`(?i)(?:паспорт[ \t]*:?[ \t]*(?:серия[ \t]*:?[ \t]*)?|серия[ \t]*:?[ \t]*)(?:№[ \t]*)?(\d{4}[ \t]+(?:(?:номер|№)[ \t]*)?\d{6})`)

// citizenshipWord matches a single alphabetic citizenship word or abbreviation
// in Russian or Latin, in any case, with an optional internal hyphen.
const citizenshipWord = `(?:[А-ЯЁа-яё]+(?:-[А-ЯЁа-яё]+)*|[A-Za-z]+(?:-[A-Za-z]+)*)`

// citizenshipRe matches "гражданство" followed by a 1-4 alphabetic citizenship
// value or a common abbreviation. A colon, dash or whitespace separator is
// allowed between the marker and the value. The value naturally stops at a
// comma, semicolon, period or newline. The captured value span is the value
// only. Matching is case-insensitive.
var citizenshipRe = regexp.MustCompile(`(?i)гражданство[ \t:—-]+(` + citizenshipWord + `(?:[ \t]+` + citizenshipWord + `){0,3})`)

// issuerRe matches the passport issuer markers "кем выдан", "орган, выдавший
// паспорт" and "паспорт выдан" followed by a non-empty issuer value. A colon,
// dash or whitespace separator is allowed after the marker. A token is any run
// of non-space, non-semicolon characters, so abbreviations, dots and digits are
// preserved and the value naturally stops at a semicolon or newline. The
// captured value span is the value only. Matching is case-insensitive.
var issuerRe = regexp.MustCompile(`(?i)(?:кем[ \t]+выдан|орган[ \t]*,[ \t]*выдавший[ \t]+паспорт|паспорт[ \t]+выдан)[ \t:—-]+([^\s;]+(?:[ \t]+[^\s;]+)*)`)

// citizenshipStopMarkers are the known passport field markers that terminate a
// citizenship value. They are stored lowercase; matching is case-insensitive.
var citizenshipStopMarkers = map[string]bool{
	"паспорт": true, "кем": true, "выдан": true, "орган": true,
	"дата": true, "выдачи": true, "код": true, "подразделения": true,
	"место": true, "рождения": true, "пол": true, "фамилия": true,
	"имя": true, "отчество": true,
}

// issuerStopMarkers are the known following passport field markers that
// terminate a passport issuer value. They are stored lowercase; matching is
// case-insensitive.
var issuerStopMarkers = map[string]bool{
	"код": true, "подразделения": true, "дата": true, "выдачи": true,
	"паспорт": true, "выдан": true, "кем": true, "орган": true,
	"место": true, "рождения": true, "пол": true, "фамилия": true,
	"имя": true, "отчество": true,
}

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
	out = append(out, identityCandidates(text, foreignPassportDigitsRe, foreignPassportContexts, detection.TypeForeignPassportNumber)...)
	out = append(out, explicitPassportCandidates(text)...)
	out = append(out, identityCandidates(text, divisionDigitsRe, divisionContexts, detection.TypePassportDivisionCode)...)
	out = append(out, identityCandidates(text, driverDigitsRe, driverContexts, detection.TypeDriverLicenseNumber)...)
	out = append(out, identityCandidates(text, driverDigits2Re, driverContexts, detection.TypeDriverLicenseNumber)...)
	out = append(out, citizenshipCandidates(text)...)
	out = append(out, issuerCandidates(text)...)

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

// digitBoundaryOK rejects a digit run embedded in a longer run, which covers
// too-long and malformed forms. A run is rejected when a letter or digit
// immediately adjoins it on either side.
func digitBoundaryOK(text string, start, end int) bool {
	if start > 0 {
		r, _ := utf8.DecodeLastRuneInString(text[:start])
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	if end < len(text) {
		r, _ := utf8.DecodeRuneInString(text[end:])
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// contextBefore reports whether one of contexts appears immediately before
// start (allowing only whitespace or a colon between) and is not part of a
// larger word. Matching is case-insensitive; contexts are stored lowercase. A
// Unicode letter or digit immediately before the context rejects the match.
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
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
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

// markerBoundaryOK reports whether the rune immediately before start is not a
// Unicode letter or digit, so a marker is not part of a larger word or number.
// Punctuation, quotes, whitespace and start-of-input are allowed.
func markerBoundaryOK(text string, start int) bool {
	if start <= 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(text[:start])
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

// explicitPassportCandidates finds explicit 4-6 passport number forms and emits
// the value span covering both digit groups and the internal separator word.
// The marker must not be part of a larger word and the value must not be
// embedded in a longer run.
func explicitPassportCandidates(text string) []detection.Candidate {
	var out []detection.Candidate
	for _, loc := range explicitPassportRe.FindAllStringSubmatchIndex(text, -1) {
		if !markerBoundaryOK(text, loc[0]) {
			continue
		}
		start, end := loc[2], loc[3]
		if start < 0 {
			continue
		}
		if !digitBoundaryOK(text, start, end) {
			continue
		}
		out = append(out, candidate(detection.TypePassportNumber, start, end))
	}
	return out
}

// citizenshipCandidates finds "гражданство:" values of 1-4 words and emits the
// value span, trimmed at the first known passport field marker.
func citizenshipCandidates(text string) []detection.Candidate {
	var out []detection.Candidate
	for _, loc := range citizenshipRe.FindAllStringSubmatchIndex(text, -1) {
		if !markerBoundaryOK(text, loc[0]) {
			continue
		}
		start, end := loc[2], loc[3]
		if start < 0 {
			continue
		}
		end, words := trimValueAtStopMarkers(text, start, end, citizenshipStopMarkers)
		if words < 1 || words > 4 {
			continue
		}
		if !valueTerminatedOK(text, end, citizenshipStopMarkers) {
			continue
		}
		out = append(out, candidate(detection.TypeCitizenship, start, end))
	}
	return out
}

// issuerCandidates finds passport issuer values after the explicit issuer
// markers and emits the value span, trimmed at the first known following field
// marker.
func issuerCandidates(text string) []detection.Candidate {
	var out []detection.Candidate
	for _, loc := range issuerRe.FindAllStringSubmatchIndex(text, -1) {
		if !markerBoundaryOK(text, loc[0]) {
			continue
		}
		start, end := loc[2], loc[3]
		if start < 0 {
			continue
		}
		end, words := trimValueAtStopMarkers(text, start, end, issuerStopMarkers)
		if words < 1 {
			continue
		}
		out = append(out, candidate(detection.TypePassportIssuer, start, end))
	}
	return out
}

// trimValueAtStopMarkers trims the value span so it stops at the first stop
// marker word, returning the trimmed end offset and the number of words
// collected. Trailing spaces, tabs and delimiter punctuation (comma, semicolon,
// period) are removed from the resulting end.
func trimValueAtStopMarkers(text string, start, end int, stop map[string]bool) (int, int) {
	i := start
	words := 0
	for i < end {
		for i < end && (text[i] == ' ' || text[i] == '\t') {
			i++
		}
		if i >= end {
			break
		}
		j := i
		for j < end && text[j] != ' ' && text[j] != '\t' {
			j++
		}
		if stop[strings.ToLower(text[i:j])] {
			end = i
			break
		}
		words++
		i = j
	}
	for end > start && (text[end-1] == ' ' || text[end-1] == '\t' || text[end-1] == ',' || text[end-1] == ';' || text[end-1] == '.') {
		end--
	}
	return end, words
}

// valueTerminatedOK reports whether the value span ending at end is properly
// terminated: by end of input, a Unicode punctuation delimiter or newline, or a
// known stop marker word. It rejects values that were truncated mid-list
// because they exceeded the allowed word count.
func valueTerminatedOK(text string, end int, stop map[string]bool) bool {
	i := end
	for i < len(text) && (text[i] == ' ' || text[i] == '\t') {
		i++
	}
	if i >= len(text) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(text[i:])
	if unicode.IsPunct(r) || r == '\n' {
		return true
	}
	j := i
	for j < len(text) && text[j] != ' ' && text[j] != '\t' {
		j++
	}
	return stop[strings.ToLower(text[i:j])]
}
