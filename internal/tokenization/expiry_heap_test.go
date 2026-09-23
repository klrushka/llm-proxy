package tokenization

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/klrushka/llm-proxy/internal/detection"
)

// constReader returns a fixed byte pattern indefinitely, so tests can issue an
// arbitrary number of tokens without exhausting a finite block list. Because
// the suffix is derived from the seed AND the digestKey, identical seeds across
// different keys still yield distinct tokens.
type constReader struct{ b byte }

func (r constReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.b
	}
	return len(p), nil
}

// TestGeneratorSweepDoesNotVisitWholeCache proves that a hot-path Token call
// with many live (unexpired) entries inspects only the expiry heap head and
// does not scan the whole cache. The unexported sweepPops counter records how
// many heap items were actually popped; with nothing expired it must stay zero
// regardless of how many live entries exist.
func TestGeneratorSweepDoesNotVisitWholeCache(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	now := start
	g := newKeyedGenerator(constReader{b: 0x11}, mustRegistry(t), keyedMasterKey(), time.Hour, func() time.Time { return now })

	// Issue a large number of live entries across many scopes.
	const entries = 5000
	for i := 0; i < entries; i++ {
		scope := fmt.Sprintf("scope-%d", i)
		value := fmt.Sprintf("value-%d@example.test", i)
		if _, err := g.Token(scope, detection.TypeEmail, value); err != nil {
			t.Fatalf("Token() error = %v", err)
		}
	}

	// Advance slightly but stay inside the TTL so nothing is expired.
	now = start.Add(time.Minute)
	g.mu.Lock()
	before := g.sweepPops
	g.mu.Unlock()

	if _, err := g.Token("fresh-scope", detection.TypeEmail, "fresh@example.com"); err != nil {
		t.Fatalf("Token() error = %v", err)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if got := g.sweepPops - before; got != 0 {
		t.Errorf("sweep popped %d heap items with %d live entries, want 0 (no full scan)", got, entries)
	}
}

// TestGeneratorMassExpiryCleanupIsBounded proves that a mass expiry cannot make
// one request drain the entire heap while the directly requested key is still
// handled fail-closed outside the sweep budget.
func TestGeneratorMassExpiryCleanupIsBounded(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	now := start
	g := newKeyedGenerator(constReader{b: 0x22}, mustRegistry(t), keyedMasterKey(), time.Minute, func() time.Time { return now })

	const entries = 2000
	for i := 0; i < entries; i++ {
		scope := fmt.Sprintf("scope-%d", i)
		value := fmt.Sprintf("value-%d@example.test", i)
		if _, err := g.Token(scope, detection.TypeEmail, value); err != nil {
			t.Fatalf("Token() error = %v", err)
		}
	}

	g.mu.Lock()
	before := g.sweepPops
	g.mu.Unlock()

	// Advance past every expiry and trigger one bounded sweep.
	now = start.Add(2 * time.Minute)
	if _, err := g.Token("fresh-scope", detection.TypeEmail, "fresh@example.com"); err != nil {
		t.Fatalf("Token() error = %v", err)
	}

	g.mu.Lock()
	popped := g.sweepPops - before
	if popped != maxSweepPops {
		t.Errorf("sweep popped %d items, want bounded batch %d", popped, maxSweepPops)
	}
	if len(g.genIndex) != len(g.tokens) {
		t.Errorf("generation index = %d, reverse token index = %d", len(g.genIndex), len(g.tokens))
	}
	g.mu.Unlock()

	// A key outside the first batch must not reuse an expired lifecycle entry.
	targetScope := fmt.Sprintf("scope-%d", entries-1)
	targetValue := fmt.Sprintf("value-%d@example.test", entries-1)
	if _, err := g.Token(targetScope, detection.TypeEmail, targetValue); err != nil {
		t.Fatalf("Token(expired target) error = %v", err)
	}
	g.mu.Lock()
	target := digestKey{scope: g.digestScope(targetScope), typ: detection.TypeEmail, value: g.digestValue(targetValue)}
	ent, ok := g.issued[target.scope][target]
	g.mu.Unlock()
	if !ok || !now.Before(ent.expiresAt) {
		t.Fatal("directly requested expired entry was not replaced with a live generation")
	}
}

// TestGeneratorStaleHeapEntryDoesNotDeleteReplacement proves that a stale heap
// record (pushed for an earlier generation of a slot) is safely ignored and
// never deletes a newer replacement issued for the same key.
func TestGeneratorStaleHeapEntryDoesNotDeleteReplacement(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	now := start
	g := newKeyedGenerator(&seqReader{blocks: [][]byte{
		block("11111111111111111111111111111111"),
		block("22222222222222222222222222222222"),
	}}, mustRegistry(t), keyedMasterKey(), time.Minute, func() time.Time { return now })

	first, err := g.Token("scope-1", detection.TypeEmail, "a@example.com")
	if err != nil {
		t.Fatalf("Token(first) error = %v", err)
	}

	// Revoke the scope so the first slot is removed but its heap record (gen 1)
	// remains as a stale entry.
	if err := g.RevokeScope(context.Background(), "scope-1"); err != nil {
		t.Fatalf("RevokeScope() error = %v", err)
	}

	// Re-issue the same key shortly after; the replacement gets a new
	// generation and a later expiry.
	now = start.Add(30 * time.Second)
	second, err := g.Token("scope-1", detection.TypeEmail, "a@example.com")
	if err != nil {
		t.Fatalf("Token(second) error = %v", err)
	}
	if first == second {
		t.Fatalf("replacement reused token %q", first)
	}

	// Advance past the stale record's expiry but before the replacement's
	// expiry, then sweep. The stale gen-1 record must be ignored.
	now = start.Add(70 * time.Second)
	g.mu.Lock()
	g.sweepExpired(now)
	g.mu.Unlock()

	g.mu.Lock()
	bucket := g.issued[g.digestScope("scope-1")]
	if bucket == nil {
		g.mu.Unlock()
		t.Fatal("replacement scope bucket was deleted by a stale heap record")
	}
	ent, ok := bucket[digestKey{
		scope: g.digestScope("scope-1"),
		typ:   detection.TypeEmail,
		value: g.digestValue("a@example.com"),
	}]
	if !ok {
		g.mu.Unlock()
		t.Fatal("replacement entry was deleted by a stale heap record")
	}
	if !now.Before(ent.expiresAt) {
		g.mu.Unlock()
		t.Fatal("replacement entry unexpectedly expired")
	}
	g.mu.Unlock()

	// The replacement must still be reusable.
	third, err := g.Token("scope-1", detection.TypeEmail, "a@example.com")
	if err != nil {
		t.Fatalf("Token(third) error = %v", err)
	}
	if third != second {
		t.Errorf("replacement reuse broken: got %q, want %q", third, second)
	}
}

// TestGeneratorRevokeClearsReverseIndexWithoutReconstructingToken proves that
// RevokeScope removes every reverse token digest of a scope using only the
// stored entry data, leaving no stale reverse index entries behind.
func TestGeneratorRevokeClearsReverseIndexWithoutReconstructingToken(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	now := start
	g := newKeyedGenerator(constReader{b: 0x44}, mustRegistry(t), keyedMasterKey(), time.Hour, func() time.Time { return now })

	const perScope = 50
	for s := 0; s < 4; s++ {
		scope := "scope-" + string(rune('a'+s))
		for i := 0; i < perScope; i++ {
			if _, err := g.Token(scope, detection.TypeEmail, "value-"+string(rune('0'+i%10))); err != nil {
				t.Fatalf("Token() error = %v", err)
			}
		}
	}

	if err := g.RevokeScope(context.Background(), "scope-a"); err != nil {
		t.Fatalf("RevokeScope() error = %v", err)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	// The revoked scope's bucket must be gone and its reverse digests removed.
	if _, ok := g.issued[g.digestScope("scope-a")]; ok {
		t.Error("revoked scope bucket still present")
	}
	// Remaining reverse index must only reference the other scopes.
	for _, dk := range g.tokens {
		if dk.scope == g.digestScope("scope-a") {
			t.Error("reverse token index still references revoked scope")
		}
	}
}
