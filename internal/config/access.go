package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Access-control environment variable names.
const (
	EnvAccessProfile = EnvPrefix + "ACCESS_PROFILE"
	EnvConsumersJSON = EnvPrefix + "CONSUMERS_JSON"
)

// Access profiles.
const (
	AccessProfileChecker    = "checker"
	AccessProfileProduction = "production"
)

// Consumer is a single registered system in the production access profile.
// It carries only the SHA-256 hash of the API key, never the key itself.
type Consumer struct {
	SystemID       string
	Enabled        bool
	APIKeySHA256   string
	EnabledTypes   []string
	AllowDemasking bool
}

// consumerJSON is the strict wire shape of a consumer object. Unknown fields,
// duplicate fields, invalid types and invalid shapes are rejected at parse
// time via DisallowUnknownFields, duplicate-key detection and strict type
// decoding.
type consumerJSON struct {
	SystemID       string   `json:"system_id"`
	Enabled        bool     `json:"enabled"`
	APIKeySHA256   string   `json:"api_key_sha256"`
	EnabledTypes   []string `json:"enabled_types"`
	AllowDemasking bool     `json:"allow_demasking"`
}

// UnmarshalJSON rejects duplicate keys within a consumer object before
// decoding it strictly. Go's encoding/json silently keeps the last value for a
// repeated key, which would hide a misconfiguration; we fail instead.
func (c *consumerJSON) UnmarshalJSON(data []byte) error {
	if err := rejectDuplicateKeys(data); err != nil {
		return err
	}
	type alias consumerJSON
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode((*alias)(c)); err != nil {
		return err
	}
	return nil
}

// Consumers holds the parsed production consumer list.
type Consumers struct {
	byHash map[string]Consumer
}

// LookupByHash returns a defensive copy of the consumer whose API key SHA-256
// equals hash, and whether it was found. The returned EnabledTypes slice never
// aliases the internal storage, so caller mutation cannot affect later lookups.
// It never exposes the key or any other secret.
func (c *Consumers) LookupByHash(hash string) (Consumer, bool) {
	cons, ok := c.byHash[hash]
	if !ok {
		return Consumer{}, false
	}
	cons.EnabledTypes = append([]string(nil), cons.EnabledTypes...)
	return cons, true
}

// ConsumerDescriptor is a defensive view of a registered consumer that never
// exposes the API key hash. It is used to enumerate consumers for policy
// resolution without disclosing any secret material.
type ConsumerDescriptor struct {
	SystemID       string
	Enabled        bool
	EnabledTypes   []string
	AllowDemasking bool
}

// All returns a defensive copy of every registered consumer, ordered
// deterministically by system ID. It never exposes API key hashes.
func (c *Consumers) All() []ConsumerDescriptor {
	out := make([]ConsumerDescriptor, 0, len(c.byHash))
	for _, cons := range c.byHash {
		out = append(out, ConsumerDescriptor{
			SystemID:       cons.SystemID,
			Enabled:        cons.Enabled,
			EnabledTypes:   append([]string(nil), cons.EnabledTypes...),
			AllowDemasking: cons.AllowDemasking,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SystemID < out[j].SystemID })
	return out
}

// parseConsumers parses and validates the strict PII_CONSUMERS_JSON value.
// It rejects unknown fields, duplicate fields, invalid types/shape, empty or
// duplicate system IDs, duplicate or malformed hashes, and a production list
// with no enabled system. Errors never reflect the JSON, a hash, an API key or
// a system ID.
func parseConsumers(raw string) (*Consumers, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()

	var rawList []consumerJSON
	if err := dec.Decode(&rawList); err != nil {
		return nil, fmt.Errorf("%s: invalid consumers JSON", EnvConsumersJSON)
	}
	// Require the input to end exactly after the array. A second JSON value or
	// any trailing non-whitespace garbage must be rejected, not silently
	// accepted.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("%s: invalid consumers JSON", EnvConsumersJSON)
	}

	byHash := make(map[string]Consumer, len(rawList))
	seenIDs := make(map[string]struct{}, len(rawList))
	seenHashes := make(map[string]struct{}, len(rawList))
	enabledCount := 0

	for _, c := range rawList {
		if c.SystemID == "" || c.SystemID != strings.TrimSpace(c.SystemID) {
			return nil, fmt.Errorf("%s: invalid consumer", EnvConsumersJSON)
		}
		if _, dup := seenIDs[c.SystemID]; dup {
			return nil, fmt.Errorf("%s: invalid consumer", EnvConsumersJSON)
		}
		seenIDs[c.SystemID] = struct{}{}

		if !validSHA256Hex(c.APIKeySHA256) {
			return nil, fmt.Errorf("%s: invalid consumer", EnvConsumersJSON)
		}
		if _, dup := seenHashes[c.APIKeySHA256]; dup {
			return nil, fmt.Errorf("%s: invalid consumer", EnvConsumersJSON)
		}
		seenHashes[c.APIKeySHA256] = struct{}{}

		if c.Enabled {
			enabledCount++
		}
		byHash[c.APIKeySHA256] = Consumer{
			SystemID:       c.SystemID,
			Enabled:        c.Enabled,
			APIKeySHA256:   c.APIKeySHA256,
			EnabledTypes:   append([]string(nil), c.EnabledTypes...),
			AllowDemasking: c.AllowDemasking,
		}
	}

	if enabledCount == 0 {
		return nil, fmt.Errorf("%s: requires at least one enabled system", EnvConsumersJSON)
	}
	return &Consumers{byHash: byHash}, nil
}

// validSHA256Hex reports whether s is exactly 64 lowercase hex characters.
func validSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// rejectDuplicateKeys walks a JSON object and returns an error if any key
// appears more than once. Nested objects and arrays are skipped recursively.
func rejectDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	return walkObject(dec)
}

func walkObject(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return errors.New("expected object")
	}
	seen := make(map[string]bool)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := keyTok.(string)
		if !ok {
			return errors.New("expected string key")
		}
		if seen[key] {
			return errors.New("duplicate key")
		}
		seen[key] = true
		if err := skipValue(dec); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	return nil
}

func skipValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		return walkObject(dec)
	case '[':
		for dec.More() {
			if err := skipValue(dec); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
		return nil
	}
	return nil
}
