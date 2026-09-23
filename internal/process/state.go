package process

import (
	"context"
	"errors"
	"sync"
)

// Sentinel errors returned by the record state machine.
var (
	// ErrNotFound is returned when no record exists for a payload_id.
	ErrNotFound = errors.New("process: record not found")
	// ErrExists is returned when a record already exists for a payload_id.
	ErrExists = errors.New("process: record already exists")
	// ErrInvalidTransition is returned when a state transition is not allowed.
	ErrInvalidTransition = errors.New("process: invalid state transition")
)

// Record is a correlation record for a payload_id. It carries the data needed
// by later tasks (original payload and issued result) but does not itself
// implement retry, restore, conflict or single-writer semantics.
type Record struct {
	PayloadID string
	State     RecordState
	Original  string
	Result    string
}

// CanTransition reports whether moving from state from to state to is allowed
// by the record state machine. claim may become ready or expired; ready may
// become expired; expired is terminal.
func CanTransition(from, to RecordState) bool {
	switch from {
	case StateClaim:
		return to == StateReady || to == StateExpired
	case StateReady:
		return to == StateExpired
	default:
		return false
	}
}

// Store is an in-memory record state machine keyed by payload_id.
type Store struct {
	mu      sync.Mutex
	records map[string]*entry
}

// entry is the private per-attempt store entry. It pairs the domain Record
// with a done channel used to coordinate concurrent first requests. The done
// channel is closed exactly once when the record leaves StateClaim (to ready
// or expired), waking any waiters. It is never exposed through Record. failErr
// holds the safe classification of a failed claim so waiters and later retries
// observe the same terminal failure; it is never exposed through Record.
type entry struct {
	rec     *Record
	done    chan struct{}
	failErr error
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{records: make(map[string]*entry)}
}

// begin binds one operation to an exact entry generation. A transient failed
// generation can be replaced only by its original payload. Existing waiters
// keep their old entry and therefore receive that attempt's failure even if a
// later retry has already started.
func (s *Store) begin(payloadID, original string) (*entry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.records[payloadID]
	if ok {
		if e.rec.State == StateExpired && original != e.rec.Original {
			return nil, false, ErrConflict
		}
		if e.rec.State != StateExpired || e.failErr != ErrModelUnavailable {
			return e, false, nil
		}
	}
	e = &entry{rec: &Record{PayloadID: payloadID, State: StateClaim, Original: original}, done: make(chan struct{})}
	s.records[payloadID] = e
	return e, true, nil
}

func (s *Store) snapshotEntry(e *entry) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.rec.State == StateExpired {
		if e.failErr == nil {
			return nil, ErrInvalidTransition
		}
		return nil, e.failErr
	}
	return e.rec.snapshot(), nil
}

func (s *Store) waitEntry(ctx context.Context, e *entry) (*Record, error) {
	select {
	case <-e.done:
		return s.snapshotEntry(e)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Store) completeEntry(e *entry, result string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records[e.rec.PayloadID] != e || e.rec.State != StateClaim {
		return ErrInvalidTransition
	}
	e.rec.Result = result
	e.rec.State = StateReady
	close(e.done)
	return nil
}

func (s *Store) failEntry(e *entry, failErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records[e.rec.PayloadID] != e || e.rec.State != StateClaim {
		return ErrInvalidTransition
	}
	e.failErr = failErr
	e.rec.State = StateExpired
	close(e.done)
	return nil
}

// CreateClaim atomically creates a new record in claim state for payload_id.
// It returns ErrExists if a record already exists for the id. The returned
// record is a snapshot copy; mutating it does not affect the stored record.
func (s *Store) CreateClaim(payloadID, original string) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[payloadID]; ok {
		return nil, ErrExists
	}
	e := &entry{
		rec: &Record{
			PayloadID: payloadID,
			State:     StateClaim,
			Original:  original,
		},
		done: make(chan struct{}),
	}
	s.records[payloadID] = e
	return e.rec.snapshot(), nil
}

