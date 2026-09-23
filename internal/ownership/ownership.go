// Package ownership deterministically decides whether each merged top-level
// entity is personal data of a physical person, belongs to an organization, is
// a public/literary mention, or is ambiguous. It preserves one merge.Entity per
// result, holds no plaintext, and never mutates or aliases caller input.
//
// Context boundary: a coherent block is a newline-delimited paragraph. Local
// context markers must appear within a bounded rune window (contextWindow)
// around the entity inside the same paragraph; words in an unrelated paragraph
// never classify an entity. Block-level co-occurrence (a name plus a linked
// identifier in the same paragraph) is evaluated on the whole paragraph.
//
// Precedence (highest wins): explicit organization context, then explicit
// public/literary context, then positive physical-person context, then field
// labels, then block-level name/identifier co-occurrence, then ambiguous.
package ownership

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/merge"
	"github.com/klrushka/llm-proxy/internal/policy"
)

// OwnerType is the stable classification of who owns an entity.
type OwnerType string

// Stable owner types. These exact values are the public contract.
const (
	OwnerTypePerson       OwnerType = "PERSON"
	OwnerTypeOrganization OwnerType = "ORGANIZATION"
	OwnerTypePublic       OwnerType = "PUBLIC"
	OwnerTypeUnknown      OwnerType = "UNKNOWN"
)

// ReasonCode is a stable, exported reason explaining an ownership decision.
type ReasonCode string

// Stable reason codes used by tests and callers.
const (
	ReasonClientContext       ReasonCode = "client_context"
	ReasonPassportContext     ReasonCode = "passport_context"
	ReasonPositiveContext     ReasonCode = "positive_context"
	ReasonLinkedEntities      ReasonCode = "linked_entities"
	ReasonFieldLabel          ReasonCode = "field_label"
	ReasonOrganizationContext ReasonCode = "organization_context"
	ReasonPublicContext       ReasonCode = "public_context"
	ReasonAmbiguous           ReasonCode = "ambiguous_context"
	// ReasonTypeDisabledByPolicy explains that a personal entity type is
	// excluded by the processing policy and therefore not tokenized.
	ReasonTypeDisabledByPolicy ReasonCode = "type_disabled_by_policy"
)

// Entity is one ownership result. It embeds the merged entity unchanged (with
// defensive copies) and adds ownership metadata. It never carries plaintext.
type Entity struct {
	merge.Entity
	Personal          bool
	OwnerType         OwnerType
	OwnerID           string
	OwnershipScore    float64
	ReasonCodes       []ReasonCode
	ReviewRecommended bool
}

// contextWindow is the bounded rune-counted window around an entity within
// which a local context marker must appear. It never crosses a newline.
const contextWindow = 60

// Deterministic scores for each decision path, bounded to 0..1.
const (
	scoreStrongPositive = 0.9
	scoreCooccurrence   = 0.8
	scoreFieldLabel     = 0.7
	scoreNegative       = 0.9
	scoreAmbiguous      = 0.3
)

// reasonScore maps a positive reason to its contribution to OwnershipScore.
var reasonScore = map[ReasonCode]float64{
	ReasonClientContext:   scoreStrongPositive,
	ReasonPassportContext: scoreStrongPositive,
	ReasonPositiveContext: scoreStrongPositive,
	ReasonLinkedEntities:  scoreCooccurrence,
	ReasonFieldLabel:      scoreFieldLabel,
}

// reasonOrder is the canonical deterministic order for reason codes.
var reasonOrder = []ReasonCode{
	ReasonClientContext,
	ReasonPassportContext,
	ReasonPositiveContext,
	ReasonLinkedEntities,
	ReasonFieldLabel,
	ReasonOrganizationContext,
	ReasonPublicContext,
	ReasonAmbiguous,
	ReasonTypeDisabledByPolicy,
}

// clientMarkers are positive physical-person role markers producing
// ReasonClientContext.
var clientMarkers = []string{
	"клиент", "заявитель", "заёмщик", "пользователь", "владелец",
	"гражданин", "фио",
}

// passportMarkers are positive passport markers producing ReasonPassportContext.
var passportMarkers = []string{
	"паспорт",
}

