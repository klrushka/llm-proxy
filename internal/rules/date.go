package rules

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/klrushka/llm-proxy/internal/detection"
)

// TypeDate is the intermediate, non-canonical DATE type emitted by the date
// rule for later contextual classification (task 6.2). It is deliberately not
// part of the 28 canonical registry/defaultTypes.
const TypeDate detection.Type = "DATE"

// numericDateRe matches the supported numeric date forms: DD.MM.YYYY,
// DD-MM-YYYY, DD/MM/YYYY, MM.DD.YYYY and the year-first forms YYYY.MM.DD,
// YYYY-DD-MM, YYYY/DD/MM. The span is the full date token.
var numericDateRe = regexp.MustCompile(`\d{2}[./-]\d{2}[./-]\d{4}|\d{4}[./-]\d{2}[./-]\d{2}`)

// textualDateRe matches a Russian textual date "D <month> YYYY" with an
// optional trailing " года". The month is a genitive Russian month name,
// matched case-insensitively. The span deliberately includes the optional
// " года" suffix when present.
var textualDateRe = regexp.MustCompile(`(?i)\d{1,2}\s+(января|февраля|марта|апреля|мая|июня|июля|августа|сентября|октября|ноября|декабря)\s+\d{4}(\s+года)?`)

// russianMonths maps a lowercase genitive Russian month name to its 1-based
// month number.
var russianMonths = map[string]int{
	"января":   1,
	"февраля":  2,
	"марта":    3,
	"апреля":   4,
	"мая":      5,
	"июня":     6,
	"июля":     7,
	"августа":  8,
	"сентября": 9,
	"октября":  10,
	"ноября":   11,
	"декабря":  12,
}

// DetectDates returns intermediate DATE candidates for text. Only calendar-valid
// dates are emitted: numeric forms must use a single consistent separator and
// pass real calendar validation (month lengths and leap years); textual forms
// must use a supported genitive Russian month. Offsets are UTF-8 byte offsets,
// start inclusive and end exclusive, preserving the original span. Candidates
// are returned deterministically in document order with no duplicates and no
// plaintext stored on them.
func DetectDates(text string) []detection.Candidate {
	var out []detection.Candidate
	for _, loc := range numericDateRe.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		if !dateBoundaryOK(text, start, end) {
			continue
		}
		if !validNumericDate(text[start:end]) {
			continue
		}
		out = append(out, validatedCandidate(TypeDate, start, end))
	}
	for _, loc := range textualDateRe.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		if !dateBoundaryOK(text, start, end) {
			continue
		}
		if !validTextualDate(text[start:end]) {
			continue
		}
		out = append(out, validatedCandidate(TypeDate, start, end))
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
	return dedupeCandidates(out)
}

// validNumericDate reports whether s is a calendar-valid date in at least one
// of the supported numeric orders: DD.MM.YYYY, MM.DD.YYYY, YYYY.MM.DD or
// YYYY.DD.MM. Mixed separators are rejected by requiring a single consistent
// separator across the whole token.
func validNumericDate(s string) bool {
	var sep byte
	switch {
	case strings.Contains(s, "."):
		sep = '.'
	case strings.Contains(s, "-"):
		sep = '-'
	case strings.Contains(s, "/"):
		sep = '/'
	default:
		return false
	}

	if strings.Count(s, string(sep)) != 2 {
		return false
	}
	parts := strings.Split(s, string(sep))
	if len(parts) != 3 {
		return false
	}

	a, b, c := atoi(parts[0]), atoi(parts[1]), atoi(parts[2])
	// Accept if the token is calendar-valid in any allowed order.
	return validCalendarDate(c, b, a) || // DD.MM.YYYY
		validCalendarDate(c, a, b) || // MM.DD.YYYY
		validCalendarDate(a, b, c) || // YYYY.MM.DD
		validCalendarDate(a, c, b) // YYYY.DD.MM
}

// validTextualDate reports whether s is a calendar-valid Russian textual date
// "D <month> YYYY" with an optional trailing " года".
func validTextualDate(s string) bool {
	lower := strings.ToLower(strings.TrimSpace(s))
	lower = strings.TrimSuffix(lower, " года")
	fields := strings.Fields(lower)
	if len(fields) != 3 {
		return false
	}
	day, err := strconv.Atoi(fields[0])
	if err != nil {
		return false
	}
	month, ok := russianMonths[fields[1]]
	if !ok {
		return false
	}
	year, err := strconv.Atoi(fields[2])
	if err != nil {
		return false
	}
	return validCalendarDate(year, month, day)
}

// validCalendarDate reports whether year/month/day form a real calendar date,
// honoring month lengths and leap years.
func validCalendarDate(year, month, day int) bool {
	if month < 1 || month > 12 || day < 1 || year < 1 {
		return false
	}
	daysInMonth := time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
	return day <= daysInMonth
}

// dateBoundaryOK rejects a date token embedded in a longer token, which covers
// malformed and embedded forms. A date is rejected when an adjacent rune is a
// Unicode letter or digit, or a date separator that connects to a longer run.
// Unicode punctuation (quotes, dashes) is allowed.
func dateBoundaryOK(text string, start, end int) bool {
	if start > 0 {
		r, _ := utf8.DecodeLastRuneInString(text[:start])
		if unicode.IsLetter(r) || unicode.IsDigit(r) || isDateSeparatorRune(r) {
			return false
		}
	}
	if end < len(text) {
		r, _ := utf8.DecodeRuneInString(text[end:])
		if unicode.IsLetter(r) || unicode.IsDigit(r) || isDateSeparatorRune(r) {
			return false
		}
	}
	return true
}

// isDateSeparatorRune reports whether r is a date separator that can connect a
// date token to a longer run.
func isDateSeparatorRune(r rune) bool {
	return r == '.' || r == '-' || r == '/'
}

// atoi parses a non-negative decimal integer, returning 0 on any error.
func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// dedupeCandidates removes duplicate candidates sharing the same Type, Start
// and End while preserving order. Candidate is not comparable (Sources is a
// slice), so identity is compared by its scalar fields.
func dedupeCandidates(in []detection.Candidate) []detection.Candidate {
	if len(in) < 2 {
		return in
	}
	out := in[:0]
	for _, c := range in {
		if len(out) > 0 && sameSpan(out[len(out)-1], c) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// sameSpan reports whether two candidates share the same Type, Start and End.
func sameSpan(a, b detection.Candidate) bool {
	return a.Type == b.Type && a.Start == b.Start && a.End == b.End
}
