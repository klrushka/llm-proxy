package tokenization

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klrushka/llm-proxy/internal/detection"
)

// keyedMasterKey returns a fixed 32-byte master key for tests.
func keyedMasterKey() [32]byte {
	var k [32]byte
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

// testMasterKey is a fixed 32-byte master key used only by the deterministic
// test seam newGenerator. Production constructors never use a fixed key.
var testMasterKey = [32]byte{
	0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
	0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
	0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
}

// newGenerator is the deterministic test seam used by tests to inject a random
// reader and registry. It uses a fixed test key and the default TTL so tests
// are reproducible. Production wiring must use NewKeyed with the real key.
func newGenerator(r io.Reader, reg *detection.Registry) *Generator {
	return newKeyedGenerator(r, reg, testMasterKey, defaultTTL, time.Now)
}

func TestNewKeyedRejectsInvalidTTL(t *testing.T) {
	for _, ttl := range []time.Duration{0, -1, -time.Hour} {
		if g, err := NewKeyed(keyedMasterKey(), ttl); err == nil || g != nil {
			t.Errorf("NewKeyed(%v) = (%v, %v), want (nil, ErrInvalidTTL)", ttl, g, err)
		} else if !errors.Is(err, ErrInvalidTTL) {
			t.Errorf("NewKeyed(%v) error = %v, want ErrInvalidTTL", ttl, err)
		}
	}
}

func TestKeyedReuseWithinTTL(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	now := start
	g := newKeyedGenerator(&seqReader{blocks: [][]byte{block("11111111111111111111111111111111")}}, mustRegistry(t), keyedMasterKey(), time.Minute, func() time.Time { return now })

	a, err := g.Token("scope-1", detection.TypeEmail, "ivanov@example.com")
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	now = start.Add(time.Minute - time.Nanosecond)
	b, err := g.Token("scope-1", detection.TypeEmail, "ivanov@example.com")
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if a != b {
		t.Errorf("reuse within TTL: got %q and %q, want equal", a, b)
	}
}

func TestKeyedNewTokenAfterTTL(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	now := start
	g := newKeyedGenerator(&seqReader{blocks: [][]byte{
		block("11111111111111111111111111111111"),
		block("22222222222222222222222222222222"),
	}}, mustRegistry(t), keyedMasterKey(), time.Minute, func() time.Time { return now })

	a, err := g.Token("scope-1", detection.TypeEmail, "ivanov@example.com")
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	now = start.Add(2 * time.Minute)
	b, err := g.Token("scope-1", detection.TypeEmail, "ivanov@example.com")
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if a == b {
		t.Errorf("after TTL: got equal tokens %q, want a fresh token", a)
	}
}

func TestKeyedNewTokenAfterRevoke(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	now := start
	g := newKeyedGenerator(&seqReader{blocks: [][]byte{
		block("11111111111111111111111111111111"),
		block("22222222222222222222222222222222"),
	}}, mustRegistry(t), keyedMasterKey(), time.Minute, func() time.Time { return now })

	a, err := g.Token("scope-1", detection.TypeEmail, "ivanov@example.com")
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if err := g.RevokeScope(context.Background(), "scope-1"); err != nil {
		t.Fatalf("RevokeScope() error = %v", err)
	}
	b, err := g.Token("scope-1", detection.TypeEmail, "ivanov@example.com")
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if a == b {
		t.Errorf("after revoke: got equal tokens %q, want a fresh token", a)
	}
}

func TestKeyedRevokeOnlyAffectsScope(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	now := start
	g := newKeyedGenerator(&seqReader{blocks: [][]byte{
		block("11111111111111111111111111111111"),
		block("22222222222222222222222222222222"),
		block("33333333333333333333333333333333"),
		block("44444444444444444444444444444444"),
	}}, mustRegistry(t), keyedMasterKey(), time.Minute, func() time.Time { return now })

	a, err := g.Token("scope-1", detection.TypeEmail, "ivanov@example.com")
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	b, err := g.Token("scope-2", detection.TypeEmail, "ivanov@example.com")
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if err := g.RevokeScope(context.Background(), "scope-1"); err != nil {
		t.Fatalf("RevokeScope() error = %v", err)
	}
	// scope-2 reuse must be preserved.
	c, err := g.Token("scope-2", detection.TypeEmail, "ivanov@example.com")
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if c != b {
		t.Errorf("unrelated scope reuse broken: got %q, want %q", c, b)
	}
	// scope-1 must issue a fresh token.
	d, err := g.Token("scope-1", detection.TypeEmail, "ivanov@example.com")
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if d == a {
		t.Errorf("revoked scope reused token %q", d)
	}
}

func TestKeyedRevokeScopeEmpty(t *testing.T) {
	g := newGenerator(&seqReader{}, mustRegistry(t))
	if err := g.RevokeScope(context.Background(), ""); !errors.Is(err, ErrEmptyScope) {
		t.Errorf("RevokeScope(empty) error = %v, want ErrEmptyScope", err)
	}
}

func TestKeyedMapsHoldNoPlaintext(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	now := start
	g := newKeyedGenerator(&seqReader{blocks: [][]byte{block("11111111111111111111111111111111")}}, mustRegistry(t), keyedMasterKey(), time.Minute, func() time.Time { return now })

	const scope = "SCOPE-MARKER-9f3a"
	const value = "synthetic-plaintext-Иванов-ivanov@example.com"
	tok, err := g.Token(scope, detection.TypeEmail, value)
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}

	// Decode the hex suffix of the issued token into its raw bytes so we can
	// prove neither the raw suffix nor the full token string is stored.
	suffixHex := tok[len("<EMAIL_") : len(tok)-1]
	rawSuffix, err := hex.DecodeString(suffixHex)
	if err != nil {
		t.Fatalf("decode suffix %q: %v", suffixHex, err)
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	// Scope buckets are keyed by digest, never by plaintext scope.
	for scopeDigest := range g.issued {
		if strings.Contains(string(scopeDigest[:]), scope) {
			t.Error("issued map key contains plaintext scope")
		}
	}
	// Bucket keys are digestKey structs; their scope/value fields must be
	// digests, never plaintext.
	for _, bucket := range g.issued {
		for dk := range bucket {
			if strings.Contains(string(dk.scope[:]), scope) {
				t.Error("issued bucket key contains plaintext scope")
			}
			if strings.Contains(string(dk.value[:]), value) {
				t.Error("issued bucket key contains plaintext value")
			}
		}
	}
	// Reverse token map keys are token digests, never the plaintext token.
	for tokDigest := range g.tokens {
		if strings.Contains(string(tokDigest[:]), tok) {
			t.Errorf("tokens map key contains plaintext token %q", tok)
		}
	}
	// Reverse token map values are digestKey structs; their scope/value fields
	// must be digests, never plaintext.
	for _, dk := range g.tokens {
		if strings.Contains(string(dk.scope[:]), scope) {
			t.Error("tokens map value contains plaintext scope")
		}
		if strings.Contains(string(dk.value[:]), value) {
			t.Error("tokens map value contains plaintext value")
		}
	}
	// Issued entries hold only a random seed, a token digest and an expiry.
	// Neither the raw suffix bytes nor the full token string may appear
	// directly in the seed or the token digest.
	for _, bucket := range g.issued {
		for _, ent := range bucket {
			if bytes.Contains(ent.seed[:], rawSuffix) {
				t.Error("issued entry seed contains raw suffix bytes")
			}
			if strings.Contains(string(ent.seed[:]), tok) {
				t.Errorf("issued entry seed contains plaintext token %q", tok)
			}
			if strings.Contains(string(ent.tokenDigest[:]), tok) {
				t.Errorf("issued entry tokenDigest contains plaintext token %q", tok)
			}
		}
	}
}

func TestKeyedConcurrentTokenRevoke(t *testing.T) {
	g, err := NewKeyed(keyedMasterKey(), time.Hour)
	if err != nil {
		t.Fatalf("NewKeyed() error = %v", err)
	}
	ctx := context.Background()
	const workers = 16
	const perWorker = 100
	var wg sync.WaitGroup
	errs := make(chan error, workers*2)
	for w := 0; w < workers; w++ {
		scope := "scope-" + string(rune('a'+w))
		wg.Add(1)
		go func(scope string) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				value := "value-" + string(rune('a'+w)) + "-" + string(rune('0'+i%10))
				tok, err := g.Token(scope, detection.TypeEmail, value)
				if err != nil {
					errs <- err
					return
				}
				if !tokenShape.MatchString(tok) {
					errs <- errors.New("bad token shape under concurrency")
					return
				}
			}
		}(scope)
		wg.Add(1)
		go func(scope string) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				if err := g.RevokeScope(ctx, scope); err != nil {
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