// positiveMarkers are strong physical-person markers producing
// ReasonPositiveContext. Classification-only markers (родился, проживает, etc.)
// are deliberately absent: they classify DATE/LOCATION/address components in the
// contextual stage but are not sufficient ownership evidence on their own.
var positiveMarkers = []string{
	"дата рождения", "адрес регистрации", "место рождения",
}

// negationWords are the words that negate a positive marker.
var negationWords = map[string]bool{"не": true, "никогда": true}

// auxiliaryVerbs are the auxiliary verbs allowed between a negation word and a
// negated phrase (e.g. "не был выдан", "никогда не был зарегистрирован").
var auxiliaryVerbs = map[string]bool{
	"был": true, "была": true, "были": true, "было": true, "быть": true,
}

// orgMarkers are explicit organization context phrases from the spec.
var orgMarkers = []string{
	"ооо", "ао", "пао", "банк", "филиал", "отделение",
	"юридический адрес", "инн организации",
}

// innOrgContexts are the narrow explicit organization contexts that keep an
// INN_PERSON candidate ORGANIZATION even though INN_PERSON is an explicit
// high-risk type. They are a strict subset of orgMarkers.
var innOrgContexts = []string{
	"инн организации", "инн юрлица",
}

// publicMarkers are explicit public/literary context phrases from the spec.
var publicMarkers = []string{
	"поэт", "писатель", "автор", "биография", "стихотворение",
}

// fieldLabels are conservative field labels that qualify a structured entity.
var fieldLabels = []string{
	"телефон", "email", "почта", "эл. почта",
}

// Assess returns one ownership result per valid merged top-level entity, in
// deterministic document order. Caller input and its component and source
// slices are never mutated or aliased. Invalid entities (negative, reversed,
// out-of-range or mid-rune offsets) are ignored. Results never carry plaintext.
func Assess(text string, entities []merge.Entity) []Entity {
	if len(entities) == 0 {
		return nil
	}

	type item struct {
		entity    merge.Entity
		lineStart int
		lineEnd   int
		valid     bool
	}
	items := make([]item, len(entities))
	for i, e := range entities {
		if !validEntity(text, e) {
			continue
		}
		ls, le := lineBounds(text, e.Start)
		items[i] = item{entity: copyEntity(e), lineStart: ls, lineEnd: le, valid: true}
	}

	blockHasName := make(map[int]bool)
	blockHasLinked := make(map[int]bool)
	for _, it := range items {
		if !it.valid {
			continue
		}
		if entityHasName(it.entity) {
			blockHasName[it.lineStart] = true
		}
		if entityHasLinked(it.entity) {
			blockHasLinked[it.lineStart] = true
		}
	}

	results := make([]Entity, 0, len(items))
	for _, it := range items {
		if !it.valid {
			continue
		}
		d := decide(text, it.entity, it.lineStart, it.lineEnd,
			blockHasName[it.lineStart], blockHasLinked[it.lineStart])
		results = append(results, Entity{
			Entity:            it.entity,
			Personal:          d.personal,
			OwnerType:         d.ownerType,
			OwnershipScore:    d.score,
			ReasonCodes:       d.reasons,
			ReviewRecommended: d.review,
		})
	}

	sort.SliceStable(results, func(i, j int) bool {
		return documentOrder(results[i].Candidate, results[j].Candidate)
	})
	assignOwnerIDs(text, results)
	return results
}

// ApplyPolicy returns a defensive copy of the Assess results with the consumer
// policy applied. For each entity Assess classified as personal, if the policy
// excludes the entity's canonical detection type, Personal is set to false and
// ReasonTypeDisabledByPolicy is added in deterministic reason order so
// downstream tokenization skips it. OwnerType, OwnerID and OwnershipScore are
// preserved; context is not reclassified. Non-personal (organization, public,
// ambiguous) entities are left unchanged. Caller input and the policy are never
// mutated or aliased.
func ApplyPolicy(results []Entity, p policy.Policy) []Entity {
	if len(results) == 0 {
		return nil
	}
	out := make([]Entity, len(results))
	for i, e := range results {
		out[i] = copyEntityResult(e)
		if !e.Personal {
			continue
		}
		if !p.AllowsType(string(e.Type)) {
			out[i].Personal = false
			out[i].ReasonCodes = dedupeSortReasons(append(out[i].ReasonCodes, ReasonTypeDisabledByPolicy))
		}
	}
	return out
}

