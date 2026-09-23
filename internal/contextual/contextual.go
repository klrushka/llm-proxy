// Package contextual performs deterministic contextual classification of
// intermediate DATE and LOCATION detection candidates into canonical PII types
// when sufficient bounded local context is present, and builds one ADDRESS
// candidate from a coherent explicit address block. It holds no plaintext and
// makes no ownership decisions; it only re-types and drops candidates.
package contextual

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/klrushka/llm-proxy/internal/detection"
)

// Intermediate, non-canonical candidate types consumed by Classify. They are
// deliberately not part of the canonical registry.
const (
	typeDate     detection.Type = "DATE"
	typeLocation detection.Type = "LOCATION"
)

// contextWindow is the bounded rune-counted window before a candidate within
// which a qualifying context phrase must appear. It never crosses a newline.
const contextWindow = 40

// addressContextWindow is the bounded rune-counted window before an address
// block within which an explicit address context phrase must appear.
const addressContextWindow = 60

// dateContexts maps a DATE context phrase to the canonical type it promotes to.
// passportContext marks phrases that additionally require a confirmed local
// passport context word before they apply.
var dateContexts = []struct {
	phrase          string
	typ             detection.Type
	passportContext bool
}{
	{"дата рождения", detection.TypeBirthDate, false},
	{"родился", detection.TypeBirthDate, false},
	{"родилась", detection.TypeBirthDate, false},
	{"дата выдачи", detection.TypePassportIssueDate, false},
	{"паспорт выдан", detection.TypePassportIssueDate, false},
	{"выдан", detection.TypePassportIssueDate, true},
}

// locationContexts maps a LOCATION context phrase to the canonical type it
// promotes to.
var locationContexts = []struct {
	phrase string
	typ    detection.Type
}{
	{"место рождения", detection.TypeBirthPlace},
	{"родился в", detection.TypeBirthPlace},
	{"родилась в", detection.TypeBirthPlace},
	{"адрес регистрации", detection.TypeAddress},
	{"адрес проживания", detection.TypeAddress},
	{"адрес", detection.TypeAddress},
	{"проживает", detection.TypeAddress},
	{"зарегистрирован", detection.TypeAddress},
}

// passportContextWords are the local context words that confirm a passport
// context for the bare "выдан" DATE phrase.
var passportContextWords = []string{"паспорт"}

// negationWords are the words that negate a context phrase.
var negationWords = map[string]bool{"не": true, "никогда": true}

// auxiliaryVerbs are the auxiliary verbs allowed between a negation word and a
// negated phrase (e.g. "не был выдан", "никогда не был зарегистрирован").
var auxiliaryVerbs = map[string]bool{
	"был": true, "была": true, "были": true, "было": true, "быть": true,
}

// clauseSeparators are the characters that end a clause for the passport
// context check.
func isClauseSeparator(b byte) bool {
	return b == ';' || b == '.' || b == '!' || b == '?' || b == '\n'
}

// addressContextPhrases are the explicit address context phrases that qualify
// an address block for ADDRESS construction.
var addressContextPhrases = []string{
	"адрес регистрации",
	"адрес проживания",
	"адрес",
	"проживает",
	"зарегистрирован",
}

// connectiveWords are the address marker words allowed between a qualifying
// context phrase and the candidate value.
var connectiveWords = map[string]bool{
	"г": true, "город": true, "городе": true, "города": true,
	"ул": true, "улица": true,
	"проспект": true, "пр-т": true, "д": true, "дом": true,
	"корп": true, "корпус": true, "стр": true, "строение": true,
	"кв": true, "квартира": true, "область": true, "край": true,
	"республика": true, "индекс": true, "в": true,
}

// canonicalSourceOrder is the deterministic order used for source unions.
var canonicalSourceOrder = []detection.Source{
	detection.SourceRubert,
	detection.SourceGliner,
	detection.SourceRegex,
	detection.SourceValidator,
}

