// Package audit provides the context-local request collector that carries
// only allowlisted safe entity metadata from the detection pipeline to the
// outer audit middleware. It never accepts or stores plaintext values, restored
// text, token mappings, ciphertext, keys, authorization headers or request and
// response bodies. The collector is created by the outer audit middleware and
// read after the handler completes.
package audit

import (
	"context"
	"sync"
)

// collectorKey is the private context key for the request collector.
type collectorKey struct{}

// Collector accumulates the safe per-entity metadata for one request. It is
// safe for concurrent use: AddEntity and Snapshot take a mutex and copy
// defensively so the detection pipeline and the middleware never share mutable
// slices. It accepts only audit.Entity, which carries no plaintext.
type Collector struct {
	mu       sync.Mutex
	entities []Entity
	model    ModelMode
	tokens   int
}

// NewCollector returns an empty request collector.
func NewCollector() *Collector {
	return &Collector{}
}

// AddEntity appends a defensive copy of ent. The caller's slices are never
// aliased.
func (c *Collector) AddEntity(ent Entity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entities = append(c.entities, copyEntity(ent))
}

// SetModelMode records the model mode for the request.
func (c *Collector) SetModelMode(m ModelMode) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.model = m
}

// AddInputTokens adds n to the number of input tokens submitted to detection
// in this request. It is a safe numeric aggregate and never carries text.
func (c *Collector) AddInputTokens(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens += n
}

// InputTokens returns the number of input tokens submitted to detection.
func (c *Collector) InputTokens() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokens
}

// Snapshot returns a defensive copy of the accumulated entities and the model
// mode. The returned slices are owned by the caller.
func (c *Collector) Snapshot() ([]Entity, ModelMode) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Entity, 0, len(c.entities))
	for _, e := range c.entities {
		out = append(out, copyEntity(e))
	}
	return out, c.model
}

// copyEntity returns a defensive deep copy of e with its own source and reason
// slices.
func copyEntity(e Entity) Entity {
	out := e
	out.Sources = append([]string(nil), e.Sources...)
	out.ReasonCodes = append([]string(nil), e.ReasonCodes...)
	return out
}

// WithCollector returns a context carrying c.
func WithCollector(ctx context.Context, c *Collector) context.Context {
	return context.WithValue(ctx, collectorKey{}, c)
}

// CollectorFromContext returns the request collector carried in ctx. ok is
// false when no collector is present (e.g. a request that did not pass through
// the outer audit middleware).
func CollectorFromContext(ctx context.Context) (*Collector, bool) {
	c, ok := ctx.Value(collectorKey{}).(*Collector)
	return c, ok
}
