// Package runtime implements the minimal Go runtime coordinator for the
// product flow: user request -> mask/tokenize -> configurable LLM boundary ->
// demask/detokenize -> user response.
//
// The coordinator is a deep module with three narrow injected boundaries:
// protection/tokenization, an LLM client, and restoration/detokenization. It
// calls them strictly in that order, passes only protected text to the LLM,
// restores the LLM output before returning it, and fails closed with an empty
// user result on any stage error. It never logs or embeds original text,
// protected text, tokens, mappings, upstream error detail or partial output.
package runtime

import (
	"context"
	"errors"

	"github.com/klrushka/llm-proxy/internal/tokenization"
)

// Protector masks/tokenizes user text into protected text for a scope. It is
// the protection/tokenization boundary. Implementations must never return the
// plaintext user text as the protected result.
type Protector func(ctx context.Context, scope, text string) (string, error)

// LLMClient is the configurable LLM boundary. It receives only protected text
// and returns a modified protected text that preserves the opaque tokens so
// they can be restored afterwards. Implementations must never receive or
// return original plaintext values.
type LLMClient func(ctx context.Context, protected string) (string, error)

// Restorer demasks/detokenizes protected text back to user text for a scope.
// It is the restoration/detokenization boundary and must not rerun detection.
type Restorer func(ctx context.Context, scope, protected string) (string, error)

// Stable sentinel errors returned by Coordinator.Run. They are fixed and safe:
// none of them embeds original text, protected text, a token, a mapping, an
// upstream error message or partial output. Callers can classify the failing
// stage with errors.Is without string matching.
var (
	// ErrProtectFailed reports a failure in the protection/tokenization stage
	// (including tokenization validation and vault persistence).
	ErrProtectFailed = errors.New("runtime: protection failed")
	// ErrLLMFailed reports a failure in the LLM boundary.
	ErrLLMFailed = errors.New("runtime: llm failed")
	// ErrRestoreFailed reports a failure in the restoration/detokenization
	// stage.
	ErrRestoreFailed = errors.New("runtime: restoration failed")
)

// Coordinator orchestrates the product flow. It is safe for concurrent use as
// long as the injected boundaries are safe for concurrent use.
type Coordinator struct {
	protect Protector
	llm     LLMClient
	restore Restorer
}

// New returns a Coordinator that runs the three injected boundaries strictly
// in order: protect, then LLM, then restore. A nil boundary fails closed with
// the corresponding safe sentinel error and an empty result.
func New(protect Protector, llm LLMClient, restore Restorer) *Coordinator {
	return &Coordinator{protect: protect, llm: llm, restore: restore}
}

// Run executes the product flow for the given scope and user text.
//
// It calls protect, then llm with only the protected text, then restore on the
// LLM output, and returns the restored user text. On any stage error it fails
// closed: it returns an empty result and a fixed safe sentinel error that never
// embeds original text, protected text, a token, a mapping, an upstream error
// message or partial output. A nil boundary is treated as a failure of its
// stage.
func (c *Coordinator) Run(ctx context.Context, scope, text string) (string, error) {
	if c.protect == nil {
		return "", ErrProtectFailed
	}
	protected, err := c.protect(ctx, scope, text)
	if err != nil {
		return "", ErrProtectFailed
	}

	if c.llm == nil {
		return "", ErrLLMFailed
	}
	llmOut, err := c.llm(ctx, protected)
	if err != nil {
		return "", ErrLLMFailed
	}

	// Fail closed if the LLM altered the opaque token sequence: it must not
	// remove, duplicate, reorder, replace or inject a token from the same
	// scope before restoration. Ordinary surrounding text may change freely.
	if !tokenization.TokensMatch(protected, llmOut) {
		return "", ErrLLMFailed
	}

	if c.restore == nil {
		return "", ErrRestoreFailed
	}
	restored, err := c.restore(ctx, scope, llmOut)
	if err != nil {
		return "", ErrRestoreFailed
	}
	return restored, nil
}
