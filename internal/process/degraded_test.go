package process

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/klrushka/llm-proxy/internal/policy"
)

// compile-time check that WithRulesOnlyFallback returns a MaskFunc.
var _ MaskFunc = WithRulesOnlyFallback(false, nil, nil)

func TestWithRulesOnlyFallbackPrimarySuccessBypassesFallback(t *testing.T) {
	var primaryCalls, rulesCalls atomic.Int64
	mask := WithRulesOnlyFallback(true,
		func(_ context.Context, p string) (string, error) {
			primaryCalls.Add(1)
			return "masked:" + p, nil
		},
		func(_ context.Context, p string) (string, error) {
			rulesCalls.Add(1)
			return "rules:" + p, nil
		})

	result, err := mask(context.Background(), "synthetic text")
	if err != nil {
		t.Fatalf("Mask() error = %v", err)
	}
	if result != "masked:synthetic text" {
		t.Errorf("result = %q, want %q", result, "masked:synthetic text")
	}
	if primaryCalls.Load() != 1 {
		t.Errorf("primary calls = %d, want 1", primaryCalls.Load())
	}
	if rulesCalls.Load() != 0 {
		t.Errorf("rules-only calls = %d, want 0 (bypass on success)", rulesCalls.Load())
	}
}

func TestWithRulesOnlyFallbackAllowedFallsBackOnceOnModelUnavailable(t *testing.T) {
	var primaryCalls, rulesCalls atomic.Int64
	mask := WithRulesOnlyFallback(true,
		func(_ context.Context, p string) (string, error) {
			primaryCalls.Add(1)
			return "", fmt.Errorf("worker down: %w", ErrModelUnavailable)
		},
		func(_ context.Context, p string) (string, error) {
			rulesCalls.Add(1)
			return "rules:" + p, nil
		})

	result, err := mask(context.Background(), "synthetic text")
	if err != nil {
		t.Fatalf("Mask() error = %v", err)
	}
	if result != "rules:synthetic text" {
		t.Errorf("result = %q, want %q", result, "rules:synthetic text")
	}
	if primaryCalls.Load() != 1 {
		t.Errorf("primary calls = %d, want 1", primaryCalls.Load())
	}
	if rulesCalls.Load() != 1 {
		t.Errorf("rules-only calls = %d, want exactly 1", rulesCalls.Load())
	}
}

func TestWithRulesOnlyFallbackDisallowedDeniesFallback(t *testing.T) {
	var rulesCalls atomic.Int64
	mask := WithRulesOnlyFallback(false,
		func(_ context.Context, _ string) (string, error) {
			return "", fmt.Errorf("worker down: %w", ErrModelUnavailable)
		},
		func(_ context.Context, _ string) (string, error) {
			rulesCalls.Add(1)
			return "rules", nil
		})

	result, err := mask(context.Background(), "synthetic text")
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("Mask() error = %v, want ErrModelUnavailable", err)
	}
	if err != ErrModelUnavailable {
		t.Errorf("Mask() error = %v, want exact bare sentinel", err)
	}
	if result != "" {
		t.Errorf("result = %q, want empty (fail closed)", result)
	}
	if rulesCalls.Load() != 0 {
		t.Errorf("rules-only calls = %d, want 0 (denied)", rulesCalls.Load())
	}
}

func TestWithRulesOnlyFallbackGenericErrorDoesNotFallBack(t *testing.T) {
	var rulesCalls atomic.Int64
	mask := WithRulesOnlyFallback(true,
		func(_ context.Context, p string) (string, error) {
			return "", errors.New("generic failure: " + p)
		},
		func(_ context.Context, _ string) (string, error) {
			rulesCalls.Add(1)
			return "rules", nil
		})

	result, err := mask(context.Background(), "synthetic text")
	if !errors.Is(err, ErrMaskingFailed) {
		t.Fatalf("Mask() error = %v, want ErrMaskingFailed", err)
	}
	if err != ErrMaskingFailed {
		t.Errorf("Mask() error = %v, want exact bare sentinel", err)
	}
	if result != "" {
		t.Errorf("result = %q, want empty (fail closed)", result)
	}
	if rulesCalls.Load() != 0 {
		t.Errorf("rules-only calls = %d, want 0 (generic never falls back)", rulesCalls.Load())
	}
	if strings.Contains(err.Error(), "synthetic text") {
		t.Errorf("error leaks payload: %q", err.Error())
	}
}

func TestWithRulesOnlyFallbackVaultErrorPreservedNoFallback(t *testing.T) {
	var rulesCalls atomic.Int64
	mask := WithRulesOnlyFallback(true,
		func(_ context.Context, _ string) (string, error) {
			return "", fmt.Errorf("vault down: %w", ErrVaultUnavailable)
		},
		func(_ context.Context, _ string) (string, error) {
			rulesCalls.Add(1)
			return "rules", nil
		})

	result, err := mask(context.Background(), "synthetic text")
	if !errors.Is(err, ErrVaultUnavailable) {
		t.Fatalf("Mask() error = %v, want ErrVaultUnavailable", err)
	}
	if err != ErrVaultUnavailable {
		t.Errorf("Mask() error = %v, want exact bare sentinel", err)
	}
	if result != "" {
		t.Errorf("result = %q, want empty (fail closed)", result)
	}
	if rulesCalls.Load() != 0 {
		t.Errorf("rules-only calls = %d, want 0 (vault never falls back)", rulesCalls.Load())
	}
}

