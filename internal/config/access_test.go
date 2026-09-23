package config

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// sha256Hex returns the lowercase hex SHA-256 digest of s.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func validConsumersJSON(t *testing.T) string {
	t.Helper()
	return `[
		{"system_id":"sys-a","enabled":true,"api_key_sha256":"` + sha256Hex("key-a") + `","enabled_types":["EMAIL"],"allow_demasking":true},
		{"system_id":"sys-b","enabled":true,"api_key_sha256":"` + sha256Hex("key-b") + `","enabled_types":["PHONE"],"allow_demasking":false}
	]`
}

func TestLoadCheckerProfileDoesNotRequireConsumers(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileChecker)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.AccessProfile != AccessProfileChecker {
		t.Errorf("AccessProfile = %q, want %q", cfg.AccessProfile, AccessProfileChecker)
	}
	if cfg.Consumers != nil {
		t.Error("Consumers must be nil for checker profile")
	}
}

func TestLoadCheckerProfileIgnoresConsumersJSON(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileChecker)
	t.Setenv(EnvConsumersJSON, validConsumersJSON(t))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Consumers != nil {
		t.Error("Consumers must be nil for checker profile even when JSON present")
	}
}

func TestLoadProductionProfileRequiresConsumers(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for production without consumers JSON")
	}
}

func TestLoadProductionProfileValidConsumers(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, validConsumersJSON(t))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.AccessProfile != AccessProfileProduction {
		t.Errorf("AccessProfile = %q, want %q", cfg.AccessProfile, AccessProfileProduction)
	}
	if cfg.Consumers == nil {
		t.Fatal("Consumers must be non-nil for production profile")
	}
	cons, ok := cfg.Consumers.LookupByHash(sha256Hex("key-a"))
	if !ok {
		t.Fatal("LookupByHash(key-a) not found")
	}
	if cons.SystemID != "sys-a" || !cons.Enabled || !cons.AllowDemasking {
		t.Errorf("sys-a consumer = %+v, want enabled demasking system", cons)
	}
	if len(cons.EnabledTypes) != 1 || cons.EnabledTypes[0] != "EMAIL" {
		t.Errorf("sys-a EnabledTypes = %v, want [EMAIL]", cons.EnabledTypes)
	}
}

func TestLoadMissingAccessProfile(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for missing access profile")
	}
}

func TestLoadInvalidAccessProfile(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, "bogus")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for invalid access profile")
	}
}

func TestLoadProductionNoEnabledSystem(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":"sys-a","enabled":false,"api_key_sha256":"`+sha256Hex("key-a")+`","enabled_types":["EMAIL"],"allow_demasking":true}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for production with no enabled system")
	}
}

func TestLoadConsumersMalformedJSON(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `not json`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for malformed consumers JSON")
	}
}

func TestLoadConsumersNotArray(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `{"system_id":"sys-a"}`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for non-array consumers JSON")
	}
}

func TestLoadConsumersUnknownField(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":"sys-a","enabled":true,"api_key_sha256":"`+sha256Hex("key-a")+`","enabled_types":["EMAIL"],"allow_demasking":true,"extra":"x"}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for unknown consumer field")
	}
}

func TestLoadConsumersInvalidType(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":"sys-a","enabled":"yes","api_key_sha256":"`+sha256Hex("key-a")+`","enabled_types":["EMAIL"],"allow_demasking":true}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for invalid consumer field type")
	}
}

func TestLoadConsumersEmptySystemID(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":"","enabled":true,"api_key_sha256":"`+sha256Hex("key-a")+`","enabled_types":["EMAIL"],"allow_demasking":true}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for empty system_id")
	}
}

func TestLoadConsumersDuplicateSystemID(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":"sys-a","enabled":true,"api_key_sha256":"`+sha256Hex("key-a")+`","enabled_types":["EMAIL"],"allow_demasking":true},
		{"system_id":"sys-a","enabled":true,"api_key_sha256":"`+sha256Hex("key-b")+`","enabled_types":["PHONE"],"allow_demasking":false}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for duplicate system_id")
	}
}

func TestLoadConsumersSystemIDLeadingWhitespace(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":" sys-a","enabled":true,"api_key_sha256":"`+sha256Hex("key-a")+`","enabled_types":["EMAIL"],"allow_demasking":true}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for system_id with leading whitespace")
	}
}

func TestLoadConsumersSystemIDTrailingWhitespace(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":"sys-a ","enabled":true,"api_key_sha256":"`+sha256Hex("key-a")+`","enabled_types":["EMAIL"],"allow_demasking":true}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for system_id with trailing whitespace")
	}
}

