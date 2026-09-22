package process

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCreateClaim(t *testing.T) {
	s := NewStore()
	rec, err := s.CreateClaim("id-1", "synthetic original")
	if err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}
	if rec.PayloadID != "id-1" {
		t.Errorf("PayloadID = %q, want %q", rec.PayloadID, "id-1")
	}
	if rec.State != StateClaim {
		t.Errorf("State = %q, want %q", rec.State, StateClaim)
	}
	if rec.Original != "synthetic original" {
		t.Errorf("Original = %q, want %q", rec.Original, "synthetic original")
	}
}

func TestCreateClaimDuplicate(t *testing.T) {
	s := NewStore()
	if _, err := s.CreateClaim("id-1", "first"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}
	if _, err := s.CreateClaim("id-1", "second"); !errors.Is(err, ErrExists) {
		t.Errorf("CreateClaim() error = %v, want ErrExists", err)
	}
}

func TestGetNotFound(t *testing.T) {
	s := NewStore()
	if _, err := s.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestGetReturnsRecord(t *testing.T) {
	s := NewStore()
	if _, err := s.CreateClaim("id-1", "synthetic original"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}
	rec, err := s.Get("id-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if rec.State != StateClaim {
		t.Errorf("State = %q, want %q", rec.State, StateClaim)
	}
}

func TestSnapshotMutationDoesNotAffectStore(t *testing.T) {
	s := NewStore()
	if _, err := s.CreateClaim("id-1", "synthetic original"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}
	snap, err := s.Get("id-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	snap.State = StateExpired
	snap.Original = "mutated"
	snap.Result = "mutated"

	rec, err := s.Get("id-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if rec.State != StateClaim {
		t.Errorf("stored State = %q, want %q", rec.State, StateClaim)
	}
	if rec.Original != "synthetic original" {
		t.Errorf("stored Original = %q, want %q", rec.Original, "synthetic original")
	}
	if rec.Result != "" {
		t.Errorf("stored Result = %q, want empty", rec.Result)
	}
}

func TestTransitionClaimToReady(t *testing.T) {
	s := NewStore()
	if _, err := s.CreateClaim("id-1", "synthetic original"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}
	if err := s.Transition("id-1", StateClaim, StateReady); err != nil {
		t.Fatalf("Transition(claim->ready) error = %v", err)
	}
	rec, err := s.Get("id-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if rec.State != StateReady {
		t.Errorf("State = %q, want %q", rec.State, StateReady)
	}
}

func TestTransitionClaimToExpired(t *testing.T) {
	s := NewStore()
	if _, err := s.CreateClaim("id-1", "synthetic original"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}
	if err := s.Transition("id-1", StateClaim, StateExpired); err != nil {
		t.Fatalf("Transition(claim->expired) error = %v", err)
	}
	rec, err := s.Get("id-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if rec.State != StateExpired {
		t.Errorf("State = %q, want %q", rec.State, StateExpired)
	}
}

func TestTransitionReadyToExpired(t *testing.T) {
	s := NewStore()
	if _, err := s.CreateClaim("id-1", "synthetic original"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}
	if err := s.Transition("id-1", StateClaim, StateReady); err != nil {
		t.Fatalf("Transition(claim->ready) error = %v", err)
	}
	if err := s.Transition("id-1", StateReady, StateExpired); err != nil {
		t.Fatalf("Transition(ready->expired) error = %v", err)
	}
}

func TestCompleteClaim(t *testing.T) {
	s := NewStore()
	if _, err := s.CreateClaim("id-1", "synthetic original"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}
	if err := s.CompleteClaim("id-1", "masked result"); err != nil {
		t.Fatalf("CompleteClaim() error = %v", err)
	}
	rec, err := s.Get("id-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if rec.State != StateReady {
		t.Errorf("State = %q, want %q", rec.State, StateReady)
	}
	if rec.Result != "masked result" {
		t.Errorf("Result = %q, want %q", rec.Result, "masked result")
	}
	if rec.Original != "synthetic original" {
		t.Errorf("Original = %q, want %q", rec.Original, "synthetic original")
	}
}

func TestCompleteClaimNotFound(t *testing.T) {
	s := NewStore()
	if err := s.CompleteClaim("missing", "result"); !errors.Is(err, ErrNotFound) {
		t.Errorf("CompleteClaim() error = %v, want ErrNotFound", err)
	}
}

func TestCompleteClaimNotInClaimState(t *testing.T) {
	s := NewStore()
	if _, err := s.CreateClaim("id-1", "synthetic original"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}
	if err := s.Transition("id-1", StateClaim, StateReady); err != nil {
		t.Fatalf("Transition(claim->ready) error = %v", err)
	}
	if err := s.CompleteClaim("id-1", "result"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("CompleteClaim() error = %v, want ErrInvalidTransition", err)
	}
}

func TestTransitionNotFound(t *testing.T) {
	s := NewStore()
	if err := s.Transition("missing", StateClaim, StateReady); !errors.Is(err, ErrNotFound) {
		t.Errorf("Transition() error = %v, want ErrNotFound", err)
	}
}

func TestTransitionForbidden(t *testing.T) {
	cases := []struct {
		name string
		from RecordState
		to   RecordState
	}{
		{"claim to claim", StateClaim, StateClaim},
		{"ready to claim", StateReady, StateClaim},
		{"ready to ready", StateReady, StateReady},
		{"expired to claim", StateExpired, StateClaim},
		{"expired to ready", StateExpired, StateReady},
		{"expired to expired", StateExpired, StateExpired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewStore()
			if _, err := s.CreateClaim("id-1", "synthetic original"); err != nil {
				t.Fatalf("CreateClaim() error = %v", err)
			}
			if err := s.Transition("id-1", c.from, c.to); !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("Transition(%q->%q) error = %v, want ErrInvalidTransition", c.from, c.to, err)
			}
		})
	}
}

