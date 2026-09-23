package vault

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestEncryptedSweepDoesNotVisitWholeCache proves that a hot-path Save/Resolve
// with many live (unexpired) mappings inspects only the expiry heap head and
// does not scan the whole cache. The unexported sweepPops counter records how
// many heap items were actually popped; with nothing expired it must stay zero
// regardless of how many live mappings exist.
func TestEncryptedSweepDoesNotVisitWholeCache(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	now := start
	e, err := newEncryptedWithClock(testMasterKey(), time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newEncryptedWithClock() error = %v", err)
	}
	ctx := context.Background()

	const entries = 5000
	for i := 0; i < entries; i++ {
		scope := fmt.Sprintf("scope-%d", i)
		token := fmt.Sprintf("tok-%d", i)
		if err := e.Save(ctx, scope, token, "synthetic-"+token); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	// Advance slightly but stay inside the TTL so nothing is expired.
	now = start.Add(time.Minute)
	e.mu.Lock()
	before := e.sweepPops
	e.mu.Unlock()

	if err := e.Save(ctx, "fresh-scope", "fresh-tok", "synthetic-fresh"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if got := e.sweepPops - before; got != 0 {
		t.Errorf("sweep popped %d heap items with %d live mappings, want 0 (no full scan)", got, entries)
	}
}

// TestEncryptedMassExpiryCleanupIsBounded proves that a mass expiry cannot make
// one operation drain the whole heap and that direct lookup still rejects an
// expired mapping outside the first sweep batch.
func TestEncryptedMassExpiryCleanupIsBounded(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	now := start
	e, err := newEncryptedWithClock(testMasterKey(), time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newEncryptedWithClock() error = %v", err)
	}
	ctx := context.Background()

	const entries = 2000
	for i := 0; i < entries; i++ {
		scope := fmt.Sprintf("scope-%d", i)
		token := fmt.Sprintf("tok-%d", i)
		if err := e.Save(ctx, scope, token, "synthetic-"+token); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	e.mu.Lock()
	before := e.sweepPops
	e.mu.Unlock()

	// Advance past every expiry and trigger one bounded sweep via a fresh save.
	now = start.Add(2 * time.Minute)
	if err := e.Save(ctx, "fresh-scope", "fresh-tok", "synthetic-fresh"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	e.mu.Lock()
	popped := e.sweepPops - before
	if popped != maxSweepPops {
		t.Errorf("sweep popped %d items, want bounded batch %d", popped, maxSweepPops)
	}
	if len(e.genIndex) != len(e.mapping) {
		t.Errorf("generation index = %d, mapping = %d", len(e.genIndex), len(e.mapping))
	}
	e.mu.Unlock()

	targetScope := fmt.Sprintf("scope-%d", entries-1)
	targetToken := fmt.Sprintf("tok-%d", entries-1)
	if _, err := e.Resolve(ctx, targetScope, targetToken); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve(expired target) error = %v, want ErrNotFound", err)
	}
	k := encKey{scope: digest(e.hmacKey, scopeDomain, []byte(targetScope)), token: digest(e.hmacKey, tokenDomain, []byte(targetToken))}
	e.mu.Lock()
	_, mappingPresent := e.mapping[k]
	for _, indexed := range e.genIndex {
		if indexed == k {
			e.mu.Unlock()
			t.Fatal("generation index still references directly requested expired mapping")
		}
	}
	e.mu.Unlock()
	if mappingPresent {
		t.Fatal("directly requested expired mapping remains stored")
	}
}

// TestEncryptedStaleHeapEntryDoesNotDeleteReplacement proves that a stale heap
// record (pushed for an earlier generation of a mapping) is safely ignored and
// never deletes a newer replacement saved for the same key.
func TestEncryptedStaleHeapEntryDoesNotDeleteReplacement(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	now := start
	e, err := newEncryptedWithClock(testMasterKey(), time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newEncryptedWithClock() error = %v", err)
	}
	ctx := context.Background()

	if err := e.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save(first) error = %v", err)
	}

	// Revoke the scope so the first mapping is removed but its heap record
	// (gen 1) remains as a stale entry.
	if err := e.RevokeScope(ctx, "scope-1"); err != nil {
		t.Fatalf("RevokeScope() error = %v", err)
	}

	// Re-save the same key shortly after; the replacement gets a new
	// generation and a later expiry.
	now = start.Add(30 * time.Second)
	if err := e.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save(second) error = %v", err)
	}

	// Advance past the stale record's expiry but before the replacement's
	// expiry, then sweep. The stale gen-1 record must be ignored.
	now = start.Add(70 * time.Second)
	e.mu.Lock()
	e.sweepExpired(now)
	e.mu.Unlock()

	e.mu.Lock()
	k := encKey{
		scope: digest(e.hmacKey, scopeDomain, []byte("scope-1")),
		token: digest(e.hmacKey, tokenDomain, []byte("tok-1")),
	}
	ent, ok := e.mapping[k]
	e.mu.Unlock()
	if !ok {
		t.Fatal("replacement mapping was deleted by a stale heap record")
	}
	if !now.Before(ent.expiresAt) {
		t.Fatal("replacement mapping unexpectedly expired")
	}

	// The replacement must still resolve.
	got, err := e.Resolve(ctx, "scope-1", "tok-1")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got != "synthetic-value-a" {
		t.Errorf("Resolve() = %q, want %q", got, "synthetic-value-a")
	}
}

// TestEncryptedRevokeScopeIsScoped proves that RevokeScope touches only the
// entries of the target scope and leaves other scopes' mappings and buckets
// intact.
func TestEncryptedRevokeScopeIsScoped(t *testing.T) {
	e, err := newEncryptedWithClock(testMasterKey(), time.Hour, time.Now)
	if err != nil {
		t.Fatalf("newEncryptedWithClock() error = %v", err)
	}
	ctx := context.Background()

	const perScope = 50
	for s := 0; s < 4; s++ {
		scope := "scope-" + string(rune('a'+s))
		for i := 0; i < perScope; i++ {
			token := "tok-" + string(rune('0'+i%10))
			if err := e.Save(ctx, scope, token, "synthetic-"+token); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
		}
	}

	if err := e.RevokeScope(ctx, "scope-a"); err != nil {
		t.Fatalf("RevokeScope() error = %v", err)
	}

	scopeADigest := digest(e.hmacKey, scopeDomain, []byte("scope-a"))
	e.mu.Lock()
	bucketPresent := false
	revokedMapping := false
	if _, ok := e.scopeBuckets[scopeADigest]; ok {
		bucketPresent = true
	}
	for k := range e.mapping {
		if k.scope == scopeADigest {
			revokedMapping = true
			break
		}
	}
	e.mu.Unlock()

	if bucketPresent {
		t.Error("revoked scope bucket still present")
	}
	if revokedMapping {
		t.Error("revoked scope mapping still present")
	}

	// Other scopes must be untouched.
	for s := 1; s < 4; s++ {
		scope := "scope-" + string(rune('a'+s))
		if _, err := e.Resolve(ctx, scope, "tok-1"); err != nil {
			t.Errorf("unrelated scope %q Resolve() error = %v, want nil", scope, err)
		}
	}
}

// TestEncryptedCurrentKeyExpiryFailClosed proves that a mapping whose current
// key has expired is never disclosed even when the expiry heap cannot reach it
// (its heap record is stale or missing). The direct lookup must independently
// check expiry and fail closed.
func TestEncryptedCurrentKeyExpiryFailClosed(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	now := start
	e, err := newEncryptedWithClock(testMasterKey(), time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newEncryptedWithClock() error = %v", err)
	}
	ctx := context.Background()

	// Save a mapping, then revoke and re-save it so the live entry has a new
	// generation while the old heap record remains stale.
	if err := e.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save(first) error = %v", err)
	}
	if err := e.RevokeScope(ctx, "scope-1"); err != nil {
		t.Fatalf("RevokeScope() error = %v", err)
	}
	now = start.Add(10 * time.Second)
	if err := e.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save(second) error = %v", err)
	}

	// Drop the live generation's heap record so the sweep cannot reach the
	// current key; only the stale gen-1 record remains.
	k := encKey{
		scope: digest(e.hmacKey, scopeDomain, []byte("scope-1")),
		token: digest(e.hmacKey, tokenDomain, []byte("tok-1")),
	}
	e.mu.Lock()
	for i := range e.expiry {
		if e.expiry[i].gen == e.mapping[k].gen {
			e.expiry = append(e.expiry[:i], e.expiry[i+1:]...)
			break
		}
	}
	e.mu.Unlock()

	// Advance past the current key's expiry. The stale gen-1 heap record is
	// ignored by the sweep, so the direct lookup must fail closed.
	now = start.Add(2 * time.Minute)
	if _, err := e.Resolve(ctx, "scope-1", "tok-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired current key Resolve() error = %v, want ErrNotFound", err)
	}
}
