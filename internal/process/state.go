package process

import (
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
	records map[string]*Record
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{records: make(map[string]*Record)}
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
	rec := &Record{
		PayloadID: payloadID,
		State:     StateClaim,
		Original:  original,
	}
	s.records[payloadID] = rec
	return rec.snapshot(), nil
}

// Get returns a snapshot copy of the record for payload_id, or ErrNotFound.
// The returned record is detached from the store; mutating it does not affect
// the stored record.
func (s *Store) Get(payloadID string) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[payloadID]
	if !ok {
		return nil, ErrNotFound
	}
	return rec.snapshot(), nil
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

// Transition moves the record for payload_id from from to to, validating the
// transition. It returns ErrNotFound if no record exists and
// ErrInvalidTransition if the current state does not match from or the
// transition is not allowed.
func (s *Store) Transition(payloadID string, from, to RecordState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[payloadID]
	if !ok {
		return ErrNotFound
	}
	if rec.State != from || !CanTransition(from, to) {
		return ErrInvalidTransition
	}
	rec.State = to
	return nil
}
