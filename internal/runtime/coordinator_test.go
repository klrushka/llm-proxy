package runtime

import (
	"context"
	"errors"
	"testing"
)

// failingProtector simulates a protection/tokenization failure (including
// tokenization validation and vault persistence).
func failingProtector(_ context.Context, _, _ string) (string, error) {
	return "", errors.New("sensitive tokenization detail")
}

// failingLLM simulates an LLM boundary failure.
func failingLLM(_ context.Context, _ string) (string, error) {
	return "", errors.New("sensitive llm detail")
}

// failingRestorer simulates a restoration/detokenization failure.
func failingRestorer(_ context.Context, _, _ string) (string, error) {
	return "", errors.New("sensitive detokenization detail")
}

// okProtector returns a fixed protected text.
func okProtector(_ context.Context, _, _ string) (string, error) {
	return "<EMAIL_00000000000000000000000000000000>", nil
}

// okLLM returns a fixed protected output.
func okLLM(_ context.Context, _ string) (string, error) {
	return "modified <EMAIL_00000000000000000000000000000000>", nil
}

// okRestorer returns a fixed restored text.
func okRestorer(_ context.Context, _, _ string) (string, error) {
	return "restored", nil
}

// TestRunFailsClosedOnProtectError proves that a protection/tokenization
// failure returns an empty result and the safe ErrProtectFailed sentinel, and
// that the LLM and restorer are never invoked.
func TestRunFailsClosedOnProtectError(t *testing.T) {
	llmCalled := false
	restoreCalled := false
	c := New(failingProtector, func(_ context.Context, _ string) (string, error) {
		llmCalled = true
		return "", nil
	}, func(_ context.Context, _, _ string) (string, error) {
		restoreCalled = true
		return "", nil
	})

	result, err := c.Run(context.Background(), "scope", "synthetic text")
	if result != "" {
		t.Errorf("result = %q, want empty", result)
	}
	if !errors.Is(err, ErrProtectFailed) {
		t.Errorf("err = %v, want ErrProtectFailed", err)
	}
	if llmCalled || restoreCalled {
		t.Error("LLM or restorer invoked after protection failure")
	}
}

// TestRunFailsClosedOnLLMError proves that an LLM failure returns an empty
// result and the safe ErrLLMFailed sentinel, and that the restorer is never
// invoked.
func TestRunFailsClosedOnLLMError(t *testing.T) {
	restoreCalled := false
	c := New(okProtector, failingLLM, func(_ context.Context, _, _ string) (string, error) {
		restoreCalled = true
		return "", nil
	})

	result, err := c.Run(context.Background(), "scope", "synthetic text")
	if result != "" {
		t.Errorf("result = %q, want empty", result)
	}
	if !errors.Is(err, ErrLLMFailed) {
		t.Errorf("err = %v, want ErrLLMFailed", err)
	}
	if restoreCalled {
		t.Error("restorer invoked after LLM failure")
	}
}

// TestRunFailsClosedOnRestoreError proves that a restoration/detokenization
// failure returns an empty result and the safe ErrRestoreFailed sentinel.
func TestRunFailsClosedOnRestoreError(t *testing.T) {
	c := New(okProtector, okLLM, failingRestorer)

	result, err := c.Run(context.Background(), "scope", "synthetic text")
	if result != "" {
		t.Errorf("result = %q, want empty", result)
	}
	if !errors.Is(err, ErrRestoreFailed) {
		t.Errorf("err = %v, want ErrRestoreFailed", err)
	}
}

// TestRunNilBoundaryFailsClosed proves that a nil boundary fails closed with
// the corresponding safe sentinel and an empty result.
func TestRunNilBoundaryFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		c    *Coordinator
		want error
	}{
		{"nil protect", New(nil, okLLM, okRestorer), ErrProtectFailed},
		{"nil llm", New(okProtector, nil, okRestorer), ErrLLMFailed},
		{"nil restore", New(okProtector, okLLM, nil), ErrRestoreFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tc.c.Run(context.Background(), "scope", "synthetic text")
			if result != "" {
				t.Errorf("result = %q, want empty", result)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestRunHappyPathOrder proves the boundaries are called strictly in order:
// protect, then LLM with the protected text, then restore with the LLM output.
func TestRunHappyPathOrder(t *testing.T) {
	var order []string
	c := New(
		func(_ context.Context, _, _ string) (string, error) {
			order = append(order, "protect")
			return "protected", nil
		},
		func(_ context.Context, protected string) (string, error) {
			order = append(order, "llm:"+protected)
			return "llm-out", nil
		},
		func(_ context.Context, _, protected string) (string, error) {
			order = append(order, "restore:"+protected)
			return "restored", nil
		},
	)

	result, err := c.Run(context.Background(), "scope", "user text")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result != "restored" {
		t.Errorf("result = %q, want restored", result)
	}
	want := []string{"protect", "llm:protected", "restore:llm-out"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("order[%d] = %q, want %q", i, order[i], want[i])
		}
	}
}
