// Package rules implements deterministic Go-side regex candidates for PII
// types. It holds no plaintext values and makes no privacy decisions; it only
// turns regex matches into detection.Candidate values with UTF-8 byte offsets.
package rules

import (
	"regexp"
	"sort"

	"github.com/klrushka/llm-proxy/internal/detection"
)

// emailRe matches an ASCII email address with a dot-TLD. It is intentionally
// permissive about the local part and domain labels; the surrounding boundary
// checks reject broken forms and substring matches inside larger tokens.
var emailRe = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)

// phoneRe matches a Russian phone with a +7 or domestic 8 prefix and exactly
// ten following digits. Separators may be spaces, hyphens, or balanced
// parentheses around the three-digit operator/area code. The corpus form
// "+7 900 123-45-67" is covered.
var phoneRe = regexp.MustCompile(`(\+7|8)[ \t-]*\(?[0-9]{3}\)?[ \t-]*[0-9]{3}[ \t-]*[0-9]{2}[ \t-]*[0-9]{2}`)

// DetectContacts returns email and phone candidates for text. Offsets are
// UTF-8 byte offsets, start inclusive and end exclusive. Candidates are
// returned deterministically in document order, with Type as a stable
// tie-breaker only. No plaintext is stored on candidates.
func DetectContacts(text string) []detection.Candidate {
	var out []detection.Candidate
	out = append(out, emailCandidates(text)...)
	out = append(out, phoneCandidates(text)...)

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

// emailCandidates finds email matches and rejects broken forms and substring
// matches embedded in a larger email-like token.
func emailCandidates(text string) []detection.Candidate {
	var out []detection.Candidate
	for _, loc := range emailRe.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		if !emailBoundaryOK(text, start, end) {
			continue
		}
		out = append(out, candidate(detection.TypeEmail, start, end))
	}
	return out
}

// emailBoundaryOK rejects a match whose immediate neighbors extend the token
// (substring inside a larger email-like identifier) and whose local part or
// domain contains an obviously broken form. A single terminal period after the
// match is treated as sentence punctuation and excluded from the span; a dot
// followed by another email-token byte is a broken continuation and rejected.
func emailBoundaryOK(text string, start, end int) bool {
	if start > 0 && isEmailTokenByte(text[start-1]) {
		return false
	}
	if end < len(text) {
		b := text[end]
		if isEmailTokenByte(b) {
			if b == '.' && (end+1 >= len(text) || !isEmailTokenByte(text[end+1])) {
				// single terminal period: sentence punctuation, excluded
			} else {
				return false
			}
		}
	}

	at := indexByte(text[start:end], '@')
	if at < 0 {
		return false
	}
	local := text[start : start+at]
	domain := text[start+at+1 : end]

	if local == "" || domain == "" {
		return false
	}
	if !validEmailLabel(local) {
		return false
	}
	for _, label := range splitDomain(domain) {
		if !validEmailLabel(label) {
			return false
		}
	}
	return true
}

// isEmailTokenByte reports whether b is a byte that can continue an email
// token. Used only for boundary rejection, not for parsing.
func isEmailTokenByte(b byte) bool {
	return b >= 'a' && b <= 'z' ||
		b >= 'A' && b <= 'Z' ||
		b >= '0' && b <= '9' ||
		b == '.' || b == '_' || b == '%' || b == '+' || b == '-'
}

// validEmailLabel rejects empty labels, labels with consecutive dots, and
// labels beginning or ending with '-'.
func validEmailLabel(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			if i == 0 || i == len(s)-1 || s[i-1] == '.' {
				return false
			}
		}
	}
	return true
}

// splitDomain splits a domain on '.' into its labels.
func splitDomain(domain string) []string {
	var labels []string
	start := 0
	for i := 0; i <= len(domain); i++ {
		if i == len(domain) || domain[i] == '.' {
			labels = append(labels, domain[start:i])
			start = i + 1
		}
	}
	return labels
}

// phoneCandidates finds phone matches and rejects unbalanced parentheses,
// letters inside, and matches embedded in a longer digit run.
func phoneCandidates(text string) []detection.Candidate {
	var out []detection.Candidate
	for _, loc := range phoneRe.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		if !phoneBoundaryOK(text, start, end) {
			continue
		}
		out = append(out, candidate(detection.TypePhone, start, end))
	}
	return out
}

// phoneBoundaryOK rejects a match embedded in a longer digit run and a match
// with unbalanced parentheses.
func phoneBoundaryOK(text string, start, end int) bool {
	if start > 0 && isPhoneTokenByte(text[start-1]) {
		return false
	}
	if end < len(text) && isPhoneTokenByte(text[end]) {
		return false
	}

	open := 0
	for i := start; i < end; i++ {
		switch text[i] {
		case '(':
			open++
		case ')':
			open--
			if open < 0 {
				return false
			}
		}
	}
	return open == 0
}

// isPhoneTokenByte reports whether b can continue a phone token. Used only
// for boundary rejection, not for parsing.
func isPhoneTokenByte(b byte) bool {
	return b >= '0' && b <= '9' || b == '+' || b == '-' || b == '(' || b == ')'
}

// candidate builds a deterministic regex candidate.
func candidate(t detection.Type, start, end int) detection.Candidate {
	return detection.Candidate{
		Type:       t,
		Start:      start,
		End:        end,
		Confidence: 1.0,
		Sources:    []detection.Source{detection.SourceRegex},
	}
}

// indexByte returns the index of b in s, or -1.
func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
