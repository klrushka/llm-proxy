// Package vault stores reversible token mappings keyed by the exact composite
// key (scope_id, token) and resolves them back to the original value. It is the
// persistence layer of the reversible-tokenization capability.
//
// The package defines a Vault interface plus a thread-safe in-memory demo
// adapter. The demo adapter stores plaintext originals in volatile memory and
// is intended only for local demos and tests; it does not survive restart.
//
// IMPORTANT: A persistent adapter MUST store only AES-256-GCM ciphertext and
// never plaintext originals, token mappings, keys or any reversible material.
// The persistent adapter is intentionally NOT implemented here; it is a later
// task. The interface below is the contract that such an adapter must satisfy.
//
// Scope of this package (task 7.3): the Vault interface and the in-memory demo
// adapter. TTL, scope revoke, detokenization and HTTP integration are separate
// tasks and are not implemented here.
package vault

import (
	"context"
	"errors"
	"sync"
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
	// scope, so a cross-scope token is never disclosed.
	ErrNotFound = errors.New("vault: mapping not found")
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
	// is never disclosed: Resolve returns ErrNotFound for it. The returned
	// error never embeds the scope or token.
	Resolve(ctx context.Context, scope, token string) (string, error)
}

// key is the private comparable composite key identifying one mapping slot.
// Using a struct instead of delimiter concatenation guarantees that arbitrary
// strings containing NUL cannot alias a different (scope, token) tuple.
type key struct {
	scope string
	token string
}

// Memory is a thread-safe in-memory demo adapter for Vault. It stores plaintext
// originals in volatile memory and is intended only for local demos and tests.
//
// It is NOT a persistent adapter: it does not survive restart and it holds
// plaintext. A persistent adapter MUST store only AES-256-GCM ciphertext and is
// intentionally not implemented here.
type Memory struct {
	mu      sync.Mutex
	mapping map[key]string
}

// NewMemory returns an empty in-memory demo adapter.
func NewMemory() *Memory {
	return &Memory{mapping: make(map[key]string)}
}

// Save records the mapping (scope, token) -> original. See Vault.Save for the
// full contract. It checks ctx cancellation before acquiring the lock.
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

	if existing, ok := m.mapping[k]; ok {
		if existing == original {
			return nil
		}
		return ErrConflict
	}
	m.mapping[k] = original
	return nil
}

// Resolve returns the original value for the exact (scope, token) key, or
// ErrNotFound. See Vault.Resolve for the full contract. It checks ctx
// cancellation before acquiring the lock.
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

	original, ok := m.mapping[k]
	if !ok {
		return "", ErrNotFound
	}
	return original, nil
}
