package rules

import (
	"regexp"
	"sort"
	"strings"

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
var cvvContexts = []string{"cvv", "cvc", "код безопасности"}

// pinContexts are the explicit local context words that classify a four-digit
// run as a card PIN. They are stored lowercase; matching is case-insensitive.
var pinContexts = []string{"pin", "пин", "пин-код"}

// cardholderNameWord matches a single personal-name word in Russian or Latin,
// in title case or all caps, with an optional internal hyphen. Title case
// requires an uppercase letter followed by at least one lowercase letter; all
// caps requires at least two uppercase letters. Lowercase prose, mixed-case
// words and single-letter initials are intentionally not matched.
const cardholderNameWord = `(?:[А-ЯЁ]{2,}(?:-[А-ЯЁ]{2,})*|[А-ЯЁ][а-яё]+(?:-[А-ЯЁ][а-яё]+)*|[A-Z]{2,}(?:-[A-Z]{2,})*|[A-Z][a-z]+(?:-[A-Z][a-z]+)*)`

// cardholderNameValue matches a deliberately small 2-4 word personal-name
// grammar, words separated by single spaces or tabs.
const cardholderNameValue = cardholderNameWord + `(?:[ \t]+` + cardholderNameWord + `){1,3}`

// cardholderRe matches an explicit cardholder marker followed by a name value.
// The marker is matched case-insensitively; the name value is matched
// case-sensitively so lowercase prose is rejected. The value span is captured.
var cardholderRe = regexp.MustCompile(`(?i)(?:cardholder name|cardholder|держатель карты|имя держателя|владелец карты|имя на карте)(?-i)[ \t:]*` + `(` + cardholderNameValue + `)`)

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
// span, not the marker. The value is trimmed at the first known payment marker
// and must retain 2-4 name words.
func cardholderCandidates(text string) []detection.Candidate {
	var out []detection.Candidate
	for _, loc := range cardholderRe.FindAllStringSubmatchIndex(text, -1) {
		if loc[0] > 0 && isLetterByte(text[loc[0]-1]) {
			continue
		}
		start, end := loc[2], loc[3]
		if start < 0 {
			continue
		}
		end, ok := trimCardholderValue(text, start, end)
		if !ok {
			continue
		}
		out = append(out, validatedCandidate(detection.TypeCardholderName, start, end))
	}
	return out
}

// trimCardholderValue trims the value span so it stops at the first known
// payment marker word, returning the trimmed end offset and whether 2-4 name
// words remain. Trailing spaces or tabs are removed from the resulting end so
// the value span is exact.
func trimCardholderValue(text string, start, end int) (int, bool) {
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
	return end, words >= 2 && words <= 4
}

// isLetterByte reports whether b is an ASCII letter or a byte of a multi-byte
// (Cyrillic) UTF-8 character. Used only for marker boundary rejection.
func isLetterByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
}
