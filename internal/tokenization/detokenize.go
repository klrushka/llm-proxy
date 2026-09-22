// Package tokenization detokenizes scoped opaque tokens back to their original
// values without running NER. It is the restore layer of the
// reversible-tokenization capability.
//
// Detokenize recognizes only exact token substrings of the form
// <PII_TYPE_32-lowercase-hex> issued by task 7.1, resolves each unique token at
// most once through a narrow Resolver, and restores known tokens right to left
// by UTF-8 byte offsets so the surrounding text stays intact. It never runs
// detection, NER or a model client, and it never logs or embeds tokens or
// plaintext values in errors.
//
// Scope of this file (task 7.5): strict/preserve detokenization. HTTP,
// integration and runtime coordinator wiring are separate tasks.
package tokenization

import (
	"context"
	"errors"
	"regexp"
	"sort"

	"github.com/klrushka/llm-proxy/internal/vault"
)

// Mode selects how Detokenize handles tokens that cannot be resolved.
type Mode string

const (
	// ModeStrict fails the whole operation if any token cannot be resolved.
	// No partial text, mappings or values are returned.
	ModeStrict Mode = "strict"
	// ModePreserve leaves unresolved tokens byte-for-byte unchanged and reports
	// them in DetokenizeResult.UnresolvedTokens in order of first appearance.
	ModePreserve Mode = "preserve"
)

// Resolver resolves a scoped token back to its original value. *vault.Memory
// satisfies this interface. It is deliberately narrow: detokenization only
// needs read access and never saves or revokes mappings.
type Resolver interface {
	Resolve(ctx context.Context, scope, token string) (string, error)
}

// DetokenizeResult is the outcome of a detokenization pass.
type DetokenizeResult struct {
	// Text is the restored text. In strict mode it is empty on any failure.
	Text string
	// UnresolvedTokens lists tokens that could not be resolved, in order of
	// first appearance, with no duplicates. It is non-empty only in preserve
	// mode.
	UnresolvedTokens []string
}

// Stable sentinel errors returned by Detokenize. None of them embeds a scope,
// token or plaintext value.
var (
	// ErrInvalidMode reports a mode other than ModeStrict or ModePreserve.
	ErrInvalidMode = errors.New("tokenization: invalid detokenize mode")
	// ErrNilResolver reports a nil resolver.
	ErrNilResolver = errors.New("tokenization: nil resolver")
	// ErrUnresolved reports that a token could not be resolved in strict mode.
	// It never embeds the token or any plaintext.
	ErrUnresolved = errors.New("tokenization: unresolved token")
	// ErrResolverFailure reports that the resolver failed with an error other
	// than vault.ErrNotFound. The returned error has a fixed safe Error() that
	// never embeds the underlying message, token, scope or plaintext, while
	// errors.Is still classifies the original cause.
	ErrResolverFailure = errors.New("tokenization: resolver failure")
)

// resolverError wraps a resolver failure so the returned error has a fixed,
// safe Error() text while preserving causal classification through errors.Is.
// It never exposes the underlying message, token, scope or plaintext value.
type resolverError struct {
	cause error
}

func (e *resolverError) Error() string {
	return ErrResolverFailure.Error()
}

func (e *resolverError) Unwrap() error {
	return e.cause
}

func (e *resolverError) Is(target error) bool {
	return target == ErrResolverFailure
}

// tokenPattern matches exactly the token format issued by task 7.1:
// <PII_TYPE_32-lowercase-hex>, where the canonical type contains only A-Z and
// underscore and the suffix is exactly 32 lowercase hex characters.
var tokenPattern = regexp.MustCompile(`<[A-Z_]+_[0-9a-f]{32}>`)

// Detokenize restores tokens in text for the given scope without running NER.
//
// It first finds every token occurrence, then resolves each unique token at
// most once through resolver. A repeated token is restored in all of its
// occurrences from that single lookup. Replacements are applied right to left
// by byte offsets so earlier offsets remain valid and UTF-8 text is preserved.
//
// In strict mode, any token that cannot be resolved (unknown, cross-scope,
// expired or revoked, all reported as vault.ErrNotFound) fails the whole
// operation with ErrUnresolved and no partial result. In preserve mode such
// tokens remain byte-for-byte unchanged and are reported in
// UnresolvedTokens in order of first appearance.
//
// Any resolver error other than vault.ErrNotFound (including context
// cancellation) fails closed in both modes: no partial result is returned and
// the error never embeds a token or plaintext value.
//
// Text without any token is returned unchanged and resolver is not called.
// An invalid mode, empty scope or nil resolver is rejected with a stable
// sentinel error.
func Detokenize(ctx context.Context, text, scope string, mode Mode, resolver Resolver) (DetokenizeResult, error) {
	var out DetokenizeResult
	if mode != ModeStrict && mode != ModePreserve {
		return out, ErrInvalidMode
	}
	if scope == "" {
		return out, ErrEmptyScope
	}
	if resolver == nil {
		return out, ErrNilResolver
	}

	occ := tokenPattern.FindAllStringIndex(text, -1)
	if len(occ) == 0 {
		out.Text = text
		return out, nil
	}

	// Unique tokens in order of first appearance.
	var unique []string
	seen := make(map[string]struct{})
	for _, m := range occ {
		tok := text[m[0]:m[1]]
		if _, ok := seen[tok]; !ok {
			seen[tok] = struct{}{}
			unique = append(unique, tok)
		}
	}

	// Resolve each unique token at most once.
	resolved := make(map[string]string, len(unique))
	var unresolved []string
	for _, tok := range unique {
		val, err := resolver.Resolve(ctx, scope, tok)
		if err != nil {
			if errors.Is(err, vault.ErrNotFound) {
				if mode == ModeStrict {
					return out, ErrUnresolved
				}
				unresolved = append(unresolved, tok)
				continue
			}
			// Any other resolver error (including context cancellation) fails
			// closed in both modes without a partial result. It is wrapped so
			// the returned error has a fixed safe Error() text that never
			// embeds the underlying message, token, scope or plaintext, while
			// errors.Is still classifies the original cause.
			return out, &resolverError{cause: err}
		}
		resolved[tok] = val
	}

	// Apply replacements right to left so earlier byte offsets stay valid.
	type repl struct {
		start, end int
		value      string
	}
	repls := make([]repl, 0, len(occ))
	for _, m := range occ {
		tok := text[m[0]:m[1]]
		if val, ok := resolved[tok]; ok {
			repls = append(repls, repl{start: m[0], end: m[1], value: val})
		}
	}
	sort.SliceStable(repls, func(i, j int) bool {
		return repls[i].start > repls[j].start
	})

	buf := []byte(text)
	for _, r := range repls {
		buf = replaceRange(buf, r.start, r.end, r.value)
	}

	out.Text = string(buf)
	out.UnresolvedTokens = unresolved
	return out, nil
}
