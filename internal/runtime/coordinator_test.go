package runtime

import (
	"context"
	"errors"
	"strings"
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

// TestRunTokenIntegrityPreserved proves that when the LLM preserves the opaque
// token sequence while changing surrounding text, the restorer is invoked and
// the restored result is returned.
func TestRunTokenIntegrityPreserved(t *testing.T) {
	const token = "<EMAIL_00000000000000000000000000000000>"
	restoreCalled := false
	c := New(
		func(_ context.Context, _, _ string) (string, error) { return token, nil },
		func(_ context.Context, protected string) (string, error) {
			return "переписал " + protected + " вокруг", nil
		},
		func(_ context.Context, _, protected string) (string, error) {
			restoreCalled = true
			if protected != "переписал "+token+" вокруг" {
				t.Errorf("restorer received %q, want preserved token text", protected)
			}
			return "restored", nil
		},
	)

	result, err := c.Run(context.Background(), "scope", "synthetic text")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result != "restored" {
		t.Errorf("result = %q, want restored", result)
	}
	if !restoreCalled {
		t.Error("restorer not invoked on preserved token sequence")
	}
}

// TestRunTokenIntegrityMismatchFailsClosed proves that when the LLM alters the
// opaque token sequence (removed, duplicated, reordered, replaced or injected),
// Run returns an empty result and the safe ErrLLMFailed sentinel, and the
// restorer is never invoked.
func TestRunTokenIntegrityMismatchFailsClosed(t *testing.T) {
	const token = "<EMAIL_00000000000000000000000000000000>"
	const other = "<PHONE_11111111111111111111111111111111>"

	cases := []struct {
		name string
		llm  func(_ context.Context, _ string) (string, error)
	}{
		{"removed", func(_ context.Context, _ string) (string, error) { return "no token", nil }},
		{"duplicated", func(_ context.Context, _ string) (string, error) { return token + " " + token, nil }},
		{"reordered", func(_ context.Context, _ string) (string, error) { return other + " " + token, nil }},
		{"replaced suffix", func(_ context.Context, _ string) (string, error) {
			return "<EMAIL_ffffffffffffffffffffffffffffffff>", nil
		}},
		{"replaced type", func(_ context.Context, _ string) (string, error) {
			return "<PHONE_00000000000000000000000000000000>", nil
		}},
		{"injected token-shaped value", func(_ context.Context, _ string) (string, error) {
			return token + " " + other, nil
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restoreCalled := false
			c := New(
				func(_ context.Context, _, _ string) (string, error) { return token, nil },
				tc.llm,
				func(_ context.Context, _, _ string) (string, error) {
					restoreCalled = true
					return "", nil
				},
			)

			result, err := c.Run(context.Background(), "scope", "synthetic text")
			if result != "" {
				t.Errorf("result = %q, want empty", result)
			}
			if !errors.Is(err, ErrLLMFailed) {
				t.Errorf("err = %v, want ErrLLMFailed", err)
			}
			if restoreCalled {
				t.Error("restorer invoked after token integrity mismatch")
			}
		})
	}
}

// TestRunTokenIntegrityErrorIsSafe proves that a token integrity mismatch
// returns the fixed safe ErrLLMFailed text that never embeds the input,
// output or any token.
func TestRunTokenIntegrityErrorIsSafe(t *testing.T) {
	const token = "<EMAIL_00000000000000000000000000000000>"
	c := New(
		func(_ context.Context, _, _ string) (string, error) { return token, nil },
		func(_ context.Context, _ string) (string, error) { return "dropped", nil },
		func(_ context.Context, _, _ string) (string, error) { return "", nil },
	)

	_, err := c.Run(context.Background(), "scope", "synthetic text")
	if err == nil {
		t.Fatal("Run() error = nil, want ErrLLMFailed")
	}
	if err.Error() != ErrLLMFailed.Error() {
		t.Errorf("Error() = %q, want fixed safe text %q", err.Error(), ErrLLMFailed.Error())
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("Error() leaked token %q: %q", token, err.Error())
	}
}
