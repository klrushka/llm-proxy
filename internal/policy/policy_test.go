package policy

import (
	"reflect"
	"testing"
)

func TestDefaultConsumerID(t *testing.T) {
	if DefaultConsumerID == "" {
		t.Fatal("DefaultConsumerID must not be empty")
	}
}

func TestNewPolicyConsumerID(t *testing.T) {
	p := NewPolicy(DefaultConsumerID, nil)
	if p.ConsumerID() != DefaultConsumerID {
		t.Errorf("ConsumerID() = %q, want %q", p.ConsumerID(), DefaultConsumerID)
	}
}

func TestAllowsTypeEnabled(t *testing.T) {
	p := NewPolicy("consumer-a", []string{"EMAIL", "PHONE"})
	if !p.AllowsType("EMAIL") {
		t.Error("AllowsType(EMAIL) = false, want true")
	}
	if !p.AllowsType("PHONE") {
		t.Error("AllowsType(PHONE) = false, want true")
	}
}

func TestAllowsTypeDisabled(t *testing.T) {
	p := NewPolicy("consumer-a", []string{"EMAIL"})
	if p.AllowsType("PHONE") {
		t.Error("AllowsType(PHONE) = true, want false")
	}
	if p.AllowsType("") {
		t.Error("AllowsType(\"\") = true, want false")
	}
}

func TestAllowsTypeEmptyPolicy(t *testing.T) {
	p := NewPolicy("consumer-a", nil)
	if p.AllowsType("EMAIL") {
		t.Error("AllowsType(EMAIL) = true for empty policy, want false")
	}
}

func TestCapabilitiesDefaultFalse(t *testing.T) {
	p := NewPolicy(DefaultConsumerID, nil)
	if p.AllowDemasking {
		t.Error("AllowDemasking = true by default, want false")
	}
	if p.AllowRulesOnlyDegraded {
		t.Error("AllowRulesOnlyDegraded = true by default, want false")
	}
}

func TestCapabilitiesReflectExplicitPolicy(t *testing.T) {
	p := NewPolicy(DefaultConsumerID, nil)
	p.AllowDemasking = true
	p.AllowRulesOnlyDegraded = true
	if !p.AllowDemasking {
		t.Error("AllowDemasking = false, want true")
	}
	if !p.AllowRulesOnlyDegraded {
		t.Error("AllowRulesOnlyDegraded = false, want true")
	}
}

func TestPolicyCopiesInputTypes(t *testing.T) {
	allowed := []string{"EMAIL"}
	p := NewPolicy("consumer-a", allowed)
	allowed[0] = "PHONE"
	if !p.AllowsType("EMAIL") {
		t.Error("policy changed after input slice mutation")
	}
	if p.AllowsType("PHONE") {
		t.Error("policy gained type after input slice mutation")
	}
}

func TestTypesReturnsCopy(t *testing.T) {
	p := NewPolicy("consumer-a", []string{"EMAIL", "PHONE"})
	got := p.Types()
	got[0] = "MUTATED"
	if !p.AllowsType("EMAIL") {
		t.Error("policy changed after mutating Types() result")
	}
}

func TestTypesReflectsPolicy(t *testing.T) {
	p := NewPolicy("consumer-a", []string{"EMAIL", "PHONE"})
	got := p.Types()
	want := []string{"EMAIL", "PHONE"}
	if !reflect.DeepEqual(sortStrings(got), sortStrings(want)) {
		t.Errorf("Types() = %v, want %v", got, want)
	}
}

func sortStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
