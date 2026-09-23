package vault

import (
	"container/heap"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"sync"
	"time"
)

// Domain-separation labels used to derive the AES and HMAC keys from the
// 32-byte master key. They are fixed, deterministic and never exposed.
var (
	aesKeyDomain  = []byte("llm-proxy/vault/v1/aes-key")
	hmacKeyDomain = []byte("llm-proxy/vault/v1/hmac-key")
	scopeDomain   = []byte("llm-proxy/vault/v1/scope")
	tokenDomain   = []byte("llm-proxy/vault/v1/token")
)

// maxSweepPops bounds the number of expiry heap items a single public operation
// may pop during lazy cleanup, so a mass simultaneous expiry cannot turn one
// request into an unbounded O(N log N) drain under the global lock. The target
// entry is still checked fail-closed by its own expiry even when the budget is
// exhausted.
const maxSweepPops = 64

// deriveKey returns HMAC-SHA-256(master, domain) as a 32-byte key. It is the
// deterministic, domain-separated derivation used for both the AES and HMAC
// keys so a single 32-byte master key yields independent keys per purpose.
func deriveKey(master [32]byte, domain []byte) [32]byte {
	mac := hmac.New(sha256.New, master[:])
	mac.Write(domain)
	var out [32]byte
	copy(out[:], mac.Sum(nil))
	return out
}

// digest returns HMAC-SHA-256(key, domain || data) as a 32-byte digest. The
// domain prefix keeps index digests for different fields (scope vs token)
// independent even when the underlying data is identical.
func digest(key [32]byte, domain, data []byte) [32]byte {
	mac := hmac.New(sha256.New, key[:])
	mac.Write(domain)
	mac.Write(data)
	var out [32]byte
	copy(out[:], mac.Sum(nil))
	return out
}

// encKey is the private comparable composite index key for one mapping slot.
// It holds only HMAC digests of the scope and token, never the plaintext
// values, so the storage index contains no reversible material.
type encKey struct {
	scope [32]byte
	token [32]byte
}

// encEntry is one stored mapping slot. It holds only the AES-256-GCM
// ciphertext of the original value, the wall-clock expiry and a generation
// counter; it never holds the plaintext original, scope or token.
type encEntry struct {
	ciphertext []byte
	expiresAt  time.Time
	gen        uint64
}

// encExpiryItem is one entry in the Encrypted adapter's expiry min-heap. It
// carries only the wall-clock expiry and an opaque generation id, never any
// HMAC-derived scope/token key. The generation id is resolved to a mapping
// through the separate genIndex, which is cleared on revoke and replacement so
// a stale heap record can be safely ignored.
type encExpiryItem struct {
	expiresAt time.Time
	gen       uint64
}

// encExpiryHeap is a min-heap of encExpiryItem ordered by expiresAt. It bounds
// the hot-path expiry sweep to the heap head instead of scanning every mapping.
type encExpiryHeap []encExpiryItem

func (h encExpiryHeap) Len() int           { return len(h) }
func (h encExpiryHeap) Less(i, j int) bool { return h[i].expiresAt.Before(h[j].expiresAt) }
func (h encExpiryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *encExpiryHeap) Push(x any) { *h = append(*h, x.(encExpiryItem)) }