// copyEntityResult returns a defensive deep copy of an ownership result with
// its own Sources, component and reason slices.
func copyEntityResult(e Entity) Entity {
	out := e
	out.Entity = copyEntity(e.Entity)
	out.ReasonCodes = append([]ReasonCode(nil), e.ReasonCodes...)
	return out
}

// decision is the computed ownership classification for one entity.
type decision struct {
	personal  bool
	ownerType OwnerType
	score     float64
	reasons   []ReasonCode
	review    bool
}

// decide classifies one entity using bounded local context and block-level
// co-occurrence. Organization and public negative context win over nearby
// positive-looking evidence, except that explicit high-risk types stay personal
// even when an organization or public marker appears nearby. The narrow
// INN-organization exception keeps an INN_PERSON candidate ORGANIZATION.
func decide(text string, e merge.Entity, lineStart, lineEnd int, blockHasName, blockHasLinked bool) decision {
	region := contextRegion(text, lineStart, lineEnd, e.Start, e.End)
	lower := strings.ToLower(region)

	// Explicit high-risk types must not become non-personal merely because of a
	// neighboring organization or public word (e.g. "банк", "отделение",
	// "поэт"); they remain personal and are masked. Non-high-risk entities
	// (addresses, names) still yield to organization/public negative context.
	// The narrow INN-organization exception keeps an INN_PERSON candidate
	// ORGANIZATION.
	highRisk := isExplicitHighRiskType(e.Type)

	if hasAnyPhrase(lower, innOrgContexts) {
		return decision{ownerType: OwnerTypeOrganization, score: scoreNegative,
			reasons: []ReasonCode{ReasonOrganizationContext}}
	}

	if !highRisk {
		if hasAnyPhrase(lower, orgMarkers) {
			return decision{ownerType: OwnerTypeOrganization, score: scoreNegative,
				reasons: []ReasonCode{ReasonOrganizationContext}}
		}
		if hasAnyPhrase(lower, publicMarkers) {
			return decision{ownerType: OwnerTypePublic, score: scoreNegative,
				reasons: []ReasonCode{ReasonPublicContext}}
		}
	}

	var reasons []ReasonCode
	if hasAnyPhrase(lower, clientMarkers) {
		reasons = append(reasons, ReasonClientContext)
	}
	if hasAnyPhrase(lower, passportMarkers) {
		reasons = append(reasons, ReasonPassportContext)
	}
	if hasAnyPhraseNotNegated(lower, positiveMarkers) {
		reasons = append(reasons, ReasonPositiveContext)
	}
	if isExplicitHighRiskType(e.Type) {
		reasons = append(reasons, ReasonFieldLabel)
	}
	if isStructuredType(e.Type) && hasAnyPhrase(lower, fieldLabels) {
		reasons = append(reasons, ReasonFieldLabel)
	}
	if (entityHasName(e) && blockHasLinked) || (entityHasLinked(e) && blockHasName) {
		reasons = append(reasons, ReasonLinkedEntities)
	}

	if len(reasons) > 0 {
		return decision{personal: true, ownerType: OwnerTypePerson,
			score: scoreFor(reasons), reasons: dedupeSortReasons(reasons)}
	}
	return decision{ownerType: OwnerTypeUnknown, score: scoreAmbiguous,
		reasons: []ReasonCode{ReasonAmbiguous}, review: true}
}

