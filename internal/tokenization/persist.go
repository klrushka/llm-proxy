package tokenization

import (
	"context"
	"errors"

	"github.com/klrushka/llm-proxy/internal/ownership"
	"github.com/klrushka/llm-proxy/internal/vault"
)

// ErrNilVault reports a nil vault passed to ReplaceAndPersist. It never embeds
// a scope, token, original value or text.
var ErrNilVault = errors.New("tokenization: nil vault")

// ReplaceAndPersist tokenizes confirmed personal entities in text for the given
// scope and persists every mapping to the vault before returning any tokenized
// text.
//
// It first obtains replacements via Replace, then saves each (scope, token) ->
// original mapping through vault.Save. The original value is extracted only from
// byte offsets that Replace has already validated against text, so no unverified
// offset is ever sliced. The tokenized result is returned only after every
// mapping has been saved successfully.
//
// On any Save failure the returned ReplaceResult is strictly empty: Text == ""
// and no replacements, so neither a full nor a partial tokenized text reaches
// the caller. No rollback is attempted because the Vault interface does not
// expose one; the guarantee of this operation is that tokenized text is never
// disclosed on failure. A nil vault returns ErrNilVault. A cancelled context is
// propagated from vault.Save as its wrapped context error. Errors returned here
// never embed the text, original value, token or scope.
func ReplaceAndPersist(ctx context.Context, text, scope string, results []ownership.Entity, issuer TokenIssuer, v vault.Vault) (ReplaceResult, error) {
	var out ReplaceResult
	if v == nil {
		return out, ErrNilVault
	}

	res, err := Replace(text, scope, results, issuer)
	if err != nil {
		return out, err
	}
	if len(res.Replacements) == 0 {
		return res, nil
	}

	for _, r := range res.Replacements {
		// Replace has already validated r.Entity offsets against text; re-check
		// defensively so an unexpected invalid span fails closed instead of
		// panicking on a slice.
		if !validSpan(text, r.Entity.Start, r.Entity.End) {
			return out, ErrInvalidSpan
		}
		original := text[r.Entity.Start:r.Entity.End]
		if err := v.Save(ctx, scope, r.Token, original); err != nil {
			return out, err
		}
	}
	return res, nil
}
