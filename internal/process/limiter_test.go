package process

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// compile-time check that Limiter.Handle is signature-compatible with the
// process operation/API function shape.
var _ func(context.Context, Request) (Response, error) = (&Limiter{}).Handle

func TestNewLimiterInvalidLimitFailsSafely(t *testing.T) {
	fn := HandlerFunc(func(_ context.Context, _ Request) (Response, error) {
		return Response{}, nil
	})
	for _, limit := range []int{0, -1} {
		l, err := NewLimiter(limit, fn)
		if err == nil {
			t.Errorf("NewLimiter(%d) error = nil, want error", limit)
		}
		if l != nil {
			t.Errorf("NewLimiter(%d) returned non-nil limiter %v", limit, l)
		}
	}
}

func TestNewLimiterNilWrappedFailsSafely(t *testing.T) {
	l, err := NewLimiter(1, nil)
	if err == nil {
		t.Fatal("NewLimiter(1, nil) error = nil, want error")
	}
	if l != nil {
		t.Errorf("NewLimiter(1, nil) returned non-nil limiter %v", l)
	}
}

func TestLimiterRejectsWhenFull(t *testing.T) {
	const limit = 2
	var calls atomic.Int64
	entered := make(chan struct{}, limit)
	release := make(chan struct{})
	l, err := NewLimiter(limit, HandlerFunc(func(_ context.Context, req Request) (Response, error) {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		return Response{Result: "masked:" + req.Payload}, nil
	}))
	if err != nil {
		t.Fatalf("NewLimiter() error = %v", err)
	}

	var wg sync.WaitGroup
	results := make([]string, limit)
	errs := make([]error, limit)
	for i := 0; i < limit; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := l.Handle(context.Background(), Request{Payload: "synthetic", PayloadID: "id"})
			results[i] = resp.Result
			errs[i] = err
		}(i)
	}
	// Wait until both accepted calls hold a permit and are blocked inside the
	// wrapped function, so capacity is provably full.
	for i := 0; i < limit; i++ {
		<-entered
	}

	resp, err := l.Handle(context.Background(), Request{Payload: "synthetic", PayloadID: "id"})
	if !errors.Is(err, ErrOverloaded) {
		t.Errorf("Handle() error = %v, want ErrOverloaded", err)
	}
	if resp.Result != "" {
		t.Errorf("Handle() result = %q, want empty", resp.Result)
	}
	if got := calls.Load(); got != limit {
		t.Errorf("wrapped calls = %d, want %d (rejected call must not invoke)", got, limit)
	}

	close(release)
	wg.Wait()
	for i := 0; i < limit; i++ {
		if errs[i] != nil {
			t.Errorf("accepted call %d error = %v", i, errs[i])
		}
		if results[i] != "masked:synthetic" {
			t.Errorf("accepted call %d result = %q, want %q", i, results[i], "masked:synthetic")
		}
	}
}

func TestLimiterCapacityReusableAfterSuccess(t *testing.T) {
	var calls atomic.Int64
	l, err := NewLimiter(1, HandlerFunc(func(_ context.Context, req Request) (Response, error) {
		calls.Add(1)
		return Response{Result: "masked:" + req.Payload}, nil
	}))
	if err != nil {
		t.Fatalf("NewLimiter() error = %v", err)
	}

	for i := 0; i < 2; i++ {
		resp, err := l.Handle(context.Background(), Request{Payload: "synthetic", PayloadID: "id"})
		if err != nil {
			t.Fatalf("Handle() %d error = %v", i, err)
		}
		if resp.Result != "masked:synthetic" {
			t.Errorf("Handle() %d result = %q, want %q", i, resp.Result, "masked:synthetic")
		}
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("wrapped calls = %d, want 2", got)
	}
}

func TestLimiterCapacityReusableAfterError(t *testing.T) {
	var calls atomic.Int64
	l, err := NewLimiter(1, HandlerFunc(func(_ context.Context, _ Request) (Response, error) {
		calls.Add(1)
		return Response{}, ErrMaskingFailed
	}))
	if err != nil {
		t.Fatalf("NewLimiter() error = %v", err)
	}

	resp, err := l.Handle(context.Background(), Request{Payload: "synthetic", PayloadID: "id"})
	if !errors.Is(err, ErrMaskingFailed) {
		t.Errorf("Handle() error = %v, want ErrMaskingFailed (propagated unchanged)", err)
	}
	if resp.Result != "" {
		t.Errorf("Handle() result = %q, want empty", resp.Result)
	}

	// Capacity must be reusable after the wrapped error.
	resp, err = l.Handle(context.Background(), Request{Payload: "synthetic", PayloadID: "id"})
	if !errors.Is(err, ErrMaskingFailed) {
		t.Errorf("second Handle() error = %v, want ErrMaskingFailed", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("wrapped calls = %d, want 2", got)
	}
}

func TestLimiterReleasesPermitOnPanic(t *testing.T) {
	var calls atomic.Int64
	l, err := NewLimiter(1, HandlerFunc(func(_ context.Context, req Request) (Response, error) {
		if calls.Add(1) == 1 {
			panic("boom")
		}
		return Response{Result: "masked:" + req.Payload}, nil
	}))
	if err != nil {
		t.Fatalf("NewLimiter() error = %v", err)
	}

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("Handle() did not propagate panic")
			}
		}()
		_, _ = l.Handle(context.Background(), Request{Payload: "synthetic", PayloadID: "id"})
	}()

	// The propagated panic must have released the permit, so a later call
	// succeeds instead of being rejected as overloaded.
	resp, err := l.Handle(context.Background(), Request{Payload: "synthetic", PayloadID: "id"})
	if err != nil {
		t.Errorf("Handle() after panic error = %v, want nil (permit released)", err)
	}
	if resp.Result != "masked:synthetic" {
		t.Errorf("Handle() result = %q, want %q", resp.Result, "masked:synthetic")
	}
}
