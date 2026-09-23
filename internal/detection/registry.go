// Package detection defines the canonical PII type registry and the common
// candidate model shared by rules, model-label mapping, merge/ownership,
// masking and audit. It holds no plaintext values and makes no privacy
// decisions; it only names types and sources.
package detection

import (
	"fmt"
	"sort"
)

// Type is a canonical PII type name. It is a string-backed type so the
// registry stays extensible without a pipeline rewrite.
type Type string

// Canonical PII types. These exact spellings are the authoritative set from
// the Supported PII type registry requirement and are shared by detection
// results, token type suffixes and API metadata.
const (
	TypeFullName             Type = "FULL_NAME"
	TypeFirstName            Type = "FIRST_NAME"
	TypeLastName             Type = "LAST_NAME"
	TypeMiddleName           Type = "MIDDLE_NAME"
	TypeBirthDate            Type = "BIRTH_DATE"
	TypeBirthPlace           Type = "BIRTH_PLACE"
	TypePassportNumber       Type = "PASSPORT_NUMBER"
	TypeCitizenship          Type = "CITIZENSHIP"
	TypePassportIssuer       Type = "PASSPORT_ISSUER"
	TypePassportDivisionCode Type = "PASSPORT_DIVISION_CODE"
	TypePassportIssueDate    Type = "PASSPORT_ISSUE_DATE"
	TypeDriverLicenseNumber  Type = "DRIVER_LICENSE_NUMBER"
	TypeAddress              Type = "ADDRESS"
	TypeAddressCountry       Type = "ADDRESS_COUNTRY"
	TypeAddressPostalCode    Type = "ADDRESS_POSTAL_CODE"
	TypeAddressRegion        Type = "ADDRESS_REGION"
	TypeAddressCity          Type = "ADDRESS_CITY"
	TypeAddressStreet        Type = "ADDRESS_STREET"
	TypeAddressHouse         Type = "ADDRESS_HOUSE"
	TypeAddressBuilding      Type = "ADDRESS_BUILDING"
	TypeAddressApartment     Type = "ADDRESS_APARTMENT"
	TypeEmail                Type = "EMAIL"
	TypePhone                Type = "PHONE"
	TypeINNPerson            Type = "INN_PERSON"
	TypeBankCardNumber       Type = "BANK_CARD_NUMBER"
	TypeCardCVV              Type = "CARD_CVV"
	TypeCardPIN              Type = "CARD_PIN"
	TypeCardholderName       Type = "CARDHOLDER_NAME"
)

// Intermediate, non-canonical candidate types produced by model-label
// resolution and consumed by contextual classification. They are deliberately
// not part of the 28 canonical registry/defaultTypes.
const (
	TypeDate     Type = "DATE"
	TypeLocation Type = "LOCATION"
)

// defaultTypes is the authoritative set of the 28 canonical types.
var defaultTypes = []Type{
	TypeFullName,
	TypeFirstName,
	TypeLastName,
	TypeMiddleName,
	TypeBirthDate,
	TypeBirthPlace,
	TypePassportNumber,
	TypeCitizenship,
	TypePassportIssuer,
	TypePassportDivisionCode,
	TypePassportIssueDate,
	TypeDriverLicenseNumber,
	TypeAddress,
	TypeAddressCountry,
	TypeAddressPostalCode,
	TypeAddressRegion,
	TypeAddressCity,
	TypeAddressStreet,
	TypeAddressHouse,
	TypeAddressBuilding,
	TypeAddressApartment,
	TypeEmail,
	TypePhone,
	TypeINNPerson,
	TypeBankCardNumber,
	TypeCardCVV,
	TypeCardPIN,
	TypeCardholderName,
}

// Source is the provenance of a detection candidate.
type Source string

// Detection sources. These exact values are preserved through merge.
const (
	SourceRubert    Source = "rubert"
	SourceGliner    Source = "gliner"
	SourceRegex     Source = "regex"
	SourceValidator Source = "validator"
)