func (h *encExpiryHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

// Encrypted is a thread-safe volatile Vault adapter that stores only
// AES-256-GCM ciphertext. It is the production adapter: reversible values are
// never held in plaintext, and the storage index keys are HMAC-SHA-256 digests
// of the scope and token rather than the plaintext values.
//
// The AES and HMAC keys are deterministically and domain-separately derived
// from a single 32-byte master key using the standard library. Every new
// mapping uses a fresh random nonce. Tampered ciphertext, a wrong key or
// corrupted records always fail closed with a safe sentinel and never disclose
// data.
//
// Mappings are grouped into per-scope buckets so RevokeScope touches only the
// entries of that scope, and expiry is tracked through a min-heap so the hot
// path inspects only the heap head rather than scanning every mapping.
type Encrypted struct {
	mu           sync.Mutex
	mapping      map[encKey]encEntry
	scopeBuckets map[[32]byte]map[encKey]struct{}
	genIndex     map[uint64]encKey // generation -> encKey
	expiry       encExpiryHeap
	aesKey       [32]byte
	hmacKey      [32]byte
	ttl          time.Duration
	now          func() time.Time
	genCounter   uint64
	sweepPops    int // unexported test seam: heap items popped by sweepExpired
}

// NewEncrypted returns an empty encrypted volatile adapter with the given TTL
// and the AES/HMAC keys derived from masterKey. A non-positive ttl is rejected
// with ErrInvalidTTL. The public constructor uses the real wall clock; tests
// should use newEncryptedWithClock to control time without sleeping.
func NewEncrypted(masterKey [32]byte, ttl time.Duration) (*Encrypted, error) {
	return newEncryptedWithClock(masterKey, ttl, time.Now)
}

// newEncryptedWithClock returns an empty encrypted adapter whose expiry
// decisions use the injected clock. It is unexported so production callers use
// the real clock via NewEncrypted while tests can drive time deterministically.
func newEncryptedWithClock(masterKey [32]byte, ttl time.Duration, now func() time.Time) (*Encrypted, error) {
	if ttl <= 0 {
		return nil, ErrInvalidTTL
	}
	return &Encrypted{
		mapping:      make(map[encKey]encEntry),
		scopeBuckets: make(map[[32]byte]map[encKey]struct{}),
		genIndex:     make(map[uint64]encKey),
		expiry:       make(encExpiryHeap, 0),
		aesKey:       deriveKey(masterKey, aesKeyDomain),
		hmacKey:      deriveKey(masterKey, hmacKeyDomain),
		ttl:          ttl,
		now:          now,
	}, nil
}

// Save records the mapping (scope, token) -> original. See Vault.Save for the
// full contract. The original is stored only as AES-256-GCM ciphertext with a
// fresh random nonce. An idempotent save of the same original leaves the
// existing ciphertext and nonce unchanged; a different original under an
// existing key returns ErrConflict. An already-expired mapping is treated as
// absent and replaced with a fresh mapping and a new nonce.
func (e *Encrypted) Save(ctx context.Context, scope, token, original string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if scope == "" {
		return ErrEmptyScope
	}
	if token == "" {
		return ErrEmptyToken
	}
	if original == "" {
		return ErrEmptyOriginal
	}

	k := encKey{
		scope: digest(e.hmacKey, scopeDomain, []byte(scope)),
		token: digest(e.hmacKey, tokenDomain, []byte(token)),
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	// Re-check cancellation after acquiring the lock so a cancelled operation
	// never writes a mapping.
	if err := ctx.Err(); err != nil {
		return err
	}

	now := e.now()
	e.sweepExpired(now)
	if existing, ok := e.mapping[k]; ok {
		if now.Before(existing.expiresAt) {
			plain, err := e.decrypt(existing.ciphertext, k.aad())
			if err != nil {
				// A corrupted or tampered existing record must fail closed
				// rather than be silently overwritten or disclosed.
				e.deleteMapping(k)
				return ErrCorrupted
			}
			if hmac.Equal([]byte(plain), []byte(original)) {
				return nil
			}
			return ErrConflict
		}
		// The existing mapping is expired and must not influence the new
		// lifecycle: treat the key as absent and replace it below.
		delete(e.genIndex, existing.gen)
	}

	ct, err := e.encrypt(original, k.aad())
	if err != nil {
		return err
	}
	gen := e.nextGen()
	expiresAt := now.Add(e.ttl)
	e.mapping[k] = encEntry{ciphertext: ct, expiresAt: expiresAt, gen: gen}
	e.addToScopeBucket(k.scope, k)
	e.genIndex[gen] = k
	heap.Push(&e.expiry, encExpiryItem{expiresAt: expiresAt, gen: gen})
	return nil
}

// Resolve returns the original value for the exact (scope, token) key, or
// ErrNotFound. See Vault.Resolve for the full contract. An expired mapping is
// not disclosed: Resolve returns ErrNotFound and lazily deletes the expired
// entry. Tampered ciphertext, a wrong key or corruption fail closed with
// ErrCorrupted and never disclose data.
func (e *Encrypted) Resolve(ctx context.Context, scope, token string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if scope == "" {
		return "", ErrEmptyScope
	}
	if token == "" {
		return "", ErrEmptyToken
	}

	k := encKey{
		scope: digest(e.hmacKey, scopeDomain, []byte(scope)),
		token: digest(e.hmacKey, tokenDomain, []byte(token)),
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	// Re-check cancellation after acquiring the lock so a cancelled operation
	// never reads plaintext or mutates state.
	if err := ctx.Err(); err != nil {
		return "", err
	}

	now := e.now()
	e.sweepExpired(now)
	ent, ok := e.mapping[k]
	if !ok {
		return "", ErrNotFound
	}
	if !now.Before(ent.expiresAt) {
		e.deleteMapping(k)
		return "", ErrNotFound
	}
	plain, err := e.decrypt(ent.ciphertext, k.aad())
	if err != nil {
		e.deleteMapping(k)
		return "", ErrCorrupted
	}
	return plain, nil
}

// RevokeScope atomically deletes every mapping belonging to scope. See
// Vault.RevokeScope for the full contract. It checks ctx cancellation before
// acquiring the lock.
func (e *Encrypted) RevokeScope(ctx context.Context, scope string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if scope == "" {
		return ErrEmptyScope
	}

	scopeDigest := digest(e.hmacKey, scopeDomain, []byte(scope))

	e.mu.Lock()
	defer e.mu.Unlock()

	// Re-check cancellation after acquiring the lock so a cancelled operation
	// never mutates state.
	if err := ctx.Err(); err != nil {
		return err
	}

	e.sweepExpired(e.now())
	bucket := e.scopeBuckets[scopeDigest]
	if bucket == nil {
		return nil
	}
	for k := range bucket {
		if ent, ok := e.mapping[k]; ok {
			delete(e.genIndex, ent.gen)
		}
		delete(e.mapping, k)
	}
	delete(e.scopeBuckets, scopeDigest)
	return nil
}

// Len returns the number of live mappings after sweeping expired ones. It is
// a safe numeric aggregate for metrics and never exposes mapping content.
func (e *Encrypted) Len() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sweepExpired(e.now())
	return len(e.mapping)
}

// sweepExpired removes genuinely expired mappings by inspecting only the head
// of the expiry min-heap, bounded to at most maxSweepPops per call so a mass
// simultaneous expiry cannot turn one operation into an unbounded drain under
// the global lock. It must be called with e.mu held. A stale heap record (whose
// generation is no longer in genIndex, or whose live entry generation differs)
// is safely ignored.
func (e *Encrypted) sweepExpired(now time.Time) {
	popped := 0
	for e.expiry.Len() > 0 && popped < maxSweepPops {
		head := e.expiry[0]
		if now.Before(head.expiresAt) {
			break
		}
		heap.Pop(&e.expiry)
		popped++
		e.sweepPops++
		k, ok := e.genIndex[head.gen]
		if !ok {
			// Stale record: the generation was cleared by a revoke or
			// replacement. Ignore it.
			continue
		}
		ent, ok := e.mapping[k]
		if !ok || ent.gen != head.gen {
			// Stale record: the mapping was replaced or otherwise changed since
			// this heap entry was pushed. Ignore it.
			delete(e.genIndex, head.gen)
			continue
		}
		e.deleteMapping(k)
	}
}

// deleteMapping removes a mapping from the storage map, its scope bucket and
// the generation index. It must be called with e.mu held.
func (e *Encrypted) deleteMapping(k encKey) {
	if ent, ok := e.mapping[k]; ok {
		delete(e.genIndex, ent.gen)
	}
	delete(e.mapping, k)
	if bucket := e.scopeBuckets[k.scope]; bucket != nil {
		delete(bucket, k)
		if len(bucket) == 0 {
			delete(e.scopeBuckets, k.scope)
		}
	}
}

// addToScopeBucket records a mapping key in its scope bucket. It must be called
// with e.mu held.
func (e *Encrypted) addToScopeBucket(scopeDigest [32]byte, k encKey) {
	bucket := e.scopeBuckets[scopeDigest]
	if bucket == nil {
		bucket = make(map[encKey]struct{})
		e.scopeBuckets[scopeDigest] = bucket
	}
	bucket[k] = struct{}{}
}

// nextGen returns the next monotonically increasing generation counter. It must
// be called with e.mu held.
func (e *Encrypted) nextGen() uint64 {
	e.genCounter++
	return e.genCounter
}

// encrypt seals plaintext with AES-256-GCM using a fresh random nonce and
// returns nonce || ciphertext. The composite index key (the HMAC digests of the
// scope and token) is bound as additional authenticated data, so a ciphertext
// moved to a different (scope, token) slot fails to open.
func (e *Encrypted) encrypt(plaintext string, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(e.aesKey[:])
	if err != nil {
		return nil, ErrCorrupted
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrCorrupted
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(plaintext), aad), nil
}

// decrypt opens nonce || ciphertext with AES-256-GCM. The composite index key
// must match the AAD used at Seal time. Any tampering, wrong key, wrong slot or
// corruption returns ErrCorrupted.
func (e *Encrypted) decrypt(sealed []byte, aad []byte) (string, error) {
	block, err := aes.NewCipher(e.aesKey[:])
	if err != nil {
		return "", ErrCorrupted
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", ErrCorrupted
	}
	ns := gcm.NonceSize()
	if len(sealed) < ns {
		return "", ErrCorrupted
	}
	nonce, ct := sealed[:ns], sealed[ns:]
	plain, err := gcm.Open(nil, nonce, ct, aad)
	if err != nil {
		return "", ErrCorrupted
	}
	return string(plain), nil
}

// aad returns the additional authenticated data binding a ciphertext to its
// exact composite index key: the concatenation of the scope and token digests.
func (k encKey) aad() []byte {
	out := make([]byte, 0, len(k.scope)+len(k.token))
	out = append(out, k.scope[:]...)
	out = append(out, k.token[:]...)
	return out
}
