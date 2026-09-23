// Package tokenization issues cryptographically random scoped opaque tokens
// of the form <PII_TYPE_SUFFIX> for confirmed personal entities. It is the
// token-generation layer of the reversible-tokenization capability.
//
// The generator retains reuse slots in volatile memory solely to provide
// deterministic reuse within a scope; it stores only HMAC-SHA-256 digests of
// the scope, value and token under distinct domains, never the plaintext
// values, scopes or tokens. It exposes no mapping, logs no values, scopes or
// tokens, and embeds none of that data in the tokens it returns. Tokens contain
// only the canonical type and a random suffix.
//
// Scope of this package (tasks 7.1, 7.2): token generation and right-to-left
// replacement of confirmed personal spans by UTF-8 byte offsets. Vault
// persistence, TTL, revoke and detokenization are separate tasks.
package tokenization

import (
	"container/heap"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

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
	// ErrInvalidTTL reports a non-positive TTL passed to NewKeyed. It never
	// embeds the TTL value.
	ErrInvalidTTL = errors.New("tokenization: invalid ttl")
)

// suffixBytes is the number of bytes in each token suffix, hex-encoded to
// 2*suffixBytes characters. The suffix is derived from a random seed through a
// domain-separated HMAC-SHA-256 PRF, so the raw suffix is never stored.
const suffixBytes = 16

// seedBytes is the number of cryptographically random bytes in each issued
// reuse slot's seed. The seed is the only per-slot randomness retained; the
// suffix is derived from it on demand.
const seedBytes = 16

// maxSweepPops bounds the number of expiry heap items a single public operation
// may pop during lazy cleanup, so a mass simultaneous expiry cannot turn one
// request into an unbounded O(N log N) drain under the global lock. The target
// entry is still checked fail-closed by its own expiry even when the budget is
// exhausted.
const maxSweepPops = 64

// defaultTTL is the reuse window used by the test-compatible New() constructor.
// Production wiring must use NewKeyed with an explicit TTL.
const defaultTTL = time.Hour

// Domain-separation labels used to derive the HMAC and PRF keys and to digest
// the scope, value and token. They are fixed, deterministic and never exposed.
// The PRF key domain is distinct from the digest key domains so the suffix
// derivation can never be confused with a scope/value/token digest.
var (
	hmacKeyDomain   = []byte("llm-proxy/tokenization/v1/hmac-key")
	prfKeyDomain    = []byte("llm-proxy/tokenization/v1/prf-key")
	prfSuffixDomain = []byte("llm-proxy/tokenization/v1/prf-suffix")
	scopeDomain     = []byte("llm-proxy/tokenization/v1/scope")
	valueDomain     = []byte("llm-proxy/tokenization/v1/value")
	tokenDomain     = []byte("llm-proxy/tokenization/v1/token")
)

// deriveHMACKey returns HMAC-SHA-256(master, domain) as the 32-byte HMAC key
// used to digest reuse-slot keys. It is the deterministic, domain-separated
// derivation from the master key using only the standard library.
func deriveHMACKey(master [32]byte) [32]byte {
	mac := hmac.New(sha256.New, master[:])
	mac.Write(hmacKeyDomain)
	var out [32]byte
	copy(out[:], mac.Sum(nil))
	return out
}

// derivePRFKey returns HMAC-SHA-256(master, prfKeyDomain) as the 32-byte key
// used to derive token suffixes from a seed and a digestKey. It is
// domain-separated from the scope/value/token digest keys so a suffix can never
// be confused with a digest of the same material.
func derivePRFKey(master [32]byte) [32]byte {
	mac := hmac.New(sha256.New, master[:])
	mac.Write(prfKeyDomain)
	var out [32]byte
	copy(out[:], mac.Sum(nil))
	return out
}

// digestKey is the private comparable tuple identifying one (scope, type,
// value) reuse slot. The scope and value are stored only as HMAC-SHA-256
// digests under distinct domains, never as plaintext, so the generator's maps
// hold no reversible material. The canonical type is not sensitive and is kept
// as-is. Using a struct instead of delimiter concatenation guarantees that
// arbitrary strings containing NUL cannot alias a different tuple.
type digestKey struct {
	scope [32]byte
	typ   detection.Type
	value [32]byte
}