// Candidate is one detected span. Start and End are UTF-8 byte offsets into
// the original input, start inclusive and end exclusive. Confidence is in the
// range 0..1. Candidate carries no plaintext value, raw text, ownership,
// personal flag or reason codes; those are added by later pipeline stages.
type Candidate struct {
	Type       Type
	Start      int
	End        int
	Confidence float64
	Sources    []Source
}

// Registry is an immutable set of canonical PII types plus model-label
// resolution. It is safe for concurrent reads after construction.
type Registry struct {
	types map[Type]struct{}
}

// New returns a Registry preloaded with the 28 canonical types. extra may
// register additional non-empty types without changing pipeline code; an
// empty or duplicate extra type is rejected deterministically.
func New(extra ...Type) (*Registry, error) {
	types := make(map[Type]struct{}, len(defaultTypes)+len(extra))
	for _, t := range defaultTypes {
		types[t] = struct{}{}
	}
	for _, t := range extra {
		if t == "" {
			return nil, fmt.Errorf("detection: cannot register empty type")
		}
		if _, ok := types[t]; ok {
			return nil, fmt.Errorf("detection: duplicate type %q", t)
		}
		types[t] = struct{}{}
	}
	return &Registry{types: types}, nil
}

// Lookup reports whether name is a registered type. Matching is exact and
// case-sensitive.
func (r *Registry) Lookup(name Type) bool {
	_, ok := r.types[name]
	return ok
}

// Contains is an alias for Lookup.
func (r *Registry) Contains(name Type) bool {
	return r.Lookup(name)
}

// Types returns a sorted defensive copy of the registered type names.
// Mutating the returned slice does not affect the registry.
func (r *Registry) Types() []Type {
	out := make([]Type, 0, len(r.types))
	for t := range r.types {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ResolveModelLabel maps a model label to a Type for a specific model source.
// The contract is strictly source-specific: each source resolves only the
// labels it actually emits, and a canonical label from a source that does not
// emit it is rejected fail-closed.
//
// RuBERT resolves its real canonical labels (FIRST_NAME, LAST_NAME,
// MIDDLE_NAME, EMAIL, PHONE), its address labels (COUNTRY, REGION, DISTRICT,
// CITY, STREET, HOUSE) to canonical address component types, and leaves its
// structural labels (PASSPORT, INN, CREDIT_CARD, DRIVER_LICENSE) unresolved
// here because they require Go validation of the value and are confirmed by the
// model boundary instead.
//
// GLiNER resolves its ru_pii_* aliases: ru_pii_person, ru_pii_phone and
// ru_pii_email to their canonical types, and ru_pii_location and ru_pii_date to
// the intermediate LOCATION and DATE types consumed by contextual
// classification. The generic ru_pii label is rejected.
//
// Unknown labels, cross-source canonical labels, structural canonical aliases
// and unknown sources return ok=false.
func (r *Registry) ResolveModelLabel(model Source, label string) (Type, bool) {
	switch model {
	case SourceRubert:
		switch label {
		case "FIRST_NAME":
			return TypeFirstName, true
		case "LAST_NAME":
			return TypeLastName, true
		case "MIDDLE_NAME":
			return TypeMiddleName, true
		case "EMAIL":
			return TypeEmail, true
		case "PHONE":
			return TypePhone, true
		case "COUNTRY":
			return TypeAddressCountry, true
		case "REGION":
			return TypeAddressRegion, true
		case "DISTRICT":
			return TypeAddressRegion, true
		case "CITY":
			return TypeAddressCity, true
		case "STREET":
			return TypeAddressStreet, true
		case "HOUSE":
			return TypeAddressHouse, true
		}
	case SourceGliner:
		switch label {
		case "ru_pii_person":
			return TypeFullName, true
		case "ru_pii_phone":
			return TypePhone, true
		case "ru_pii_email":
			return TypeEmail, true
		case "ru_pii_location":
			return TypeLocation, true
		case "ru_pii_date":
			return TypeDate, true
		}
	}
	return "", false
}
