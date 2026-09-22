package tokenization

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/ownership"
	"github.com/klrushka/llm-proxy/internal/vault"
)

// savedCall records one Save invocation for assertions.
type savedCall struct {
	scope    string
	token    string
	original string
}

// fakeVault is a Vault that records Save calls and can be forced to fail on a
// specific 1-based Save index. It never stores anything, so it is only useful
// for failure-injection and call-observation tests.
type fakeVault struct {
	mu      sync.Mutex
	saved   []savedCall
	failAt  int
	failErr error
}

func (f *fakeVault) Save(ctx context.Context, scope, token, original string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = append(f.saved, savedCall{scope: scope, token: token, original: original})
	if f.failAt > 0 && len(f.saved) == f.failAt {
		return f.failErr
	}
	return nil
}

func (f *fakeVault) Resolve(ctx context.Context, scope, token string) (string, error) {
	return "", vault.ErrNotFound
}

func (f *fakeVault) RevokeScope(ctx context.Context, scope string) error {
	return nil
}

func (f *fakeVault) calls() []savedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]savedCall(nil), f.saved...)
}

// persistEntities builds three personal entities (name, phone, email) with
// offsets resolved from text, in document order.
func persistEntities(t *testing.T, text string) []ownership.Entity {
	t.Helper()
	nameStart := strings.Index(text, "Иванов Иван")
	phoneStart := strings.Index(text, "+7 900 123-45-67")
	emailStart := strings.Index(text, "ivanov@example.com")
	return []ownership.Entity{
		ownEnt(detection.TypeFullName, nameStart, nameStart+len("Иванов Иван"), true),
		ownEnt(detection.TypePhone, phoneStart, phoneStart+len("+7 900 123-45-67"), true),
		ownEnt(detection.TypeEmail, emailStart, emailStart+len("ivanov@example.com"), true),
	}
}

func TestReplaceAndPersistFailureOnFirstSave(t *testing.T) {
	text := "Клиент Иванов Иван, телефон +7 900 123-45-67, email ivanov@example.com"
	results := persistEntities(t, text)
	boom := errors.New("synthetic vault failure")
	fv := &fakeVault{failAt: 1, failErr: boom}

	got, err := ReplaceAndPersist(context.Background(), text, "scope-1", results, &fakeIssuer{tokens: map[string]string{}}, fv)
	if !errors.Is(err, boom) {
		t.Fatalf("ReplaceAndPersist() error = %v, want %v", err, boom)
	}
	if got.Text != "" {
		t.Errorf("partial tokenized text returned on first Save failure: %q", got.Text)
	}
	if len(got.Replacements) != 0 {
		t.Errorf("partial replacements returned on first Save failure: %+v", got.Replacements)
	}
}

func TestReplaceAndPersistFailureOnLaterSave(t *testing.T) {
	text := "Клиент Иванов Иван, телефон +7 900 123-45-67, email ivanov@example.com"
	results := persistEntities(t, text)
	boom := errors.New("synthetic vault failure")
	fv := &fakeVault{failAt: 2, failErr: boom}

	got, err := ReplaceAndPersist(context.Background(), text, "scope-1", results, &fakeIssuer{tokens: map[string]string{}}, fv)
	if !errors.Is(err, boom) {
		t.Fatalf("ReplaceAndPersist() error = %v, want %v", err, boom)
	}
	if got.Text != "" {
		t.Errorf("partial tokenized text returned on later Save failure: %q", got.Text)
	}
	if len(got.Replacements) != 0 {
		t.Errorf("partial replacements returned on later Save failure: %+v", got.Replacements)
	}
}

func TestReplaceAndPersistErrorLeaksNoSyntheticPII(t *testing.T) {
	text := "Клиент Иванов Иван, телефон +7 900 123-45-67, email ivanov@example.com"
	results := persistEntities(t, text)
	boom := errors.New("synthetic vault failure")
	fv := &fakeVault{failAt: 1, failErr: boom}

	_, err := ReplaceAndPersist(context.Background(), text, "scope-1", results, &fakeIssuer{tokens: map[string]string{}}, fv)
	if err == nil {
		t.Fatal("ReplaceAndPersist() error = nil, want failure")
	}
	msg := err.Error()
	for _, marker := range []string{"Иванов Иван", "+7 900 123-45-67", "ivanov@example.com", "scope-1", "<EMAIL_", "<PHONE_", "<FULL_NAME_"} {
		if strings.Contains(msg, marker) {
			t.Errorf("error leaks synthetic PII marker %q: %q", marker, msg)
		}
	}
}

