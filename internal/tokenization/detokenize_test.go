package tokenization

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/ownership"
	"github.com/klrushka/llm-proxy/internal/vault"
)

// countingResolver wraps a Resolver and counts how many times each token is
// resolved, so tests can assert that a repeated token is looked up once.
type countingResolver struct {
	inner Resolver
	calls map[string]int
}

func (c *countingResolver) Resolve(ctx context.Context, scope, token string) (string, error) {
	c.calls[token]++
	return c.inner.Resolve(ctx, scope, token)
}

// failingResolver returns a fixed error for every Resolve call.
type failingResolver struct {
	err error
}

func (f *failingResolver) Resolve(ctx context.Context, scope, token string) (string, error) {
	return "", f.err
}

// mustVault builds an in-memory vault with a generous TTL.
func mustVault(t *testing.T) *vault.Memory {
	t.Helper()
	v, err := vault.NewMemory(time.Hour)
	if err != nil {
		t.Fatalf("vault.NewMemory() error = %v", err)
	}
	return v
}

// tokenizeAndSave runs Replace for the given personal entities, saves every
// issued mapping into v under scope, and returns the tokenized text.
func tokenizeAndSave(t *testing.T, text, scope string, results []ownership.Entity, v *vault.Memory) string {
	t.Helper()
	g, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	replaced, err := Replace(text, scope, results, g)
	if err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	ctx := context.Background()
	for _, r := range replaced.Replacements {
		orig := text[r.Entity.Start:r.Entity.End]
		if err := v.Save(ctx, scope, r.Token, orig); err != nil {
			t.Fatalf("vault.Save() error = %v", err)
		}
	}
	return replaced.Text
}

func TestDetokenizeRoundTripCyrillic(t *testing.T) {
	text := "Клиент Иванов Иван, телефон +7 900 123-45-67, email ivanov@example.com"
	nameStart := strings.Index(text, "Иванов Иван")
	phoneStart := strings.Index(text, "+7 900 123-45-67")
	emailStart := strings.Index(text, "ivanov@example.com")
	results := []ownership.Entity{
		ownEnt(detection.TypeFullName, nameStart, nameStart+len("Иванов Иван"), true),
		ownEnt(detection.TypePhone, phoneStart, phoneStart+len("+7 900 123-45-67"), true),
		ownEnt(detection.TypeEmail, emailStart, emailStart+len("ivanov@example.com"), true),
	}

	v := mustVault(t)
	tokenized := tokenizeAndSave(t, text, "scope-1", results, v)

	got, err := Detokenize(context.Background(), tokenized, "scope-1", ModeStrict, v)
	if err != nil {
		t.Fatalf("Detokenize() error = %v", err)
	}
	if got.Text != text {
		t.Errorf("round trip Text = %q, want %q", got.Text, text)
	}
	if len(got.UnresolvedTokens) != 0 {
		t.Errorf("UnresolvedTokens = %v, want none", got.UnresolvedTokens)
	}
}

