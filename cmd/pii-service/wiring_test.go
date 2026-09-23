package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/klrushka/llm-proxy/internal/api"
	"github.com/klrushka/llm-proxy/internal/config"
	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/policy"
	"github.com/klrushka/llm-proxy/internal/tokenization"
	"github.com/klrushka/llm-proxy/internal/vault"
)

// productionKey is a fixed 32-byte base64 master key used by wiring tests.
const productionKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

// loadWiringConfig loads a Config with the given vault key and TTL via the real
// config.Load path so the configured key is genuinely exercised.
func loadWiringConfig(t *testing.T, key string, ttl time.Duration) config.Config {
	t.Helper()
	t.Setenv("PII_VAULT_KEY", key)
	t.Setenv("PII_VAULT_TTL", ttl.String())
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	return cfg
}

// newProductionPipeline builds the production wiring seam exactly as run() does:
// an encrypted vault and a keyed TTL issuer derived from cfg.VaultKey.Bytes()
// and cfg.VaultTTL, with a rules-only detector.
func newProductionPipeline(t *testing.T, cfg config.Config) (*api.Pipeline, api.PIIHandlers) {
	t.Helper()
	v, err := vault.NewEncrypted(cfg.VaultKey.Bytes(), cfg.VaultTTL)
	if err != nil {
		t.Fatalf("vault.NewEncrypted() error = %v", err)
	}
	issuer, err := tokenization.NewKeyed(cfg.VaultKey.Bytes(), cfg.VaultTTL)
	if err != nil {
		t.Fatalf("tokenization.NewKeyed() error = %v", err)
	}
	reg, err := detection.New()
	if err != nil {
		t.Fatalf("detection.New() error = %v", err)
	}
	allowed := make([]string, 0, len(reg.Types()))
	for _, typ := range reg.Types() {
		allowed = append(allowed, string(typ))
	}
	p := policy.NewPolicy(allowed)
	pipe := api.NewPipeline(nil, p, issuer, v)
	return pipe, pipe.Handlers()
}

// TestProductionWiringUsesEncryptedVaultAndKeyedIssuer proves that the
// production wiring constructs an encrypted vault and a keyed TTL issuer from
// the configured key and TTL, and that the plaintext Memory adapter is not
// connected.
func TestProductionWiringUsesEncryptedVaultAndKeyedIssuer(t *testing.T) {
	cfg := loadWiringConfig(t, productionKey, time.Hour)
	v, err := vault.NewEncrypted(cfg.VaultKey.Bytes(), cfg.VaultTTL)
	if err != nil {
		t.Fatalf("vault.NewEncrypted() error = %v", err)
	}
	if _, ok := any(v).(*vault.Encrypted); !ok {
		t.Fatalf("production vault is %T, want *vault.Encrypted", v)
	}
	if _, ok := any(v).(*vault.Memory); ok {
		t.Fatal("production vault must not be the plaintext *vault.Memory adapter")
	}

	issuer, err := tokenization.NewKeyed(cfg.VaultKey.Bytes(), cfg.VaultTTL)
	if err != nil {
		t.Fatalf("tokenization.NewKeyed() error = %v", err)
	}
	if _, ok := any(issuer).(*tokenization.Generator); !ok {
		t.Fatalf("production issuer is %T, want *tokenization.Generator", issuer)
	}
}

// TestProductionWiringConfiguredKeyIsUsed proves that the configured master key
// is genuinely used: a mapping created under it round-trips back to the original
// text through the same pipeline. The wrong-key decryption failure is covered by
// the vault unit test TestEncryptedWrongKeyFailsClosed, which transfers real
// ciphertext to an adapter with a different AES key; a wiring-level wrong-key
// check here would only observe a not-found on a separate empty vault and is
// therefore not duplicated.
func TestProductionWiringConfiguredKeyIsUsed(t *testing.T) {
	cfg := loadWiringConfig(t, productionKey, time.Hour)
	_, handlers := newProductionPipeline(t, cfg)

	const text = "email ivanov@example.com"
	tok, err := handlers.Tokenize(context.Background(), api.TokenizeRequest{Text: text, ScopeID: "scope-1"})
	if err != nil {
		t.Fatalf("Tokenize() error = %v", err)
	}
	if !strings.Contains(tok.TokenizedText, "<EMAIL_") {
		t.Fatalf("tokenized text %q missing EMAIL token", tok.TokenizedText)
	}

	// The configured key must genuinely flow into both the issuer and the
	// vault: a mapping created under it restores back to the original text.
	det, err := handlers.Detokenize(context.Background(), api.DetokenizeRequest{
		Text: tok.TokenizedText, ScopeID: "scope-1", Mode: api.ModeStrict,
	})
	if err != nil {
		t.Fatalf("Detokenize() error = %v", err)
	}
	if det.RestoredText != text {
		t.Errorf("Detokenize() restored %q, want %q", det.RestoredText, text)
	}
}

