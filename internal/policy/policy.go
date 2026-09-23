// Package policy defines the service's static PII processing policy.
package policy

// Policy describes enabled PII types and processing fallback behavior.
type Policy struct {
	types map[string]struct{}
	// AllowRulesOnlyDegraded permits rules-only processing when the model
	// worker is unavailable. It is false by default (fail closed).
	AllowRulesOnlyDegraded bool
}

// NewPolicy builds a static processing policy. The allowed type names are
// copied, so later mutation of the input slice does not change the policy.
func NewPolicy(allowedTypes []string) Policy {
	types := make(map[string]struct{}, len(allowedTypes))
	for _, name := range allowedTypes {
		types[name] = struct{}{}
	}
	return Policy{types: types}
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
