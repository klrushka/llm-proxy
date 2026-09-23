package process

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// compile-time check that Operation.Handle is signature-compatible with
// api.ProcessFunc (func(context.Context, Request) (Response, error)).
var _ func(context.Context, Request) (Response, error) = (&Operation{}).Handle

// signalCtx wraps a context and closes a channel the first time Done() is
// called. Store.WaitReady evaluates ctx.Done() in its waiting select, so this
// lets tests observe deterministically that a waiter has reached the
// context-aware wait rather than merely that its goroutine was launched.
type signalCtx struct {
	context.Context
	once sync.Once
	done chan struct{}
}

func (c *signalCtx) Done() <-chan struct{} {
	c.once.Do(func() { close(c.done) })
	return c.Context.Done()
}

func TestOperationNewPayloadMasksOnce(t *testing.T) {
	calls := 0
	op := NewOperation(NewStore(), func(_ context.Context, payload string) (string, error) {
		calls++
		return "masked:" + payload, nil
	})

	resp, err := op.Handle(context.Background(), Request{Payload: "synthetic text", PayloadID: "id-1"})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if resp.Result != "masked:synthetic text" {
		t.Errorf("Result = %q, want %q", resp.Result, "masked:synthetic text")
	}
	if calls != 1 {
		t.Errorf("mask calls = %d, want 1", calls)
	}

	rec, err := op.store.Get("id-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if rec.State != StateReady {
		t.Errorf("State = %q, want %q", rec.State, StateReady)
	}
	if rec.Original != "synthetic text" {
		t.Errorf("Original = %q, want %q", rec.Original, "synthetic text")
	}
	if rec.Result != "masked:synthetic text" {
		t.Errorf("Result = %q, want %q", rec.Result, "masked:synthetic text")
	}
}

