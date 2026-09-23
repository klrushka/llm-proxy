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
		EnvMetricsListenAddress,
		EnvModelWorkerURL,
		EnvVaultTTL,
		EnvModelMode,
		EnvVaultKey,
		EnvModelClientTimeout,
		EnvLLMURL,
		EnvLLMModel,
		EnvLLMAPIKey,
		EnvLLMTimeout,
		EnvLogLevel,
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
	if cfg.MetricsListenAddress != DefaultMetricsListenAddress {
		t.Errorf("MetricsListenAddress = %q, want %q", cfg.MetricsListenAddress, DefaultMetricsListenAddress)
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
	if cfg.LogLevel != DefaultLogLevel {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, DefaultLogLevel)
	}

	want := make([]byte, 32)
	for i := range want {
		want[i] = byte(i + 1)
	}
	if got := cfg.VaultKey.Bytes(); got != [32]byte(want) {
		t.Errorf("VaultKey bytes mismatch")
	}
}

func TestLoadLogLevel(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvLogLevel, "DEBUG")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.LogLevel != LogLevelDebug {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, LogLevelDebug)
	}
}

func TestLoadRejectsInvalidLogLevel(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvLogLevel, "trace-with-secret")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), EnvLogLevel) {
		t.Fatalf("Load() error = %v, want %s error", err, EnvLogLevel)
	}
	if strings.Contains(err.Error(), "trace-with-secret") {
		t.Fatalf("Load() error leaks value: %q", err)
	}
}

func TestLoadOverrides(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvAPIListenAddress, "0.0.0.0:9090")
	t.Setenv(EnvMetricsListenAddress, "0.0.0.0:9464")
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
	if cfg.MetricsListenAddress != "0.0.0.0:9464" {
		t.Errorf("MetricsListenAddress = %q", cfg.MetricsListenAddress)
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

func TestLoadLLMDisabledByDefault(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.LLM.Enabled() {
		t.Error("LLM enabled by default, want disabled")
	}
	if cfg.LLM.Timeout != DefaultLLMTimeout {
		t.Errorf("LLM.Timeout = %v, want %v", cfg.LLM.Timeout, DefaultLLMTimeout)
	}
}

func TestLoadLLMCompleteGroup(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvLLMURL, "https://llm.example.com/v1/chat/completions")
	t.Setenv(EnvLLMModel, "test-model")
	t.Setenv(EnvLLMAPIKey, "secret-key")
	t.Setenv(EnvLLMTimeout, "45s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.LLM.Enabled() {
		t.Error("LLM not enabled, want enabled")
	}
	if cfg.LLM.URL != "https://llm.example.com/v1/chat/completions" {
		t.Errorf("LLM.URL = %q", cfg.LLM.URL)
	}
	if cfg.LLM.Model != "test-model" {
		t.Errorf("LLM.Model = %q", cfg.LLM.Model)
	}
	if cfg.LLM.APIKey != "secret-key" {
		t.Errorf("LLM.APIKey = %q", cfg.LLM.APIKey)
	}
	if cfg.LLM.Timeout != 45*time.Second {
		t.Errorf("LLM.Timeout = %v", cfg.LLM.Timeout)
	}
}

func TestLoadLLMPartialGroupRejected(t *testing.T) {
	cases := []struct {
		name string
		url  string
		mod  string
	}{
		{"url only", "https://llm.example.com/v1/chat/completions", ""},
		{"model only", "", "test-model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetEnv(t)
			t.Setenv(EnvVaultKey, validKey(t))
			if tc.url != "" {
				t.Setenv(EnvLLMURL, tc.url)
			}
			if tc.mod != "" {
				t.Setenv(EnvLLMModel, tc.mod)
			}
			if _, err := Load(); err == nil {
				t.Fatal("Load() expected error for partial LLM group")
			}
		})
	}
}

// TestLoadLLMAPIKeyWithoutGroupRejected proves that a non-empty API key with
// both URL and model absent is rejected as partial configuration instead of
// being silently ignored, and that the error does not echo the key value.
func TestLoadLLMAPIKeyWithoutGroupRejected(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvLLMAPIKey, "secret-key")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for API key without URL+model group")
	}
	if strings.Contains(err.Error(), "secret-key") {
		t.Fatalf("error echoes API key value: %q", err.Error())
	}
}

func TestLoadLLMNonHTTPURL(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvLLMURL, "ftp://example.com")
	t.Setenv(EnvLLMModel, "test-model")

	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for non-http LLM URL")
	}
}

func TestLoadLLMMalformedURL(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvLLMURL, "not a url")
	t.Setenv(EnvLLMModel, "test-model")

	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for malformed LLM URL")
	}
}

func TestLoadLLMUserinfoURL(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvLLMURL, "https://user:secret@llm.example.com/v1/chat/completions")
	t.Setenv(EnvLLMModel, "test-model")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for LLM URL with userinfo")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error echoes credentials: %q", err.Error())
	}
}

func TestLoadLLMInvalidTimeout(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvLLMURL, "https://llm.example.com/v1/chat/completions")
	t.Setenv(EnvLLMModel, "test-model")
	t.Setenv(EnvLLMTimeout, "not-a-duration")

	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for invalid LLM timeout")
	}
}

func TestLoadLLMNonPositiveTimeout(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvLLMURL, "https://llm.example.com/v1/chat/completions")
	t.Setenv(EnvLLMModel, "test-model")
	t.Setenv(EnvLLMTimeout, "0s")

	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for non-positive LLM timeout")
	}
}

func TestLoadRejectsInvalidMetricsListenAddress(t *testing.T) {
	cases := map[string]string{
		"empty":       "",
		"same as API": DefaultAPIListenAddress,
	}
	for name, addr := range cases {
		t.Run(name, func(t *testing.T) {
			resetEnv(t)
			t.Setenv(EnvVaultKey, validKey(t))
			t.Setenv(EnvMetricsListenAddress, addr)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), EnvMetricsListenAddress) {
				t.Fatalf("Load() error = %v, want %s error", err, EnvMetricsListenAddress)
			}
		})
	}
}
