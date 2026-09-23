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

// ErrReviewRequired marks an ambiguous entity that needs review. It never
// carries the entity value, text, offsets or source-specific details.
var ErrReviewRequired = errors.New("process: review required")

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
// the record. A masking error fails closed (no plaintext) and expires that
// attempt. A later retry of the same original may replace a transient model
// or cancellation failure. Concurrent requests are coordinated by
// a single writer: only the goroutine that wins the claim invokes the masker,
// and the others wait for the winner to resolve before classifying their
// payload against the stored record.
func (o *Operation) Handle(ctx context.Context, req Request) (Response, error) {
	e, owner, err := o.store.begin(req.PayloadID, req.Payload)
	if err != nil {
		return Response{}, err
	}
	if owner {
		return o.handleFirst(ctx, req, e)
	}
	rec, err := o.store.snapshotEntry(e)
	if err != nil {
		return Response{}, err
	}
	if rec.State == StateClaim {
		return o.handleWaiter(ctx, req, e)
	}
	return o.classifyReady(req, rec)
}

// handleFirst is the single-writer path for one claim generation. It invokes
// the masker once and resolves only its own entry to ready or expired.
func (o *Operation) handleFirst(ctx context.Context, req Request, e *entry) (Response, error) {
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
		case errors.Is(err, ErrReviewRequired):
			failErr = ErrReviewRequired
		case errors.Is(err, ErrModelUnavailable):
			failErr = ErrModelUnavailable
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			failErr = ErrModelUnavailable
		}
		if expireErr := o.store.failEntry(e, failErr); expireErr != nil {
			// The claim could not be safely expired; fail closed with an empty
			// response and the store error rather than leaking the mask error.
			return Response{}, expireErr
		}
		return Response{}, failErr
	}

	if err := o.store.completeEntry(e, result); err != nil {
		return Response{}, err
	}
	return Response{Result: result}, nil
}

// handleWaiter blocks on the exact claim generation it observed, so a later
// retry cannot turn the old attempt's failure into a new result.
func (o *Operation) handleWaiter(ctx context.Context, req Request, e *entry) (Response, error) {
	rec, err := o.store.waitEntry(ctx, e)
	if err != nil {
		if req.Payload != e.rec.Original && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			return Response{}, ErrConflict
		}
		return Response{}, err
	}
	return o.classifyReady(req, rec)
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
