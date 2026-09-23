package process

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestOperationRetentionAndCapacity(t *testing.T) {
	now := time.Unix(100, 0)
	s := NewStoreWithLimits(time.Minute, 1, len("id")+len("original")+len("masked"))
	s.now = func() time.Time { return now }
	op := NewOperation(s, func(context.Context, string) (string, error) { return "masked", nil })
	original := Request{PayloadID: "id", Payload: "original"}
	first, err := op.Handle(context.Background(), original)
	if err != nil || first.Result != "masked" || s.bytes != s.maxBytes {
		t.Fatalf("first: result=%q err=%v bytes=%d", first.Result, err, s.bytes)
	}
	if _, err := op.Handle(context.Background(), Request{PayloadID: "other", Payload: "text"}); !errors.Is(err, ErrStoreCapacity) {
		t.Fatalf("new ID at cap: %v", err)
	}
	if retry, err := op.Handle(context.Background(), original); err != nil || retry != first {
		t.Fatalf("retry at cap: %+v %v", retry, err)
	}
	if _, err := op.Handle(context.Background(), Request{PayloadID: "id", Payload: "other"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict at cap: %v", err)
	}
	if restored, err := op.Handle(context.Background(), Request{PayloadID: "id", Payload: "masked"}); err != nil || restored.Result != "original" {
		t.Fatalf("restore at cap: %+v %v", restored, err)
	}
	now = now.Add(time.Minute - time.Nanosecond)
	if restored, err := op.Handle(context.Background(), Request{PayloadID: "id", Payload: "masked"}); err != nil || restored.Result != "original" {
		t.Fatalf("restore before expiry: %+v %v", restored, err)
	}
	now = now.Add(time.Nanosecond)
	if _, err := s.Get("id"); !errors.Is(err, ErrNotFound) || s.bytes != 0 {
		t.Fatalf("expired: err=%v bytes=%d", err, s.bytes)
	}
	if _, err := op.Handle(context.Background(), Request{PayloadID: "other", Payload: "text"}); err != nil {
		t.Fatalf("capacity after expiry: %v", err)
	}
}

func TestStorePinsClaimAndKeepsFailedGeneration(t *testing.T) {
	now := time.Unix(100, 0)
	s := NewStoreWithLimits(time.Second, 1, 64)
	s.now = func() time.Time { return now }
	old, owner, err := s.begin("id", "original")
	if err != nil || !owner {
		t.Fatalf("begin: %v %v", owner, err)
	}
	now = now.Add(time.Hour)
	if _, _, err := s.begin("other", "text"); !errors.Is(err, ErrStoreCapacity) {
		t.Fatalf("inflight evicted: %v", err)
	}
	if err := s.failEntry(old, ErrModelUnavailable); err != nil {
		t.Fatal(err)
	}
	newEntry, owner, err := s.begin("id", "original")
	if err != nil || !owner || newEntry == old {
		t.Fatalf("retry generation: %v %v", owner, err)
	}
	if _, err := s.waitEntry(context.Background(), old); !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("old waiter: %v", err)
	}
	if e, owner, err := s.begin("id", "different"); err != nil || owner || e != newEntry {
		t.Fatalf("conflicting pending: %v %v", owner, err)
	}
	if err := s.completeEntry(newEntry, "masked"); err != nil {
		t.Fatal(err)
	}
	if len(s.records) != 1 || s.completed.Len() != 1 || s.bytes != len("id")+len("original")+len("masked") {
		t.Fatalf("accounting after retry: entries=%d completed=%d bytes=%d", len(s.records), s.completed.Len(), s.bytes)
	}
}

func TestResultCapacityResolvesWaiters(t *testing.T) {
	s := NewStoreWithLimits(time.Minute, 1, 12)
	e, owner, err := s.begin("id", "text")
	if err != nil || !owner {
		t.Fatalf("begin: %v %v", owner, err)
	}
	if err := s.completeEntry(e, "oversized result"); !errors.Is(err, ErrStoreCapacity) {
		t.Fatalf("complete: %v", err)
	}
	if _, err := s.waitEntry(context.Background(), e); !errors.Is(err, ErrStoreCapacity) {
		t.Fatalf("waiter: %v", err)
	}
	if s.bytes != len("id")+len("text") {
		t.Fatalf("bytes after failed result: %d", s.bytes)
	}
	if _, _, err := s.begin("id", "different"); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting retry: %v", err)
	}
	newEntry, owner, err := s.begin("id", "text")
	if err != nil || !owner || newEntry == e {
		t.Fatalf("retry: %v %v", owner, err)
	}
	if _, err := s.waitEntry(context.Background(), e); !errors.Is(err, ErrStoreCapacity) {
		t.Fatalf("old waiter after retry: %v", err)
	}
}