func TestWithRulesOnlyFallbackNilPrimaryFailsClosed(t *testing.T) {
	var rulesCalls atomic.Int64
	mask := WithRulesOnlyFallback(true, nil,
		func(_ context.Context, _ string) (string, error) {
			rulesCalls.Add(1)
			return "rules", nil
		})

	result, err := mask(context.Background(), "synthetic text")
	if !errors.Is(err, ErrMaskingFailed) {
		t.Fatalf("Mask() error = %v, want ErrMaskingFailed", err)
	}
	if err != ErrMaskingFailed {
		t.Errorf("Mask() error = %v, want exact bare sentinel", err)
	}
	if result != "" {
		t.Errorf("result = %q, want empty (fail closed)", result)
	}
	if rulesCalls.Load() != 0 {
		t.Errorf("rules-only calls = %d, want 0 (nil primary never falls back)", rulesCalls.Load())
	}
}

func TestWithRulesOnlyFallbackNilRulesOnlyFailsClosed(t *testing.T) {
	mask := WithRulesOnlyFallback(true,
		func(_ context.Context, _ string) (string, error) {
			return "", fmt.Errorf("worker down: %w", ErrModelUnavailable)
		},
		nil)

	result, err := mask(context.Background(), "synthetic text")
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("Mask() error = %v, want ErrModelUnavailable", err)
	}
	if err != ErrModelUnavailable {
		t.Errorf("Mask() error = %v, want exact bare sentinel", err)
	}
	if result != "" {
		t.Errorf("result = %q, want empty (fail closed)", result)
	}
}

func TestWithRulesOnlyFallbackRulesOnlySuccessBecomesResult(t *testing.T) {
	mask := WithRulesOnlyFallback(true,
		func(_ context.Context, _ string) (string, error) {
			return "", ErrModelUnavailable
		},
		func(_ context.Context, p string) (string, error) {
			return "rules:" + p, nil
		})

	result, err := mask(context.Background(), "synthetic text")
	if err != nil {
		t.Fatalf("Mask() error = %v", err)
	}
	if result != "rules:synthetic text" {
		t.Errorf("result = %q, want %q", result, "rules:synthetic text")
	}
}

func TestWithRulesOnlyFallbackRulesOnlyFailureSanitized(t *testing.T) {
	cases := []struct {
		name     string
		rulesErr error
		want     error
	}{
		{"generic", errors.New("rules dependency detail"), ErrMaskingFailed},
		{"vault", fmt.Errorf("vault down: %w", ErrVaultUnavailable), ErrVaultUnavailable},
		{"model", fmt.Errorf("worker down: %w", ErrModelUnavailable), ErrModelUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mask := WithRulesOnlyFallback(true,
				func(_ context.Context, _ string) (string, error) {
					return "", ErrModelUnavailable
				},
				func(_ context.Context, _ string) (string, error) {
					// Return a non-empty synthetic result together with the
					// error to prove the result is discarded on failure.
					return "leaked-rules-result", tc.rulesErr
				})

			result, err := mask(context.Background(), "synthetic text")
			if !errors.Is(err, tc.want) {
				t.Fatalf("Mask() error = %v, want %v", err, tc.want)
			}
			if err != tc.want {
				t.Errorf("Mask() error = %v, want exact bare sentinel", err)
			}
			if result != "" {
				t.Errorf("result = %q, want empty (fail closed, result discarded)", result)
			}
			if strings.Contains(err.Error(), "dependency detail") {
				t.Errorf("error leaks dependency detail: %q", err.Error())
			}
		})
	}
}

func TestWithRulesOnlyFallbackPolicyCapabilityDemonstration(t *testing.T) {
	// Demonstrates that the zero-value processing policy capability is
	// false (denies fallback) and that an explicitly enabled capability allows
	// it. policy is used only in this test; production process code does not
	// import the policy package.
	denied := policy.NewPolicy(nil)
	if denied.AllowRulesOnlyDegraded {
		t.Fatal("default AllowRulesOnlyDegraded = true, want false")
	}

	allowed := policy.NewPolicy(nil)
	allowed.AllowRulesOnlyDegraded = true

	var rulesCalls atomic.Int64
	primary := func(_ context.Context, _ string) (string, error) {
		return "", ErrModelUnavailable
	}
	rules := func(_ context.Context, p string) (string, error) {
		rulesCalls.Add(1)
		return "rules:" + p, nil
	}

	deniedMask := WithRulesOnlyFallback(denied.AllowRulesOnlyDegraded, primary, rules)
	if _, err := deniedMask(context.Background(), "synthetic text"); !errors.Is(err, ErrModelUnavailable) {
		t.Errorf("denied Mask() error = %v, want ErrModelUnavailable", err)
	}
	if rulesCalls.Load() != 0 {
		t.Errorf("rules-only calls under denied policy = %d, want 0", rulesCalls.Load())
	}

	allowedMask := WithRulesOnlyFallback(allowed.AllowRulesOnlyDegraded, primary, rules)
	result, err := allowedMask(context.Background(), "synthetic text")
	if err != nil {
		t.Fatalf("allowed Mask() error = %v", err)
	}
	if result != "rules:synthetic text" {
		t.Errorf("allowed result = %q, want %q", result, "rules:synthetic text")
	}
	if rulesCalls.Load() != 1 {
		t.Errorf("rules-only calls under allowed policy = %d, want 1", rulesCalls.Load())
	}
}