// Get returns a snapshot copy of the record for payload_id, or ErrNotFound.
// The returned record is detached from the store; mutating it does not affect
// the stored record.
func (s *Store) Get(payloadID string) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.records[payloadID]
	if !ok {
		return nil, ErrNotFound
	}
	return e.rec.snapshot(), nil
}

// WaitReady blocks until the record for payload_id leaves StateClaim (becomes
// ready or expired), or until ctx is done. It returns ErrNotFound if no record
// exists. If the record is already ready or expired it returns an immediate
// snapshot. For a claim in progress it captures the private per-entry done
// channel under the store mutex, releases the mutex, and selects on the
// channel versus ctx.Done(). The returned record is a detached snapshot and
// never exposes the coordination channel.
func (s *Store) WaitReady(ctx context.Context, payloadID string) (*Record, error) {
	s.mu.Lock()
	e, ok := s.records[payloadID]
	if !ok {
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	if e.rec.State != StateClaim {
		rec := e.rec.snapshot()
		s.mu.Unlock()
		return rec, nil
	}
	done := e.done
	s.mu.Unlock()

	select {
	case <-done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// The record has left StateClaim; return a fresh detached snapshot.
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok = s.records[payloadID]
	if !ok {
		return nil, ErrNotFound
	}
	return e.rec.snapshot(), nil
}

// snapshot returns a detached copy of the record. It must be called while the
// store mutex is held.
func (r *Record) snapshot() *Record {
	if r == nil {
		return nil
	}
	cp := *r
	return &cp
}

// CompleteClaim atomically sets the result on a claim record and transitions
// it to ready, waking any waiters. It returns ErrNotFound if no record exists
// and ErrInvalidTransition if the record is not in claim state.
func (s *Store) CompleteClaim(payloadID, result string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.records[payloadID]
	if !ok {
		return ErrNotFound
	}
	if e.rec.State != StateClaim {
		return ErrInvalidTransition
	}
	e.rec.Result = result
	e.rec.State = StateReady
	close(e.done)
	return nil
}

// Transition moves the record for payload_id from from to to, validating the
// transition. It returns ErrNotFound if no record exists and
// ErrInvalidTransition if the current state does not match from or the
// transition is not allowed. When the transition moves a record out of
// StateClaim (to ready or expired), any waiters are woken exactly once.
func (s *Store) Transition(payloadID string, from, to RecordState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.records[payloadID]
	if !ok {
		return ErrNotFound
	}
	if e.rec.State != from || !CanTransition(from, to) {
		return ErrInvalidTransition
	}
	e.rec.State = to
	if from == StateClaim {
		close(e.done)
	}
	return nil
}

// expireClaim atomically records a safe failure classification on a claim
// record, transitions it to expired, and wakes any waiters exactly once. It
// returns ErrNotFound if no record exists and ErrInvalidTransition if the
// record is not in claim state. The stored value is always exactly
// ErrVaultUnavailable, exactly ErrModelUnavailable or exactly
// ErrMaskingFailed, never the caller-provided error object, so raw dependency
// errors and their messages are never retained.
func (s *Store) expireClaim(payloadID string, failErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.records[payloadID]
	if !ok {
		return ErrNotFound
	}
	if e.rec.State != StateClaim {
		return ErrInvalidTransition
	}
	switch {
	case errors.Is(failErr, ErrVaultUnavailable):
		e.failErr = ErrVaultUnavailable
	case errors.Is(failErr, ErrReviewRequired):
		e.failErr = ErrReviewRequired
	case errors.Is(failErr, ErrModelUnavailable):
		e.failErr = ErrModelUnavailable
	default:
		e.failErr = ErrMaskingFailed
	}
	e.rec.State = StateExpired
	close(e.done)
	return nil
}

// expiredFailure returns the safe classification recorded when a claim was
// expired by expireClaim. It returns ErrInvalidTransition if no classified
// operation failure was recorded for the record. It returns ErrNotFound if no
// record exists.
func (s *Store) expiredFailure(payloadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.records[payloadID]
	if !ok {
		return ErrNotFound
	}
	if e.failErr == nil {
		return ErrInvalidTransition
	}
	return e.failErr
}
