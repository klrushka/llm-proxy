package process

import (
	"context"
	"errors"
)

// ErrOverloaded is returned when the bounded concurrency limit is reached. It
// is a safe sentinel that never carries a request, result, or any PII.
var ErrOverloaded = errors.New("process: overloaded")

// HandlerFunc performs the /process operation. It is signature-compatible
// with api.ProcessFunc and process.Operation.Handle.
type HandlerFunc func(ctx context.Context, req Request) (Response, error)

// Limiter bounds the concurrency of a wrapped HandlerFunc. Accepted calls
// hold a permit for the full wrapped call and always release it, including
// error and panic paths. When capacity is full, a call is rejected immediately
// with ErrOverloaded; there is no waiter queue.
type Limiter struct {
	permits chan struct{}
	next    HandlerFunc
}

// NewLimiter returns a Limiter that allows at most limit concurrent calls to
// next. limit must be positive and next must be non-nil; otherwise it returns
// an error and a nil Limiter.
func NewLimiter(limit int, next HandlerFunc) (*Limiter, error) {
	if limit <= 0 {
		return nil, errors.New("process: concurrency limit must be positive")
	}
	if next == nil {
		return nil, errors.New("process: wrapped operation must not be nil")
	}
	return &Limiter{
		permits: make(chan struct{}, limit),
		next:    next,
	}, nil
}

// Handle is signature-compatible with api.ProcessFunc. It acquires a permit
// or returns ErrOverloaded immediately when capacity is full. The permit is
// released on success, error, and propagated panic.
func (l *Limiter) Handle(ctx context.Context, req Request) (resp Response, err error) {
	select {
	case l.permits <- struct{}{}:
	default:
		return Response{}, ErrOverloaded
	}
	defer func() { <-l.permits }()
	return l.next(ctx, req)
}