func TestTransitionWrongCurrentState(t *testing.T) {
	s := NewStore()
	if _, err := s.CreateClaim("id-1", "synthetic original"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}
	if err := s.Transition("id-1", StateReady, StateExpired); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("Transition(ready->expired) on claim record error = %v, want ErrInvalidTransition", err)
	}
}

func TestCanTransition(t *testing.T) {
	cases := []struct {
		from RecordState
		to   RecordState
		want bool
	}{
		{StateClaim, StateReady, true},
		{StateClaim, StateExpired, true},
		{StateReady, StateExpired, true},
		{StateClaim, StateClaim, false},
		{StateReady, StateClaim, false},
		{StateReady, StateReady, false},
		{StateExpired, StateClaim, false},
		{StateExpired, StateReady, false},
		{StateExpired, StateExpired, false},
	}
	for _, c := range cases {
		if got := CanTransition(c.from, c.to); got != c.want {
			t.Errorf("CanTransition(%q, %q) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestWaitReadyNotFound(t *testing.T) {
	s := NewStore()
	if _, err := s.WaitReady(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("WaitReady() error = %v, want ErrNotFound", err)
	}
}

func TestWaitReadyImmediateReady(t *testing.T) {
	s := NewStore()
	if _, err := s.CreateClaim("id-1", "synthetic original"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}
	if err := s.CompleteClaim("id-1", "masked result"); err != nil {
		t.Fatalf("CompleteClaim() error = %v", err)
	}
	rec, err := s.WaitReady(context.Background(), "id-1")
	if err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	if rec.State != StateReady || rec.Result != "masked result" {
		t.Errorf("WaitReady() = %+v, want ready with result", rec)
	}
}

func TestWaitReadyImmediateExpired(t *testing.T) {
	s := NewStore()
	if _, err := s.CreateClaim("id-1", "synthetic original"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}
	if err := s.Transition("id-1", StateClaim, StateExpired); err != nil {
		t.Fatalf("Transition(claim->expired) error = %v", err)
	}
	rec, err := s.WaitReady(context.Background(), "id-1")
	if err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	if rec.State != StateExpired {
		t.Errorf("WaitReady() State = %q, want %q", rec.State, StateExpired)
	}
}

func TestWaitReadyBlocksUntilCompleteClaim(t *testing.T) {
	s := NewStore()
	if _, err := s.CreateClaim("id-1", "synthetic original"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}

	started := make(chan struct{})
	got := make(chan *Record, 1)
	go func() {
		close(started)
		rec, err := s.WaitReady(context.Background(), "id-1")
		if err != nil {
			got <- nil
			return
		}
		got <- rec
	}()

	<-started
	select {
	case <-got:
		t.Fatal("WaitReady returned before the claim resolved")
	default:
	}

	if err := s.CompleteClaim("id-1", "masked result"); err != nil {
		t.Fatalf("CompleteClaim() error = %v", err)
	}
	rec := <-got
	if rec == nil || rec.State != StateReady || rec.Result != "masked result" {
		t.Errorf("WaitReady() = %+v, want ready with result", rec)
	}
}

func TestWaitReadyBlocksUntilExpired(t *testing.T) {
	s := NewStore()
	if _, err := s.CreateClaim("id-1", "synthetic original"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}

	started := make(chan struct{})
	got := make(chan *Record, 1)
	go func() {
		close(started)
		rec, err := s.WaitReady(context.Background(), "id-1")
		if err != nil {
			got <- nil
			return
		}
		got <- rec
	}()

	<-started
	select {
	case <-got:
		t.Fatal("WaitReady returned before the claim resolved")
	default:
	}

	if err := s.Transition("id-1", StateClaim, StateExpired); err != nil {
		t.Fatalf("Transition(claim->expired) error = %v", err)
	}
	rec := <-got
	if rec == nil || rec.State != StateExpired {
		t.Errorf("WaitReady() = %+v, want expired", rec)
	}
}

func TestWaitReadyContextCancellation(t *testing.T) {
	s := NewStore()
	if _, err := s.CreateClaim("id-1", "synthetic original"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	got := make(chan error, 1)
	go func() {
		close(started)
		_, err := s.WaitReady(ctx, "id-1")
		got <- err
	}()

	<-started
	cancel()
	if err := <-got; !errors.Is(err, context.Canceled) {
		t.Errorf("WaitReady() error = %v, want context.Canceled", err)
	}
}

func TestWaitReadySnapshotDetached(t *testing.T) {
	s := NewStore()
	if _, err := s.CreateClaim("id-1", "synthetic original"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}
	if err := s.CompleteClaim("id-1", "masked result"); err != nil {
		t.Fatalf("CompleteClaim() error = %v", err)
	}
	rec, err := s.WaitReady(context.Background(), "id-1")
	if err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	rec.State = StateExpired
	rec.Original = "mutated"
	rec.Result = "mutated"

	stored, err := s.Get("id-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if stored.State != StateReady || stored.Original != "synthetic original" || stored.Result != "masked result" {
		t.Errorf("stored record changed by snapshot mutation: %+v", stored)
	}
}

func TestExpireClaimRecordsVaultUnavailable(t *testing.T) {
	s := NewStore()
	if _, err := s.CreateClaim("id-1", "synthetic original"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}
	if err := s.expireClaim("id-1", ErrVaultUnavailable); err != nil {
		t.Fatalf("expireClaim() error = %v", err)
	}
	if err := s.expiredFailure("id-1"); !errors.Is(err, ErrVaultUnavailable) {
		t.Errorf("expiredFailure() = %v, want ErrVaultUnavailable", err)
	}
	rec, err := s.Get("id-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if rec.State != StateExpired {
		t.Errorf("State = %q, want %q", rec.State, StateExpired)
	}
	if rec.Result != "" {
		t.Errorf("Result = %q, want empty (classification not in public Record)", rec.Result)
	}
}

func TestExpireClaimStoresExactSentinelNotWrapper(t *testing.T) {
	const sensitive = "unique-sensitive-vault-detail"
	s := NewStore()
	if _, err := s.CreateClaim("id-1", "synthetic original"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}
	wrapped := fmt.Errorf("%s: %w", sensitive, ErrVaultUnavailable)
	if err := s.expireClaim("id-1", wrapped); err != nil {
		t.Fatalf("expireClaim() error = %v", err)
	}
	got := s.expiredFailure("id-1")
	if got != ErrVaultUnavailable {
		t.Errorf("expiredFailure() = %v, want exact ErrVaultUnavailable by identity", got)
	}
	if !errors.Is(got, ErrVaultUnavailable) {
		t.Errorf("expiredFailure() = %v, want errors.Is ErrVaultUnavailable", got)
	}
	if strings.Contains(got.Error(), sensitive) {
		t.Errorf("expiredFailure() error leaks sensitive detail: %q", got.Error())
	}
	if strings.Contains(got.Error(), "unique-sensitive") {
		t.Errorf("expiredFailure() error leaks wrapper message: %q", got.Error())
	}
}

func TestExpireClaimNormalizesUnknownError(t *testing.T) {
	s := NewStore()
	if _, err := s.CreateClaim("id-1", "synthetic original"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}
	if err := s.expireClaim("id-1", errors.New("raw dependency detail")); err != nil {
		t.Fatalf("expireClaim() error = %v", err)
	}
	got := s.expiredFailure("id-1")
	if got != ErrMaskingFailed {
		t.Errorf("expiredFailure() = %v, want exact ErrMaskingFailed by identity", got)
	}
	if !errors.Is(got, ErrMaskingFailed) {
		t.Errorf("expiredFailure() = %v, want errors.Is ErrMaskingFailed", got)
	}
	if errors.Is(got, ErrVaultUnavailable) {
		t.Errorf("expiredFailure() must not be ErrVaultUnavailable for unknown error")
	}
	if strings.Contains(got.Error(), "raw dependency detail") {
		t.Errorf("expiredFailure() error leaks raw detail: %q", got.Error())
	}
}

func TestExpireClaimNotInClaimState(t *testing.T) {
	s := NewStore()
	if _, err := s.CreateClaim("id-1", "synthetic original"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}
	if err := s.CompleteClaim("id-1", "masked result"); err != nil {
		t.Fatalf("CompleteClaim() error = %v", err)
	}
	if err := s.expireClaim("id-1", ErrVaultUnavailable); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("expireClaim() error = %v, want ErrInvalidTransition", err)
	}
}

func TestExpiredFailureNoneRecorded(t *testing.T) {
	s := NewStore()
	if _, err := s.CreateClaim("id-1", "synthetic original"); err != nil {
		t.Fatalf("CreateClaim() error = %v", err)
	}
	if err := s.Transition("id-1", StateClaim, StateExpired); err != nil {
		t.Fatalf("Transition(claim->expired) error = %v", err)
	}
	if err := s.expiredFailure("id-1"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("expiredFailure() = %v, want ErrInvalidTransition", err)
	}
}

func TestExpiredFailureNotFound(t *testing.T) {
	s := NewStore()
	if err := s.expiredFailure("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expiredFailure() error = %v, want ErrNotFound", err)
	}
}
