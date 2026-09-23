package vault

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// testMasterKey returns a fixed 32-byte master key for tests.
func testMasterKey() [32]byte {
	var k [32]byte
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

// mustEncrypted builds an encrypted adapter with a generous TTL.
func mustEncrypted(t *testing.T) *Encrypted {
	t.Helper()
	e, err := NewEncrypted(testMasterKey(), testTTL)
	if err != nil {
		t.Fatalf("NewEncrypted() error = %v", err)
	}
	return e
}

func TestEncryptedRoundTrip(t *testing.T) {
	e := mustEncrypted(t)
	ctx := context.Background()
	if err := e.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := e.Resolve(ctx, "scope-1", "tok-1")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got != "synthetic-value-a" {
		t.Errorf("Resolve() = %q, want %q", got, "synthetic-value-a")
	}
}

func TestEncryptedIdempotentSameOriginal(t *testing.T) {
	e := mustEncrypted(t)
	ctx := context.Background()
	if err := e.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := e.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Errorf("repeated same original: Save() error = %v, want nil", err)
	}
	got, err := e.Resolve(ctx, "scope-1", "tok-1")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got != "synthetic-value-a" {
		t.Errorf("Resolve() = %q, want %q", got, "synthetic-value-a")
	}
}

func TestEncryptedConflictDoesNotOverwrite(t *testing.T) {
	e := mustEncrypted(t)
	ctx := context.Background()
	if err := e.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := e.Save(ctx, "scope-1", "tok-1", "synthetic-value-b"); !errors.Is(err, ErrConflict) {
		t.Errorf("different original: Save() error = %v, want ErrConflict", err)
	}
	got, err := e.Resolve(ctx, "scope-1", "tok-1")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got != "synthetic-value-a" {
		t.Errorf("Resolve() = %q, want original %q preserved", got, "synthetic-value-a")
	}
}

