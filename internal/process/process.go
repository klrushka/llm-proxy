// Package process defines the domain types for the POST /process benchmark
// adapter and the record state machine. It contains only types and their
// wire/validity contracts; handler, storage and state-transition behavior
// live in later tasks.
package process

// Request is the JSON body accepted by POST /process. Both fields are
// required and must be strings.
type Request struct {
	Payload   string `json:"payload"`
	PayloadID string `json:"payload_id"`
}

// Response is the JSON body returned by a successful POST /process. It
// contains exactly one field, result, and no others.
type Response struct {
	Result string `json:"result"`
}

// RecordState is the state of a correlation record for a payload_id.
type RecordState string

// Record states. These exact wire values are part of the contract.
const (
	StateClaim   RecordState = "claim"
	StateReady   RecordState = "ready"
	StateExpired RecordState = "expired"
)

// Valid reports whether s is one of the known record states.
func (s RecordState) Valid() bool {
	switch s {
	case StateClaim, StateReady, StateExpired:
		return true
	default:
		return false
	}
}