func TestOperationRetrySameOriginalReturnsSavedResult(t *testing.T) {
	calls := 0
	op := NewOperation(NewStore(), func(_ context.Context, payload string) (string, error) {
		calls++
		return "masked:" + payload, nil
	})

	first, err := op.Handle(context.Background(), Request{Payload: "synthetic text", PayloadID: "id-1"})
	if err != nil {
		t.Fatalf("first Handle() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("mask calls after first = %d, want 1", calls)
	}

	second, err := op.Handle(context.Background(), Request{Payload: "synthetic text", PayloadID: "id-1"})
	if err != nil {
		t.Fatalf("retry Handle() error = %v", err)
	}
	if second.Result != first.Result {
		t.Errorf("retry Result = %q, want %q", second.Result, first.Result)
	}
	if calls != 1 {
		t.Errorf("mask calls after retry = %d, want 1 (no re-masking)", calls)
	}
}

func TestOperationMaskingErrorFailsClosed(t *testing.T) {
	const payload = "synthetic text"
	op := NewOperation(NewStore(), func(_ context.Context, _ string) (string, error) {
		return "", errors.New("masking failed: " + payload)
	})

	resp, err := op.Handle(context.Background(), Request{Payload: payload, PayloadID: "id-1"})
	if !errors.Is(err, ErrMaskingFailed) {
		t.Fatalf("Handle() error = %v, want ErrMaskingFailed", err)
	}
	if err != nil && strings.Contains(err.Error(), payload) {
		t.Errorf("returned error leaks payload: %q", err.Error())
	}
	if resp.Result != "" {
		t.Errorf("Result = %q, want empty (no plaintext leak)", resp.Result)
	}

	rec, err := op.store.Get("id-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if rec.State != StateExpired {
		t.Errorf("State = %q, want %q (claim safely terminated)", rec.State, StateExpired)
	}
	if rec.Result != "" {
		t.Errorf("Result = %q, want empty", rec.Result)
	}
}

func TestOperationVaultUnavailableFailsClosed(t *testing.T) {
	const payload = "synthetic text"
	op := NewOperation(NewStore(), func(_ context.Context, _ string) (string, error) {
		return "", fmt.Errorf("vault down: %w", ErrVaultUnavailable)
	})

	resp, err := op.Handle(context.Background(), Request{Payload: payload, PayloadID: "id-1"})
	if !errors.Is(err, ErrVaultUnavailable) {
		t.Fatalf("Handle() error = %v, want ErrVaultUnavailable", err)
	}
	if errors.Is(err, ErrMaskingFailed) {
		t.Errorf("Handle() error = %v, must not be ErrMaskingFailed", err)
	}
	if err != nil && strings.Contains(err.Error(), "vault down") {
		t.Errorf("returned error leaks dependency detail: %q", err.Error())
	}
	if err != nil && strings.Contains(err.Error(), payload) {
		t.Errorf("returned error leaks payload: %q", err.Error())
	}
	if resp.Result != "" {
		t.Errorf("Result = %q, want empty (no plaintext leak)", resp.Result)
	}

	rec, err := op.store.Get("id-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if rec.State != StateExpired {
		t.Errorf("State = %q, want %q (claim safely terminated)", rec.State, StateExpired)
	}
	if rec.Result != "" {
		t.Errorf("Result = %q, want empty", rec.Result)
	}
}

func TestOperationVaultUnavailableOwnerAndWaiterAgree(t *testing.T) {
	const payloadID = "id-1"
	var calls atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	op := NewOperation(NewStore(), func(_ context.Context, p string) (string, error) {
		calls.Add(1)
		close(entered)
		<-release
		return "", fmt.Errorf("vault down: %w", ErrVaultUnavailable)
	})

	ownerDone := make(chan struct{})
	var ownerResp Response
	var ownerErr error
	go func() {
		ownerResp, ownerErr = op.Handle(context.Background(), Request{Payload: "synthetic payload", PayloadID: payloadID})
		close(ownerDone)
	}()
	<-entered

	waiterInWait := make(chan struct{})
	waiterDone := make(chan struct{})
	var waiterResp Response
	var waiterErr error
	go func() {
		waiterResp, waiterErr = op.Handle(&signalCtx{Context: context.Background(), done: waiterInWait}, Request{Payload: "synthetic payload", PayloadID: payloadID})
		close(waiterDone)
	}()
	<-waiterInWait

	close(release)
	<-ownerDone
	<-waiterDone

	if got := calls.Load(); got != 1 {
		t.Errorf("mask calls = %d, want 1", got)
	}
	if !errors.Is(ownerErr, ErrVaultUnavailable) {
		t.Errorf("owner error = %v, want ErrVaultUnavailable", ownerErr)
	}
	if ownerResp.Result != "" {
		t.Errorf("owner result = %q, want empty (fail closed)", ownerResp.Result)
	}
	if !errors.Is(waiterErr, ErrVaultUnavailable) {
		t.Errorf("waiter error = %v, want ErrVaultUnavailable", waiterErr)
	}
	if waiterResp.Result != "" {
		t.Errorf("waiter result = %q, want empty (fail closed)", waiterResp.Result)
	}

	rec, err := op.store.Get(payloadID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if rec.State != StateExpired {
		t.Errorf("record State = %q, want %q", rec.State, StateExpired)
	}
	if rec.Result != "" {
		t.Errorf("record Result = %q, want empty", rec.Result)
	}
}

func TestOperationVaultUnavailableLaterRetryObservesSame(t *testing.T) {
	const payload = "synthetic text"
	const payloadID = "id-1"
	var calls atomic.Int64
	op := NewOperation(NewStore(), func(_ context.Context, _ string) (string, error) {
		calls.Add(1)
		return "", fmt.Errorf("vault down: %w", ErrVaultUnavailable)
	})

	first, err := op.Handle(context.Background(), Request{Payload: payload, PayloadID: payloadID})
	if !errors.Is(err, ErrVaultUnavailable) {
		t.Fatalf("first Handle() error = %v, want ErrVaultUnavailable", err)
	}
	if first.Result != "" {
		t.Errorf("first Result = %q, want empty", first.Result)
	}

	retry, err := op.Handle(context.Background(), Request{Payload: payload, PayloadID: payloadID})
	if !errors.Is(err, ErrVaultUnavailable) {
		t.Errorf("retry Handle() error = %v, want ErrVaultUnavailable", err)
	}
	if retry.Result != "" {
		t.Errorf("retry Result = %q, want empty", retry.Result)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("mask calls = %d, want 1 (no re-masking on retry)", got)
	}
}

func TestOperationModelUnavailableOwnerWaiterRetryShareClassification(t *testing.T) {
	const payload = "synthetic text"
	const payloadID = "id-1"
	var calls atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	composed := WithRulesOnlyFallback(false,
		func(_ context.Context, p string) (string, error) {
			if calls.Add(1) == 1 {
				close(entered)
				<-release
				return "", fmt.Errorf("worker down: %w", ErrModelUnavailable)
			}
			return "masked synthetic", nil
		},
		func(_ context.Context, _ string) (string, error) {
			return "rules", nil
		})
	op := NewOperation(NewStore(), composed)

	ownerDone := make(chan struct{})
	var ownerResp Response
	var ownerErr error
	go func() {
		ownerResp, ownerErr = op.Handle(context.Background(), Request{Payload: payload, PayloadID: payloadID})
		close(ownerDone)
	}()
	<-entered

	waiterInWait := make(chan struct{})
	waiterDone := make(chan struct{})
	var waiterResp Response
	var waiterErr error
	go func() {
		waiterResp, waiterErr = op.Handle(&signalCtx{Context: context.Background(), done: waiterInWait}, Request{Payload: payload, PayloadID: payloadID})
		close(waiterDone)
	}()
	<-waiterInWait

	close(release)
	<-ownerDone
	<-waiterDone

	if got := calls.Load(); got != 1 {
		t.Errorf("composed mask calls = %d, want 1", got)
	}
	if !errors.Is(ownerErr, ErrModelUnavailable) {
		t.Errorf("owner error = %v, want ErrModelUnavailable", ownerErr)
	}
	if ownerErr != ErrModelUnavailable {
		t.Errorf("owner error = %v, want exact bare sentinel", ownerErr)
	}
	if ownerResp.Result != "" {
		t.Errorf("owner result = %q, want empty (fail closed)", ownerResp.Result)
	}
	if !errors.Is(waiterErr, ErrModelUnavailable) {
		t.Errorf("waiter error = %v, want ErrModelUnavailable", waiterErr)
	}
	if waiterErr != ErrModelUnavailable {
		t.Errorf("waiter error = %v, want exact bare sentinel", waiterErr)
	}
	if waiterResp.Result != "" {
		t.Errorf("waiter result = %q, want empty (fail closed)", waiterResp.Result)
	}

	rec, err := op.store.Get(payloadID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if rec.State != StateExpired {
		t.Errorf("record State = %q, want %q", rec.State, StateExpired)
	}
	if rec.Result != "" {
		t.Errorf("record Result = %q, want empty", rec.Result)
	}

	retry, err := op.Handle(context.Background(), Request{Payload: payload, PayloadID: payloadID})
	if err != nil || retry.Result != "masked synthetic" {
		t.Errorf("retry = (%q, %v), want successful remask", retry.Result, err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("composed mask calls after retry = %d, want 2", got)
	}
}

func TestOperationGenericMaskingErrorOwnerAndWaiterStayMaskingFailed(t *testing.T) {
	const payloadID = "id-1"
	var calls atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	op := NewOperation(NewStore(), func(_ context.Context, p string) (string, error) {
		calls.Add(1)
		close(entered)
		<-release
		return "", errors.New("masking failed: " + p)
	})

	ownerDone := make(chan struct{})
	var ownerErr error
	go func() {
		_, ownerErr = op.Handle(context.Background(), Request{Payload: "synthetic payload", PayloadID: payloadID})
		close(ownerDone)
	}()
	<-entered

	waiterInWait := make(chan struct{})
	waiterDone := make(chan struct{})
	var waiterErr error
	go func() {
		_, waiterErr = op.Handle(&signalCtx{Context: context.Background(), done: waiterInWait}, Request{Payload: "synthetic payload", PayloadID: payloadID})
		close(waiterDone)
	}()
	<-waiterInWait

	close(release)
	<-ownerDone
	<-waiterDone

	if got := calls.Load(); got != 1 {
		t.Errorf("mask calls = %d, want 1", got)
	}
	if !errors.Is(ownerErr, ErrMaskingFailed) {
		t.Errorf("owner error = %v, want ErrMaskingFailed", ownerErr)
	}
	if errors.Is(ownerErr, ErrVaultUnavailable) {
		t.Errorf("owner error = %v, must not be ErrVaultUnavailable", ownerErr)
	}
	if !errors.Is(waiterErr, ErrMaskingFailed) {
		t.Errorf("waiter error = %v, want ErrMaskingFailed", waiterErr)
	}
	if errors.Is(waiterErr, ErrVaultUnavailable) {
		t.Errorf("waiter error = %v, must not be ErrVaultUnavailable", waiterErr)
	}
}

func TestOperationDistinctPayloadIDsMaskIndependently(t *testing.T) {
	calls := 0
	op := NewOperation(NewStore(), func(_ context.Context, payload string) (string, error) {
		calls++
		return "masked:" + payload, nil
	})

	r1, err := op.Handle(context.Background(), Request{Payload: "alpha", PayloadID: "id-a"})
	if err != nil {
		t.Fatalf("Handle(id-a) error = %v", err)
	}
	r2, err := op.Handle(context.Background(), Request{Payload: "beta", PayloadID: "id-b"})
	if err != nil {
		t.Fatalf("Handle(id-b) error = %v", err)
	}
	if r1.Result != "masked:alpha" || r2.Result != "masked:beta" {
		t.Errorf("results = %q, %q", r1.Result, r2.Result)
	}
	if calls != 2 {
		t.Errorf("mask calls = %d, want 2", calls)
	}
}

func TestOperationRestoreByMaskReturnsOriginal(t *testing.T) {
	calls := 0
	op := NewOperation(NewStore(), func(_ context.Context, payload string) (string, error) {
		calls++
		return "masked:" + payload, nil
	})

	first, err := op.Handle(context.Background(), Request{Payload: "synthetic original", PayloadID: "id-1"})
	if err != nil {
		t.Fatalf("first Handle() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("mask calls after first = %d, want 1", calls)
	}

	restored, err := op.Handle(context.Background(), Request{Payload: first.Result, PayloadID: "id-1"})
	if err != nil {
		t.Fatalf("restore Handle() error = %v", err)
	}
	if restored.Result != "synthetic original" {
		t.Errorf("restored Result = %q, want %q", restored.Result, "synthetic original")
	}
	if calls != 1 {
		t.Errorf("mask calls after restore = %d, want 1 (no re-masking)", calls)
	}
}

func TestOperationRestoreIsRepeatableRead(t *testing.T) {
	calls := 0
	op := NewOperation(NewStore(), func(_ context.Context, payload string) (string, error) {
		calls++
		return "masked:" + payload, nil
	})

	first, err := op.Handle(context.Background(), Request{Payload: "synthetic original", PayloadID: "id-1"})
	if err != nil {
		t.Fatalf("first Handle() error = %v", err)
	}
	before, err := op.store.Get("id-1")
	if err != nil {
		t.Fatalf("Get() before restore error = %v", err)
	}

	for i := 0; i < 2; i++ {
		restored, err := op.Handle(context.Background(), Request{Payload: first.Result, PayloadID: "id-1"})
		if err != nil {
			t.Fatalf("restore %d Handle() error = %v", i, err)
		}
		if restored.Result != "synthetic original" {
			t.Errorf("restore %d Result = %q, want %q", i, restored.Result, "synthetic original")
		}
	}

	if calls != 1 {
		t.Errorf("mask calls = %d, want 1 (no re-masking on restore)", calls)
	}

	after, err := op.store.Get("id-1")
	if err != nil {
		t.Fatalf("Get() after restore error = %v", err)
	}
	if before.PayloadID != after.PayloadID ||
		before.State != after.State ||
		before.Original != after.Original ||
		before.Result != after.Result {
		t.Errorf("record changed after restore:\nbefore = %+v\nafter  = %+v", before, after)
	}
	if after.State != StateReady {
		t.Errorf("State = %q, want %q (record stays ready)", after.State, StateReady)
	}
}

func TestOperationThirdUnrelatedPayloadConflicts(t *testing.T) {
	calls := 0
	op := NewOperation(NewStore(), func(_ context.Context, payload string) (string, error) {
		calls++
		return "masked:" + payload, nil
	})

	first, err := op.Handle(context.Background(), Request{Payload: "synthetic original", PayloadID: "id-1"})
	if err != nil {
		t.Fatalf("first Handle() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("mask calls after first = %d, want 1", calls)
	}
	before, err := op.store.Get("id-1")
	if err != nil {
		t.Fatalf("Get() before conflict error = %v", err)
	}

	// Third unrelated payload: neither the original nor the issued result.
	resp, err := op.Handle(context.Background(), Request{Payload: "unrelated third payload", PayloadID: "id-1"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Handle() error = %v, want ErrConflict", err)
	}
	if resp.Result != "" {
		t.Errorf("Result = %q, want empty (no plaintext leak)", resp.Result)
	}
	if calls != 1 {
		t.Errorf("mask calls after conflict = %d, want 1 (no re-masking)", calls)
	}

	after, err := op.store.Get("id-1")
	if err != nil {
		t.Fatalf("Get() after conflict error = %v", err)
	}
	if before.PayloadID != after.PayloadID ||
		before.State != after.State ||
		before.Original != after.Original ||
		before.Result != after.Result {
		t.Errorf("record changed after conflict:\nbefore = %+v\nafter  = %+v", before, after)
	}
	if after.State != StateReady {
		t.Errorf("State = %q, want %q (record stays ready)", after.State, StateReady)
	}
	if after.Original != "synthetic original" || after.Result != first.Result {
		t.Errorf("record content changed: original=%q result=%q", after.Original, after.Result)
	}
}

func TestConcurrentIdenticalFirstRequestsShareResult(t *testing.T) {
	const (
		payload   = "synthetic original"
		payloadID = "id-1"
	)
	var calls atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	op := NewOperation(NewStore(), func(_ context.Context, p string) (string, error) {
		calls.Add(1)
		close(entered)
		<-release
		return "masked:" + p, nil
	})

	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, err := op.Handle(context.Background(), Request{Payload: payload, PayloadID: payloadID})
			results[i] = resp.Result
			errs[i] = err
		}(i)
	}
	close(start)
	// Wait until the masker has been entered (the winner is in flight) before
	// releasing it, so all callers are racing on the same claim.
	<-entered
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("mask calls = %d, want 1", got)
	}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d error = %v", i, errs[i])
		}
		if results[i] != "masked:"+payload {
			t.Errorf("caller %d result = %q, want %q", i, results[i], "masked:"+payload)
		}
	}

	rec, err := op.store.Get(payloadID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if rec.State != StateReady || rec.Result != "masked:"+payload {
		t.Errorf("record = %+v, want ready with shared result", rec)
	}
}

func TestConcurrentDifferentFirstPayloadsOneWinner(t *testing.T) {
	const payloadID = "id-1"
	var calls atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	op := NewOperation(NewStore(), func(_ context.Context, p string) (string, error) {
		calls.Add(1)
		close(entered)
		<-release
		return "masked:" + p, nil
	})

	start := make(chan struct{})
	var wg sync.WaitGroup
	type outcome struct {
		result string
		err    error
	}
	outcomes := make([]outcome, 2)
	payloads := []string{"first payload", "second payload"}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, err := op.Handle(context.Background(), Request{Payload: payloads[i], PayloadID: payloadID})
			outcomes[i] = outcome{result: resp.Result, err: err}
		}(i)
	}
	close(start)
	<-entered
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("mask calls = %d, want 1", got)
	}

	var winner, loser int
	if outcomes[0].err == nil {
		winner, loser = 0, 1
	} else {
		winner, loser = 1, 0
	}
	if outcomes[winner].err != nil {
		t.Fatalf("winner error = %v, want nil", outcomes[winner].err)
	}
	if outcomes[winner].result != "masked:"+payloads[winner] {
		t.Errorf("winner result = %q, want %q", outcomes[winner].result, "masked:"+payloads[winner])
	}
	if !errors.Is(outcomes[loser].err, ErrConflict) {
		t.Errorf("loser error = %v, want ErrConflict", outcomes[loser].err)
	}
	if outcomes[loser].result != "" {
		t.Errorf("loser result = %q, want empty", outcomes[loser].result)
	}

	rec, err := op.store.Get(payloadID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if rec.Original != payloads[winner] || rec.Result != "masked:"+payloads[winner] {
		t.Errorf("record = %+v, want winner's original/result (not overwritten)", rec)
	}
}

func TestConcurrentWaiterContextCancellation(t *testing.T) {
	const payloadID = "id-1"
	entered := make(chan struct{})
	release := make(chan struct{})
	op := NewOperation(NewStore(), func(_ context.Context, p string) (string, error) {
		close(entered)
		<-release
		return "masked:" + p, nil
	})

	// The winner claims first and blocks in the masker; the waiter then waits
	// on the in-flight claim and is cancelled only after it has definitely
	// entered the context-aware wait in Store.WaitReady.
	winnerDone := make(chan struct{})
	var winnerErr error
	go func() {
		_, winnerErr = op.Handle(context.Background(), Request{Payload: "winner payload", PayloadID: payloadID})
		close(winnerDone)
	}()
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	waiterInWait := make(chan struct{})
	waiterDone := make(chan struct{})
	var waiterErr error
	go func() {
		_, waiterErr = op.Handle(&signalCtx{Context: ctx, done: waiterInWait}, Request{Payload: "waiter payload", PayloadID: payloadID})
		close(waiterDone)
	}()

	// Wait until the waiter has reached the WaitReady select before cancelling.
	<-waiterInWait
	cancel()
	select {
	case <-waiterDone:
	case <-winnerDone:
		t.Fatal("winner finished before waiter was cancelled")
	}
	if !errors.Is(waiterErr, context.Canceled) {
		t.Errorf("waiter error = %v, want context.Canceled", waiterErr)
	}

	close(release)
	<-winnerDone
	if winnerErr != nil {
		t.Errorf("winner error = %v, want nil", winnerErr)
	}
}

func TestConcurrentMaskingFailureReleasesWaiters(t *testing.T) {
	const payloadID = "id-1"
	var calls atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	op := NewOperation(NewStore(), func(_ context.Context, p string) (string, error) {
		calls.Add(1)
		close(entered)
		<-release
		return "", errors.New("masking failed: " + p)
	})

	// Start the owner first and wait until it has claimed and entered the
	// blocked masker, so the claim is definitely held before the waiter starts.
	ownerDone := make(chan struct{})
	var ownerResp Response
	var ownerErr error
	go func() {
		ownerResp, ownerErr = op.Handle(context.Background(), Request{Payload: "synthetic payload", PayloadID: payloadID})
		close(ownerDone)
	}()
	<-entered

	// Start the waiter second and wait until it has reached the WaitReady
	// select before releasing the owner. This guarantees the waiter observes
	// the expired record through the done channel, not the direct-expired path.
	waiterInWait := make(chan struct{})
	waiterDone := make(chan struct{})
	var waiterResp Response
	var waiterErr error
	go func() {
		waiterResp, waiterErr = op.Handle(&signalCtx{Context: context.Background(), done: waiterInWait}, Request{Payload: "synthetic payload", PayloadID: payloadID})
		close(waiterDone)
	}()
	<-waiterInWait

	close(release)
	<-ownerDone
	<-waiterDone

	if got := calls.Load(); got != 1 {
		t.Errorf("mask calls = %d, want 1", got)
	}
	if !errors.Is(ownerErr, ErrMaskingFailed) {
		t.Errorf("owner error = %v, want ErrMaskingFailed", ownerErr)
	}
	if ownerResp.Result != "" {
		t.Errorf("owner result = %q, want empty (fail closed)", ownerResp.Result)
	}
	if !errors.Is(waiterErr, ErrMaskingFailed) {
		t.Errorf("waiter error = %v, want ErrMaskingFailed", waiterErr)
	}
	if waiterResp.Result != "" {
		t.Errorf("waiter result = %q, want empty (fail closed)", waiterResp.Result)
	}

	rec, err := op.store.Get(payloadID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if rec.State != StateExpired {
		t.Errorf("record State = %q, want %q", rec.State, StateExpired)
	}
	if rec.Result != "" {
		t.Errorf("record Result = %q, want empty", rec.Result)
	}
}

func TestBlockedMaskForOneIDDoesNotBlockAnother(t *testing.T) {
	enteredBlocked := make(chan struct{})
	release := make(chan struct{})
	op := NewOperation(NewStore(), func(_ context.Context, p string) (string, error) {
		if p == "blocked" {
			close(enteredBlocked)
			<-release
		}
		return "masked:" + p, nil
	})

	blockedDone := make(chan struct{})
	go func() {
		_, _ = op.Handle(context.Background(), Request{Payload: "blocked", PayloadID: "id-blocked"})
		close(blockedDone)
	}()

	// Wait until id-blocked is actually inside its blocked masker before
	// starting id-free, so the free id provably runs while the other is stuck.
	<-enteredBlocked

	// The blocked mask for id-blocked must not prevent id-free from completing.
	done := make(chan struct{})
	go func() {
		resp, err := op.Handle(context.Background(), Request{Payload: "free", PayloadID: "id-free"})
		if err != nil || resp.Result != "masked:free" {
			t.Errorf("free Handle() = %q, %v", resp.Result, err)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-blockedDone:
		t.Fatal("blocked id finished before the free id")
	}

	close(release)
	<-blockedDone
}