// Classify returns a deterministic copy of candidates with intermediate DATE
// and LOCATION candidates promoted to canonical PII types only when sufficient
// bounded local context is present, and with one ADDRESS candidate built from a
// coherent explicit address block when address components are present. Caller
// input and its Sources slices are never mutated or aliased. Invalid candidates
// (negative, reversed, out-of-range or mid-rune offsets) are ignored. Exact
// duplicates are combined deterministically independent of input order.
// Ambiguous DATE and LOCATION candidates are dropped rather than leaked as
// canonical types.
func Classify(text string, candidates []detection.Candidate) []detection.Candidate {
	if len(candidates) == 0 {
		return nil
	}

	var out []detection.Candidate
	for _, c := range candidates {
		if !validCandidate(text, c) {
			continue
		}
		switch c.Type {
		case typeDate:
			if t, ok := classifyDate(text, c); ok {
				out = append(out, promoted(c, t))
			}
		case typeLocation:
			if t, ok := classifyLocation(text, c); ok {
				out = append(out, promoted(c, t))
			}
		default:
			out = append(out, copyCandidate(c))
		}
	}

	blocks := buildAddressBlocks(text, candidates)
	out = dropContainedAddresses(out, blocks)
	out = append(out, blocks...)

	out = mergeExactDuplicates(out)

	sort.SliceStable(out, func(i, j int) bool {
		return documentOrder(out[i], out[j])
	})
	return out
}

// validCandidate reports whether c has a valid span within text: 0 <= Start <
// End <= len(text) and both Start and End are UTF-8 rune boundaries.
func validCandidate(text string, c detection.Candidate) bool {
	if c.Start < 0 || c.End <= c.Start || c.End > len(text) {
		return false
	}
	return isRuneBoundary(text, c.Start) && isRuneBoundary(text, c.End)
}

// isRuneBoundary reports whether p is a UTF-8 rune boundary in text.
func isRuneBoundary(text string, p int) bool {
	if p < 0 || p > len(text) {
		return false
	}
	if p == 0 || p == len(text) {
		return true
	}
	return text[p]&0xC0 != 0x80
}

// classifyDate promotes a DATE candidate when a qualifying date context phrase
// applies within the bounded local region before it. Phrases marked with a
// passport-context requirement additionally need a confirmed local passport
// context word in the same clause.
func classifyDate(text string, c detection.Candidate) (detection.Type, bool) {
	for _, ctx := range dateContexts {
		if ctx.passportContext {
			if !contextAppliesInClause(text, c.Start, ctx.phrase, contextWindow) {
				continue
			}
			if !passportContextIn(text, c.Start) {
				continue
			}
			return ctx.typ, true
		}
		if !contextApplies(text, c.Start, ctx.phrase) {
			continue
		}
		return ctx.typ, true
	}
	return "", false
}

// passportContextIn reports whether a passport context word appears within the
// bounded local region before start in the same clause.
func passportContextIn(text string, start int) bool {
	region := clauseRegionIn(text, start, contextWindow)
	lower := strings.ToLower(region)
	for _, w := range passportContextWords {
		if lastPhraseIndex(lower, w) >= 0 {
			return true
		}
	}
	return false
}

// classifyLocation promotes a LOCATION candidate when a qualifying location
// context phrase applies within the bounded local region before it.
func classifyLocation(text string, c detection.Candidate) (detection.Type, bool) {
	for _, ctx := range locationContexts {
		if contextApplies(text, c.Start, ctx.phrase) {
			return ctx.typ, true
		}
	}
	return "", false
}

// contextApplies reports whether phrase appears within the bounded local region
// before start and the text between the phrase and start is connective.
func contextApplies(text string, start int, phrase string) bool {
	return contextAppliesIn(text, start, phrase, contextWindow)
}

// contextAppliesIn is contextApplies with an explicit window. Both phrase
// matching and connective checking run on a single lowercased region so indexes
// stay consistent with the string they index. A phrase preceded by an explicit
// negation word does not apply.
func contextAppliesIn(text string, start int, phrase string, window int) bool {
	return contextAppliesInRegion(contextRegionIn(text, start, window), phrase)
}

// contextAppliesInClause is contextAppliesIn bounded to the same clause: the
// region never crosses a clause separator or newline.
func contextAppliesInClause(text string, start int, phrase string, window int) bool {
	return contextAppliesInRegion(clauseRegionIn(text, start, window), phrase)
}

// contextAppliesInRegion reports whether phrase applies within region with word
// boundaries, not negated, and connective text after it.
func contextAppliesInRegion(region, phrase string) bool {
	lower := strings.ToLower(region)
	idx := lastPhraseIndex(lower, phrase)
	if idx < 0 {
		return false
	}
	if isNegated(lower, idx) {
		return false
	}
	phraseEnd := idx + len(phrase)
	return isConnective(lower, phraseEnd, len(lower))
}

