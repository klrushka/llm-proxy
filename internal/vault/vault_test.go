package vault

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestSaveAndResolve(t *testing.T) {
	m := NewMemory()
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
	m := NewMemory()
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
	m := NewMemory()
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
	m := NewMemory()
	ctx := context.Background()

	if _, err := m.Resolve(ctx, "scope-1", "tok-unknown"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown key: Resolve() error = %v, want ErrNotFound", err)
	}
}

func TestCrossScopeTokenNotDisclosed(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()

	if err := m.Save(ctx, "scope-1", "tok-1", "synthetic-value-a"); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := m.Resolve(ctx, "scope-2", "tok-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-scope token: Resolve() error = %v, want ErrNotFound", err)
	}
}

func TestEmptyInputs(t *testing.T) {
	m := NewMemory()
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.op == "save" {
				if err := m.Save(ctx, tt.scope, tt.token, tt.original); !errors.Is(err, tt.wantErr) {
					t.Errorf("Save() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if _, err := m.Resolve(ctx, tt.scope, tt.token); !errors.Is(err, tt.wantErr) {
				t.Errorf("Resolve() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestCancelledContext(t *testing.T) {
	m := NewMemory()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := m.Save(ctx, "scope-1", "tok-1", "v"); !errors.Is(err, context.Canceled) {
		t.Errorf("Save() error = %v, want context.Canceled", err)
	}
	if _, err := m.Resolve(ctx, "scope-1", "tok-1"); !errors.Is(err, context.Canceled) {
		t.Errorf("Resolve() error = %v, want context.Canceled", err)
	}
}

func TestConcurrentSafety(t *testing.T) {
	m := NewMemory()
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
	m := NewMemory()
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
