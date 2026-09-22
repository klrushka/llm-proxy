package process

import (
	"errors"
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
