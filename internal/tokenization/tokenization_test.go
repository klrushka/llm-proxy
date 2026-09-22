package tokenization

import (
	"errors"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/klrushka/llm-proxy/internal/detection"
)

// seqReader returns a fixed sequence of blocks, one per Read call. It is used
// only by tests to drive deterministic random suffixes.
type seqReader struct {
	blocks [][]byte
	i      int
}

func (s *seqReader) Read(p []byte) (int, error) {
	if s.i >= len(s.blocks) {
		return 0, io.EOF
	}
	b := s.blocks[s.i]
	s.i++
	return copy(p, b), nil
}

func block(hexStr string) []byte {
	b := make([]byte, len(hexStr)/2)
	for i := 0; i < len(b); i++ {
		hi := hexVal(hexStr[2*i])
		lo := hexVal(hexStr[2*i+1])
		b[i] = hi<<4 | lo
	}
	return b
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return 0
}

func mustRegistry(t *testing.T) *detection.Registry {
	t.Helper()
	reg, err := detection.New()
	if err != nil {
		t.Fatalf("detection.New() error = %v", err)
	}
	return reg
}

var tokenShape = regexp.MustCompile(`^<[A-Z_]+_[0-9a-f]{32}>$`)

func TestTokenShape(t *testing.T) {
	g := newGenerator(&seqReader{blocks: [][]byte{block("00000000000000000000000000000000")}}, mustRegistry(t))
	tok, err := g.Token("scope-1", detection.TypeFullName, "Иванов Иван")
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if !tokenShape.MatchString(tok) {
		t.Errorf("Token() = %q, does not match shape %v", tok, tokenShape)
	}
	want := "<" + string(detection.TypeFullName) + "_00000000000000000000000000000000>"
	if tok != want {
		t.Errorf("Token() = %q, want %q", tok, want)
	}
}

func TestReuseWithinScope(t *testing.T) {
	g := newGenerator(&seqReader{blocks: [][]byte{block("11111111111111111111111111111111")}}, mustRegistry(t))
	a, err := g.Token("scope-1", detection.TypeEmail, "ivanov@example.com")
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	b, err := g.Token("scope-1", detection.TypeEmail, "ivanov@example.com")
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if a != b {
		t.Errorf("repeated value in same scope: got %q and %q, want equal", a, b)
	}
}

