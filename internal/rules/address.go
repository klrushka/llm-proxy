package rules

import (
	"regexp"
	"sort"
	"strings"

	"github.com/klrushka/llm-proxy/internal/detection"
)

// wordValue matches one or more Cyrillic words (with optional hyphens)
// separated by single spaces or tabs. It stops at non-Cyrillic delimiters such
// as commas, semicolons, newlines and digits.
const wordValue = `[А-Яа-яЁё]+(?:-[А-Яа-яЁё]+)?(?:[ \t]+[А-Яа-яЁё]+(?:-[А-Яа-яЁё]+)?)*`

// digitValue matches a run of ASCII digits with an optional single Cyrillic or
// Latin letter suffix (e.g. "5а").
const digitValue = `\d+[A-Za-zА-Яа-яЁё]?`

// cityRe matches a city marker (г. or город) followed by a word value.
var cityRe = regexp.MustCompile(`(?i)(?:г\.|город)[ \t]*(` + wordValue + `)`)

// streetRe matches a street marker (ул., улица, проспект, пр-т) followed by a
// word value.
var streetRe = regexp.MustCompile(`(?i)(?:ул\.|улица|проспект|пр-т)[ \t]*(` + wordValue + `)`)

// houseRe matches a house marker (д. or дом) followed by a digit value.
var houseRe = regexp.MustCompile(`(?i)(?:д\.|дом)[ \t]*(` + digitValue + `)`)

// buildingRe matches a building marker (корп., корпус, стр., строение)
// followed by a digit value.
var buildingRe = regexp.MustCompile(`(?i)(?:корп\.|корпус|стр\.|строение)[ \t]*(` + digitValue + `)`)

// apartmentRe matches an apartment marker (кв. or квартира) followed by a
// digit value.
var apartmentRe = regexp.MustCompile(`(?i)(?:кв\.|квартира)[ \t]*(` + digitValue + `)`)

// regionSuffixRe matches a region of the form "<word> область|край".
var regionSuffixRe = regexp.MustCompile(`(?i)([А-Яа-яЁё]+(?:-[А-Яа-яЁё]+)?)[ \t]+(область|край)`)

// regionRepublicRe matches a region of the form "Республика <word>".
var regionRepublicRe = regexp.MustCompile(`(?i)Республика[ \t]+([А-Яа-яЁё]+(?:-[А-Яа-яЁё]+)?)`)

// countryRe matches an explicit Russian country form.
var countryRe = regexp.MustCompile(`(?i)Российская Федерация|Россия|РФ`)

// postalRe matches a six-digit Russian postal index.
var postalRe = regexp.MustCompile(`\d{6}`)

// addressMarkerWords are the marker words that terminate a marker-based value
// span. They are stored lowercase; matching is case-insensitive.
var addressMarkerWords = map[string]bool{
	"г": true, "город": true, "ул": true, "улица": true,
	"проспект": true, "пр-т": true, "д": true, "дом": true,
	"корп": true, "корпус": true, "стр": true, "строение": true,
	"кв": true, "квартира": true, "область": true, "край": true,
	"республика": true, "индекс": true,
}

// addressBlockMarkers are the markers that indicate an explicit address block
// for postal-code context. They are stored lowercase; matching is
// case-insensitive.
var addressBlockMarkers = []string{
	"г", "город", "ул", "улица", "проспект", "пр-т", "д", "дом",
	"корп", "корпус", "стр", "строение", "кв", "квартира",
	"область", "край", "республика", "индекс", "почтовый индекс", "адрес",
}

// postalBlockWindow is the character window around a six-digit run within
// which an address marker qualifies it as a postal index inside an address
// block.
const postalBlockWindow = 60

// DetectAddressComponents returns Russian address component candidates for
// text. It emits canonical component types only: ADDRESS_POSTAL_CODE,
// ADDRESS_COUNTRY, ADDRESS_REGION, ADDRESS_CITY, ADDRESS_STREET,
// ADDRESS_HOUSE, ADDRESS_BUILDING and ADDRESS_APARTMENT. A full ADDRESS
// candidate is intentionally not emitted here; task 6.2 builds it from
// sufficient address context. Offsets are UTF-8 byte offsets, start inclusive
// and end exclusive. Candidates are returned deterministically in document
// order with exact duplicate removal and no plaintext stored on them.
func DetectAddressComponents(text string) []detection.Candidate {
	var out []detection.Candidate
	out = append(out, postalCandidates(text)...)
	out = append(out, countryCandidates(text)...)
	out = append(out, regionCandidates(text)...)
	out = append(out, markerCandidates(text, cityRe, detection.TypeAddressCity)...)
	out = append(out, markerCandidates(text, streetRe, detection.TypeAddressStreet)...)
	out = append(out, markerCandidates(text, houseRe, detection.TypeAddressHouse)...)
	out = append(out, markerCandidates(text, buildingRe, detection.TypeAddressBuilding)...)
	out = append(out, markerCandidates(text, apartmentRe, detection.TypeAddressApartment)...)

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		if out[i].End != out[j].End {
			return out[i].End < out[j].End
		}
		return out[i].Type < out[j].Type
	})
	return dedupeCandidates(out)
}

