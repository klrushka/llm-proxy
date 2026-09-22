package config

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"
)

func resetEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		EnvAPIListenAddress,
		EnvModelWorkerURL,
		EnvVaultTTL,
		EnvModelMode,
		EnvVaultKey,
		EnvModelClientTimeout,
	} {
		value, present := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("Unsetenv(%q): %v", name, err)
		}
		t.Cleanup(func() {
			if present {
				os.Setenv(name, value)
			} else {
				os.Unsetenv(name)
			}
		})
	}
}

func validKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func TestLoadDefaults(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.APIListenAddress != DefaultAPIListenAddress {
		t.Errorf("APIListenAddress = %q, want %q", cfg.APIListenAddress, DefaultAPIListenAddress)
	}
	if cfg.ModelWorkerURL != DefaultModelWorkerURL {
		t.Errorf("ModelWorkerURL = %q, want %q", cfg.ModelWorkerURL, DefaultModelWorkerURL)
	}
	if cfg.VaultTTL != DefaultVaultTTL {
		t.Errorf("VaultTTL = %v, want %v", cfg.VaultTTL, DefaultVaultTTL)
	}
	if cfg.ModelMode != DefaultModelMode {
		t.Errorf("ModelMode = %q, want %q", cfg.ModelMode, DefaultModelMode)
	}
	if cfg.ModelClientTimeout != DefaultModelClientTimeout {
		t.Errorf("ModelClientTimeout = %v, want %v", cfg.ModelClientTimeout, DefaultModelClientTimeout)
	}

	want := make([]byte, 32)
	for i := range want {
		want[i] = byte(i + 1)
	}
	if got := cfg.VaultKey.Bytes(); got != [32]byte(want) {
		t.Errorf("VaultKey bytes mismatch")
	}
}

func TestLoadOverrides(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvAPIListenAddress, "0.0.0.0:9090")
	t.Setenv(EnvModelWorkerURL, "https://worker.example.com:8443")
	t.Setenv(EnvVaultTTL, "30s")
	t.Setenv(EnvModelMode, ModelModeFast)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvModelClientTimeout, "45s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.APIListenAddress != "0.0.0.0:9090" {
		t.Errorf("APIListenAddress = %q", cfg.APIListenAddress)
	}
	if cfg.ModelWorkerURL != "https://worker.example.com:8443" {
		t.Errorf("ModelWorkerURL = %q", cfg.ModelWorkerURL)
	}
	if cfg.VaultTTL != 30*time.Second {
		t.Errorf("VaultTTL = %v", cfg.VaultTTL)
	}
	if cfg.ModelMode != ModelModeFast {
		t.Errorf("ModelMode = %q", cfg.ModelMode)
	}
	if cfg.ModelClientTimeout != 45*time.Second {
		t.Errorf("ModelClientTimeout = %v", cfg.ModelClientTimeout)
	}
}

func TestLoadInvalidTTL(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultTTL, "not-a-duration")
	t.Setenv(EnvVaultKey, validKey(t))

	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for invalid TTL")
	}
}

func TestLoadNonPositiveTTL(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultTTL, "0s")
	t.Setenv(EnvVaultKey, validKey(t))

	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for non-positive TTL")
	}
}

func TestLoadInvalidModelMode(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvModelMode, "bogus")
	t.Setenv(EnvVaultKey, validKey(t))

	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for invalid model mode")
	}
}

func TestLoadInvalidModelClientTimeout(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvModelClientTimeout, "not-a-duration")
	t.Setenv(EnvVaultKey, validKey(t))

	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for invalid model client timeout")
	}
}

func TestLoadNonPositiveModelClientTimeout(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvModelClientTimeout, "0s")
	t.Setenv(EnvVaultKey, validKey(t))

	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for non-positive model client timeout")
	}
}

func TestLoadNonHTTPWorkerURL(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvModelWorkerURL, "ftp://example.com")
	t.Setenv(EnvVaultKey, validKey(t))

	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for non-http worker URL")
	}
}

func TestLoadMalformedWorkerURL(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvModelWorkerURL, "not a url")
	t.Setenv(EnvVaultKey, validKey(t))

	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for malformed worker URL")
	}
}

func TestLoadMissingKey(t *testing.T) {
	resetEnv(t)

	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for missing vault key")
	}
}

func TestLoadMalformedKey(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, "!!!not-base64!!!")

	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for malformed vault key")
	}
}

func TestLoadWrongLengthKey(t *testing.T) {
	resetEnv(t)
	short := make([]byte, 16)
	t.Setenv(EnvVaultKey, base64.StdEncoding.EncodeToString(short))

	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for wrong-length vault key")
	}
}

func TestErrorDoesNotLeakSecret(t *testing.T) {
	resetEnv(t)
	raw := base64.StdEncoding.EncodeToString(make([]byte, 16))
	t.Setenv(EnvVaultKey, raw)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error")
	}
	if strings.Contains(err.Error(), raw) {
		t.Fatalf("error leaks secret value: %q", err.Error())
	}
}

func TestMalformedKeyErrorDoesNotLeakValue(t *testing.T) {
	resetEnv(t)
	raw := "!!!not-base64!!!"
	t.Setenv(EnvVaultKey, raw)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error")
	}
	if strings.Contains(err.Error(), raw) {
		t.Fatalf("error leaks raw key value: %q", err.Error())
	}
}
