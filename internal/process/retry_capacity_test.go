package process

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestTransientModelFailureAllowsOriginalRetry(t *testing.T) {
	const original = "synthetic original"
	var calls atomic.Int32
	op := NewOperation(NewStore(), func(_ context.Context, payload string) (string, error) {
		if calls.Add(1) == 1 {
			return "", ErrModelUnavailable
		}
		return "masked synthetic result", nil
	})
	req := Request{PayloadID: "id-1", Payload: original}
	if resp, err := op.Handle(context.Background(), req); !errors.Is(err, ErrModelUnavailable) || resp.Result != "" {
		t.Fatalf("first attempt = (%q, %v), want safe transient failure", resp.Result, err)
	}
	if resp, err := op.Handle(context.Background(), Request{PayloadID: "id-1", Payload: "different synthetic input"}); !errors.Is(err, ErrConflict) || resp.Result != "" {
		t.Fatalf("conflicting retry = (%q, %v), want safe 409 classification", resp.Result, err)
	}
	resp, err := op.Handle(context.Background(), req)
	if err != nil || resp.Result != "masked synthetic result" {
		t.Fatalf("retry = (%q, %v), want successful remask", resp.Result, err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("mask calls = %d, want 2", got)
	}
}
