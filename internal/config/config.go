// Package config loads service configuration from the environment.
package config

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Environment variable names. Exported so tests and future wiring do not
// duplicate the strings.
const (
	EnvPrefix               = "PII_"
	EnvAPIListenAddress     = EnvPrefix + "API_LISTEN_ADDRESS"
	EnvMetricsListenAddress = EnvPrefix + "METRICS_LISTEN_ADDRESS"
	EnvModelWorkerURL       = EnvPrefix + "MODEL_WORKER_URL"
	EnvVaultTTL             = EnvPrefix + "VAULT_TTL"
	EnvModelMode            = EnvPrefix + "MODEL_MODE"
	EnvVaultKey             = EnvPrefix + "VAULT_KEY"
	EnvModelClientTimeout   = EnvPrefix + "MODEL_CLIENT_TIMEOUT"
	EnvLLMURL               = EnvPrefix + "LLM_URL"
	EnvLLMModel             = EnvPrefix + "LLM_MODEL"
	EnvLLMAPIKey            = EnvPrefix + "LLM_API_KEY"
	EnvLLMTimeout           = EnvPrefix + "LLM_TIMEOUT"
	EnvLogLevel             = EnvPrefix + "LOG_LEVEL"
)

// Defaults.
const (
	DefaultAPIListenAddress     = "127.0.0.1:8080"
	DefaultMetricsListenAddress = "127.0.0.1:9464"
	DefaultModelWorkerURL       = "http://127.0.0.1:8000"
	DefaultVaultTTL             = 15 * time.Minute
	DefaultModelMode            = ModelModeFull
	DefaultModelClientTimeout   = 30 * time.Second
	DefaultLLMTimeout           = 60 * time.Second
	DefaultLogLevel             = LogLevelInfo
)

// Model modes.
const (
	ModelModeFull = "full"
	ModelModeFast = "fast"
	LogLevelInfo  = "info"
	LogLevelDebug = "debug"
)

// Config holds the resolved service configuration.
type Config struct {
	APIListenAddress string
	// MetricsListenAddress is the separate internal listener for GET /metrics.
	// It is not behind admission or audit and must be reachable only by the metrics
	// collector.
	MetricsListenAddress string
	ModelWorkerURL       string
	VaultTTL             time.Duration
	ModelMode            string
	VaultKey             VaultKey
	ModelClientTimeout   time.Duration
	LLM                  LLMConfig
	LogLevel             string
}

// LLMConfig holds the optional downstream LLM configuration. It is optional as
// a complete group: when both URL and model are absent the runtime route stays
// fail-closed 503. Any partial configuration is a startup validation error.
type LLMConfig struct {
	URL     string
	Model   string
	APIKey  string
	Timeout time.Duration
}

// Enabled reports whether the LLM group is fully configured. It is true only
// when both URL and model are present.
func (l LLMConfig) Enabled() bool {
	return l.URL != "" && l.Model != ""
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
		APIListenAddress:     DefaultAPIListenAddress,
		MetricsListenAddress: DefaultMetricsListenAddress,
		ModelWorkerURL:       DefaultModelWorkerURL,
		VaultTTL:             DefaultVaultTTL,
		ModelMode:            DefaultModelMode,
		ModelClientTimeout:   DefaultModelClientTimeout,
		LogLevel:             DefaultLogLevel,
		LLM: LLMConfig{
			Timeout: DefaultLLMTimeout,
		},
	}

	if v, ok := os.LookupEnv(EnvAPIListenAddress); ok {
		cfg.APIListenAddress = v
	}
	if v, ok := os.LookupEnv(EnvMetricsListenAddress); ok {
		cfg.MetricsListenAddress = v
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

	if v, ok := os.LookupEnv(EnvLLMURL); ok {
		cfg.LLM.URL = v
	}
	if v, ok := os.LookupEnv(EnvLLMModel); ok {
		cfg.LLM.Model = v
	}
	if v, ok := os.LookupEnv(EnvLLMAPIKey); ok {
		cfg.LLM.APIKey = v
	}
	if v, ok := os.LookupEnv(EnvLLMTimeout); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("%s: invalid duration: %w", EnvLLMTimeout, err)
		}
		cfg.LLM.Timeout = d
	}
	if v, ok := os.LookupEnv(EnvLogLevel); ok {
		cfg.LogLevel = strings.ToLower(v)
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
	if c.MetricsListenAddress == "" {
		return fmt.Errorf("%s: must not be empty", EnvMetricsListenAddress)
	}
	if c.MetricsListenAddress == c.APIListenAddress {
		return fmt.Errorf("%s: must differ from %s", EnvMetricsListenAddress, EnvAPIListenAddress)
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
	if err := c.validateLLM(); err != nil {
		return err
	}
	if c.LogLevel != LogLevelInfo && c.LogLevel != LogLevelDebug {
		return fmt.Errorf("%s: must be %q or %q", EnvLogLevel, LogLevelInfo, LogLevelDebug)
	}
	return nil
}

// validateLLM validates the optional downstream LLM group. The group is
// optional as a whole: when both URL and model are absent it is disabled and
// the runtime route stays fail-closed 503. Any partial configuration is a
// startup validation error, including an API key set without the URL+model
// group.
func (c Config) validateLLM() error {
	urlSet := c.LLM.URL != ""
	modelSet := c.LLM.Model != ""
	if urlSet != modelSet {
		return fmt.Errorf("%s and %s must be set together", EnvLLMURL, EnvLLMModel)
	}
	if !urlSet {
		if c.LLM.APIKey != "" {
			return fmt.Errorf("%s requires %s and %s", EnvLLMAPIKey, EnvLLMURL, EnvLLMModel)
		}
		return nil
	}
	if err := validateLLMURL(c.LLM.URL); err != nil {
		return err
	}
	if c.LLM.Timeout <= 0 {
		return fmt.Errorf("%s: must be greater than zero", EnvLLMTimeout)
	}
	return nil
}

func validateLLMURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: invalid URL", EnvLLMURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s: scheme must be http or https", EnvLLMURL)
	}
	if u.Host == "" {
		return fmt.Errorf("%s: host is required", EnvLLMURL)
	}
	if u.User != nil {
		return fmt.Errorf("%s: userinfo is not allowed", EnvLLMURL)
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
