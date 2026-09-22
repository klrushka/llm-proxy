package vault

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// testTTL is a generous TTL used by tests that do not exercise expiry.
const testTTL = time.Hour

// fixedClock returns a clock that always reports the given time.
func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

func TestSaveAndResolve(t *testing.T) {
	m, err := NewMemory(testTTL)
	if err != nil {
		t.Fatalf("NewMemory() error = %v", err)
	}
	ctx := context.Background()

	if err := m.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := m.Resolve(ctx, "scope-1", "tok-1")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got != "synthetic-value-a" {
		t.Errorf("Resolve() = %q, want %q", got, "synthetic-value-a")
	}
}

func TestSaveIdempotentSameOriginal(t *testing.T) {
	m, err := NewMemory(testTTL)
	if err != nil {
		t.Fatalf("NewMemory() error = %v", err)
	}
	ctx := context.Background()

	if err := m.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := m.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Errorf("repeated same original: Save() error = %v, want nil", err)
	}
	got, err := m.Resolve(ctx, "scope-1", "tok-1")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got != "synthetic-value-a" {
		t.Errorf("Resolve() = %q, want %q", got, "synthetic-value-a")
	}
}

func TestSaveConflictDoesNotOverwrite(t *testing.T) {
	m, err := NewMemory(testTTL)
	if err != nil {
		t.Fatalf("NewMemory() error = %v", err)
	}
	ctx := context.Background()

	if err := m.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := m.Save(ctx, "scope-1", "tok-1", "synthetic-value-b"); !errors.Is(err, ErrConflict) {
		t.Errorf("different original: Save() error = %v, want ErrConflict", err)
	}
	got, err := m.Resolve(ctx, "scope-1", "tok-1")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got != "synthetic-value-a" {
		t.Errorf("Resolve() = %q, want original %q preserved", got, "synthetic-value-a")
	}
}

func TestResolveNotFound(t *testing.T) {
	m, err := NewMemory(testTTL)
	if err != nil {
		t.Fatalf("NewMemory() error = %v", err)
	}
	ctx := context.Background()

	if _, err := m.Resolve(ctx, "scope-1", "tok-unknown"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown key: Resolve() error = %v, want ErrNotFound", err)
	}
}

func TestCrossScopeTokenNotDisclosed(t *testing.T) {
	m, err := NewMemory(testTTL)
	if err != nil {
		t.Fatalf("NewMemory() error = %v", err)
	}
	ctx := context.Background()

	if err := m.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := m.Resolve(ctx, "scope-2", "tok-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-scope token: Resolve() error = %v, want ErrNotFound", err)
	}
}

func TestEmptyInputs(t *testing.T) {
	m, err := NewMemory(testTTL)
	if err != nil {
		t.Fatalf("NewMemory() error = %v", err)
	}
	ctx := context.Background()

	tests := []struct {
		name     string
		op       string
		scope    string
		token    string
		original string
		wantErr  error
	}{
		{name: "empty scope save", op: "save", scope: "", token: "tok-1", original: "v", wantErr: ErrEmptyScope},
		{name: "empty token save", op: "save", scope: "scope-1", token: "", original: "v", wantErr: ErrEmptyToken},
		{name: "empty original save", op: "save", scope: "scope-1", token: "tok-1", original: "", wantErr: ErrEmptyOriginal},
		{name: "empty scope resolve", op: "resolve", scope: "", token: "tok-1", wantErr: ErrEmptyScope},
		{name: "empty token resolve", op: "resolve", scope: "scope-1", token: "", wantErr: ErrEmptyToken},
		{name: "empty scope revoke", op: "revoke", scope: "", wantErr: ErrEmptyScope},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			switch tt.op {
			case "save":
				if err := m.Save(ctx, tt.scope, tt.token, tt.original); !errors.Is(err, tt.wantErr) {
					t.Errorf("Save() error = %v, want %v", err, tt.wantErr)
				}
			case "resolve":
				if _, err := m.Resolve(ctx, tt.scope, tt.token); !errors.Is(err, tt.wantErr) {
					t.Errorf("Resolve() error = %v, want %v", err, tt.wantErr)
				}
			case "revoke":
				if err := m.RevokeScope(ctx, tt.scope); !errors.Is(err, tt.wantErr) {
					t.Errorf("RevokeScope() error = %v, want %v", err, tt.wantErr)
				}
			}
		})
	}
}

