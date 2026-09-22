package process

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// compile-time check that Operation.Handle is signature-compatible with
// api.ProcessFunc (func(context.Context, Request) (Response, error)).
var _ func(context.Context, Request) (Response, error) = (&Operation{}).Handle

func TestOperationNewPayloadMasksOnce(t *testing.T) {
	calls := 0
	op := NewOperation(NewStore(), func(_ context.Context, payload string) (string, error) {
		calls++
		return "masked:" + payload, nil
	})

	resp, err := op.Handle(context.Background(), Request{Payload: "synthetic text", PayloadID: "id-1"})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if resp.Result != "masked:synthetic text" {
		t.Errorf("Result = %q, want %q", resp.Result, "masked:synthetic text")
	}
	if calls != 1 {
		t.Errorf("mask calls = %d, want 1", calls)
	}

	rec, err := op.store.Get("id-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if rec.State != StateReady {
		t.Errorf("State = %q, want %q", rec.State, StateReady)
	}
	if rec.Original != "synthetic text" {
		t.Errorf("Original = %q, want %q", rec.Original, "synthetic text")
	}
	if rec.Result != "masked:synthetic text" {
		t.Errorf("Result = %q, want %q", rec.Result, "masked:synthetic text")
	}
}

func TestOperationRetrySameOriginalReturnsSavedResult(t *testing.T) {
	calls := 0
	op := NewOperation(NewStore(), func(_ context.Context, payload string) (string, error) {
		calls++
		return "masked:" + payload, nil
	})

	first, err := op.Handle(context.Background(), Request{Payload: "synthetic text", PayloadID: "id-1"})
	if err != nil {
		t.Fatalf("first Handle() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("mask calls after first = %d, want 1", calls)
	}

	second, err := op.Handle(context.Background(), Request{Payload: "synthetic text", PayloadID: "id-1"})
	if err != nil {
		t.Fatalf("retry Handle() error = %v", err)
	}
	if second.Result != first.Result {
		t.Errorf("retry Result = %q, want %q", second.Result, first.Result)
	}
	if calls != 1 {
		t.Errorf("mask calls after retry = %d, want 1 (no re-masking)", calls)
	}
}

func TestOperationMaskingErrorFailsClosed(t *testing.T) {
	const payload = "synthetic text"
	op := NewOperation(NewStore(), func(_ context.Context, _ string) (string, error) {
		return "", errors.New("masking failed: " + payload)
	})

	resp, err := op.Handle(context.Background(), Request{Payload: payload, PayloadID: "id-1"})
	if !errors.Is(err, ErrMaskingFailed) {
		t.Fatalf("Handle() error = %v, want ErrMaskingFailed", err)
	}
	if err != nil && strings.Contains(err.Error(), payload) {
		t.Errorf("returned error leaks payload: %q", err.Error())
	}
	if resp.Result != "" {
		t.Errorf("Result = %q, want empty (no plaintext leak)", resp.Result)
	}

	rec, err := op.store.Get("id-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if rec.State != StateExpired {
		t.Errorf("State = %q, want %q (claim safely terminated)", rec.State, StateExpired)
	}
	if rec.Result != "" {
		t.Errorf("Result = %q, want empty", rec.Result)
	}
}

func TestOperationDistinctPayloadIDsMaskIndependently(t *testing.T) {
	calls := 0
	op := NewOperation(NewStore(), func(_ context.Context, payload string) (string, error) {
		calls++
		return "masked:" + payload, nil
	})

	r1, err := op.Handle(context.Background(), Request{Payload: "alpha", PayloadID: "id-a"})
	if err != nil {
		t.Fatalf("Handle(id-a) error = %v", err)
	}
	r2, err := op.Handle(context.Background(), Request{Payload: "beta", PayloadID: "id-b"})
	if err != nil {
		t.Fatalf("Handle(id-b) error = %v", err)
	}
	if r1.Result != "masked:alpha" || r2.Result != "masked:beta" {
		t.Errorf("results = %q, %q", r1.Result, r2.Result)
	}
	if calls != 2 {
		t.Errorf("mask calls = %d, want 2", calls)
	}
}

func TestOperationRestoreByMaskReturnsOriginal(t *testing.T) {
	calls := 0
	op := NewOperation(NewStore(), func(_ context.Context, payload string) (string, error) {
		calls++
		return "masked:" + payload, nil
	})

	first, err := op.Handle(context.Background(), Request{Payload: "synthetic original", PayloadID: "id-1"})
	if err != nil {
		t.Fatalf("first Handle() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("mask calls after first = %d, want 1", calls)
	}

	restored, err := op.Handle(context.Background(), Request{Payload: first.Result, PayloadID: "id-1"})
	if err != nil {
		t.Fatalf("restore Handle() error = %v", err)
	}
	if restored.Result != "synthetic original" {
		t.Errorf("restored Result = %q, want %q", restored.Result, "synthetic original")
	}
	if calls != 1 {
		t.Errorf("mask calls after restore = %d, want 1 (no re-masking)", calls)
	}
}

func TestOperationRestoreIsRepeatableRead(t *testing.T) {
	calls := 0
	op := NewOperation(NewStore(), func(_ context.Context, payload string) (string, error) {
		calls++
		return "masked:" + payload, nil
	})

	first, err := op.Handle(context.Background(), Request{Payload: "synthetic original", PayloadID: "id-1"})
	if err != nil {
		t.Fatalf("first Handle() error = %v", err)
	}
	before, err := op.store.Get("id-1")
	if err != nil {
		t.Fatalf("Get() before restore error = %v", err)
	}

	for i := 0; i < 2; i++ {
		restored, err := op.Handle(context.Background(), Request{Payload: first.Result, PayloadID: "id-1"})
		if err != nil {
			t.Fatalf("restore %d Handle() error = %v", i, err)
		}
		if restored.Result != "synthetic original" {
			t.Errorf("restore %d Result = %q, want %q", i, restored.Result, "synthetic original")
		}
	}

	if calls != 1 {
		t.Errorf("mask calls = %d, want 1 (no re-masking on restore)", calls)
	}

	after, err := op.store.Get("id-1")
	if err != nil {
		t.Fatalf("Get() after restore error = %v", err)
	}
	if before.PayloadID != after.PayloadID ||
		before.State != after.State ||
		before.Original != after.Original ||
		before.Result != after.Result {
		t.Errorf("record changed after restore:\nbefore = %+v\nafter  = %+v", before, after)
	}
	if after.State != StateReady {
		t.Errorf("State = %q, want %q (record stays ready)", after.State, StateReady)
	}
}

func TestOperationThirdUnrelatedPayloadConflicts(t *testing.T) {
	calls := 0
	op := NewOperation(NewStore(), func(_ context.Context, payload string) (string, error) {
		calls++
		return "masked:" + payload, nil
	})

	first, err := op.Handle(context.Background(), Request{Payload: "synthetic original", PayloadID: "id-1"})
	if err != nil {
		t.Fatalf("first Handle() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("mask calls after first = %d, want 1", calls)
	}
	before, err := op.store.Get("id-1")
	if err != nil {
		t.Fatalf("Get() before conflict error = %v", err)
	}

	// Third unrelated payload: neither the original nor the issued result.
	resp, err := op.Handle(context.Background(), Request{Payload: "unrelated third payload", PayloadID: "id-1"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Handle() error = %v, want ErrConflict", err)
	}
	if resp.Result != "" {
		t.Errorf("Result = %q, want empty (no plaintext leak)", resp.Result)
	}
	if calls != 1 {
		t.Errorf("mask calls after conflict = %d, want 1 (no re-masking)", calls)
	}

	after, err := op.store.Get("id-1")
	if err != nil {
		t.Fatalf("Get() after conflict error = %v", err)
	}
	if before.PayloadID != after.PayloadID ||
		before.State != after.State ||
		before.Original != after.Original ||
		before.Result != after.Result {
		t.Errorf("record changed after conflict:\nbefore = %+v\nafter  = %+v", before, after)
	}
	if after.State != StateReady {
		t.Errorf("State = %q, want %q (record stays ready)", after.State, StateReady)
	}
	if after.Original != "synthetic original" || after.Result != first.Result {
		t.Errorf("record content changed: original=%q result=%q", after.Original, after.Result)
	}
}