// isNegated reports whether the phrase at phraseStart is negated by a negation
// word within the same clause, allowing only auxiliary verbs between the
// negation word and the phrase. lower must be lowercased. Scanning never
// crosses a clause separator or newline.
func isNegated(lower string, phraseStart int) bool {
	i := phraseStart
	for {
		for i > 0 && (lower[i-1] == ' ' || lower[i-1] == '\t') {
			i--
		}
		if i == 0 {
			return false
		}
		if isClauseSeparator(lower[i-1]) {
			return false
		}
		j := i
		for j > 0 && isWordByte(lower[j-1]) {
			j--
		}
		word := lower[j:i]
		if negationWords[word] {
			return true
		}
		if !auxiliaryVerbs[word] {
			return false
		}
		i = j
	}
}

// clauseRegionIn returns the bounded window before start within the same
// clause, never crossing a clause separator or newline, and always beginning on
// a UTF-8 rune boundary.
func clauseRegionIn(text string, start, window int) string {
	if start < 0 || start > len(text) {
		return ""
	}
	clauseStart := start
	for clauseStart > 0 && !isClauseSeparator(text[clauseStart-1]) {
		clauseStart--
	}
	lo := start
	for i := 0; i < window && lo > clauseStart; i++ {
		_, size := utf8.DecodeLastRuneInString(text[:lo])
		lo -= size
	}
	return text[lo:start]
}

// contextRegionIn returns the bounded window before start, counting window
// runes back from start, never crossing a newline, and always beginning on a
// UTF-8 rune boundary.
func contextRegionIn(text string, start, window int) string {
	if start < 0 || start > len(text) {
		return ""
	}
	lineStart := start
	for lineStart > 0 && text[lineStart-1] != '\n' {
		lineStart--
	}
	lo := start
	for i := 0; i < window && lo > lineStart; i++ {
		_, size := utf8.DecodeLastRuneInString(text[:lo])
		lo -= size
	}
	return text[lo:start]
}

// lastPhraseIndex returns the byte index of the last occurrence of phrase in s
// with word boundaries on both sides, or -1 if absent. s must already be
// lowercased to match phrase.
func lastPhraseIndex(s, phrase string) int {
	for i := len(s) - len(phrase); i >= 0; i-- {
		if s[i:i+len(phrase)] != phrase {
			continue
		}
		if i > 0 && isWordByte(s[i-1]) {
			continue
		}
		if i+len(phrase) < len(s) && isWordByte(s[i+len(phrase)]) {
			continue
		}
		return i
	}
	return -1
}

// isConnective reports whether the substring [start,end) contains only
// whitespace, punctuation and connective address marker words. s must already
// be lowercased to match connectiveWords.
func isConnective(s string, start, end int) bool {
	i := start
	for i < end {
		b := s[i]
		if b == ' ' || b == '\t' || b == '\n' || b == ',' || b == ':' || b == ';' || b == '.' {
			i++
			continue
		}
		j := i
		for j < end && !isConnectiveDelim(s[j]) {
			j++
		}
		if !connectiveWords[s[i:j]] {
			return false
		}
		i = j
	}
	return true
}

// isConnectiveDelim reports whether b is a delimiter that ends a connective
// word token.
func isConnectiveDelim(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == ',' || b == ':' || b == ';' || b == '.'
}

// isWordByte reports whether b can continue a word: a Latin letter, digit, or
// a Cyrillic UTF-8 byte (>= 0x80).
func isWordByte(b byte) bool {
	if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' {
		return true
	}
	return b >= 0x80
}

// buildAddressBlocks builds one ADDRESS candidate per coherent explicit address
// block: valid address component candidates on the same line with explicit
// address context before the block. The ADDRESS spans the first to last
// component.
func buildAddressBlocks(text string, candidates []detection.Candidate) []detection.Candidate {
	var comps []detection.Candidate
	for _, c := range candidates {
		if !validCandidate(text, c) {
			continue
		}
		if isAddressComponent(c.Type) {
			comps = append(comps, c)
		}
	}
	if len(comps) == 0 {
		return nil
	}

	var blocks []detection.Candidate
	for _, line := range groupByLine(text, comps) {
		if len(line) == 0 || !hasAddressContext(text, line) {
			continue
		}
		start, end := line[0].Start, line[0].End
		conf := line[0].Confidence
		var sources []detection.Source
		for _, c := range line {
			if c.Start < start {
				start = c.Start
			}
			if c.End > end {
				end = c.End
			}
			if c.Confidence > conf {
				conf = c.Confidence
			}
			sources = append(sources, c.Sources...)
		}
		blocks = append(blocks, detection.Candidate{
			Type:       detection.TypeAddress,
			Start:      start,
			End:        end,
			Confidence: conf,
			Sources:    unionSources(sources),
		})
	}
	return blocks
}

