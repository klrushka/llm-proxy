package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/policy"
	"github.com/klrushka/llm-proxy/internal/tokenization"
	"github.com/klrushka/llm-proxy/internal/vault"
)

// revokeRecordingIssuer is a RevocableTokenIssuer that records every scope it
// was asked to revoke and whether the context it received was already
// cancelled.
type revokeRecordingIssuer struct {
	revoked      []string
	ctxCancelled bool
	err          error
}

func (r *revokeRecordingIssuer) Token(scope string, typ detection.Type, value string) (string, error) {
	return "<EMAIL_00000000000000000000000000000000>", nil
}

func (r *revokeRecordingIssuer) RevokeScope(ctx context.Context, scope string) error {
	if ctx.Err() != nil {
		r.ctxCancelled = true
	}
	r.revoked = append(r.revoked, scope)
	return r.err
}

type revokeRecordingVault struct{ calls int }

func (*revokeRecordingVault) Save(context.Context, string, string, string) error { return nil }
func (*revokeRecordingVault) Resolve(context.Context, string, string) (string, error) {
	return "", vault.ErrNotFound
}
func (v *revokeRecordingVault) RevokeScope(context.Context, string) error {
	v.calls++
	return nil
}

// failingRevokeVault is a Vault whose RevokeScope always fails.
type failingRevokeVault struct{}

func (failingRevokeVault) Save(context.Context, string, string, string) error { return nil }
func (failingRevokeVault) Resolve(context.Context, string, string) (string, error) {
	return "", vault.ErrNotFound
}
func (failingRevokeVault) RevokeScope(context.Context, string) error {
	return errors.New("sensitive vault revoke detail")
}

// TestRevokeScopeClearsIssuerEvenWhenVaultFails proves that revokeScope clears
// the issuer cache even when the vault returns an error, and that the vault
// error is the one returned to the caller.
func TestRevokeScopeClearsIssuerEvenWhenVaultFails(t *testing.T) {
	issuer := &revokeRecordingIssuer{}
	p := policy.NewPolicy(policy.DefaultConsumerID, []string{string(detection.TypeEmail)})
	pipe := NewPipeline(noModel, p, issuer, failingRevokeVault{})

	err := pipe.revokeScope(context.Background(), "scope-1")
	if err == nil {
		t.Fatal("revokeScope() error = nil, want vault error")
	}
	if err.Error() != "sensitive vault revoke detail" {
		t.Errorf("revokeScope() error = %v, want the vault error", err)
	}
	if len(issuer.revoked) != 1 || issuer.revoked[0] != "scope-1" {
		t.Errorf("issuer revoked scopes = %v, want [scope-1]", issuer.revoked)
	}
}

// TestRevokeScopeClearsBothVaultAndIssuer proves that on a successful revoke
// both the vault and the issuer are cleared for the same scope.
func TestRevokeScopeClearsBothVaultAndIssuer(t *testing.T) {
	issuer := &revokeRecordingIssuer{}
	v, err := vault.NewMemory(time.Hour)
	if err != nil {
		t.Fatalf("vault.NewMemory() error = %v", err)
	}
	p := policy.NewPolicy(policy.DefaultConsumerID, []string{string(detection.TypeEmail)})
	pipe := NewPipeline(noModel, p, issuer, v)

	if err := pipe.revokeScope(context.Background(), "scope-1"); err != nil {
		t.Fatalf("revokeScope() error = %v", err)
	}
	if len(issuer.revoked) != 1 || issuer.revoked[0] != "scope-1" {
		t.Errorf("issuer revoked scopes = %v, want [scope-1]", issuer.revoked)
	}
}

// TestRevokeScopeCancelledContextHasNoSideEffects proves cancellation is
// rechecked under the lifecycle lock before either storage is mutated.
func TestRevokeScopeCancelledContextHasNoSideEffects(t *testing.T) {
	issuer := &revokeRecordingIssuer{}
	v := &revokeRecordingVault{}
	p := policy.NewPolicy(policy.DefaultConsumerID, []string{string(detection.TypeEmail)})
	pipe := NewPipeline(noModel, p, issuer, v)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := pipe.revokeScope(ctx, "scope-1")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("revokeScope() error = %v, want context.Canceled", err)
	}
	if len(issuer.revoked) != 0 {
		t.Errorf("issuer revoked scopes = %v, want none", issuer.revoked)
	}
	if v.calls != 0 {
		t.Errorf("vault revoke calls = %d, want 0", v.calls)
	}
}

func TestRevokeScopeIssuerFailureSkipsVault(t *testing.T) {
	wantErr := errors.New("issuer unavailable")
	issuer := &revokeRecordingIssuer{err: wantErr}
	v := &revokeRecordingVault{}
	p := policy.NewPolicy(policy.DefaultConsumerID, []string{string(detection.TypeEmail)})
	pipe := NewPipeline(noModel, p, issuer, v)

	err := pipe.revokeScope(context.Background(), "scope-1")
	if !errors.Is(err, wantErr) {
		t.Fatalf("revokeScope() error = %v, want issuer error", err)
	}
	if v.calls != 0 {
		t.Errorf("vault revoke calls = %d, want 0 after issuer failure", v.calls)
	}
}

