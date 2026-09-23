// Package audit implements structured audit logging of only allowlisted
// metadata. It never accepts or logs plaintext PII values, restored text,
// token mappings, CVV/PIN, ciphertext, keys, authorization headers or request
// and response bodies. Callers pass only safe detection/ownership metadata
// (types, personal flags, sources, reason codes) plus request_id, operation,
// duration, model mode and result; the package aggregates types, sources and
// reason codes deterministically and emits machine-readable JSON that is safe
// for concurrent calls.
package audit

import (
	"encoding/json"
	"io"
	"sort"
	"sync"
	"time"
)

// Operation is the audited operation name.
type Operation string

// Known operations. These exact values are the public contract.
const (
	OpDetect     Operation = "detect"
	OpTokenize   Operation = "tokenize"
	OpDetokenize Operation = "detokenize"
	OpProcess    Operation = "process"
	OpRuntime    Operation = "runtime"
	OpRevoke     Operation = "revoke"
)

// Result is the operation outcome.
type Result string

// Known results. These exact values are the public contract.
const (
	ResultSuccess Result = "success"
	ResultError   Result = "error"
)

// ModelMode mirrors the config/modelclient model modes.
type ModelMode string

// Known model modes. These exact values mirror config.ModelMode.
const (
	ModeFull ModelMode = "full"
	ModeFast ModelMode = "fast"
)

// Entity is the safe per-entity metadata accepted by the audit logger. It
// carries only the canonical type, the personal flag, the detection sources
// and the ownership reason codes. It never carries the plaintext value, the
// restored text, offsets into the input, mappings, ciphertext or keys.
type Entity struct {
	Type        string
	Personal    bool
	Sources     []string
	ReasonCodes []string
}

// Event is the allowlisted metadata for one audited operation. It is the only
// input the logger accepts; there is deliberately no field for message, error,
// body, header, value, mapping, ciphertext or key. HasPersonalData is derived
// from the entities and is not accepted as input, so the caller cannot supply
// contradictory values.
type Event struct {
	RequestID string
	Operation Operation
	Entities  []Entity
	Duration  time.Duration
	ModelMode ModelMode
	Result    Result
}

// jsonEvent is the exact wire shape. Struct field order fixes the JSON key
// order so output is deterministic; the field set is the allowlist and nothing
// else.
type jsonEvent struct {
	RequestID       string   `json:"request_id"`
	Operation       string   `json:"operation"`
	HasPersonalData bool     `json:"has_personal_data"`
	DetectedTypes   []string `json:"detected_types"`
	EntityCount     int      `json:"entity_count"`
	PersonalFlags   []bool   `json:"personal_flags"`
	Sources         []string `json:"sources"`
	ReasonCodes     []string `json:"reason_codes"`
	DurationMs      int64    `json:"duration_ms"`
	ModelMode       string   `json:"model_mode"`
	Result          string   `json:"result"`
}

// Logger writes allowlisted audit events as JSON lines. It is safe for
// concurrent use: each Log call marshals to a private buffer and writes the
// whole line under a mutex, so concurrent callers never interleave output.
type Logger struct {
	mu sync.Mutex
	w  io.Writer
}

// New returns a Logger that writes JSON lines to w. w must be non-nil.
func New(w io.Writer) *Logger {
	return &Logger{w: w}
}

// Log aggregates the allowlisted metadata from e and writes one JSON line.
// The aggregated sets (detected_types, sources, reason_codes) are
// de-duplicated and sorted so they are deterministic regardless of input
// order; personal_flags preserve the document order of the entities. It never
// inspects or logs any plaintext value. A write error is returned to the
// caller; the error message never contains event content.
func (l *Logger) Log(e Event) error {
	line, err := json.Marshal(buildJSON(e))
	if err != nil {
		return err
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = l.w.Write(line)
	return err
}

// buildJSON derives the deterministic aggregated fields from the safe event
// metadata. It never reads plaintext because the event carries none.
func buildJSON(e Event) jsonEvent {
	types := uniqueSorted(e.Entities, func(ent Entity) []string { return []string{ent.Type} })
	sources := uniqueSorted(e.Entities, func(ent Entity) []string { return ent.Sources })
	reasons := uniqueSorted(e.Entities, func(ent Entity) []string { return ent.ReasonCodes })

	flags := make([]bool, 0, len(e.Entities))
	hasPersonal := false
	for _, ent := range e.Entities {
		flags = append(flags, ent.Personal)
		if ent.Personal {
			hasPersonal = true
		}
	}

	return jsonEvent{
		RequestID:       e.RequestID,
		Operation:       string(e.Operation),
		HasPersonalData: hasPersonal,
		DetectedTypes:   types,
		EntityCount:     len(e.Entities),
		PersonalFlags:   flags,
		Sources:         sources,
		ReasonCodes:     reasons,
		DurationMs:      e.Duration.Milliseconds(),
		ModelMode:       string(e.ModelMode),
		Result:          string(e.Result),
	}
}

// uniqueSorted returns the de-duplicated, sorted set of all non-empty values
// produced by pick over all entities.
func uniqueSorted(entities []Entity, pick func(Entity) []string) []string {
	seen := make(map[string]struct{})
	for _, ent := range entities {
		for _, v := range pick(ent) {
			if v != "" {
				seen[v] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