// hasAddressContext reports whether an explicit address context phrase applies
// within the bounded window before the first component of line.
func hasAddressContext(text string, line []detection.Candidate) bool {
	first := line[0].Start
	for _, phrase := range addressContextPhrases {
		if contextAppliesIn(text, first, phrase, addressContextWindow) {
			return true
		}
	}
	return false
}

// groupByLine groups address components by their line start offset, preserving
// document order within each line.
func groupByLine(text string, comps []detection.Candidate) [][]detection.Candidate {
	sorted := append([]detection.Candidate(nil), comps...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })

	var lines [][]detection.Candidate
	var cur []detection.Candidate
	curLine := lineStartOf(text, sorted[0].Start)
	for _, c := range sorted {
		ls := lineStartOf(text, c.Start)
		if ls != curLine {
			lines = append(lines, cur)
			cur = nil
			curLine = ls
		}
		cur = append(cur, c)
	}
	if len(cur) > 0 {
		lines = append(lines, cur)
	}
	return lines
}

// lineStartOf returns the byte offset of the start of the line containing pos.
func lineStartOf(text string, pos int) int {
	for pos > 0 && text[pos-1] != '\n' {
		pos--
	}
	return pos
}

// isAddressComponent reports whether t is a canonical address component type.
func isAddressComponent(t detection.Type) bool {
	switch t {
	case detection.TypeAddressCountry,
		detection.TypeAddressPostalCode,
		detection.TypeAddressRegion,
		detection.TypeAddressCity,
		detection.TypeAddressStreet,
		detection.TypeAddressHouse,
		detection.TypeAddressBuilding,
		detection.TypeAddressApartment:
		return true
	}
	return false
}

// dropContainedAddresses removes promoted ADDRESS candidates fully contained
// within a built ADDRESS block so each block yields exactly one ADDRESS.
func dropContainedAddresses(out, blocks []detection.Candidate) []detection.Candidate {
	if len(blocks) == 0 {
		return out
	}
	var kept []detection.Candidate
	for _, c := range out {
		if c.Type != detection.TypeAddress {
			kept = append(kept, c)
			continue
		}
		contained := false
		for _, b := range blocks {
			if b.Start <= c.Start && c.End <= b.End {
				contained = true
				break
			}
		}
		if !contained {
			kept = append(kept, c)
		}
	}
	return kept
}

// dupKey identifies an exact duplicate by Type plus span.
type dupKey struct {
	t     detection.Type
	start int
	end   int
}

// mergeExactDuplicates combines candidates sharing Type+Start+End into one
// candidate with the maximum confidence and a deterministic canonical
// de-duplicated union of sources, independent of input order. Returned
// candidates own their Sources slices.
func mergeExactDuplicates(in []detection.Candidate) []detection.Candidate {
	if len(in) < 2 {
		return in
	}
	groups := make(map[dupKey]*detection.Candidate)
	var order []dupKey
	for _, c := range in {
		k := dupKey{c.Type, c.Start, c.End}
		g, ok := groups[k]
		if !ok {
			cp := copyCandidate(c)
			groups[k] = &cp
			order = append(order, k)
			continue
		}
		if c.Confidence > g.Confidence {
			g.Confidence = c.Confidence
		}
		merged := append(append([]detection.Source(nil), g.Sources...), c.Sources...)
		g.Sources = unionSources(merged)
	}
	out := make([]detection.Candidate, 0, len(order))
	for _, k := range order {
		out = append(out, *groups[k])
	}
	return out
}

// promoted returns a defensive copy of c with its type replaced by t.
func promoted(c detection.Candidate, t detection.Type) detection.Candidate {
	out := copyCandidate(c)
	out.Type = t
	return out
}

// copyCandidate returns a defensive copy of c with its own Sources slice.
func copyCandidate(c detection.Candidate) detection.Candidate {
	out := c
	out.Sources = append([]detection.Source(nil), c.Sources...)
	return out
}

// unionSources returns the deterministic de-duplicated union of sources in
// canonical order.
func unionSources(sources []detection.Source) []detection.Source {
	present := make(map[detection.Source]bool)
	for _, s := range sources {
		present[s] = true
	}
	var out []detection.Source
	for _, s := range canonicalSourceOrder {
		if present[s] {
			out = append(out, s)
		}
	}
	return out
}

// documentOrder sorts by Start, then End, then Type.
func documentOrder(a, b detection.Candidate) bool {
	if a.Start != b.Start {
		return a.Start < b.Start
	}
	if a.End != b.End {
		return a.End < b.End
	}
	return a.Type < b.Type
}