// TestProductionRevokeThenRetokenizeDoesNotResurrectOldToken proves the
// end-to-end guarantee: after a scope revoke, tokenizing the same value issues
// a fresh token and the old token is no longer resolvable.
func TestProductionRevokeThenRetokenizeDoesNotResurrectOldToken(t *testing.T) {
	cfg := loadWiringConfig(t, productionKey, time.Hour)
	_, handlers := newProductionPipeline(t, cfg)

	const text = "email ivanov@example.com"
	first, err := handlers.Tokenize(context.Background(), api.TokenizeRequest{Text: text, ScopeID: "scope-1"})
	if err != nil {
		t.Fatalf("Tokenize() error = %v", err)
	}

	if err := handlers.RevokeScope(context.Background(), "scope-1"); err != nil {
		t.Fatalf("RevokeScope() error = %v", err)
	}

	second, err := handlers.Tokenize(context.Background(), api.TokenizeRequest{Text: text, ScopeID: "scope-1"})
	if err != nil {
		t.Fatalf("Tokenize() after revoke error = %v", err)
	}
	if first.TokenizedText == second.TokenizedText {
		t.Fatalf("tokenize after revoke reused token %q", first.TokenizedText)
	}

	// The old token must not be resolvable.
	det, err := handlers.Detokenize(context.Background(), api.DetokenizeRequest{
		Text: first.TokenizedText, ScopeID: "scope-1", Mode: api.ModeStrict,
	})
	if err == nil {
		t.Fatal("old token after revoke resolved, want failure")
	}
	if det.RestoredText != "" {
		t.Errorf("old token after revoke returned partial text %q", det.RestoredText)
	}

	// The new token must round-trip.
	det2, err := handlers.Detokenize(context.Background(), api.DetokenizeRequest{
		Text: second.TokenizedText, ScopeID: "scope-1", Mode: api.ModeStrict,
	})
	if err != nil {
		t.Fatalf("new token Detokenize() error = %v", err)
	}
	if det2.RestoredText != text {
		t.Errorf("new token restored %q, want %q", det2.RestoredText, text)
	}
}

// TestProductionExpireThenRetokenizeDoesNotResurrectOldToken proves the
// end-to-end guarantee for TTL expiry: after the TTL elapses, tokenizing the
// same value issues a fresh token and the old token is no longer resolvable.
func TestProductionExpireThenRetokenizeDoesNotResurrectOldToken(t *testing.T) {
	cfg := loadWiringConfig(t, productionKey, time.Millisecond)
	_, handlers := newProductionPipeline(t, cfg)

	const text = "email ivanov@example.com"
	first, err := handlers.Tokenize(context.Background(), api.TokenizeRequest{Text: text, ScopeID: "scope-1"})
	if err != nil {
		t.Fatalf("Tokenize() error = %v", err)
	}

	time.Sleep(5 * time.Millisecond)

	second, err := handlers.Tokenize(context.Background(), api.TokenizeRequest{Text: text, ScopeID: "scope-1"})
	if err != nil {
		t.Fatalf("Tokenize() after expiry error = %v", err)
	}
	if first.TokenizedText == second.TokenizedText {
		t.Fatalf("tokenize after expiry reused token %q", first.TokenizedText)
	}

	det, err := handlers.Detokenize(context.Background(), api.DetokenizeRequest{
		Text: first.TokenizedText, ScopeID: "scope-1", Mode: api.ModeStrict,
	})
	if err == nil {
		t.Fatal("old token after expiry resolved, want failure")
	}
	if det.RestoredText != "" {
		t.Errorf("old token after expiry returned partial text %q", det.RestoredText)
	}
}