// assignOwnerIDs gives related personal entities in the same coherent block one
// shared deterministic person-N id, and related organization entities one shared
// organization-N id, in document order. Public and unknown entities keep empty
// ids. IDs are sequential by document order and never derived from plaintext,
// hashes, randomness, time or input order.
func assignOwnerIDs(text string, results []Entity) {
	personCounter := 0
	orgCounter := 0
	personAssigned := make(map[int]bool)
	orgAssigned := make(map[int]bool)

	for i := range results {
		r := &results[i]
		ls := lineStartOf(text, r.Start)
		switch r.OwnerType {
		case OwnerTypePerson:
			if personAssigned[ls] {
				continue
			}
			personCounter++
			id := fmt.Sprintf("person-%d", personCounter)
			for j := range results {
				if results[j].OwnerType == OwnerTypePerson && lineStartOf(text, results[j].Start) == ls {
					results[j].OwnerID = id
				}
			}
			personAssigned[ls] = true
		case OwnerTypeOrganization:
			if orgAssigned[ls] {
				continue
			}
			orgCounter++
			id := fmt.Sprintf("organization-%d", orgCounter)
			for j := range results {
				if results[j].OwnerType == OwnerTypeOrganization && lineStartOf(text, results[j].Start) == ls {
					results[j].OwnerID = id
				}
			}
			orgAssigned[ls] = true
		}
	}
}

// scoreFor returns the maximum deterministic score among the positive reasons.
func scoreFor(reasons []ReasonCode) float64 {
	score := 0.0
	for _, r := range reasons {
		if s := reasonScore[r]; s > score {
			score = s
		}
	}
	return score
}

// dedupeSortReasons returns reasons de-duplicated and ordered canonically.
func dedupeSortReasons(in []ReasonCode) []ReasonCode {
	present := make(map[ReasonCode]bool)
	for _, r := range in {
		present[r] = true
	}
	var out []ReasonCode
	for _, r := range reasonOrder {
		if present[r] {
			out = append(out, r)
		}
	}
	return out
}

// hasAnyPhrase reports whether any phrase appears in s with word boundaries.
func hasAnyPhrase(s string, phrases []string) bool {
	for _, p := range phrases {
		if phrasePresent(s, p) {
			return true
		}
	}
	return false
}

// hasAnyPhraseNotNegated reports whether any phrase appears in s with word
// boundaries and is not immediately negated by a negation word.
func hasAnyPhraseNotNegated(s string, phrases []string) bool {
	for _, p := range phrases {
		if phrasePresentNotNegated(s, p) {
			return true
		}
	}
	return false
}

// phrasePresentNotNegated reports whether phrase appears in s with word
// boundaries and is not immediately preceded by a negation word. s must already
// be lowercased to match phrase.
func phrasePresentNotNegated(s, phrase string) bool {
	for i := 0; i+len(phrase) <= len(s); i++ {
		if s[i:i+len(phrase)] != phrase {
			continue
		}
		if i > 0 && isWordByte(s[i-1]) {
			continue
		}
		if i+len(phrase) < len(s) && isWordByte(s[i+len(phrase)]) {
			continue
		}
		if isNegated(s, i) {
			continue
		}
		return true
	}
	return false
}

// isNegated reports whether the word immediately before phraseStart (allowing
// only whitespace) is a negation word. s must already be lowercased.
func isNegated(s string, phraseStart int) bool {
	i := phraseStart
	for {
		for i > 0 && (s[i-1] == ' ' || s[i-1] == '\t') {
			i--
		}
		if i == 0 {
			return false
		}
		if isClauseSeparator(s[i-1]) {
			return false
		}
		j := i
		for j > 0 && isWordByte(s[j-1]) {
			j--
		}
		word := s[j:i]
		if negationWords[word] {
			return true
		}
		if !auxiliaryVerbs[word] {
			return false
		}
		i = j
	}
}

// isClauseSeparator reports whether b ends a clause for negation scanning.
func isClauseSeparator(b byte) bool {
	return b == ';' || b == '.' || b == '!' || b == '?' || b == '\n'
}

// phrasePresent reports whether phrase appears in s with word boundaries on
// both sides. s must already be lowercased to match phrase.
func phrasePresent(s, phrase string) bool {
	for i := 0; i+len(phrase) <= len(s); i++ {
		if s[i:i+len(phrase)] != phrase {
			continue
		}
		if i > 0 && isWordByte(s[i-1]) {
			continue
		}
		if i+len(phrase) < len(s) && isWordByte(s[i+len(phrase)]) {
			continue
		}
		return true
	}
	return false
}

// isWordByte reports whether b can continue a word: a Latin letter, digit, or a
// Cyrillic UTF-8 byte (>= 0x80).
func isWordByte(b byte) bool {
	if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' {
		return true
	}
	return b >= 0x80
}