func TestLoadConsumersDuplicateHash(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":"sys-a","enabled":true,"api_key_sha256":"`+sha256Hex("key-a")+`","enabled_types":["EMAIL"],"allow_demasking":true},
		{"system_id":"sys-b","enabled":true,"api_key_sha256":"`+sha256Hex("key-a")+`","enabled_types":["PHONE"],"allow_demasking":false}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for duplicate api_key_sha256")
	}
}

func TestLoadConsumersMalformedHash(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":"sys-a","enabled":true,"api_key_sha256":"not-a-hash","enabled_types":["EMAIL"],"allow_demasking":true}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for malformed api_key_sha256")
	}
}

func TestLoadConsumersShortHash(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":"sys-a","enabled":true,"api_key_sha256":"`+sha256Hex("key-a")[:32]+`","enabled_types":["EMAIL"],"allow_demasking":true}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for short api_key_sha256")
	}
}

func TestLoadConsumersUppercaseHash(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":"sys-a","enabled":true,"api_key_sha256":"`+strings.ToUpper(sha256Hex("key-a"))+`","enabled_types":["EMAIL"],"allow_demasking":true}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for uppercase api_key_sha256")
	}
}

func TestLoadConsumersErrorDoesNotLeakJSON(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	raw := `[
		{"system_id":"sys-a","enabled":true,"api_key_sha256":"` + sha256Hex("key-a") + `","enabled_types":["EMAIL"],"allow_demasking":true,"extra":"x"}
	]`
	t.Setenv(EnvConsumersJSON, raw)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error")
	}
	if strings.Contains(err.Error(), raw) {
		t.Fatalf("error leaks consumers JSON: %q", err.Error())
	}
}

func TestLoadConsumersErrorDoesNotLeakHash(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	hash := sha256Hex("key-a")
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":"sys-a","enabled":true,"api_key_sha256":"`+hash+`","enabled_types":["EMAIL"],"allow_demasking":true,"extra":"x"}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error")
	}
	if strings.Contains(err.Error(), hash) {
		t.Fatalf("error leaks api key hash: %q", err.Error())
	}
}

func TestLoadConsumersErrorDoesNotLeakSystemID(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":"secret-system","enabled":true,"api_key_sha256":"`+sha256Hex("key-a")+`","enabled_types":["EMAIL"],"allow_demasking":true,"extra":"x"}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error")
	}
	if strings.Contains(err.Error(), "secret-system") {
		t.Fatalf("error leaks system_id: %q", err.Error())
	}
}

func TestLoadConsumersErrorDoesNotLeakAPIKey(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":"sys-a","enabled":true,"api_key_sha256":"`+sha256Hex("secret-key-value")+`","enabled_types":["EMAIL"],"allow_demasking":true,"extra":"x"}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error")
	}
	if strings.Contains(err.Error(), "secret-key-value") {
		t.Fatalf("error leaks API key: %q", err.Error())
	}
}

func TestConsumersLookupByHash(t *testing.T) {
	consumers, err := parseConsumers(validConsumersJSON(t))
	if err != nil {
		t.Fatalf("parseConsumers error = %v", err)
	}
	if _, ok := consumers.LookupByHash(sha256Hex("key-a")); !ok {
		t.Error("LookupByHash(key-a) not found")
	}
	if _, ok := consumers.LookupByHash(sha256Hex("key-b")); !ok {
		t.Error("LookupByHash(key-b) not found")
	}
	if _, ok := consumers.LookupByHash(sha256Hex("unknown")); ok {
		t.Error("LookupByHash(unknown) found, want not found")
	}
}

func TestConsumersLookupByHashDefensiveCopy(t *testing.T) {
	consumers, err := parseConsumers(validConsumersJSON(t))
	if err != nil {
		t.Fatalf("parseConsumers error = %v", err)
	}
	cons, ok := consumers.LookupByHash(sha256Hex("key-a"))
	if !ok {
		t.Fatal("LookupByHash(key-a) not found")
	}
	// Mutating the returned EnabledTypes must not affect later lookups.
	cons.EnabledTypes[0] = "MUTATED"
	cons2, ok := consumers.LookupByHash(sha256Hex("key-a"))
	if !ok {
		t.Fatal("LookupByHash(key-a) not found on second lookup")
	}
	if len(cons2.EnabledTypes) != 1 || cons2.EnabledTypes[0] != "EMAIL" {
		t.Errorf("second lookup EnabledTypes = %v, want [EMAIL] (must not alias internal slice)", cons2.EnabledTypes)
	}
}