func TestEncryptedDifferentRecordsUseDifferentCiphertext(t *testing.T) {
	e := mustEncrypted(t)
	ctx := context.Background()
	if err := e.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := e.Save(ctx, "scope-1", "tok-2", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	k1 := encKey{
		scope: digest(e.hmacKey, scopeDomain, []byte("scope-1")),
		token: digest(e.hmacKey, tokenDomain, []byte("tok-1")),
	}
	k2 := encKey{
		scope: digest(e.hmacKey, scopeDomain, []byte("scope-1")),
		token: digest(e.hmacKey, tokenDomain, []byte("tok-2")),
	}
	e.mu.Lock()
	c1 := e.mapping[k1].ciphertext
	c2 := e.mapping[k2].ciphertext
	e.mu.Unlock()
	if bytes.Equal(c1, c2) {
		t.Error("two distinct records produced identical ciphertext; nonce reuse suspected")
	}
}

func TestEncryptedStoredBytesContainNoPlaintext(t *testing.T) {
	e := mustEncrypted(t)
	ctx := context.Background()
	const scope = "SCOPE-MARKER-9f3a"
	const token = "TOKEN-MARKER-7b2c"
	const original = "synthetic-plaintext-Иванов-ivanov@example.com"
	if err := e.Save(ctx, scope, token, original); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	for k, ent := range e.mapping {
		// Index keys must not contain plaintext scope or token.
		if strings.Contains(string(k.scope[:]), scope) || strings.Contains(string(k.token[:]), token) {
			t.Error("index key contains plaintext scope or token")
		}
		// Stored bytes must not contain the plaintext original.
		if bytes.Contains(ent.ciphertext, []byte(original)) {
			t.Error("stored ciphertext contains plaintext original")
		}
	}
}

func TestEncryptedTamperedCiphertextFailsClosed(t *testing.T) {
	e := mustEncrypted(t)
	ctx := context.Background()
	if err := e.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	k := encKey{
		scope: digest(e.hmacKey, scopeDomain, []byte("scope-1")),
		token: digest(e.hmacKey, tokenDomain, []byte("tok-1")),
	}
	e.mu.Lock()
	ct := append([]byte(nil), e.mapping[k].ciphertext...)
	ct[len(ct)-1] ^= 0xFF
	e.mapping[k] = encEntry{ciphertext: ct, expiresAt: e.mapping[k].expiresAt}
	e.mu.Unlock()

	if _, err := e.Resolve(ctx, "scope-1", "tok-1"); !errors.Is(err, ErrCorrupted) {
		t.Errorf("Resolve() error = %v, want ErrCorrupted", err)
	}
}

func TestEncryptedWrongKeyFailsClosed(t *testing.T) {
	e := mustEncrypted(t)
	ctx := context.Background()
	if err := e.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	k := encKey{
		scope: digest(e.hmacKey, scopeDomain, []byte("scope-1")),
		token: digest(e.hmacKey, tokenDomain, []byte("tok-1")),
	}
	e.mu.Lock()
	ct := append([]byte(nil), e.mapping[k].ciphertext...)
	e.mu.Unlock()

	// A second adapter with a different AES key but the same HMAC key (so the
	// index matches) must actually attempt to decrypt the existing record and
	// fail closed with ErrCorrupted, not report a missing entry.
	var wrongAES [32]byte
	wrongAES[0] = 0xFF
	other := &Encrypted{
		mapping: make(map[encKey]encEntry),
		aesKey:  wrongAES,
		hmacKey: e.hmacKey,
		ttl:     testTTL,
		now:     time.Now,
	}
	other.mu.Lock()
	other.mapping[k] = encEntry{ciphertext: ct, expiresAt: time.Now().Add(testTTL)}
	other.mu.Unlock()

	if _, err := other.Resolve(ctx, "scope-1", "tok-1"); !errors.Is(err, ErrCorrupted) {
		t.Errorf("wrong-key Resolve() error = %v, want ErrCorrupted", err)
	}
}

func TestEncryptedCiphertextSwapFailsClosed(t *testing.T) {
	e := mustEncrypted(t)
	ctx := context.Background()
	if err := e.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := e.Save(ctx, "scope-1", "tok-2", "synthetic-value-b"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	k1 := encKey{
		scope: digest(e.hmacKey, scopeDomain, []byte("scope-1")),
		token: digest(e.hmacKey, tokenDomain, []byte("tok-1")),
	}
	k2 := encKey{
		scope: digest(e.hmacKey, scopeDomain, []byte("scope-1")),
		token: digest(e.hmacKey, tokenDomain, []byte("tok-2")),
	}
	e.mu.Lock()
	c1 := e.mapping[k1].ciphertext
	c2 := e.mapping[k2].ciphertext
	e.mapping[k1] = encEntry{ciphertext: c2, expiresAt: e.mapping[k1].expiresAt}
	e.mapping[k2] = encEntry{ciphertext: c1, expiresAt: e.mapping[k2].expiresAt}
	e.mu.Unlock()

	// Because each ciphertext is bound to its composite index as AAD, swapping
	// the ciphertexts between two slots must fail closed for both.
	if _, err := e.Resolve(ctx, "scope-1", "tok-1"); !errors.Is(err, ErrCorrupted) {
		t.Errorf("swapped tok-1 Resolve() error = %v, want ErrCorrupted", err)
	}
	if _, err := e.Resolve(ctx, "scope-1", "tok-2"); !errors.Is(err, ErrCorrupted) {
		t.Errorf("swapped tok-2 Resolve() error = %v, want ErrCorrupted", err)
	}
}

func TestEncryptedCrossScopeDenied(t *testing.T) {
	e := mustEncrypted(t)
	ctx := context.Background()
	if err := e.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := e.Resolve(ctx, "scope-2", "tok-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-scope token: Resolve() error = %v, want ErrNotFound", err)
	}
}

func TestEncryptedExpiryNotDisclosedAndLazilyDeleted(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	now := start
	e, err := newEncryptedWithClock(testMasterKey(), time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newEncryptedWithClock() error = %v", err)
	}
	ctx := context.Background()
	if err := e.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	now = start.Add(time.Minute - time.Nanosecond)
	if got, err := e.Resolve(ctx, "scope-1", "tok-1"); err != nil || got != "synthetic-value-a" {
		t.Errorf("before expiry: Resolve() = (%q, %v), want (%q, nil)", got, err, "synthetic-value-a")
	}

	now = start.Add(2 * time.Minute)
	if _, err := e.Resolve(ctx, "scope-1", "tok-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after expiry: Resolve() error = %v, want ErrNotFound", err)
	}
	k := encKey{
		scope: digest(e.hmacKey, scopeDomain, []byte("scope-1")),
		token: digest(e.hmacKey, tokenDomain, []byte("tok-1")),
	}
	e.mu.Lock()
	_, ok := e.mapping[k]
	e.mu.Unlock()
	if ok {
		t.Error("expired mapping was not lazily deleted")
	}
}

