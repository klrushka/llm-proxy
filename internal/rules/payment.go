package rules

import (
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/klrushka/llm-proxy/internal/detection"
)

// cvvRe matches exactly three ASCII digits, the CVV value shape. The span is
// the digit run only; the surrounding CVV/CVC/код безопасности context is
// required separately.
var cvvRe = regexp.MustCompile(`\d{3}`)

// pinRe matches exactly four ASCII digits, the PIN value shape. The span is
// the digit run only; the surrounding PIN/ПИН/ПИН-код context is required
// separately.
var pinRe = regexp.MustCompile(`\d{4}`)

// cvvContexts are the explicit local context words that classify a three-digit
// run as a card CVV. They are stored lowercase; matching is case-insensitive.
var cvvContexts = []string{"cvv", "cvc", "cvv-код", "код безопасности"}

// pinContexts are the explicit local context words that classify a four-digit
// run as a card PIN. They are stored lowercase; matching is case-insensitive.
var pinContexts = []string{"pin", "пин", "пин-код"}

// cardholderNameWord matches a single personal-name word in Russian or Latin,
// in any case, with an optional internal hyphen. Matching is case-insensitive,
// so lowercase and mixed-case names are accepted.
const cardholderNameWord = `(?:[А-ЯЁа-яё]+(?:-[А-ЯЁа-яё]+)*|[A-Za-z]+(?:-[A-Za-z]+)*)`

// cardholderNameValue matches a deliberately small 2-4 word personal-name
// grammar, words separated by single spaces or tabs.
const cardholderNameValue = cardholderNameWord + `(?:[ \t]+` + cardholderNameWord + `){1,3}`

// cardholderRe matches an explicit cardholder marker followed by a name value.
// A real separator (whitespace or a colon with optional surrounding whitespace)
// is required after the marker. The marker and the name value are matched
// case-insensitively. The value span is captured.
var cardholderRe = regexp.MustCompile(`(?i)(?:cardholder name|cardholder|держатель карты|имя держателя|владелец карты|имя на карте)[ \t:]+` + `(` + cardholderNameValue + `)`)

// cardholderStopMarkers are the known following payment markers that terminate
// a cardholder name value. They are stored lowercase; matching is
// case-insensitive.
var cardholderStopMarkers = map[string]bool{
	"cvv": true, "cvc": true, "pin": true,
	"expiry": true, "exp": true, "valid": true, "thru": true,
	"bank": true, "срок": true, "действия": true, "действует": true,
	"до": true, "банк": true, "код": true, "безопасности": true,
	"пин": true, "пин-код": true,
}

// DetectPaymentSecrets returns card CVV, card PIN and cardholder name
// candidates for text. Each value is emitted only when an explicit local
// context or marker immediately precedes it. Offsets are UTF-8 byte offsets,
// start inclusive and end exclusive, covering only the value span. Candidates
// are returned deterministically in document order with exact duplicate
// removal. No plaintext is stored on candidates.
func DetectPaymentSecrets(text string) []detection.Candidate {
	var out []detection.Candidate
	out = append(out, cvvCandidates(text)...)
	out = append(out, pinCandidates(text)...)
	out = append(out, cardholderCandidates(text)...)

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

// cvvCandidates finds three-digit runs and keeps only those with an explicit
// CVV context immediately before and not embedded in a longer digit run.
func cvvCandidates(text string) []detection.Candidate {
	var out []detection.Candidate
	for _, loc := range cvvRe.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		if !digitBoundaryOK(text, start, end) {
			continue
		}
		if !contextBefore(text, start, cvvContexts) {
			continue
		}
		out = append(out, validatedCandidate(detection.TypeCardCVV, start, end))
	}
	return out
}

// pinCandidates finds four-digit runs and keeps only those with an explicit
// PIN context immediately before and not embedded in a longer digit run.
func pinCandidates(text string) []detection.Candidate {
	var out []detection.Candidate
	for _, loc := range pinRe.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		if !digitBoundaryOK(text, start, end) {
			continue
		}
		if !contextBefore(text, start, pinContexts) {
			continue
		}
		out = append(out, validatedCandidate(detection.TypeCardPIN, start, end))
	}
	return out
}

// cardholderCandidates finds marker-based cardholder names and emits the value
// span, not the marker. The value is trimmed at the first known payment marker,
// must retain 2-4 name words with name-like casing for 3-4 word forms, and must
// be terminated by a right boundary (end-of-input, punctuation/newline, or a
// known payment marker).
func cardholderCandidates(text string) []detection.Candidate {
	var out []detection.Candidate
	for _, loc := range cardholderRe.FindAllStringSubmatchIndex(text, -1) {
		if !markerBoundaryOK(text, loc[0]) {
			continue
		}
		start, end := loc[2], loc[3]
		if start < 0 {
			continue
		}
		end, words := trimCardholderValue(text, start, end)
		if !cardholderNameOK(text, start, end, words) {
			continue
		}
		if !valueTerminatedOK(text, end, cardholderStopMarkers) {
			continue
		}
		out = append(out, validatedCandidate(detection.TypeCardholderName, start, end))
	}
	return out
}

// trimCardholderValue trims the value span so it stops at the first known
// payment marker word, returning the trimmed end offset and the number of name
// words collected. Trailing spaces or tabs are removed from the resulting end so
// the value span is exact.
func trimCardholderValue(text string, start, end int) (int, int) {
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
		if cardholderStopMarkers[strings.ToLower(text[i:j])] {
			end = i
			break
		}
		words++
		i = j
	}
	for end > start && (text[end-1] == ' ' || text[end-1] == '\t') {
		end--
	}
	return end, words
}

// cardholderNameOK reports whether the trimmed cardholder value is acceptable:
// exactly two words in any casing, or 3-4 words where every word uses name-like
// casing (Title or UPPER). Fully lowercase 3-4 word phrases are rejected so an
// arbitrary prose phrase is not masked as a name.
func cardholderNameOK(text string, start, end, words int) bool {
	if words < 2 || words > 4 {
		return false
	}
	if words == 2 {
		return true
	}
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
		if !isNameLikeWord(text[i:j]) {
			return false
		}
		i = j
	}
	return true
}

// isNameLikeWord reports whether a cardholder name word uses name-like casing:
// all uppercase, or title case (first letter uppercase, rest lowercase), applied
// to each hyphen-separated part.
func isNameLikeWord(word string) bool {
	for _, part := range strings.Split(word, "-") {
		if part == "" {
			continue
		}
		runes := []rune(part)
		allUpper := true
		for _, r := range runes {
			if !unicode.IsUpper(r) {
				allUpper = false
				break
			}
		}
		if allUpper {
			continue
		}
		if !unicode.IsUpper(runes[0]) {
			return false
		}
		for _, r := range runes[1:] {
			if !unicode.IsLower(r) {
				return false
			}
		}
	}
	return true
}