func TestCancelledContext(t *testing.T) {
	m, err := NewMemory(testTTL)
	if err != nil {
		t.Fatalf("NewMemory() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := m.Save(ctx, "scope-1", "tok-1", "v"); !errors.Is(err, context.Canceled) {
		t.Errorf("Save() error = %v, want context.Canceled", err)
	}
	if _, err := m.Resolve(ctx, "scope-1", "tok-1"); !errors.Is(err, context.Canceled) {
		t.Errorf("Resolve() error = %v, want context.Canceled", err)
	}
	if err := m.RevokeScope(ctx, "scope-1"); !errors.Is(err, context.Canceled) {
		t.Errorf("RevokeScope() error = %v, want context.Canceled", err)
	}
}

func TestConcurrentSafety(t *testing.T) {
	m, err := NewMemory(testTTL)
	if err != nil {
		t.Fatalf("NewMemory() error = %v", err)
	}
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
				if err := m.Save(ctx, scope, token, original); err != nil {
					errs <- err
					return
				}
				got, err := m.Resolve(ctx, scope, token)
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

func TestConcurrentConflictDoesNotOverwrite(t *testing.T) {
	m, err := NewMemory(testTTL)
	if err != nil {
		t.Fatalf("NewMemory() error = %v", err)
	}
	ctx := context.Background()

	if err := m.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	const workers = 64
	var wg sync.WaitGroup
	errs := make(chan error, workers)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Save(ctx, "scope-1", "tok-1", "synthetic-value-b"); err != nil && !errors.Is(err, ErrConflict) {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	got, err := m.Resolve(ctx, "scope-1", "tok-1")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got != "synthetic-value-a" {
		t.Errorf("Resolve() = %q, want original %q preserved", got, "synthetic-value-a")
	}
}

func TestNewMemoryRejectsInvalidTTL(t *testing.T) {
	for _, ttl := range []time.Duration{0, -1, -time.Hour} {
		if m, err := NewMemory(ttl); err == nil || m != nil {
			t.Errorf("NewMemory(%v) = (%v, %v), want (nil, ErrInvalidTTL)", ttl, m, err)
		} else if !errors.Is(err, ErrInvalidTTL) {
			t.Errorf("NewMemory(%v) error = %v, want ErrInvalidTTL", ttl, err)
		}
	}
}

func TestExpiryNotDisclosed(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	now := start
	m, err := newMemoryWithClock(time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newMemoryWithClock() error = %v", err)
	}
	ctx := context.Background()

	if err := m.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	now = start.Add(time.Minute - time.Nanosecond)
	if got, err := m.Resolve(ctx, "scope-1", "tok-1"); err != nil || got != "synthetic-value-a" {
		t.Errorf("before expiry: Resolve() = (%q, %v), want (%q, nil)", got, err, "synthetic-value-a")
	}

	now = start.Add(time.Minute)
	if _, err := m.Resolve(ctx, "scope-1", "tok-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("at expiry: Resolve() error = %v, want ErrNotFound", err)
	}
}

func TestExpiryLazilyDeletes(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	now := start
	m, err := newMemoryWithClock(time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newMemoryWithClock() error = %v", err)
	}
	ctx := context.Background()

	if err := m.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	now = start.Add(2 * time.Minute)
	if _, err := m.Resolve(ctx, "scope-1", "tok-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve() error = %v, want ErrNotFound", err)
	}

	if _, ok := m.mapping[key{scope: "scope-1", token: "tok-1"}]; ok {
		t.Error("expired mapping was not lazily deleted")
	}
}

func TestSaveDoesNotRefreshTTL(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	now := start
	m, err := newMemoryWithClock(time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newMemoryWithClock() error = %v", err)
	}
	ctx := context.Background()

	if err := m.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	now = start.Add(30 * time.Second)
	if err := m.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("idempotent Save() error = %v", err)
	}

	now = start.Add(time.Minute)
	if _, err := m.Resolve(ctx, "scope-1", "tok-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after original expiry: Resolve() error = %v, want ErrNotFound (TTL must not refresh)", err)
	}
}

