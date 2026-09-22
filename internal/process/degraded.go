package process

import (
	"context"
	"errors"
)

// ErrModelUnavailable is returned when the primary model-backed masker
// reports that the model worker is unavailable. It is a safe sentinel that
// never carries the input payload, the wrapped dependency error, its message
// or any plaintext.
var ErrModelUnavailable = errors.New("process: model worker unavailable")

// WithRulesOnlyFallback composes a primary model-backed masker with a
// rules-only fallback, gated by an explicit consumer capability. The boolean
// represents the already-defined consumer policy capability
// (policy.Policy.AllowRulesOnlyDegraded); transport resolution of that policy
// is a later task and is not performed here. The returned MaskFunc never
// returns the plaintext payload as the result.
//
// Behavior:
//   - primary success returns its result; rulesOnly is never invoked.
//   - primary error matching ErrModelUnavailable invokes rulesOnly exactly
//     once only when allowRulesOnlyDegraded is true and rulesOnly is non-nil;
//     otherwise it fails closed with an empty result and the exact bare
//     ErrModelUnavailable sentinel.
//   - primary generic error never invokes rulesOnly and fails closed with an
//     empty result and the exact bare ErrMaskingFailed sentinel.
//   - an ErrVaultUnavailable classification is preserved as the exact bare
//     vault sentinel.
//   - a nil primary fails closed with an empty result and the exact bare
//     ErrMaskingFailed sentinel; it never panics.
//   - a rulesOnly failure fails closed with an empty result sanitized to an
//     exact bare safe sentinel (vault, model-unavailable, or generic masking
//     failed); a wrapped dependency error or a non-empty dependency result is
//     never retained or returned.
//   - only model-unavailability from primary may trigger fallback; no other
//     error may do so.
func WithRulesOnlyFallback(allowRulesOnlyDegraded bool, primary, rulesOnly MaskFunc) MaskFunc {
	return func(ctx context.Context, payload string) (string, error) {
		if primary == nil {
			return "", ErrMaskingFailed
		}

		result, err := primary(ctx, payload)
		if err == nil {
			return result, nil
		}

		if errors.Is(err, ErrModelUnavailable) {
			if allowRulesOnlyDegraded && rulesOnly != nil {
				// Invoke rulesOnly exactly once. Its result is returned only on
				// success; on failure any non-empty result is discarded and the
				// error is sanitized to an exact bare safe sentinel.
				result, err = rulesOnly(ctx, payload)
				if err == nil {
					return result, nil
				}
				return "", sanitizeMaskError(err)
			}
			return "", ErrModelUnavailable
		}

		return "", sanitizeMaskError(err)
	}
}

// sanitizeMaskError maps a masking dependency error to an exact bare safe
// sentinel. It never retains or returns the wrapped dependency error, its
// message, or any plaintext.
func sanitizeMaskError(err error) error {
	switch {
	case errors.Is(err, ErrVaultUnavailable):
		return ErrVaultUnavailable
	case errors.Is(err, ErrModelUnavailable):
		return ErrModelUnavailable
	default:
		return ErrMaskingFailed
	}
}