func TestEncryptedExpirySweptOnSave(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	now := start
	e, err := newEncryptedWithClock(testMasterKey(), time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newEncryptedWithClock() error = %v", err)
	}
	ctx := context.Background()
	if err := e.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := e.Save(ctx, "scope-2", "tok-2", "synthetic-value-b"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// Advance past the first record's expiry and save an unrelated record. The
	// expired record must be opportunistically swept.
	now = start.Add(2 * time.Minute)
	if err := e.Save(ctx, "scope-3", "tok-3", "synthetic-value-c"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	k1 := encKey{
		scope: digest(e.hmacKey, scopeDomain, []byte("scope-1")),
		token: digest(e.hmacKey, tokenDomain, []byte("tok-1")),
	}
	e.mu.Lock()
	_, ok1 := e.mapping[k1]
	e.mu.Unlock()
	if ok1 {
		t.Error("expired record was not opportunistically swept on Save")
	}
}

func TestEncryptedRevokeScope(t *testing.T) {
	e := mustEncrypted(t)
	ctx := context.Background()
	if err := e.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := e.Save(ctx, "scope-1", "tok-2", "synthetic-value-b"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := e.Save(ctx, "scope-2", "tok-1", "synthetic-value-c"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if err := e.RevokeScope(ctx, "scope-1"); err != nil {
		t.Fatalf("RevokeScope() error = %v", err)
	}
	if _, err := e.Resolve(ctx, "scope-1", "tok-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked scope-1 tok-1: Resolve() error = %v, want ErrNotFound", err)
	}
	if _, err := e.Resolve(ctx, "scope-1", "tok-2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked scope-1 tok-2: Resolve() error = %v, want ErrNotFound", err)
	}
	if got, err := e.Resolve(ctx, "scope-2", "tok-1"); err != nil || got != "synthetic-value-c" {
		t.Errorf("other scope: Resolve() = (%q, %v), want (%q, nil)", got, err, "synthetic-value-c")
	}
}

func TestEncryptedEmptyInputs(t *testing.T) {
	e := mustEncrypted(t)
	ctx := context.Background()
	if err := e.Save(ctx, "", "tok-1", "v"); !errors.Is(err, ErrEmptyScope) {
		t.Errorf("empty scope save: %v, want ErrEmptyScope", err)
	}
	if err := e.Save(ctx, "scope-1", "", "v"); !errors.Is(err, ErrEmptyToken) {
		t.Errorf("empty token save: %v, want ErrEmptyToken", err)
	}
	if err := e.Save(ctx, "scope-1", "tok-1", ""); !errors.Is(err, ErrEmptyOriginal) {
		t.Errorf("empty original save: %v, want ErrEmptyOriginal", err)
	}
	if _, err := e.Resolve(ctx, "", "tok-1"); !errors.Is(err, ErrEmptyScope) {
		t.Errorf("empty scope resolve: %v, want ErrEmptyScope", err)
	}
	if err := e.RevokeScope(ctx, ""); !errors.Is(err, ErrEmptyScope) {
		t.Errorf("empty scope revoke: %v, want ErrEmptyScope", err)
	}
}

func TestEncryptedRejectsInvalidTTL(t *testing.T) {
	for _, ttl := range []time.Duration{0, -1, -time.Hour} {
		if e, err := NewEncrypted(testMasterKey(), ttl); err == nil || e != nil {
			t.Errorf("NewEncrypted(%v) = (%v, %v), want (nil, ErrInvalidTTL)", ttl, e, err)
		} else if !errors.Is(err, ErrInvalidTTL) {
			t.Errorf("NewEncrypted(%v) error = %v, want ErrInvalidTTL", ttl, err)
		}
	}
}

func TestEncryptedConcurrentSafety(t *testing.T) {
	e := mustEncrypted(t)
	ctx := context.Background()
	const workers = 32
	const perWorker = 200
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			scope := "scope-" + string(rune('a'+w))
			for i := 0; i < perWorker; i++ {
				token := "tok-" + string(rune('a'+w)) + "-" + string(rune('0'+i%10))
				original := "synthetic-" + string(rune('a'+w)) + "-" + string(rune('0'+i%10))
				if err := e.Save(ctx, scope, token, original); err != nil {
					errs <- err
					return
				}
				got, err := e.Resolve(ctx, scope, token)
				if err != nil {
					errs <- err
					return
				}
				if got != original {
					errs <- errors.New("resolve mismatch under concurrency")
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestEncryptedConcurrentRevokeRace(t *testing.T) {
	e := mustEncrypted(t)
	ctx := context.Background()
	const scopes = 8
	const tokensPerScope = 50
	for s := 0; s < scopes; s++ {
		scope := "scope-" + string(rune('a'+s))
		for i := 0; i < tokensPerScope; i++ {
			token := "tok-" + string(rune('a'+s)) + "-" + string(rune('0'+i%10))
			if err := e.Save(ctx, scope, token, "synthetic-"+token); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, scopes*2)
	for s := 0; s < scopes; s++ {
		scope := "scope-" + string(rune('a'+s))
		wg.Add(1)
		go func(scope string) {
			defer wg.Done()
			if err := e.RevokeScope(ctx, scope); err != nil {
				errs <- err
			}
		}(scope)
		wg.Add(1)
		go func(scope string) {
			defer wg.Done()
			for i := 0; i < tokensPerScope; i++ {
				token := "tok-" + scope[len("scope-"):] + "-" + string(rune('0'+i%10))
				if _, err := e.Resolve(ctx, scope, token); err != nil && !errors.Is(err, ErrNotFound) {
					errs <- err
					return
				}
			}
		}(scope)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