func TestDifferentScopesGetDifferentTokens(t *testing.T) {
	g := newGenerator(&seqReader{blocks: [][]byte{
		block("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		block("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
	}}, mustRegistry(t))
	a, err := g.Token("scope-1", detection.TypePhone, "+7 900 123-45-67")
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	b, err := g.Token("scope-2", detection.TypePhone, "+7 900 123-45-67")
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if a == b {
		t.Errorf("same value in different scopes: got equal tokens %q", a)
	}
}

func TestDifferentTypesDoNotReuseToken(t *testing.T) {
	g := newGenerator(&seqReader{blocks: [][]byte{
		block("cccccccccccccccccccccccccccccccc"),
		block("dddddddddddddddddddddddddddddddd"),
	}}, mustRegistry(t))
	a, err := g.Token("scope-1", detection.TypeFullName, "Иванов Иван")
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	b, err := g.Token("scope-1", detection.TypeEmail, "Иванов Иван")
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if a == b {
		t.Errorf("different types in same scope: got equal tokens %q", a)
	}
}

func TestDifferentValuesDoNotReuseToken(t *testing.T) {
	g := newGenerator(&seqReader{blocks: [][]byte{
		block("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"),
		block("ffffffffffffffffffffffffffffffff"),
	}}, mustRegistry(t))
	a, err := g.Token("scope-1", detection.TypeEmail, "a@example.com")
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	b, err := g.Token("scope-1", detection.TypeEmail, "b@example.com")
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if a == b {
		t.Errorf("different values in same scope: got equal tokens %q", a)
	}
}

func TestNULContainingTuplesDoNotAlias(t *testing.T) {
	g := newGenerator(&seqReader{blocks: [][]byte{
		block("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		block("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
	}}, mustRegistry(t))

	// Under delimiter concatenation (scope + "\x00" + type + "\x00" + value)
	// these two tuples produce the identical string and would alias into one
	// reuse slot. The struct key must keep them distinct.
	a, err := g.Token("s\x00EMAIL", detection.TypePhone, "v")
	if err != nil {
		t.Fatalf("Token(A) error = %v", err)
	}
	b, err := g.Token("s", detection.TypeEmail, "PHONE\x00v")
	if err != nil {
		t.Fatalf("Token(B) error = %v", err)
	}
	if a == b {
		t.Fatalf("NUL-containing tuples aliased: got equal tokens %q", a)
	}
}

func TestInvalidInput(t *testing.T) {
	g := newGenerator(&seqReader{}, mustRegistry(t))

	if _, err := g.Token("", detection.TypeEmail, "a@example.com"); !errors.Is(err, ErrEmptyScope) {
		t.Errorf("empty scope: got %v, want ErrEmptyScope", err)
	}
	if _, err := g.Token("scope-1", detection.TypeEmail, ""); !errors.Is(err, ErrEmptyValue) {
		t.Errorf("empty value: got %v, want ErrEmptyValue", err)
	}
	if _, err := g.Token("scope-1", detection.Type("BOGUS"), "a@example.com"); !errors.Is(err, ErrUnknownType) {
		t.Errorf("unknown type: got %v, want ErrUnknownType", err)
	}
	if _, err := g.Token("scope-1", detection.Type(""), "a@example.com"); !errors.Is(err, ErrUnknownType) {
		t.Errorf("empty type: got %v, want ErrUnknownType", err)
	}
}

func TestCollisionRetry(t *testing.T) {
	// First token for key A consumes suffix S1. The token for key B (same type,
	// different value) first draws S1 again (a collision with key A) and must
	// retry, drawing S2 on the second attempt.
	g := newGenerator(&seqReader{blocks: [][]byte{
		block("11111111111111111111111111111111"), // S1 for key A
		block("11111111111111111111111111111111"), // S1 again -> collision
		block("22222222222222222222222222222222"), // S2 for key B
	}}, mustRegistry(t))

	a, err := g.Token("scope-1", detection.TypeEmail, "a@example.com")
	if err != nil {
		t.Fatalf("Token(A) error = %v", err)
	}
	b, err := g.Token("scope-1", detection.TypeEmail, "b@example.com")
	if err != nil {
		t.Fatalf("Token(B) error = %v", err)
	}
	if a == b {
		t.Fatalf("collision retry failed: tokens equal %q", a)
	}
	if !strings.HasSuffix(a, "11111111111111111111111111111111>") {
		t.Errorf("Token(A) = %q, want suffix 1111...>", a)
	}
	if !strings.HasSuffix(b, "22222222222222222222222222222222>") {
		t.Errorf("Token(B) = %q, want suffix 2222...>", b)
	}
}

func TestConcurrentSafety(t *testing.T) {
	g, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

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
				again, err := g.Token(scope, detection.TypeEmail, value)
				if err != nil {
					errs <- err
					return
				}
				if again != tok {
					errs <- errors.New("reuse violated under concurrency")
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

func TestTokenNeverContainsPlaintextOrScope(t *testing.T) {
	g := newGenerator(&seqReader{blocks: [][]byte{block("00000000000000000000000000000000")}}, mustRegistry(t))

	scope := "SCOPE-UNIQUE-9f3a"
	value := "Иванов Иван Иванович ivanov@example.com +7 900 123-45-67"
	tok, err := g.Token(scope, detection.TypeFullName, value)
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if tok == scope || tok == value {
		t.Errorf("token %q equals scope or value", tok)
	}
	if strings.Contains(tok, scope) {
		t.Errorf("token %q contains scope %q", tok, scope)
	}
	if strings.Contains(tok, value) {
		t.Errorf("token %q contains value %q", tok, value)
	}
	// Substantial synthetic plaintext markers that cannot appear in a lowercase
	// hex suffix (they contain non-hex characters), so the check is
	// deterministic and free of probabilistic collisions.
	for _, part := range []string{"Иванов", "Иван", "ivanov", "example.com"} {
		if strings.Contains(tok, part) {
			t.Errorf("token %q contains plaintext part %q", tok, part)
		}
	}
	if !tokenShape.MatchString(tok) {
		t.Errorf("Token() = %q, does not match shape %v", tok, tokenShape)
	}
}