// postalCandidates finds six-digit runs and keeps only those with explicit
// postal context or inside an explicit nearby address block. All-zero and
// digit-embedded runs are rejected.
func postalCandidates(text string) []detection.Candidate {
	var out []detection.Candidate
	for _, loc := range postalRe.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		if !digitBoundaryOK(text, start, end) {
			continue
		}
		if isAllZero(text[start:end]) {
			continue
		}
		if !postalContextOK(text, start) && !postalInAddressBlock(text, start, end) {
			continue
		}
		out = append(out, validatedCandidate(detection.TypeAddressPostalCode, start, end))
	}
	return out
}

// postalContextOK reports whether an explicit postal context word appears
// immediately before start.
func postalContextOK(text string, start int) bool {
	return contextBefore(text, start, []string{"индекс", "почтовый индекс"})
}

// postalInAddressBlock reports whether an address marker appears within a
// bounded window around the six-digit run on the same line, indicating an
// explicit nearby address block. The window never crosses a newline, so an
// address marker on a preceding or following line does not qualify a plain
// six-digit number.
func postalInAddressBlock(text string, start, end int) bool {
	lineStart := start
	for lineStart > 0 && text[lineStart-1] != '\n' {
		lineStart--
	}
	lineEnd := end
	for lineEnd < len(text) && text[lineEnd] != '\n' {
		lineEnd++
	}

	lo := start - postalBlockWindow
	if lo < lineStart {
		lo = lineStart
	}
	hi := end + postalBlockWindow
	if hi > lineEnd {
		hi = lineEnd
	}
	lower := strings.ToLower(text[lo:hi])
	for _, m := range addressBlockMarkers {
		if containsWord(lower, m) {
			return true
		}
	}
	return false
}

// isAllZero reports whether s consists only of the digit '0'.
func isAllZero(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}

// countryCandidates finds explicit Russian country forms.
func countryCandidates(text string) []detection.Candidate {
	var out []detection.Candidate
	for _, loc := range countryRe.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		if !wordBoundaryOK(text, start, end) {
			continue
		}
		out = append(out, validatedCandidate(detection.TypeAddressCountry, start, end))
	}
	return out
}

// regionCandidates finds marked region forms.
func regionCandidates(text string) []detection.Candidate {
	var out []detection.Candidate
	for _, loc := range regionSuffixRe.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		if !wordBoundaryOK(text, start, end) {
			continue
		}
		out = append(out, validatedCandidate(detection.TypeAddressRegion, start, end))
	}
	for _, loc := range regionRepublicRe.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		if !wordBoundaryOK(text, start, end) {
			continue
		}
		out = append(out, validatedCandidate(detection.TypeAddressRegion, start, end))
	}
	return out
}

// markerCandidates finds marker-based component values and emits the value
// span, not the marker. The value is trimmed at the first address marker word.
func markerCandidates(text string, re *regexp.Regexp, t detection.Type) []detection.Candidate {
	var out []detection.Candidate
	for _, loc := range re.FindAllStringSubmatchIndex(text, -1) {
		if loc[0] > 0 && isCyrillicByte(text[loc[0]-1]) {
			continue
		}
		start, end := loc[2], loc[3]
		if start < 0 {
			continue
		}
		end = trimValueAtMarker(text, start, end)
		if start >= end {
			continue
		}
		out = append(out, validatedCandidate(t, start, end))
	}
	return out
}

// trimValueAtMarker trims the value span so it stops at the first address
// marker word, returning the trimmed end offset. Trailing spaces or tabs are
// removed from the resulting end so the value span is exact.
func trimValueAtMarker(text string, start, end int) int {
	i := start
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
		if addressMarkerWords[strings.ToLower(text[i:j])] {
			end = i
			break
		}
		i = j
	}
	for end > start && (text[end-1] == ' ' || text[end-1] == '\t') {
		end--
	}
	return end
}

// wordBoundaryOK rejects a match whose immediate neighbors extend the word.
func wordBoundaryOK(text string, start, end int) bool {
	if start > 0 && isCyrillicByte(text[start-1]) {
		return false
	}
	if end < len(text) && isCyrillicByte(text[end]) {
		return false
	}
	return true
}

// containsWord reports whether word appears in s as a whole Cyrillic word.
func containsWord(s, word string) bool {
	for i := 0; i+len(word) <= len(s); i++ {
		if s[i:i+len(word)] != word {
			continue
		}
		if i > 0 && isCyrillicByte(s[i-1]) {
			continue
		}
		if i+len(word) < len(s) && isCyrillicByte(s[i+len(word)]) {
			continue
		}
		return true
	}
	return false
}

// isCyrillicByte reports whether b is a byte of a multi-byte (Cyrillic) UTF-8
// character. All Cyrillic UTF-8 bytes are >= 0x80.
func isCyrillicByte(b byte) bool {
	return b >= 0x80
}