// isNameType reports whether t is a full name or a name component.
func isNameType(t detection.Type) bool {
	switch t {
	case detection.TypeFullName, detection.TypeFirstName,
		detection.TypeLastName, detection.TypeMiddleName:
		return true
	}
	return false
}

// entityHasName reports whether the entity's top-level type or any of its
// contained components is a name type.
func entityHasName(e merge.Entity) bool {
	if isNameType(e.Type) {
		return true
	}
	for _, c := range e.Components {
		if isNameType(c.Type) {
			return true
		}
	}
	return false
}

// entityHasLinked reports whether the entity's top-level type or any of its
// contained components is a linked identifier type.
func entityHasLinked(e merge.Entity) bool {
	if isLinkedType(e.Type) {
		return true
	}
	for _, c := range e.Components {
		if isLinkedType(c.Type) {
			return true
		}
	}
	return false
}

// isLinkedType reports whether t is an identifier that links to a name.
func isLinkedType(t detection.Type) bool {
	switch t {
	case detection.TypePassportNumber, detection.TypeForeignPassportNumber, detection.TypePhone,
		detection.TypeEmail, detection.TypeBirthDate:
		return true
	}
	return false
}

// isStructuredType reports whether t is a structured entity that a field label
// may qualify conservatively.
func isStructuredType(t detection.Type) bool {
	switch t {
	case detection.TypePhone, detection.TypeEmail:
		return true
	}
	return false
}

// isExplicitHighRiskType reports whether t is a canonical type whose detection
// is already structural, validator-backed or marker-based. Such an entity is
// personal without requiring name co-occurrence or a field label phrase: leaving
// it plaintext is unsafe. Organization and public negative context still win
// because they are evaluated before this path.
func isExplicitHighRiskType(t detection.Type) bool {
	switch t {
	case detection.TypeEmail, detection.TypePhone,
		detection.TypePassportNumber, detection.TypeForeignPassportNumber, detection.TypePassportDivisionCode,
		detection.TypePassportIssueDate, detection.TypePassportIssuer,
		detection.TypeDriverLicenseNumber, detection.TypeINNPerson,
		detection.TypeBankCardNumber, detection.TypeCardCVV,
		detection.TypeCardPIN, detection.TypeCardholderName,
		detection.TypeCitizenship:
		return true
	}
	return false
}

// validEntity reports whether e has a valid span within text.
func validEntity(text string, e merge.Entity) bool {
	if e.Start < 0 || e.End <= e.Start || e.End > len(text) {
		return false
	}
	return isRuneBoundary(text, e.Start) && isRuneBoundary(text, e.End)
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

// lineBounds returns the byte offsets of the start and end of the line
// containing pos. The end is exclusive and does not include the newline.
func lineBounds(text string, pos int) (int, int) {
	start := pos
	for start > 0 && text[start-1] != '\n' {
		start--
	}
	end := pos
	for end < len(text) && text[end] != '\n' {
		end++
	}
	return start, end
}

// lineStartOf returns the byte offset of the start of the line containing pos.
func lineStartOf(text string, pos int) int {
	ls, _ := lineBounds(text, pos)
	return ls
}

// contextRegion returns the bounded rune window around the entity span within
// the line, never crossing the line boundaries.
func contextRegion(text string, lineStart, lineEnd, entStart, entEnd int) string {
	lo := entStart
	for i := 0; i < contextWindow && lo > lineStart; i++ {
		_, size := utf8.DecodeLastRuneInString(text[:lo])
		lo -= size
	}
	hi := entEnd
	for i := 0; i < contextWindow && hi < lineEnd; i++ {
		_, size := utf8.DecodeRuneInString(text[hi:])
		hi += size
	}
	return text[lo:hi]
}

// copyEntity returns a defensive deep copy of e with its own Sources and
// component slices.
func copyEntity(e merge.Entity) merge.Entity {
	out := e
	out.Sources = append([]detection.Source(nil), e.Sources...)
	if e.Components != nil {
		out.Components = make([]detection.Candidate, len(e.Components))
		for i, c := range e.Components {
			out.Components[i] = c
			out.Components[i].Sources = append([]detection.Source(nil), c.Sources...)
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
