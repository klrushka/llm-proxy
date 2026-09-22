// Package tokenization issues cryptographically random scoped opaque tokens
// of the form <PII_TYPE_SUFFIX> for confirmed personal entities. It is the
// token-generation layer of the reversible-tokenization capability.
//
// The generator retains exact (scope, type, value) tuples in volatile memory
// solely to provide deterministic reuse within a scope; it exposes no mapping,
// logs no values, scopes or tokens, and embeds none of that data in the tokens
// it returns. Tokens contain only the canonical type and a random suffix.
//
// Scope of this package (tasks 7.1, 7.2): token generation and right-to-left
// replacement of confirmed personal spans by UTF-8 byte offsets. Vault
// persistence, TTL, revoke and detokenization are separate tasks.
package tokenization

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/ownership"
)

// Stable sentinel errors returned by Generator.Token. They are exported so
// callers can distinguish validation failures without string matching.
var (
	// ErrEmptyScope reports an empty scope identifier.
	ErrEmptyScope = errors.New("tokenization: empty scope")
	// ErrEmptyValue reports an empty plaintext value.
	ErrEmptyValue = errors.New("tokenization: empty value")
	// ErrUnknownType reports a type that is not in the canonical detection registry.
	ErrUnknownType = errors.New("tokenization: unknown type")
)

// suffixBytes is the number of cryptographically random bytes in each token
// suffix, hex-encoded to 2*suffixBytes characters.
const suffixBytes = 16

// key is the private comparable tuple identifying one (scope, type, value)
// reuse slot. Using a struct instead of delimiter concatenation guarantees that
// arbitrary strings containing NUL cannot alias a different tuple.
type key struct {
	scope string
	typ   detection.Type
	value string
}

// Generator issues scoped opaque tokens. It is safe for concurrent use.
//
// Tokens are deterministic per (scope, type, value): repeated requests for the
// same canonical type and exact value inside one scope reuse the same token,
// while the same type and value in different scopes receive different tokens.
// Different types or different exact values in one scope never reuse a token.
// Randomness comes from crypto/rand.Reader unless an unexported test seam
// injects a different reader.
type Generator struct {
	registry *detection.Registry
	rand     io.Reader

	mu     sync.Mutex
	issued map[key]string // key -> token
	tokens map[string]key // token -> key
}

// New returns a Generator backed by the canonical detection registry and
// crypto/rand.Reader. It fails only if the canonical registry cannot be built.
func New() (*Generator, error) {
	reg, err := detection.New()
	if err != nil {
		return nil, err
	}
	return newGenerator(rand.Reader, reg), nil
}

// newGenerator is the unexported constructor used by New and by tests to inject
// a deterministic random reader and registry.
func newGenerator(r io.Reader, reg *detection.Registry) *Generator {
	return &Generator{
		registry: reg,
		rand:     r,
		issued:   make(map[key]string),
		tokens:   make(map[string]key),
	}
}

// Token returns the opaque token for the given scope, canonical type and exact
// value. It validates inputs, reuses an already-issued token for the same
// (scope, type, value), and retries random generation if a freshly drawn suffix
// would collide with a token already issued for a different key. The returned
// token is of the form <PII_TYPE_SUFFIX>: it contains the canonical type and a
// random suffix only, and never contains the value, a hash of the value,
// ciphertext, the scope identifier or any other reversible material.
func (g *Generator) Token(scope string, typ detection.Type, value string) (string, error) {
	if scope == "" {
		return "", ErrEmptyScope
	}
	if value == "" {
		return "", ErrEmptyValue
	}
	if !g.registry.Lookup(typ) {
		return "", ErrUnknownType
	}

	k := key{scope: scope, typ: typ, value: value}

	g.mu.Lock()
	defer g.mu.Unlock()

	if tok, ok := g.issued[k]; ok {
		return tok, nil
	}

	for {
		suffix, err := g.randomSuffix()
		if err != nil {
			return "", err
		}
		tok := "<" + string(typ) + "_" + suffix + ">"
		if existingKey, ok := g.tokens[tok]; ok && existingKey != k {
			continue
		}
		g.issued[k] = tok
		g.tokens[tok] = k
		return tok, nil
	}
}