// issuedEntry is one issued reuse slot. It holds only a random seed, the HMAC
// digest of the issued token, the wall-clock time after which the slot must no
// longer be reused, and a generation counter. The raw suffix and the full token
// string are never retained: the suffix is derived on demand from the seed and
// the digestKey through the PRF, and the token digest is used for the reverse
// collision index and cleanup without reconstructing the stored token.
type issuedEntry struct {
	seed        [seedBytes]byte
	tokenDigest [32]byte
	expiresAt   time.Time
	gen         uint64
}

// expiryItem is one entry in the Generator's expiry min-heap. It carries only
// the wall-clock expiry and an opaque generation id, never any HMAC-derived
// scope/value/token key. The generation id is resolved to a reuse slot through
// the separate genIndex, which is cleared on revoke and replacement so a stale
// heap record can be safely ignored.
type expiryItem struct {
	expiresAt time.Time
	gen       uint64
}

// expiryHeap is a min-heap of expiryItem ordered by expiresAt. It is used to
// bound the hot-path expiry sweep to the heap head instead of scanning every
// issued slot.
type expiryHeap []expiryItem

func (h expiryHeap) Len() int           { return len(h) }
func (h expiryHeap) Less(i, j int) bool { return h[i].expiresAt.Before(h[j].expiresAt) }
func (h expiryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *expiryHeap) Push(x any) { *h = append(*h, x.(expiryItem)) }

