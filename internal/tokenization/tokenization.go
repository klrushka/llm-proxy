// Package tokenization issues cryptographically random scoped opaque tokens
// of the form <PII_TYPE_SUFFIX> for confirmed personal entities. It is the
// token-generation layer of the reversible-tokenization capability.
//
// The generator retains exact (scope, type, value) tuples in volatile memory
// solely to provide deterministic reuse within a scope; it exposes no mapping,
// logs no values, scopes or tokens, and embeds none of that data in the tokens
// it returns. Tokens contain only the canonical type and a random suffix.
//
// Scope of this package (task 7.1): token generation only. Text replacement,
// vault persistence, TTL, revoke and detokenization are separate tasks.
package tokenization

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/klrushka/llm-proxy/internal/detection"
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
