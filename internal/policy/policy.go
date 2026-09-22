// Package policy defines consumer policy types for the /process adapter.
// It covers the benchmark/default consumer, per-consumer allowed PII type
// names, demasking and rules-only degraded mode. Transport identity
// resolution, config file format, auth and the canonical PII registry are
// out of scope here.
package policy

// DefaultConsumerID is the stable identifier of the benchmark/default
// consumer applied when no trusted transport identity is present.
const DefaultConsumerID = "benchmark"

// Policy describes the capabilities granted to a single consumer.
type Policy struct {
	consumerID string
	types      map[string]struct{}
	// AllowDemasking permits restoring original values from masks.
	AllowDemasking bool
	// AllowRulesOnlyDegraded permits rules-only processing when the model
	// worker is unavailable. It is false by default (fail closed).
	AllowRulesOnlyDegraded bool
}

// NewPolicy builds a Policy for the given consumer. The allowed type names
// are copied, so later mutation of the input slice does not change the
// policy. Boolean capabilities default to false.
func NewPolicy(consumerID string, allowedTypes []string) Policy {
	types := make(map[string]struct{}, len(allowedTypes))
	for _, name := range allowedTypes {
		types[name] = struct{}{}
	}
	return Policy{
		consumerID: consumerID,
		types:      types,
	}
}

// ConsumerID returns the consumer identifier.
func (p Policy) ConsumerID() string {
	return p.consumerID
}

// AllowsType reports whether the consumer may process the given PII type
// name. Type names are treated as opaque string identifiers.
func (p Policy) AllowsType(name string) bool {
	_, ok := p.types[name]
	return ok
}

// Types returns a copy of the allowed type names. Mutating the returned
// slice does not affect the policy.
func (p Policy) Types() []string {
	out := make([]string, 0, len(p.types))
	for name := range p.types {
		out = append(out, name)
	}
	return out
}
