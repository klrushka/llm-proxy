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

// ErrVaultUnavailable is returned when the masking dependency reports that
// the vault is unavailable. It is a safe sentinel that never carries the
// input payload, the wrapped dependency error, its message or any plaintext.
var ErrVaultUnavailable = errors.New("process: vault unavailable")

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
// claim. Concurrent first requests for the same payload_id are coordinated by
// a single writer: only the goroutine that wins the claim invokes the masker,
// and the others wait for the winner to resolve before classifying their
// payload against the stored record.
func (o *Operation) Handle(ctx context.Context, req Request) (Response, error) {
	rec, err := o.store.Get(req.PayloadID)
	switch {
	case errors.Is(err, ErrNotFound):
		return o.handleFirst(ctx, req)
	case err != nil:
		return Response{}, err
	}

	switch rec.State {
	case StateReady:
		return o.classifyReady(req, rec)
	case StateClaim:
		// A concurrent first request holds the claim. Wait for it to resolve,
		// then classify against the resolved record.
		return o.handleWaiter(ctx, req)
	case StateExpired:
		// A later retry that encounters an already-expired record returns the
		// recorded safe classification. If no classified operation failure was
		// recorded, preserve the existing invalid-transition behavior.
		return Response{}, o.store.expiredFailure(req.PayloadID)
	default:
		return Response{}, ErrInvalidTransition
	}
}

// handleFirst is the single-writer path for a new payload_id. It wins the
// claim, invokes the masker exactly once, and resolves the claim to ready or
// expired. If it loses the claim race to a concurrent first request, it waits
// for the winner to resolve instead of surfacing ErrExists.
func (o *Operation) handleFirst(ctx context.Context, req Request) (Response, error) {
	if _, err := o.store.CreateClaim(req.PayloadID, req.Payload); err != nil {
		if errors.Is(err, ErrExists) {
			return o.handleWaiter(ctx, req)
		}
		return Response{}, err
	}

	result, err := o.mask(ctx, req.Payload)
	if err != nil {
		// Fail closed: never return plaintext or the dependency error (which
		// may carry the input). Classify the failure to a safe sentinel and
		// record it on the private store entry so waiters and later retries
		// observe the same classification. Expire the claim safely and wake
		// waiters exactly once.
		failErr := ErrMaskingFailed
		switch {
		case errors.Is(err, ErrVaultUnavailable):
			failErr = ErrVaultUnavailable
		case errors.Is(err, ErrModelUnavailable):
			failErr = ErrModelUnavailable
		}
		if expireErr := o.store.expireClaim(req.PayloadID, failErr); expireErr != nil {
			// The claim could not be safely expired; fail closed with an empty
			// response and the store error rather than leaking the mask error.
			return Response{}, expireErr
		}
		return Response{}, failErr
	}

	if err := o.store.CompleteClaim(req.PayloadID, result); err != nil {
		return Response{}, err
	}
	return Response{Result: result}, nil
}

// handleWaiter blocks until the in-flight claim for req.PayloadID resolves,
// then classifies the request against the resolved record. It never invokes
// the masker. If the winning claim expired because masking failed, it fails
// closed with the recorded safe classification and an empty response.
func (o *Operation) handleWaiter(ctx context.Context, req Request) (Response, error) {
	rec, err := o.store.WaitReady(ctx, req.PayloadID)
	if err != nil {
		return Response{}, err
	}
	switch rec.State {
	case StateReady:
		return o.classifyReady(req, rec)
	case StateExpired:
		// The winning claim failed closed; expired is terminal and must not be
		// re-claimed from this call. Return the recorded safe classification.
		return Response{}, o.store.expiredFailure(req.PayloadID)
	default:
		return Response{}, ErrInvalidTransition
	}
}

// classifyReady applies the ready-record semantics shared by the direct path
// and waiters: original retry returns the stored mask, restore by the issued
// mask returns the original, and any other payload is a safe conflict.
func (o *Operation) classifyReady(req Request, rec *Record) (Response, error) {
	switch req.Payload {
	case rec.Original:
		// Idempotent retry of the original: return the stored mask.
		return Response{Result: rec.Result}, nil
	case rec.Result:
		// Restore by previously issued mask: return the original without
		// re-masking. Repeatable read; the record stays ready.
		return Response{Result: rec.Original}, nil
	default:
		// Third unrelated payload for an existing ready record: safe conflict.
		// The record is left unchanged and masking is not re-invoked.
		return Response{}, ErrConflict
	}
}