func (h *expiryHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

// Generator issues scoped opaque tokens. It is safe for concurrent use.
//
// Tokens are deterministic per (scope, type, value): repeated requests for the
// same canonical type and exact value inside one scope reuse the same token
// while the slot is within its TTL, while the same type and value in different
// scopes receive different tokens. Different types or different exact values in
// one scope never reuse a token. Randomness comes from crypto/rand.Reader
// unless an unexported test seam injects a different reader.
//
// The generator's maps are keyed by HMAC digests of the scope, value and token
// under distinct domains and never hold the plaintext scope, value or token.
// Each reuse slot retains only a random seed, the token digest and an expiry;
// the token suffix is derived on demand from the seed and the digestKey through
// a domain-separated HMAC-SHA-256 PRF, so the raw suffix and the full token
// string are never stored. RevokeScope clears every slot of a scope so a later
// request for the same value issues a fresh token.
type Generator struct {
	registry *detection.Registry
	rand     io.Reader
	hmacKey  [32]byte
	prfKey   [32]byte
	ttl      time.Duration
	now      func() time.Time

	mu         sync.Mutex
	issued     map[[32]byte]map[digestKey]issuedEntry // scopeDigest -> bucket
	tokens     map[[32]byte]digestKey                 // tokenDigest -> digestKey
	genIndex   map[uint64]digestKey                   // generation -> digestKey
	expiry     expiryHeap                             // min-heap of expiryItem
	genCounter uint64
	sweepPops  int // unexported test seam: heap items popped by sweepExpired
}

// New returns a Generator backed by the canonical detection registry and
// crypto/rand.Reader, with a process-local random master key and a generous
// default TTL. It is intended for local demos and tests. Production wiring must
// use NewKeyed with the real master key and TTL. It fails only if the canonical
// registry cannot be built or the random key cannot be drawn.
func New() (*Generator, error) {
	reg, err := detection.New()
	if err != nil {
		return nil, err
	}
	var master [32]byte
	if _, err := rand.Read(master[:]); err != nil {
		return nil, err
	}
	return newKeyedGenerator(rand.Reader, reg, master, defaultTTL, time.Now), nil
}

// NewKeyed returns a Generator backed by the canonical detection registry and
// crypto/rand.Reader, with the HMAC key deterministically derived from
// masterKey and the given reuse TTL. It is the production constructor. A
// non-positive ttl is rejected with ErrInvalidTTL.
func NewKeyed(masterKey [32]byte, ttl time.Duration) (*Generator, error) {
	reg, err := detection.New()
	if err != nil {
		return nil, err
	}
	if ttl <= 0 {
		return nil, ErrInvalidTTL
	}
	return newKeyedGenerator(rand.Reader, reg, masterKey, ttl, time.Now), nil
}

// newKeyedGenerator is the unexported constructor shared by New, NewKeyed and
// tests. It injects the random reader, registry, master key, TTL and clock.
func newKeyedGenerator(r io.Reader, reg *detection.Registry, masterKey [32]byte, ttl time.Duration, now func() time.Time) *Generator {
	return &Generator{
		registry: reg,
		rand:     r,
		hmacKey:  deriveHMACKey(masterKey),
		prfKey:   derivePRFKey(masterKey),
		ttl:      ttl,
		now:      now,
		issued:   make(map[[32]byte]map[digestKey]issuedEntry),
		tokens:   make(map[[32]byte]digestKey),
		genIndex: make(map[uint64]digestKey),
		expiry:   make(expiryHeap, 0),
	}
}

// Token returns the opaque token for the given scope, canonical type and exact
// value. It validates inputs, reuses an already-issued token for the same
// (scope, type, value) while the slot is within its TTL, and retries random
// generation if a freshly drawn seed would produce a token that collides with a
// token already issued for a different key. An expired slot is treated as
// absent and replaced with a fresh token. The returned token is of the form
// <PII_TYPE_SUFFIX>: it contains the canonical type and a PRF-derived suffix
// only, and never contains the value, a hash of the value, ciphertext, the
// scope identifier or any other reversible material.
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

	k := digestKey{
		scope: g.digestScope(scope),
		typ:   typ,
		value: g.digestValue(value),
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	g.sweepExpired(now)
	bucket := g.issued[k.scope]
	if ent, ok := bucket[k]; ok {
		if now.Before(ent.expiresAt) {
			return g.tokenString(typ, g.suffixFor(ent.seed, k)), nil
		}
		// The slot is expired and must not be reused: drop it and issue a
		// fresh token below. The reverse token digest and the generation
		// reference are removed from the entry data without reconstructing the
		// stored token.
		delete(bucket, k)
		delete(g.tokens, ent.tokenDigest)
		delete(g.genIndex, ent.gen)
	}

	for {
		seed, err := g.randomSeed()
		if err != nil {
			return "", err
		}
		suffix := g.suffixFor(seed, k)
		tok := g.tokenString(typ, suffix)
		tokDigest := g.digestToken(tok)
		if existingKey, ok := g.tokens[tokDigest]; ok && existingKey != k {
			continue
		}
		if bucket == nil {
			bucket = make(map[digestKey]issuedEntry)
			g.issued[k.scope] = bucket
		}
		gen := g.nextGen()
		expiresAt := now.Add(g.ttl)
		bucket[k] = issuedEntry{seed: seed, tokenDigest: tokDigest, expiresAt: expiresAt, gen: gen}
		g.tokens[tokDigest] = k
		g.genIndex[gen] = k
		heap.Push(&g.expiry, expiryItem{expiresAt: expiresAt, gen: gen})
		return tok, nil
	}
}

// RevokeScope atomically clears every issued reuse slot belonging to scope, so
// a later request for the same value issues a fresh token. It is idempotent:
// revoking an unknown or already-revoked scope succeeds and leaves other scopes
// untouched. An empty scope returns ErrEmptyScope. The returned error never
// embeds the scope.
func (g *Generator) RevokeScope(ctx context.Context, scope string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if scope == "" {
		return ErrEmptyScope
	}

	scopeDigest := g.digestScope(scope)

	g.mu.Lock()
	defer g.mu.Unlock()

	// Re-check cancellation after acquiring the lock so a cancelled operation
	// never mutates the cache.
	if err := ctx.Err(); err != nil {
		return err
	}

	g.sweepExpired(g.now())
	bucket := g.issued[scopeDigest]
	if bucket == nil {
		return nil
	}
	for _, ent := range bucket {
		delete(g.tokens, ent.tokenDigest)
		delete(g.genIndex, ent.gen)
	}
	delete(g.issued, scopeDigest)
	return nil
}

// sweepExpired removes genuinely expired reuse slots by inspecting only the
// head of the expiry min-heap, bounded to at most maxSweepPops per call so a
// mass simultaneous expiry cannot turn one operation into an unbounded drain
// under the global lock. It must be called with g.mu held. A stale heap record
// (whose generation is no longer in genIndex, or whose live entry generation
// differs) is safely ignored.
func (g *Generator) sweepExpired(now time.Time) {
	popped := 0
	for g.expiry.Len() > 0 && popped < maxSweepPops {
		head := g.expiry[0]
		if now.Before(head.expiresAt) {
			break
		}
		heap.Pop(&g.expiry)
		popped++
		g.sweepPops++
		k, ok := g.genIndex[head.gen]
		if !ok {
			// Stale record: the generation was cleared by a revoke or
			// replacement. Ignore it.
			continue
		}
		bucket := g.issued[k.scope]
		if bucket == nil {
			delete(g.genIndex, head.gen)
			continue
		}
		ent, ok := bucket[k]
		if !ok || ent.gen != head.gen {
			// Stale record: the slot was replaced or otherwise changed since
			// this heap entry was pushed. Ignore it.
			delete(g.genIndex, head.gen)
			continue
		}
		delete(bucket, k)
		delete(g.tokens, ent.tokenDigest)
		delete(g.genIndex, head.gen)
		if len(bucket) == 0 {
			delete(g.issued, k.scope)
		}
	}
}

// suffixFor derives the token suffix for a reuse slot from its random seed and
// digestKey through the domain-separated HMAC-SHA-256 PRF, taking the first
// suffixBytes bytes of the result. The suffix is produced only on demand and is
// never retained in the cache.
func (g *Generator) suffixFor(seed [seedBytes]byte, k digestKey) [suffixBytes]byte {
	mac := hmac.New(sha256.New, g.prfKey[:])
	mac.Write(prfSuffixDomain)
	mac.Write(seed[:])
	mac.Write(k.scope[:])
	mac.Write([]byte(k.typ))
	mac.Write(k.value[:])
	var out [suffixBytes]byte
	copy(out[:], mac.Sum(nil)[:suffixBytes])
	return out
}

// nextGen returns the next monotonically increasing generation counter. It must
// be called with g.mu held.
func (g *Generator) nextGen() uint64 {
	g.genCounter++
	return g.genCounter
}

// tokenString assembles the opaque token from a canonical type and random
// suffix bytes. It is the only place a token string is produced.
func (g *Generator) tokenString(typ detection.Type, suffix [suffixBytes]byte) string {
	return "<" + string(typ) + "_" + hex.EncodeToString(suffix[:]) + ">"
}

// digestScope returns the HMAC digest of a scope identifier under the scope
// domain.
func (g *Generator) digestScope(scope string) [32]byte {
	return g.digest(scopeDomain, []byte(scope))
}

// digestValue returns the HMAC digest of a plaintext value under the value
// domain.
func (g *Generator) digestValue(value string) [32]byte {
	return g.digest(valueDomain, []byte(value))
}

// digestToken returns the HMAC digest of a full token string under the token
// domain. It is used as the reverse collision index so no plaintext token is
// retained as a map key.
func (g *Generator) digestToken(tok string) [32]byte {
	return g.digest(tokenDomain, []byte(tok))
}

// digest returns HMAC-SHA-256(hmacKey, domain || data) as a 32-byte digest.
func (g *Generator) digest(domain, data []byte) [32]byte {
	mac := hmac.New(sha256.New, g.hmacKey[:])
	mac.Write(domain)
	mac.Write(data)
	var out [32]byte
	copy(out[:], mac.Sum(nil))
	return out
}

// randomSeed draws seedBytes cryptographically random bytes for a reuse slot.
func (g *Generator) randomSeed() ([seedBytes]byte, error) {
	var buf [seedBytes]byte
	if _, err := io.ReadFull(g.rand, buf[:]); err != nil {
		return buf, fmt.Errorf("tokenization: read random: %w", err)
	}
	return buf, nil
}

// TokenIssuer issues a scoped opaque token for a canonical type and exact
// value. *Generator satisfies this interface.
type TokenIssuer interface {
	Token(scope string, typ detection.Type, value string) (string, error)
}

// RevocableTokenIssuer is a TokenIssuer that can also atomically clear every
// issued reuse slot of a scope. *Generator satisfies this interface. It is the
// issuer contract used by the production pipeline so a scope revoke clears both
// the vault and the issuer cache.
type RevocableTokenIssuer interface {
	TokenIssuer
	RevokeScope(ctx context.Context, scope string) error
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
