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

func TestTransientGenerationKeepsOldWaiterFailure(t *testing.T) {
	s := NewStore()
	old, owner, err := s.begin("id", "synthetic original")
	if err != nil || !owner {
		t.Fatalf("first begin = (%v, %v)", owner, err)
	}
	if err := s.failEntry(old, ErrModelUnavailable); err != nil {
		t.Fatal(err)
	}
	newEntry, owner, err := s.begin("id", "synthetic original")
	if err != nil || !owner || newEntry == old {
		t.Fatalf("retry begin = (%v, %v)", owner, err)
	}
	if _, err := s.waitEntry(context.Background(), old); !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("old waiter error = %v, want first attempt failure", err)
	}
	if conflicting, owner, err := s.begin("id", "different synthetic"); err != nil || owner || conflicting != newEntry {
		t.Fatalf("conflicting in-flight request must observe current writer: (%v, %v)", owner, err)
	}
	if err := s.completeEntry(newEntry, "masked synthetic"); err != nil {
		t.Fatal(err)
	}
	if rec, err := s.snapshotEntry(newEntry); err != nil || rec.Result != "masked synthetic" {
		t.Fatalf("new ready = (%v, %v)", rec, err)
	}
}

func TestCanceledMaskAllowsOriginalRetry(t *testing.T) {
	var calls atomic.Int32
	op := NewOperation(NewStore(), func(ctx context.Context, _ string) (string, error) {
		if calls.Add(1) == 1 {
			return "", ctx.Err()
		}
		return "masked synthetic", nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := Request{PayloadID: "id", Payload: "synthetic original"}
	if resp, err := op.Handle(ctx, req); !errors.Is(err, ErrModelUnavailable) || resp.Result != "" {
		t.Fatalf("canceled first = (%q, %v)", resp.Result, err)
	}
	if resp, err := op.Handle(context.Background(), req); err != nil || resp.Result != "masked synthetic" {
		t.Fatalf("retry = (%q, %v)", resp.Result, err)
	}
}
