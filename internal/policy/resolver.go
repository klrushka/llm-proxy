package policy

import (
	"context"
	"errors"
	"strings"
)

// ErrUnknownConsumer is returned when a non-empty identity is not registered.
// It is a safe sentinel that never carries the supplied identity.
var ErrUnknownConsumer = errors.New("policy: unknown consumer")

// Resolver maps a trusted transport-resolved identity to a Policy. It never
// reads HTTP headers or knows about net/http; the identity is already resolved
// by a trusted transport layer before being passed to Resolve. A blank or
// whitespace-only identity resolves to the preconfigured benchmark/default
// consumer. A non-empty unknown identity fails closed with ErrUnknownConsumer.
type Resolver struct {
	defaultPolicy Policy
	byID          map[string]Policy
}

// NewResolver builds a Resolver. defaultPolicy is used for blank identities
// and for the explicit DefaultConsumerID. entries maps identity to policy.
// Both defaultPolicy and every entry are deep-copied, so later caller mutation
// cannot affect the resolver. The resolver is immutable after construction and
// safe for concurrent reads.
func NewResolver(defaultPolicy Policy, entries map[string]Policy) *Resolver {
	r := &Resolver{
		defaultPolicy: defaultPolicy.clone(),
		byID:          make(map[string]Policy, len(entries)),
	}
	for id, p := range entries {
		r.byID[id] = p.clone()
	}
	return r
}

// Resolve returns the policy for identity. A blank or whitespace-only identity
// returns a deep copy of the default policy with nil error. identity equal to
// DefaultConsumerID also returns the default policy regardless of any entry.
// A registered non-empty identity returns a deep copy of its policy. An
// unknown non-empty identity returns the zero Policy and the exact bare
// ErrUnknownConsumer. The returned Policy is a deep copy, so caller mutation
// of exported capability fields cannot affect future resolutions.
func (r *Resolver) Resolve(identity string) (Policy, error) {
	if strings.TrimSpace(identity) == "" || identity == DefaultConsumerID {
		return r.defaultPolicy.clone(), nil
	}
	p, ok := r.byID[identity]
	if !ok {
		return Policy{}, ErrUnknownConsumer
	}
	return p.clone(), nil
}

// ResolveRegistered is the strict resolution path used by the access-control
// layer. Unlike Resolve it never applies a default fallback: a blank or
// whitespace-only identity, the benchmark/default identity, and any unknown
// identity all fail closed with the exact bare ErrUnknownConsumer. Only a
// registered non-empty identity resolves to a deep copy of its policy.
func (r *Resolver) ResolveRegistered(identity string) (Policy, error) {
	if strings.TrimSpace(identity) == "" || identity == DefaultConsumerID {
		return Policy{}, ErrUnknownConsumer
	}
	p, ok := r.byID[identity]
	if !ok {
		return Policy{}, ErrUnknownConsumer
	}
	return p.clone(), nil
}

// policyContextKey is a private, unexported context key type. Using a private
// type prevents any other package from colliding with or reading the stored
// policy through a guessed key.
type policyContextKey struct{}

// WithPolicy returns a context carrying a deep copy of p. Downstream handlers
// can retrieve it with PolicyFromContext without reading user headers.
func WithPolicy(ctx context.Context, p Policy) context.Context {
	return context.WithValue(ctx, policyContextKey{}, p.clone())
}

// PolicyFromContext returns the policy stored by WithPolicy and whether it was
// present. The returned Policy is a defensive deep copy, so caller mutation of
// exported capability fields cannot affect the stored value or future reads.
func PolicyFromContext(ctx context.Context) (Policy, bool) {
	p, ok := ctx.Value(policyContextKey{}).(Policy)
	if !ok {
		return Policy{}, false
	}
	return p.clone(), true
}