func TestConsumersLookupDisabledSystem(t *testing.T) {
	raw := `[
		{"system_id":"sys-a","enabled":false,"api_key_sha256":"` + sha256Hex("key-a") + `","enabled_types":["EMAIL"],"allow_demasking":true},
		{"system_id":"sys-b","enabled":true,"api_key_sha256":"` + sha256Hex("key-b") + `","enabled_types":["PHONE"],"allow_demasking":false}
	]`
	consumers, err := parseConsumers(raw)
	if err != nil {
		t.Fatalf("parseConsumers error = %v", err)
	}
	cons, ok := consumers.LookupByHash(sha256Hex("key-a"))
	if !ok {
		t.Fatal("LookupByHash(key-a) not found")
	}
	if cons.Enabled {
		t.Error("sys-a must be disabled")
	}
}

func TestLoadConsumersEnvVarName(t *testing.T) {
	if EnvAccessProfile != "PII_ACCESS_PROFILE" {
		t.Errorf("EnvAccessProfile = %q, want PII_ACCESS_PROFILE", EnvAccessProfile)
	}
	if EnvConsumersJSON != "PII_CONSUMERS_JSON" {
		t.Errorf("EnvConsumersJSON = %q, want PII_CONSUMERS_JSON", EnvConsumersJSON)
	}
}

func TestLoadConsumersEmptyArray(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for empty consumers array")
	}
}

func TestLoadConsumersTrailingGarbage(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[{"system_id":"sys-a","enabled":true,"api_key_sha256":"`+sha256Hex("key-a")+`","enabled_types":["EMAIL"],"allow_demasking":true}] extra`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for trailing garbage after JSON")
	}
}

func TestLoadConsumersSecondJSONValue(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[{"system_id":"sys-a","enabled":true,"api_key_sha256":"`+sha256Hex("key-a")+`","enabled_types":["EMAIL"],"allow_demasking":true}] {}`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for a second JSON value after the array")
	}
}

func TestLoadConsumersDuplicateField(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":"sys-a","system_id":"sys-b","enabled":true,"api_key_sha256":"`+sha256Hex("key-a")+`","enabled_types":["EMAIL"],"allow_demasking":true}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for duplicate JSON field")
	}
}

func TestLoadConsumersMissingRequiredField(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":"sys-a","enabled":true,"enabled_types":["EMAIL"],"allow_demasking":true}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for missing api_key_sha256")
	}
}

func TestLoadConsumersEnabledTypesWrongShape(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":"sys-a","enabled":true,"api_key_sha256":"`+sha256Hex("key-a")+`","enabled_types":"EMAIL","allow_demasking":true}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for enabled_types wrong shape")
	}
}

func TestLoadConsumersAllowDemaskingWrongType(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":"sys-a","enabled":true,"api_key_sha256":"`+sha256Hex("key-a")+`","enabled_types":["EMAIL"],"allow_demasking":"yes"}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for allow_demasking wrong type")
	}
}

func TestLoadConsumersEnabledWrongType(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":"sys-a","enabled":1,"api_key_sha256":"`+sha256Hex("key-a")+`","enabled_types":["EMAIL"],"allow_demasking":true}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for enabled wrong type")
	}
}

func TestLoadConsumersSystemIDWrongType(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":123,"enabled":true,"api_key_sha256":"`+sha256Hex("key-a")+`","enabled_types":["EMAIL"],"allow_demasking":true}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for system_id wrong type")
	}
}

func TestLoadConsumersHashWrongType(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":"sys-a","enabled":true,"api_key_sha256":123,"enabled_types":["EMAIL"],"allow_demasking":true}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for api_key_sha256 wrong type")
	}
}

func TestLoadConsumersEnabledTypesWrongElementType(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":"sys-a","enabled":true,"api_key_sha256":"`+sha256Hex("key-a")+`","enabled_types":[123],"allow_demasking":true}
	]`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected error for enabled_types wrong element type")
	}
}

func TestLoadConsumersMixedEnabledDisabled(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvVaultKey, validKey(t))
	t.Setenv(EnvAccessProfile, AccessProfileProduction)
	t.Setenv(EnvConsumersJSON, `[
		{"system_id":"sys-a","enabled":false,"api_key_sha256":"`+sha256Hex("key-a")+`","enabled_types":["EMAIL"],"allow_demasking":true},
		{"system_id":"sys-b","enabled":true,"api_key_sha256":"`+sha256Hex("key-b")+`","enabled_types":["PHONE"],"allow_demasking":false}
	]`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Consumers == nil {
		t.Fatal("Consumers must be non-nil")
	}
	if _, ok := cfg.Consumers.LookupByHash(sha256Hex("key-a")); !ok {
		t.Error("disabled sys-a must still be registered for lookup")
	}
	if _, ok := cfg.Consumers.LookupByHash(sha256Hex("key-b")); !ok {
		t.Error("enabled sys-b must be registered for lookup")
	}
}