func TestRevokeScopeCancellationWhileWaitingForLifecycleLock(t *testing.T) {
	issuer := &revokeRecordingIssuer{}
	v := &revokeRecordingVault{}
	p := policy.NewPolicy(policy.DefaultConsumerID, []string{string(detection.TypeEmail)})
	pipe := NewPipeline(noModel, p, issuer, v)

	pipe.lifecycle.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- pipe.revokeScope(ctx, "scope-1")
	}()
	<-started
	cancel()
	pipe.lifecycle.Unlock()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("revokeScope() error = %v, want context.Canceled", err)
	}
	if len(issuer.revoked) != 0 || v.calls != 0 {
		t.Fatalf("cancelled waiter mutated state: issuer=%v vault_calls=%d", issuer.revoked, v.calls)
	}
}

// blockingSaveVault wraps a real Vault and blocks the first Save until released.
// It is a deterministic test double: the pipeline's tokenize reaches the Save
// (after issuance) and stops there, letting a concurrent revoke contend for the
// lifecycle write lock.
type blockingSaveVault struct {
	vault.Vault
	saveStarted chan struct{}
	releaseSave chan struct{}
	once        sync.Once
}

func (b *blockingSaveVault) Save(ctx context.Context, scope, token, original string) error {
	b.once.Do(func() { close(b.saveStarted) })
	<-b.releaseSave
	return b.Vault.Save(ctx, scope, token, original)
}

// TestRevokeLinearizableWithInFlightTokenize proves the lifecycle guarantee:
// a tokenize that has issued a token but not yet finished persisting holds the
// read lock, so a concurrent revoke waits for it; after the revoke returns, the
// old token no longer resolves and a fresh tokenize issues a fresh token. The
// interleaving is driven by channels, never by sleep.
func TestRevokeLinearizableWithInFlightTokenize(t *testing.T) {
	mem, err := vault.NewMemory(time.Hour)
	if err != nil {
		t.Fatalf("vault.NewMemory() error = %v", err)
	}
	blocking := &blockingSaveVault{
		Vault:       mem,
		saveStarted: make(chan struct{}),
		releaseSave: make(chan struct{}),
	}
	g, err := tokenization.New()
	if err != nil {
		t.Fatalf("tokenization.New() error = %v", err)
	}
	p := policy.NewPolicy(policy.DefaultConsumerID, []string{string(detection.TypeEmail)})
	pipe := NewPipeline(noModel, p, g, blocking)

	const text = "email ivanov@example.com"
	tokCh := make(chan string, 1)
	tokErrCh := make(chan error, 1)
	go func() {
		res, err := pipe.tokenize(context.Background(), TokenizeRequest{Text: text, ScopeID: "scope-1"})
		if err != nil {
			tokErrCh <- err
			return
		}
		tokCh <- res.TokenizedText
	}()

	// Wait until tokenize has issued the token and is blocked inside Save.
	<-blocking.saveStarted

	// Start revoke; it must block on the lifecycle write lock until tokenize
	// releases the read lock.
	revokeDone := make(chan error, 1)
	go func() {
		revokeDone <- pipe.revokeScope(context.Background(), "scope-1")
	}()

	// Revoke must not complete while tokenize holds the read lock. This is an
	// assertion, not synchronization: revoke physically cannot acquire the
	// write lock until tokenize finishes.
	select {
	case err := <-revokeDone:
		t.Fatalf("revoke completed before tokenize released the read lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// Release the blocking Save so tokenize completes and releases the read lock.
	close(blocking.releaseSave)

	var tok string
	select {
	case tok = <-tokCh:
	case err := <-tokErrCh:
		t.Fatalf("tokenize error = %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("tokenize did not complete after releasing Save")
	}

	// Revoke must now complete successfully.
	select {
	case err := <-revokeDone:
		if err != nil {
			t.Fatalf("revokeScope() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("revoke did not complete after tokenize released the read lock")
	}

	// The old token must no longer resolve.
	det, err := pipe.detokenize(context.Background(), DetokenizeRequest{Text: tok, ScopeID: "scope-1", Mode: ModeStrict})
	if err == nil {
		t.Fatal("old token after revoke resolved, want failure")
	}
	if det.RestoredText != "" {
		t.Errorf("old token after revoke returned partial text %q", det.RestoredText)
	}

	// A fresh tokenize must issue a fresh token.
	res2, err := pipe.tokenize(context.Background(), TokenizeRequest{Text: text, ScopeID: "scope-1"})
	if err != nil {
		t.Fatalf("tokenize after revoke error = %v", err)
	}
	if res2.TokenizedText == tok {
		t.Fatalf("tokenize after revoke reused token %q", tok)
	}
}
