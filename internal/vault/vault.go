// Package vault stores reversible token mappings keyed by the exact composite
// key (scope_id, token) and resolves them back to the original value. It is the
// persistence layer of the reversible-tokenization capability.
//
// The package defines a Vault interface plus two volatile adapters. The
// Encrypted adapter is the production adapter: it stores only AES-256-GCM
// ciphertext of the original value and indexes mappings by HMAC-SHA-256 digests
// of the scope and token, so no reversible material is held in plaintext. The
// Memory adapter stores plaintext originals in volatile memory and is intended
// only for local demos and tests; it does not survive restart and must not be
// used in production wiring.
//
// Scope of this package (task 7.4): the Vault interface, the encrypted volatile
// adapter and the in-memory demo adapter, with TTL and scope revoke.
// Detokenization and HTTP integration are separate tasks.
package vault

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Stable sentinel errors returned by Vault operations. They are exported so
// callers can distinguish validation and lookup failures without string
// matching. None of them embeds a scope, token or original value.
var (
	// ErrEmptyScope reports an empty scope identifier.
	ErrEmptyScope = errors.New("vault: empty scope")
	// ErrEmptyToken reports an empty token.
	ErrEmptyToken = errors.New("vault: empty token")
	// ErrEmptyOriginal reports an empty original value.
	ErrEmptyOriginal = errors.New("vault: empty original")
	// ErrConflict reports an attempt to store a different original under an
	// already-existing (scope, token) key. The existing mapping is not
	// overwritten.
	ErrConflict = errors.New("vault: mapping conflict")
	// ErrNotFound reports that no mapping exists for the requested (scope,
	// token) key. It is also returned when a token belongs to a different
	// scope, or when the mapping has expired, so an expired or cross-scope
	// token is never disclosed.
	ErrNotFound = errors.New("vault: mapping not found")
	// ErrInvalidTTL reports a non-positive TTL passed to the Memory
	// constructor. It never embeds the TTL value.
	ErrInvalidTTL = errors.New("vault: invalid ttl")
	// ErrCorrupted reports that a stored record could not be decrypted because
	// it was tampered with, was produced under a different key, or is
	// otherwise corrupted. It is the fail-closed sentinel for the encrypted
	// adapter and never discloses the ciphertext, key or original value.
	ErrCorrupted = errors.New("vault: corrupted record")
)

// Vault stores and resolves reversible token mappings by the exact composite
// key (scope_id, token). Implementations MUST be safe for concurrent use and
// MUST check ctx cancellation before performing work.
//
// A persistent implementation MUST store only AES-256-GCM ciphertext of the
// original value and MUST NOT store, log or expose plaintext originals, token
// mappings, keys or any reversible material. The in-memory demo adapter is the
// only implementation that may hold plaintext originals, and only in volatile
// memory.
type Vault interface {
	// Save records the mapping (scope, token) -> original. It is idempotent:
	// saving the same original under an already-existing (scope, token) key
	// succeeds and leaves the mapping unchanged. Saving a different original
	// under an existing key returns ErrConflict and does not overwrite the
	// mapping. Empty scope, token or original return their respective sentinel
	// errors. The returned error never embeds the scope, token or original.
	Save(ctx context.Context, scope, token, original string) error

	// Resolve returns the original value for the exact (scope, token) key, or
	// ErrNotFound if no mapping exists. A token stored under a different scope
	// is never disclosed: Resolve returns ErrNotFound for it. An expired
	// mapping is also never disclosed: Resolve returns ErrNotFound and may
	// lazily delete the expired entry. The returned error never embeds the
	// scope or token.
	Resolve(ctx context.Context, scope, token string) (string, error)

	// RevokeScope atomically deletes every mapping belonging to scope. It is
	// idempotent: revoking an unknown or already-revoked scope succeeds and
	// leaves other scopes untouched. After revocation, tokens of that scope
	// are no longer disclosed, while new mappings may be saved under the same
	// scope again. An empty scope returns ErrEmptyScope. The returned error
	// never embeds the scope.
	RevokeScope(ctx context.Context, scope string) error
}