func TestSaveReplacesExpiredMapping(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	now := start
	m, err := newMemoryWithClock(time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newMemoryWithClock() error = %v", err)
	}
	ctx := context.Background()

	if err := m.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	now = start.Add(2 * time.Minute)
	if err := m.Save(ctx, "scope-1", "tok-1", "synthetic-value-b"); err != nil {
		t.Fatalf("Save() after expiry error = %v", err)
	}

	got, err := m.Resolve(ctx, "scope-1", "tok-1")
	if err != nil {
		t.Fatalf("Resolve() after resave error = %v", err)
	}
	if got != "synthetic-value-b" {
		t.Errorf("Resolve() = %q, want %q", got, "synthetic-value-b")
	}

	now = start.Add(3 * time.Minute)
	if _, err := m.Resolve(ctx, "scope-1", "tok-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("at new expiry: Resolve() error = %v, want ErrNotFound", err)
	}
}

func TestRevokeScope(t *testing.T) {
	m, err := NewMemory(testTTL)
	if err != nil {
		t.Fatalf("NewMemory() error = %v", err)
	}
	ctx := context.Background()

	if err := m.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := m.Save(ctx, "scope-1", "tok-2", "synthetic-value-b"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := m.Save(ctx, "scope-2", "tok-1", "synthetic-value-c"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if err := m.RevokeScope(ctx, "scope-1"); err != nil {
		t.Fatalf("RevokeScope() error = %v", err)
	}

	if _, err := m.Resolve(ctx, "scope-1", "tok-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked scope-1 tok-1: Resolve() error = %v, want ErrNotFound", err)
	}
	if _, err := m.Resolve(ctx, "scope-1", "tok-2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked scope-1 tok-2: Resolve() error = %v, want ErrNotFound", err)
	}
	if got, err := m.Resolve(ctx, "scope-2", "tok-1"); err != nil || got != "synthetic-value-c" {
		t.Errorf("other scope: Resolve() = (%q, %v), want (%q, nil)", got, err, "synthetic-value-c")
	}
}

func TestRevokeScopeIdempotentUnknown(t *testing.T) {
	m, err := NewMemory(testTTL)
	if err != nil {
		t.Fatalf("NewMemory() error = %v", err)
	}
	ctx := context.Background()

	if err := m.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if err := m.RevokeScope(ctx, "scope-unknown"); err != nil {
		t.Fatalf("RevokeScope(unknown) error = %v", err)
	}
	if err := m.RevokeScope(ctx, "scope-unknown"); err != nil {
		t.Fatalf("RevokeScope(unknown) second time error = %v", err)
	}

	if got, err := m.Resolve(ctx, "scope-1", "tok-1"); err != nil || got != "synthetic-value-a" {
		t.Errorf("unrelated scope: Resolve() = (%q, %v), want (%q, nil)", got, err, "synthetic-value-a")
	}
}

func TestResaveAfterRevoke(t *testing.T) {
	m, err := NewMemory(testTTL)
	if err != nil {
		t.Fatalf("NewMemory() error = %v", err)
	}
	ctx := context.Background()

	if err := m.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := m.RevokeScope(ctx, "scope-1"); err != nil {
		t.Fatalf("RevokeScope() error = %v", err)
	}

	if err := m.Save(ctx, "scope-1", "tok-1", "synthetic-value-b"); err != nil {
		t.Fatalf("Save() after revoke error = %v", err)
	}
	got, err := m.Resolve(ctx, "scope-1", "tok-1")
	if err != nil {
		t.Fatalf("Resolve() after resave error = %v", err)
	}
	if got != "synthetic-value-b" {
		t.Errorf("Resolve() = %q, want %q", got, "synthetic-value-b")
	}
}

func TestConcurrentRevokeRace(t *testing.T) {
	m, err := NewMemory(testTTL)
	if err != nil {
		t.Fatalf("NewMemory() error = %v", err)
	}
	ctx := context.Background()

	const scopes = 8
	const tokensPerScope = 50
	for s := 0; s < scopes; s++ {
		scope := "scope-" + string(rune('a'+s))
		for i := 0; i < tokensPerScope; i++ {
			token := "tok-" + string(rune('a'+s)) + "-" + string(rune('0'+i%10))
			if err := m.Save(ctx, scope, token, "synthetic-"+token); err != nil {
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
			if err := m.RevokeScope(ctx, scope); err != nil {
				errs <- err
			}
		}(scope)
		wg.Add(1)
		go func(scope string) {
			defer wg.Done()
			for i := 0; i < tokensPerScope; i++ {
				token := "tok-" + scope[len("scope-"):] + "-" + string(rune('0'+i%10))
				if _, err := m.Resolve(ctx, scope, token); err != nil && !errors.Is(err, ErrNotFound) {
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
