// Package testcorpus defines the local synthetic corpus schema and provides
// loading and validation. It contains no scorer or harness; it only proves
// that fixtures load and validate.
package testcorpus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// OffsetUnit is the fixed offset unit for all spans.
const OffsetUnit = "utf8_byte"

// Corpus is the top-level synthetic corpus document.
type Corpus struct {
	Version    string `json:"version"`
	OffsetUnit string `json:"offset_unit"`
	Cases      []Case `json:"cases"`
}

// Case is a single synthetic input with its expected mask and spans.
type Case struct {
	ID           string `json:"id"`
	Input        string `json:"input"`
	ExpectedMask string `json:"expected_mask"`
	Spans        []Span `json:"spans"`
}

// Span describes one detected entity. Offsets are UTF-8 byte offsets with
// start inclusive and end exclusive. MaskToken is present only when Personal
// is true.
type Span struct {
	Type      string `json:"type"`
	Start     int    `json:"start"`
	End       int    `json:"end"`
	Value     string `json:"value"`
	Personal  bool   `json:"personal"`
	MaskToken string `json:"mask_token,omitempty"`
}

// Load reads and parses a corpus file, enforcing the sibling JSON Schema
// (pii-corpus.schema.json) against the raw document before unmarshalling.
func Load(path string) (Corpus, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Corpus{}, err
	}
	if err := validateSchema(filepath.Join(filepath.Dir(path), "pii-corpus.schema.json"), data); err != nil {
		return Corpus{}, err
	}
	var c Corpus
	if err := json.Unmarshal(data, &c); err != nil {
		return Corpus{}, err
	}
	return c, nil
}

// validateSchema compiles the JSON Schema at schemaPath and validates the raw
// corpus document against it.
func validateSchema(schemaPath string, data []byte) error {
	schemaBytes, err := os.ReadFile(schemaPath)
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	schemaDoc, err := decodeJSON(schemaBytes)
	if err != nil {
		return fmt.Errorf("decode schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("pii-corpus.schema.json", schemaDoc); err != nil {
		return fmt.Errorf("compile schema: %w", err)
	}
	schema, err := compiler.Compile("pii-corpus.schema.json")
	if err != nil {
		return fmt.Errorf("compile schema: %w", err)
	}
	doc, err := decodeJSON(data)
	if err != nil {
		return fmt.Errorf("decode corpus: %w", err)
	}
	if err := schema.Validate(doc); err != nil {
		return fmt.Errorf("schema validation failed: %w", err)
	}
	return nil
}

// decodeJSON decodes raw JSON into a generic value, preserving number types so
// that integer constraints in the schema are evaluated correctly.
func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// Validate checks the structural and offset consistency of a corpus.
func Validate(c Corpus) error {
	if c.Version != "1" {
		return fmt.Errorf("unsupported version %q", c.Version)
	}
	if c.OffsetUnit != OffsetUnit {
		return fmt.Errorf("unsupported offset_unit %q, want %q", c.OffsetUnit, OffsetUnit)
	}

	seen := make(map[string]bool, len(c.Cases))
	for _, tc := range c.Cases {
		if tc.ID == "" {
			return fmt.Errorf("case with empty id")
		}
		if seen[tc.ID] {
			return fmt.Errorf("duplicate case id %q", tc.ID)
		}
		seen[tc.ID] = true

		if err := validateCase(tc); err != nil {
			return fmt.Errorf("case %q: %w", tc.ID, err)
		}
	}
	return nil
}

func validateCase(tc Case) error {
	for i := range tc.Spans {
		s := &tc.Spans[i]
		if s.Type == "" {
			return fmt.Errorf("span %d: empty type", i)
		}
		if s.Start < 0 || s.End < s.Start || s.End > len(tc.Input) {
			return fmt.Errorf("span %d: out of bounds [%d,%d) for input length %d", i, s.Start, s.End, len(tc.Input))
		}
		if got := tc.Input[s.Start:s.End]; got != s.Value {
			return fmt.Errorf("span %d: input[%d:%d] = %q, want %q", i, s.Start, s.End, got, s.Value)
		}
		if s.Personal {
			if s.MaskToken == "" {
				return fmt.Errorf("span %d: personal span missing mask_token", i)
			}
			if !strings.Contains(tc.ExpectedMask, s.MaskToken) {
				return fmt.Errorf("span %d: expected_mask missing mask_token %q", i, s.MaskToken)
			}
		} else if s.MaskToken != "" {
			return fmt.Errorf("span %d: non-personal span must not have mask_token", i)
		}
	}

	for i := 0; i < len(tc.Spans); i++ {
		for j := i + 1; j < len(tc.Spans); j++ {
			a, b := tc.Spans[i], tc.Spans[j]
			if a.Start < b.End && b.Start < a.End {
				return fmt.Errorf("spans %d and %d overlap", i, j)
			}
		}
	}
	return nil
}
