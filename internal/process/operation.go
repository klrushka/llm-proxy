package process

import (
	"context"
	"errors"
)

// MaskFunc masks a payload and returns the masked result. It must never
// return the plaintext payload as the result.
type MaskFunc func(ctx context.Context, payload string) (string, error)

// ErrMaskingFailed is returned when the masking dependency fails. It is a
// safe sentinel that never carries the input payload or any plaintext.
var ErrMaskingFailed = errors.New("process: masking failed")

// ErrConflict is returned when a ready record already exists for a payload_id
// and the incoming payload matches neither the stored original nor the
// previously issued result. It is a safe sentinel that never carries the
// input payload, the stored original, the stored result or any plaintext.
var ErrConflict = errors.New("process: payload conflicts with existing record")

// Operation implements idempotent masking for POST /process. Its Handle
// method is signature-compatible with api.ProcessFunc.
type Operation struct {
	store *Store
	mask  MaskFunc
}

// NewOperation returns an Operation that masks new payloads with mask and
// stores correlation records in store.
func NewOperation(store *Store, mask MaskFunc) *Operation {
	return &Operation{store: store, mask: mask}
}

// Handle processes a request idempotently. For a new payload_id it masks the
// payload exactly once, stores original+result and transitions the record to
// ready. A retry of the same original returns the stored result without
// re-masking. Passing the previously issued result (mask) restores the
// original without re-masking and is a repeatable read that does not change
// the record. A masking error fails closed (no plaintext) and expires the
// claim.
func (o *Operation) Handle(ctx context.Context, req Request) (Response, error) {
	rec, err := o.store.Get(req.PayloadID)
	switch {
	case errors.Is(err, ErrNotFound):
		return o.handleNew(ctx, req)
	case err != nil:
		return Response{}, err
	}

	if rec.State == StateReady {
		switch req.Payload {
		case rec.Original:
			// Idempotent retry of the original: return the stored mask.
			return Response{Result: rec.Result}, nil
		case rec.Result:
			// Restore by previously issued mask: return the original without
			// re-masking. Repeatable read; the record stays ready.
			return Response{Result: rec.Original}, nil
		default:
			// Third unrelated payload for an existing ready record: safe
			// conflict. The record is left unchanged and masking is not
			// re-invoked.
			return Response{}, ErrConflict
		}
	}
	return Response{}, ErrInvalidTransition
}

func (o *Operation) handleNew(ctx context.Context, req Request) (Response, error) {
	if _, err := o.store.CreateClaim(req.PayloadID, req.Payload); err != nil {
		return Response{}, err
	}

	result, err := o.mask(ctx, req.Payload)
	if err != nil {
		// Fail closed: never return plaintext or the dependency error (which
		// may carry the input). Expire the claim safely.
		_ = o.store.Transition(req.PayloadID, StateClaim, StateExpired)
		return Response{}, ErrMaskingFailed
	}

	if err := o.store.CompleteClaim(req.PayloadID, result); err != nil {
		return Response{}, err
	}
	return Response{Result: result}, nil
}
