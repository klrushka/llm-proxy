// Package config loads service configuration from the environment.
package config

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"time"
)

// Environment variable names. Exported so tests and future wiring do not
// duplicate the strings.
const (
	EnvPrefix             = "PII_"
	EnvAPIListenAddress   = EnvPrefix + "API_LISTEN_ADDRESS"
	EnvModelWorkerURL     = EnvPrefix + "MODEL_WORKER_URL"
	EnvVaultTTL           = EnvPrefix + "VAULT_TTL"
	EnvModelMode          = EnvPrefix + "MODEL_MODE"
	EnvVaultKey           = EnvPrefix + "VAULT_KEY"
	EnvModelClientTimeout = EnvPrefix + "MODEL_CLIENT_TIMEOUT"
)

// Defaults.
const (
	DefaultAPIListenAddress   = "127.0.0.1:8080"
	DefaultModelWorkerURL     = "http://127.0.0.1:8000"
	DefaultVaultTTL           = 15 * time.Minute
	DefaultModelMode          = ModelModeFull
	DefaultModelClientTimeout = 30 * time.Second
)

// Model modes.
const (
	ModelModeFull = "full"
	ModelModeFast = "fast"
)

// Config holds the resolved service configuration.
type Config struct {
	APIListenAddress   string
	ModelWorkerURL     string
	VaultTTL           time.Duration
	ModelMode          string
	VaultKey           VaultKey
	ModelClientTimeout time.Duration
}

// VaultKey holds the decoded vault encryption key. Its bytes are never
// included in validation errors.
type VaultKey struct {
	bytes [32]byte
}

// Bytes returns a copy of the key bytes.
func (k VaultKey) Bytes() [32]byte {
	return k.bytes
}

// Load reads configuration from the environment and validates it.
func Load() (Config, error) {
	cfg := Config{
		APIListenAddress:   DefaultAPIListenAddress,
		ModelWorkerURL:     DefaultModelWorkerURL,
		VaultTTL:           DefaultVaultTTL,
		ModelMode:          DefaultModelMode,
		ModelClientTimeout: DefaultModelClientTimeout,
	}

	if v, ok := os.LookupEnv(EnvAPIListenAddress); ok {
		cfg.APIListenAddress = v
	}
	if v, ok := os.LookupEnv(EnvModelWorkerURL); ok {
		cfg.ModelWorkerURL = v
	}
	if v, ok := os.LookupEnv(EnvVaultTTL); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("%s: invalid duration: %w", EnvVaultTTL, err)
		}
		cfg.VaultTTL = d
	}
	if v, ok := os.LookupEnv(EnvModelMode); ok {
		cfg.ModelMode = v
	}
	if v, ok := os.LookupEnv(EnvModelClientTimeout); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("%s: invalid duration: %w", EnvModelClientTimeout, err)
		}
		cfg.ModelClientTimeout = d
	}

	rawKey, ok := os.LookupEnv(EnvVaultKey)
	if !ok {
		return Config{}, fmt.Errorf("%s: required", EnvVaultKey)
	}
	key, err := parseVaultKey(rawKey)
	if err != nil {
		return Config{}, err
	}
	cfg.VaultKey = key

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func parseVaultKey(raw string) (VaultKey, error) {
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return VaultKey{}, fmt.Errorf("%s: invalid base64", EnvVaultKey)
	}
	if len(decoded) != 32 {
		return VaultKey{}, fmt.Errorf("%s: decoded key must be 32 bytes, got %d", EnvVaultKey, len(decoded))
	}
	var key VaultKey
	copy(key.bytes[:], decoded)
	return key, nil
}

func (c Config) validate() error {
	if c.APIListenAddress == "" {
		return fmt.Errorf("%s: must not be empty", EnvAPIListenAddress)
	}
	if err := validateWorkerURL(c.ModelWorkerURL); err != nil {
		return err
	}
	if c.VaultTTL <= 0 {
		return fmt.Errorf("%s: must be greater than zero", EnvVaultTTL)
	}
	if c.ModelMode != ModelModeFull && c.ModelMode != ModelModeFast {
		return fmt.Errorf("%s: must be %q or %q", EnvModelMode, ModelModeFull, ModelModeFast)
	}
	if c.ModelClientTimeout <= 0 {
		return fmt.Errorf("%s: must be greater than zero", EnvModelClientTimeout)
	}
	return nil
}

func validateWorkerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: invalid URL: %w", EnvModelWorkerURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s: scheme must be http or https", EnvModelWorkerURL)
	}
	if u.Host == "" {
		return fmt.Errorf("%s: host is required", EnvModelWorkerURL)
	}
	return nil
}