// randomSuffix draws suffixBytes cryptographically random bytes and returns
// their lowercase hex encoding.
func (g *Generator) randomSuffix() (string, error) {
	buf := make([]byte, suffixBytes)
	if _, err := io.ReadFull(g.rand, buf); err != nil {
		return "", fmt.Errorf("tokenization: read random: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// TokenIssuer issues a scoped opaque token for a canonical type and exact
// value. *Generator satisfies this interface.
type TokenIssuer interface {
	Token(scope string, typ detection.Type, value string) (string, error)
}

// Replacement is one safe replacement metadata entry. It carries the ownership
// entity (offsets, type, ownership metadata and components) and the issued
// token, but never the original plaintext value or a plaintext mapping.
type Replacement struct {
	Entity ownership.Entity
	Token  string
}

// ReplaceResult is the outcome of a tokenization pass.
type ReplaceResult struct {
	Text         string
	Replacements []Replacement
}

// Stable sentinel errors returned by Replace.
var (
	// ErrInvalidSpan reports a personal entity whose UTF-8 byte span is
	// negative, reversed, out of range, or mid-rune.
	ErrInvalidSpan = errors.New("tokenization: invalid span")
	// ErrPartialOverlap reports two personal top-level spans that share bytes
	// without one fully containing the other.
	ErrPartialOverlap = errors.New("tokenization: partial overlap")
	// ErrNilIssuer reports a nil token issuer.
	ErrNilIssuer = errors.New("tokenization: nil issuer")
)

// Replace tokenizes confirmed personal entities in text for the given scope.
//
// Only entities with Personal=true are replaced; organization, public,
// ambiguous and policy-disabled entities remain byte-for-byte unchanged. The
// exact original value is extracted from validated UTF-8 byte offsets and its
// scoped token is obtained from issuer. Replacements are applied right to left
// so earlier offsets remain valid. Repeated exact values of the same type in
// one scope reuse the issuer token.
//
// A merged outer span with nested Components produces exactly one outer token;
// components remain metadata and are not separately tokenized. Duplicate or
// fully-contained top-level personal spans deterministically keep one outer
// span. Partial overlaps and invalid spans are rejected with stable errors.
// Caller-owned entity, source, component and reason slices are never mutated
// or aliased. On issuer failure no partial tokenized result is returned.
func Replace(text string, scope string, results []ownership.Entity, issuer TokenIssuer) (ReplaceResult, error) {
	var out ReplaceResult
	if len(results) == 0 {
		out.Text = text
		return out, nil
	}
	if issuer == nil {
		return out, ErrNilIssuer
	}

	personal := make([]ownership.Entity, 0, len(results))
	for _, e := range results {
		if !e.Personal {
			continue
		}
		if !validSpan(text, e.Start, e.End) {
			return out, ErrInvalidSpan
		}
		personal = append(personal, e)
	}
	if len(personal) == 0 {
		out.Text = text
		return out, nil
	}

	kept, err := resolveSpans(personal)
	if err != nil {
		return out, err
	}

	// Process right to left so earlier offsets remain valid in the buffer.
	sort.SliceStable(kept, func(i, j int) bool {
		return kept[i].Start > kept[j].Start
	})

	type item struct {
		entity ownership.Entity
		token  string
	}
	items := make([]item, 0, len(kept))
	for _, e := range kept {
		value := text[e.Start:e.End]
		tok, err := issuer.Token(scope, e.Type, value)
		if err != nil {
			return out, err
		}
		items = append(items, item{entity: copyOwnershipEntity(e), token: tok})
	}

	buf := []byte(text)
	for _, it := range items {
		buf = replaceRange(buf, it.entity.Start, it.entity.End, it.token)
	}

	out.Text = string(buf)
	out.Replacements = make([]Replacement, 0, len(items))
	for _, it := range items {
		out.Replacements = append(out.Replacements, Replacement{Entity: it.entity, Token: it.token})
	}
	sort.SliceStable(out.Replacements, func(i, j int) bool {
		return out.Replacements[i].Entity.Start < out.Replacements[j].Entity.Start
	})
	return out, nil
}

// resolveSpans rejects partial overlaps and deterministically keeps one outer
// span for duplicate or fully-contained personal top-level spans. It returns
// the surviving spans in document order (Start ascending, End descending).
//
// For exact-coordinate duplicates the winner is chosen by a total order that is
// independent of caller input order: Start ascending, End descending, then
// canonical Type ascending, Confidence descending, OwnershipScore descending,
// OwnerType, OwnerID, and stable lexicographic comparison of Sources,
// Components and ReasonCodes. Caller input is never mutated.
func resolveSpans(entities []ownership.Entity) ([]ownership.Entity, error) {
	sorted := make([]ownership.Entity, len(entities))
	copy(sorted, entities)
	sort.SliceStable(sorted, func(i, j int) bool {
		return lessEntity(sorted[i], sorted[j])
	})

	var kept []ownership.Entity
	for _, c := range sorted {
		contained := false
		for _, k := range kept {
			if c.Start >= k.Start && c.End <= k.End {
				contained = true
				break
			}
			if c.Start < k.End && k.Start < c.End {
				return nil, ErrPartialOverlap
			}
		}
		if !contained {
			kept = append(kept, c)
		}
	}
	return kept, nil
}

// lessEntity is a total order over ownership entities used to make exact-span
// winner selection deterministic independent of caller input order. It orders
// by Start ascending, End descending (outermost first), then by a full
// tie-break over all returned metadata: canonical Type ascending, Confidence
// descending, OwnershipScore descending, OwnerType, OwnerID, and stable
// lexicographic comparison of Sources, Components and ReasonCodes.
func lessEntity(a, b ownership.Entity) bool {
	if a.Start != b.Start {
		return a.Start < b.Start
	}
	if a.End != b.End {
		return a.End > b.End
	}
	if a.Type != b.Type {
		return a.Type < b.Type
	}
	if a.Confidence != b.Confidence {
		return a.Confidence > b.Confidence
	}
	if a.OwnershipScore != b.OwnershipScore {
		return a.OwnershipScore > b.OwnershipScore
	}
	if a.OwnerType != b.OwnerType {
		return a.OwnerType < b.OwnerType
	}
	if a.OwnerID != b.OwnerID {
		return a.OwnerID < b.OwnerID
	}
	if c := compareSources(a.Sources, b.Sources); c != 0 {
		return c < 0
	}
	if c := compareComponents(a.Components, b.Components); c != 0 {
		return c < 0
	}
	return compareReasons(a.ReasonCodes, b.ReasonCodes) < 0
}

// compareSources returns -1, 0 or 1 comparing two source slices
// lexicographically by canonical source value.
func compareSources(a, b []detection.Source) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// compareComponents returns -1, 0 or 1 comparing two component slices
// lexicographically by candidate fields and their source slices.
func compareComponents(a, b []detection.Candidate) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if c := compareCandidate(a[i], b[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// compareCandidate returns -1, 0 or 1 comparing two candidates by Type, Start,
// End, Confidence and Sources.
func compareCandidate(a, b detection.Candidate) int {
	if a.Type != b.Type {
		if a.Type < b.Type {
			return -1
		}
		return 1
	}
	if a.Start != b.Start {
		if a.Start < b.Start {
			return -1
		}
		return 1
	}
	if a.End != b.End {
		if a.End < b.End {
			return -1
		}
		return 1
	}
	if a.Confidence != b.Confidence {
		if a.Confidence < b.Confidence {
			return -1
		}
		return 1
	}
	return compareSources(a.Sources, b.Sources)
}

// compareReasons returns -1, 0 or 1 comparing two reason-code slices
// lexicographically by reason value.
func compareReasons(a, b []ownership.ReasonCode) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// validSpan reports whether [start, end) is a valid UTF-8 byte span within
// text: non-negative, non-reversed, in range, and on rune boundaries.
func validSpan(text string, start, end int) bool {
	if start < 0 || end <= start || end > len(text) {
		return false
	}
	return isRuneBoundary(text, start) && isRuneBoundary(text, end)
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

// replaceRange returns a new buffer with [start, end) replaced by token.
func replaceRange(buf []byte, start, end int, token string) []byte {
	out := make([]byte, 0, len(buf)-(end-start)+len(token))
	out = append(out, buf[:start]...)
	out = append(out, token...)
	out = append(out, buf[end:]...)
	return out
}

// copyOwnershipEntity returns a defensive deep copy of e with its own Sources,
// component and reason slices.
func copyOwnershipEntity(e ownership.Entity) ownership.Entity {
	out := e
	out.Sources = append([]detection.Source(nil), e.Sources...)
	if e.Components != nil {
		out.Components = make([]detection.Candidate, len(e.Components))
		for i, c := range e.Components {
			out.Components[i] = c
			out.Components[i].Sources = append([]detection.Source(nil), c.Sources...)
		}
	}
	out.ReasonCodes = append([]ownership.ReasonCode(nil), e.ReasonCodes...)
	return out
}