func TestReplaceAndPersistSuccessSavesAllMappings(t *testing.T) {
	text := "Клиент Иванов Иван, телефон +7 900 123-45-67, email ivanov@example.com"
	results := persistEntities(t, text)
	scope := "scope-1"

	mem, err := vault.NewMemory(time.Hour)
	if err != nil {
		t.Fatalf("vault.NewMemory() error = %v", err)
	}

	got, err := ReplaceAndPersist(context.Background(), text, scope, results, &fakeIssuer{tokens: map[string]string{}}, mem)
	if err != nil {
		t.Fatalf("ReplaceAndPersist() error = %v", err)
	}
	if got.Text == "" {
		t.Fatal("ReplaceAndPersist() returned empty text on success")
	}
	if len(got.Replacements) != 3 {
		t.Fatalf("Replacements = %d, want 3", len(got.Replacements))
	}

	// Every replacement token must resolve back to the exact original slice.
	for _, r := range got.Replacements {
		want := text[r.Entity.Start:r.Entity.End]
		gotOriginal, err := mem.Resolve(context.Background(), scope, r.Token)
		if err != nil {
			t.Fatalf("Resolve(%q) error = %v", r.Token, err)
		}
		if gotOriginal != want {
			t.Errorf("Resolve(%q) = %q, want %q", r.Token, gotOriginal, want)
		}
	}
}

func TestReplaceAndPersistNilVault(t *testing.T) {
	text := "Клиент Иванов Иван"
	start := strings.Index(text, "Иванов Иван")
	results := []ownership.Entity{ownEnt(detection.TypeFullName, start, start+len("Иванов Иван"), true)}

	got, err := ReplaceAndPersist(context.Background(), text, "scope-1", results, &fakeIssuer{tokens: map[string]string{}}, nil)
	if !errors.Is(err, ErrNilVault) {
		t.Fatalf("ReplaceAndPersist() error = %v, want ErrNilVault", err)
	}
	if got.Text != "" {
		t.Errorf("Text = %q, want empty on nil vault", got.Text)
	}
	if len(got.Replacements) != 0 {
		t.Errorf("Replacements = %+v, want none on nil vault", got.Replacements)
	}
}

func TestReplaceAndPersistCancelledContext(t *testing.T) {
	text := "Клиент Иванов Иван, телефон +7 900 123-45-67, email ivanov@example.com"
	results := persistEntities(t, text)
	fv := &fakeVault{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := ReplaceAndPersist(ctx, text, "scope-1", results, &fakeIssuer{tokens: map[string]string{}}, fv)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReplaceAndPersist() error = %v, want context.Canceled", err)
	}
	if got.Text != "" {
		t.Errorf("Text = %q, want empty on cancelled context", got.Text)
	}
	if len(got.Replacements) != 0 {
		t.Errorf("Replacements = %+v, want none on cancelled context", got.Replacements)
	}
}

func TestReplaceAndPersistNoPersonalEntitiesSkipsVault(t *testing.T) {
	text := "Клиент Иванов Иван"
	start := strings.Index(text, "Иванов Иван")
	results := []ownership.Entity{ownEnt(detection.TypeFullName, start, start+len("Иванов Иван"), false)}
	fv := &fakeVault{}

	got, err := ReplaceAndPersist(context.Background(), text, "scope-1", results, &fakeIssuer{tokens: map[string]string{}}, fv)
	if err != nil {
		t.Fatalf("ReplaceAndPersist() error = %v", err)
	}
	if got.Text != text {
		t.Errorf("Text = %q, want %q", got.Text, text)
	}
	if len(got.Replacements) != 0 {
		t.Errorf("Replacements = %+v, want none", got.Replacements)
	}
	if calls := fv.calls(); len(calls) != 0 {
		t.Errorf("vault.Save called %d times for no personal entities: %+v", len(calls), calls)
	}
}
