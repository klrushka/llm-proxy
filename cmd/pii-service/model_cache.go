package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"sync"
	"time"

	"github.com/klrushka/llm-proxy/internal/detection"
)

const (
	nerCacheTTL        = 30 * time.Second
	nerCacheMaxEntries = 512
	nerCacheMaxFlights = 512
	nerCacheMaxBytes   = 4 << 20
)

// nerCache holds only validated model-derived spans. One instance belongs to
// one immutable model client, registry and window configuration. Keys are
// process-secret HMACs of exact text plus the fixed model/config identity;
// plaintext, scoped tokens, mappings, ownership and final results stay out.
type nerCache struct {
	secret  [32]byte
	domain  string
	mu      sync.Mutex
	entries map[[32]byte]nerCacheEntry
	flights map[[32]byte]*nerFlight
	bytes   int
	now     func() time.Time
}

type nerCacheEntry struct {
	spans   []detection.Candidate
	size    int
	expires time.Time
}

type nerFlight struct {
	done      chan struct{}
	cancel    context.CancelFunc
	waiters   int
	abandoned bool
	spans     []detection.Candidate
	err       error
}

func newNERCache(domain string) (*nerCache, error) {
	c := &nerCache{domain: domain, entries: make(map[[32]byte]nerCacheEntry), flights: make(map[[32]byte]*nerFlight), now: time.Now}
	if _, err := rand.Read(c.secret[:]); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *nerCache) digest(text string) [32]byte {
	h := hmac.New(sha256.New, c.secret[:])
	_, _ = h.Write([]byte(c.domain))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(text))
	var key [32]byte
	copy(key[:], h.Sum(nil))
	return key
}

func cloneSpans(in []detection.Candidate) []detection.Candidate {
	out := make([]detection.Candidate, len(in))
	for i, span := range in {
		out[i] = span
		out[i].Sources = append([]detection.Source(nil), span.Sources...)
	}
	return out
}

func spanBytes(in []detection.Candidate) int {
	n := 0
	for _, span := range in {
		n += 64 + len(span.Type) + len(span.Sources)*16
	}
	return n
}

// detect coalesces identical in-flight NER while callers retain independent
// cancellation. Shared work has its own bounded lifetime and is canceled when
// its last subscriber leaves. Only successful, still-live results enter the
// bounded cache. report receives a closed outcome name and no request data.
func (c *nerCache) detect(ctx context.Context, text string, compute func(context.Context, string) ([]detection.Candidate, error), report func(string)) ([]detection.Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := c.digest(text)
	for {
		c.mu.Lock()
		if e, ok := c.entries[key]; ok {
			if c.now().Before(e.expires) {
				spans := cloneSpans(e.spans)
				c.mu.Unlock()
				if report != nil {
					report("hit")
				}
				return spans, nil
			}
			delete(c.entries, key)
			c.bytes -= e.size
		}
		if f := c.flights[key]; f != nil {
			if f.abandoned {
				c.mu.Unlock()
				select {
				case <-f.done:
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			f.waiters++
			c.mu.Unlock()
			if report != nil {
				report("coalesced")
			}
			return c.await(ctx, f)
		}
		if len(c.flights) >= nerCacheMaxFlights {
			c.mu.Unlock()
			if report != nil {
				report("uncached")
			}
			return compute(ctx, text)
		}
		workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		f := &nerFlight{done: make(chan struct{}), cancel: cancel, waiters: 1}
		c.flights[key] = f
		c.mu.Unlock()
		if report != nil {
			report("miss")
		}
		go c.runFlight(workCtx, key, text, f, compute)
		return c.await(ctx, f)
	}
}

func (c *nerCache) await(ctx context.Context, f *nerFlight) ([]detection.Candidate, error) {
	select {
	case <-f.done:
		return cloneSpans(f.spans), f.err
	case <-ctx.Done():
		c.mu.Lock()
		select {
		case <-f.done:
			c.mu.Unlock()
			return nil, ctx.Err()
		default:
		}
		f.waiters--
		if f.waiters == 0 {
			f.abandoned = true
			f.cancel()
		}
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (c *nerCache) runFlight(ctx context.Context, key [32]byte, text string, f *nerFlight, compute func(context.Context, string) ([]detection.Candidate, error)) {
	defer f.cancel()
	spans, err := compute(ctx, text)
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		spans = nil
	}
	c.mu.Lock()
	delete(c.flights, key)
	if err == nil && !f.abandoned {
		size := spanBytes(spans)
		if size <= nerCacheMaxBytes {
			for len(c.entries) >= nerCacheMaxEntries || c.bytes+size > nerCacheMaxBytes {
				for victim, e := range c.entries {
					delete(c.entries, victim)
					c.bytes -= e.size
					break
				}
			}
			c.entries[key] = nerCacheEntry{spans: cloneSpans(spans), size: size, expires: c.now().Add(nerCacheTTL)}
			c.bytes += size
		}
	}
	f.spans, f.err = cloneSpans(spans), err
	close(f.done)
	c.mu.Unlock()
}