// key is the private comparable composite key identifying one mapping slot.
// Using a struct instead of delimiter concatenation guarantees that arbitrary
// strings containing NUL cannot alias a different (scope, token) tuple.
type key struct {
	scope string
	token string
}

// entry is one stored mapping slot. expiresAt is the wall-clock time after
// which the mapping must no longer be disclosed.
type entry struct {
	original  string
	expiresAt time.Time
}

// Memory is a thread-safe in-memory demo adapter for Vault. It stores plaintext
// originals in volatile memory and is intended only for local demos and tests.
//
// It is NOT a production adapter: it does not survive restart and it holds
// plaintext. Production wiring must use the Encrypted adapter, which stores
// only AES-256-GCM ciphertext.
type Memory struct {
	mu      sync.Mutex
	mapping map[key]entry
	ttl     time.Duration
	now     func() time.Time
}

// NewMemory returns an empty in-memory demo adapter with the given TTL. Every
// mapping saved through it expires ttl after it is first stored. A non-positive
// ttl is rejected with ErrInvalidTTL. The public constructor uses the real
// wall clock; tests should use newMemoryWithClock to control time without
// sleeping.
func NewMemory(ttl time.Duration) (*Memory, error) {
	return newMemoryWithClock(ttl, time.Now)
}

// newMemoryWithClock returns an empty in-memory demo adapter whose expiry
// decisions use the injected clock. It is unexported so production callers use
// the real clock via NewMemory while tests can drive time deterministically.
func newMemoryWithClock(ttl time.Duration, now func() time.Time) (*Memory, error) {
	if ttl <= 0 {
		return nil, ErrInvalidTTL
	}
	return &Memory{
		mapping: make(map[key]entry),
		ttl:     ttl,
		now:     now,
	}, nil
}

// Save records the mapping (scope, token) -> original. See Vault.Save for the
// full contract. It checks ctx cancellation before acquiring the lock. The
// mapping expires ttl after it is first stored; a repeated idempotent save of
// the same original does not refresh the expiry. An already-expired mapping is
// treated as absent: it is replaced by a fresh mapping with a new expiry,
// regardless of whether the original matches.
func (m *Memory) Save(ctx context.Context, scope, token, original string) error {
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

	k := key{scope: scope, token: token}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Re-check cancellation after acquiring the lock so a cancelled operation
	// never writes a mapping.
	if err := ctx.Err(); err != nil {
		return err
	}

	now := m.now()
	if existing, ok := m.mapping[k]; ok {
		if now.Before(existing.expiresAt) {
			if existing.original == original {
				return nil
			}
			return ErrConflict
		}
		// The existing mapping is expired and must not influence the new
		// lifecycle: treat the key as absent and replace it below.
	}
	m.mapping[k] = entry{original: original, expiresAt: now.Add(m.ttl)}
	return nil
}

// Resolve returns the original value for the exact (scope, token) key, or
// ErrNotFound. See Vault.Resolve for the full contract. It checks ctx
// cancellation before acquiring the lock. An expired mapping is not disclosed:
// Resolve returns ErrNotFound and lazily deletes the expired entry.
func (m *Memory) Resolve(ctx context.Context, scope, token string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if scope == "" {
		return "", ErrEmptyScope
	}
	if token == "" {
		return "", ErrEmptyToken
	}

	k := key{scope: scope, token: token}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Re-check cancellation after acquiring the lock so a cancelled operation
	// never reads plaintext or mutates state.
	if err := ctx.Err(); err != nil {
		return "", err
	}

	e, ok := m.mapping[k]
	if !ok {
		return "", ErrNotFound
	}
	if !m.now().Before(e.expiresAt) {
		delete(m.mapping, k)
		return "", ErrNotFound
	}
	return e.original, nil
}

// RevokeScope atomically deletes every mapping belonging to scope. See
// Vault.RevokeScope for the full contract. It checks ctx cancellation before
// acquiring the lock.
func (m *Memory) RevokeScope(ctx context.Context, scope string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if scope == "" {
		return ErrEmptyScope
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Re-check cancellation after acquiring the lock so a cancelled operation
	// never mutates state.
	if err := ctx.Err(); err != nil {
		return err
	}

	for k := range m.mapping {
		if k.scope == scope {
			delete(m.mapping, k)
		}
	}
	return nil
}