func TestDetokenizeStrictUnknownToken(t *testing.T) {
	v := mustVault(t)
	ctx := context.Background()
	if err := v.Save(ctx, "scope-1", "<EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa>", "a@example.com"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	text := "Клиент <EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa> и <EMAIL_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb>"

	got, err := Detokenize(ctx, text, "scope-1", ModeStrict, v)
	if !errors.Is(err, ErrUnresolved) {
		t.Fatalf("Detokenize() error = %v, want ErrUnresolved", err)
	}
	if got.Text != "" {
		t.Errorf("strict failure returned partial text %q", got.Text)
	}
	if len(got.UnresolvedTokens) != 0 {
		t.Errorf("strict failure returned partial unresolved tokens %v", got.UnresolvedTokens)
	}
}

func TestDetokenizeStrictCrossScopeToken(t *testing.T) {
	v := mustVault(t)
	ctx := context.Background()
	if err := v.Save(ctx, "scope-1", "<EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa>", "a@example.com"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	text := "Клиент <EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa>"

	got, err := Detokenize(ctx, text, "scope-2", ModeStrict, v)
	if !errors.Is(err, ErrUnresolved) {
		t.Fatalf("Detokenize() error = %v, want ErrUnresolved", err)
	}
	if got.Text != "" {
		t.Errorf("cross-scope strict failure returned partial text %q", got.Text)
	}
}

func TestDetokenizeStrictRevokedToken(t *testing.T) {
	v := mustVault(t)
	ctx := context.Background()
	if err := v.Save(ctx, "scope-1", "<EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa>", "a@example.com"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := v.RevokeScope(ctx, "scope-1"); err != nil {
		t.Fatalf("RevokeScope() error = %v", err)
	}
	text := "Клиент <EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa>"

	got, err := Detokenize(ctx, text, "scope-1", ModeStrict, v)
	if !errors.Is(err, ErrUnresolved) {
		t.Fatalf("Detokenize() error = %v, want ErrUnresolved", err)
	}
	if got.Text != "" {
		t.Errorf("revoked strict failure returned partial text %q", got.Text)
	}
}

func TestDetokenizeStrictExpiredToken(t *testing.T) {
	// Expiry surfaces to the detokenizer as vault.ErrNotFound (the vault's own
	// expiry tests cover the clock-driven side). A resolver returning
	// ErrNotFound represents an expired mapping and must fail strict mode
	// without partial plaintext.
	resolver := &failingResolver{err: vault.ErrNotFound}
	text := "Клиент <EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa>"

	got, err := Detokenize(context.Background(), text, "scope-1", ModeStrict, resolver)
	if !errors.Is(err, ErrUnresolved) {
		t.Fatalf("Detokenize() error = %v, want ErrUnresolved", err)
	}
	if got.Text != "" {
		t.Errorf("expired strict failure returned partial text %q", got.Text)
	}
}

func TestDetokenizePreserveMixedKnownUnknown(t *testing.T) {
	v := mustVault(t)
	ctx := context.Background()
	if err := v.Save(ctx, "scope-1", "<EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa>", "a@example.com"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	text := "Клиент <EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa> и <EMAIL_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb>"

	got, err := Detokenize(ctx, text, "scope-1", ModePreserve, v)
	if err != nil {
		t.Fatalf("Detokenize() error = %v", err)
	}
	want := "Клиент a@example.com и <EMAIL_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb>"
	if got.Text != want {
		t.Errorf("Text = %q, want %q", got.Text, want)
	}
	if !reflect.DeepEqual(got.UnresolvedTokens, []string{"<EMAIL_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb>"}) {
		t.Errorf("UnresolvedTokens = %v, want the unknown token", got.UnresolvedTokens)
	}
}

func TestDetokenizePreserveUnresolvedOrderDeterministic(t *testing.T) {
	v := mustVault(t)
	ctx := context.Background()
	if err := v.Save(ctx, "scope-1", "<EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa>", "a@example.com"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	// Unknown tokens appear in first-appearance order, with duplicates removed.
	text := "<PHONE_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb> <EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa> " +
		"<PHONE_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb> <FULL_NAME_cccccccccccccccccccccccccccccccc>"

	got, err := Detokenize(ctx, text, "scope-1", ModePreserve, v)
	if err != nil {
		t.Fatalf("Detokenize() error = %v", err)
	}
	want := "<PHONE_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb> a@example.com " +
		"<PHONE_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb> <FULL_NAME_cccccccccccccccccccccccccccccccc>"
	if got.Text != want {
		t.Errorf("Text = %q, want %q", got.Text, want)
	}
	wantUnresolved := []string{
		"<PHONE_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb>",
		"<FULL_NAME_cccccccccccccccccccccccccccccccc>",
	}
	if !reflect.DeepEqual(got.UnresolvedTokens, wantUnresolved) {
		t.Errorf("UnresolvedTokens = %v, want %v", got.UnresolvedTokens, wantUnresolved)
	}
}

func TestDetokenizeRepeatedTokenResolvedOnce(t *testing.T) {
	v := mustVault(t)
	ctx := context.Background()
	if err := v.Save(ctx, "scope-1", "<EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa>", "a@example.com"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	text := "<EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa> и <EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa> и <EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa>"

	counting := &countingResolver{inner: v, calls: map[string]int{}}
	got, err := Detokenize(ctx, text, "scope-1", ModeStrict, counting)
	if err != nil {
		t.Fatalf("Detokenize() error = %v", err)
	}
	if got.Text != "a@example.com и a@example.com и a@example.com" {
		t.Errorf("Text = %q, want all occurrences restored", got.Text)
	}
	if n := counting.calls["<EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa>"]; n != 1 {
		t.Errorf("repeated token resolved %d times, want 1", n)
	}
}

func TestDetokenizeUTF8AdjacentTokens(t *testing.T) {
	v := mustVault(t)
	ctx := context.Background()
	if err := v.Save(ctx, "scope-1", "<EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa>", "a@example.com"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := v.Save(ctx, "scope-1", "<PHONE_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb>", "+7 900 123-45-67"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	// Adjacent tokens with Cyrillic around them; byte offsets must stay valid.
	text := "Привет<EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa><PHONE_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb>мир"

	got, err := Detokenize(ctx, text, "scope-1", ModeStrict, v)
	if err != nil {
		t.Fatalf("Detokenize() error = %v", err)
	}
	want := "Приветa@example.com+7 900 123-45-67мир"
	if got.Text != want {
		t.Errorf("Text = %q, want %q", got.Text, want)
	}
}

func TestDetokenizeInvalidInputs(t *testing.T) {
	v := mustVault(t)
	ctx := context.Background()
	text := "<EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa>"

	if _, err := Detokenize(ctx, text, "scope-1", Mode("bogus"), v); !errors.Is(err, ErrInvalidMode) {
		t.Errorf("invalid mode: got %v, want ErrInvalidMode", err)
	}
	if _, err := Detokenize(ctx, text, "", ModeStrict, v); !errors.Is(err, ErrEmptyScope) {
		t.Errorf("empty scope: got %v, want ErrEmptyScope", err)
	}
	if _, err := Detokenize(ctx, text, "scope-1", ModeStrict, nil); !errors.Is(err, ErrNilResolver) {
		t.Errorf("nil resolver: got %v, want ErrNilResolver", err)
	}
}

func TestDetokenizeResolverFailureFailsClosed(t *testing.T) {
	boom := errors.New("resolver boom")
	resolver := &failingResolver{err: boom}
	text := "Клиент <EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa>"

	for _, mode := range []Mode{ModeStrict, ModePreserve} {
		got, err := Detokenize(context.Background(), text, "scope-1", mode, resolver)
		if !errors.Is(err, boom) {
			t.Errorf("mode %s: errors.Is(cause) = false, want true", mode)
		}
		if !errors.Is(err, ErrResolverFailure) {
			t.Errorf("mode %s: errors.Is(ErrResolverFailure) = false, want true", mode)
		}
		if err == nil || err.Error() != ErrResolverFailure.Error() {
			t.Errorf("mode %s: Error() = %q, want fixed safe text %q", mode, err, ErrResolverFailure.Error())
		}
		if got.Text != "" {
			t.Errorf("mode %s: partial text returned on resolver failure: %q", mode, got.Text)
		}
		if len(got.UnresolvedTokens) != 0 {
			t.Errorf("mode %s: partial unresolved tokens returned on resolver failure: %v", mode, got.UnresolvedTokens)
		}
	}
}

func TestDetokenizeContextCancellationFailsClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resolver := &failingResolver{err: context.Canceled}
	text := "Клиент <EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa>"

	for _, mode := range []Mode{ModeStrict, ModePreserve} {
		got, err := Detokenize(ctx, text, "scope-1", mode, resolver)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("mode %s: errors.Is(context.Canceled) = false, want true", mode)
		}
		if !errors.Is(err, ErrResolverFailure) {
			t.Errorf("mode %s: errors.Is(ErrResolverFailure) = false, want true", mode)
		}
		if err == nil || err.Error() != ErrResolverFailure.Error() {
			t.Errorf("mode %s: Error() = %q, want fixed safe text %q", mode, err, ErrResolverFailure.Error())
		}
		if got.Text != "" {
			t.Errorf("mode %s: partial text returned on cancellation: %q", mode, got.Text)
		}
	}
}

func TestDetokenizeResolverErrorTextIsSafe(t *testing.T) {
	// A resolver whose Error() embeds a synthetic token and plaintext marker
	// must not leak either into the detokenizer's returned error text.
	const tokenMarker = "<EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa>"
	const plainMarker = "synthetic-plaintext-Иванов"
	cause := errors.New("resolver leaked " + tokenMarker + " and " + plainMarker)
	resolver := &failingResolver{err: cause}
	text := "Клиент " + tokenMarker

	for _, mode := range []Mode{ModeStrict, ModePreserve} {
		got, err := Detokenize(context.Background(), text, "scope-1", mode, resolver)
		if !errors.Is(err, cause) {
			t.Errorf("mode %s: errors.Is(cause) = false, want true", mode)
		}
		if !errors.Is(err, ErrResolverFailure) {
			t.Errorf("mode %s: errors.Is(ErrResolverFailure) = false, want true", mode)
		}
		if err == nil {
			t.Fatalf("mode %s: err = nil, want resolver failure", mode)
		}
		if strings.Contains(err.Error(), tokenMarker) {
			t.Errorf("mode %s: Error() leaked token %q: %q", mode, tokenMarker, err.Error())
		}
		if strings.Contains(err.Error(), plainMarker) {
			t.Errorf("mode %s: Error() leaked plaintext %q: %q", mode, plainMarker, err.Error())
		}
		if err.Error() != ErrResolverFailure.Error() {
			t.Errorf("mode %s: Error() = %q, want fixed safe text %q", mode, err.Error(), ErrResolverFailure.Error())
		}
		if got.Text != "" {
			t.Errorf("mode %s: partial text returned on resolver failure: %q", mode, got.Text)
		}
		if len(got.UnresolvedTokens) != 0 {
			t.Errorf("mode %s: partial unresolved tokens returned on resolver failure: %v", mode, got.UnresolvedTokens)
		}
	}
}

func TestDetokenizeNoTokensReturnsUnchanged(t *testing.T) {
	v := mustVault(t)
	text := "Обычный текст без токенов, Иванов Иван и +7 900 123-45-67"

	got, err := Detokenize(context.Background(), text, "scope-1", ModeStrict, v)
	if err != nil {
		t.Fatalf("Detokenize() error = %v", err)
	}
	if got.Text != text {
		t.Errorf("Text = %q, want unchanged %q", got.Text, text)
	}
	if len(got.UnresolvedTokens) != 0 {
		t.Errorf("UnresolvedTokens = %v, want none", got.UnresolvedTokens)
	}
}

func TestDetokenizeNoTokensDoesNotCallResolver(t *testing.T) {
	// A resolver that panics on any call proves the no-token path never
	// touches the resolver.
	panicResolver := &failingResolver{err: errors.New("must not be called")}
	text := "Обычный текст без токенов"

	got, err := Detokenize(context.Background(), text, "scope-1", ModeStrict, panicResolver)
	if err != nil {
		t.Fatalf("Detokenize() error = %v", err)
	}
	if got.Text != text {
		t.Errorf("Text = %q, want unchanged %q", got.Text, text)
	}
}

func TestDetokenizePreserveRepeatedUnknownTokenOnce(t *testing.T) {
	v := mustVault(t)
	ctx := context.Background()
	text := "<EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa> <EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa>"

	counting := &countingResolver{inner: v, calls: map[string]int{}}
	got, err := Detokenize(ctx, text, "scope-1", ModePreserve, counting)
	if err != nil {
		t.Fatalf("Detokenize() error = %v", err)
	}
	if got.Text != text {
		t.Errorf("Text = %q, want unchanged %q", got.Text, text)
	}
	if !reflect.DeepEqual(got.UnresolvedTokens, []string{"<EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa>"}) {
		t.Errorf("UnresolvedTokens = %v, want single unique token", got.UnresolvedTokens)
	}
	if n := counting.calls["<EMAIL_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa>"]; n != 1 {
		t.Errorf("repeated unknown token resolved %d times, want 1", n)
	}
}
