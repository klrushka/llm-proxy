package policy

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestResolveEmptyIdentityReturnsDefault(t *testing.T) {
	def := NewPolicy(DefaultConsumerID, []string{"EMAIL"})
	r := NewResolver(def, nil)

	p, err := r.Resolve("")
	if err != nil {
		t.Fatalf("Resolve(\"\") error = %v, want nil", err)
	}
	if p.ConsumerID() != DefaultConsumerID {
		t.Errorf("ConsumerID() = %q, want %q", p.ConsumerID(), DefaultConsumerID)
	}
	if !p.AllowsType("EMAIL") {
		t.Error("default policy lost allowed type")
	}
}

func TestResolveWhitespaceIdentityReturnsDefault(t *testing.T) {
	def := NewPolicy(DefaultConsumerID, nil)
	r := NewResolver(def, nil)

	for _, id := range []string{" ", "\t", "  \n  "} {
		p, err := r.Resolve(id)
		if err != nil {
			t.Fatalf("Resolve(%q) error = %v, want nil", id, err)
		}
		if p.ConsumerID() != DefaultConsumerID {
			t.Errorf("Resolve(%q) ConsumerID() = %q, want %q", id, p.ConsumerID(), DefaultConsumerID)
		}
	}
}

func TestResolveExplicitDefaultIgnoresConflictingEntry(t *testing.T) {
	def := NewPolicy(DefaultConsumerID, []string{"EMAIL"})
	override := NewPolicy("other", []string{"PHONE"})
	r := NewResolver(def, map[string]Policy{DefaultConsumerID: override})

	p, err := r.Resolve(DefaultConsumerID)
	if err != nil {
		t.Fatalf("Resolve(DefaultConsumerID) error = %v, want nil", err)
	}
	if p.ConsumerID() != DefaultConsumerID {
		t.Errorf("ConsumerID() = %q, want %q", p.ConsumerID(), DefaultConsumerID)
	}
	if !p.AllowsType("EMAIL") {
		t.Error("explicit default must use default policy, not conflicting entry")
	}
	if p.AllowsType("PHONE") {
		t.Error("explicit default leaked conflicting entry type")
	}
}

func TestResolveKnownIdentityReturnsItsPolicy(t *testing.T) {
	def := NewPolicy(DefaultConsumerID, nil)
	consumer := NewPolicy("consumer-a", []string{"EMAIL", "PHONE"})
	consumer.AllowDemasking = true
	consumer.AllowRulesOnlyDegraded = true
	r := NewResolver(def, map[string]Policy{"consumer-a": consumer})

	p, err := r.Resolve("consumer-a")
	if err != nil {
		t.Fatalf("Resolve(\"consumer-a\") error = %v, want nil", err)
	}
	if p.ConsumerID() != "consumer-a" {
		t.Errorf("ConsumerID() = %q, want %q", p.ConsumerID(), "consumer-a")
	}
	if !p.AllowsType("EMAIL") || !p.AllowsType("PHONE") {
		t.Error("known identity policy lost allowed types")
	}
	if !p.AllowDemasking || !p.AllowRulesOnlyDegraded {
		t.Error("known identity policy lost capabilities")
	}
}

func TestResolveUnknownIdentityFailsClosed(t *testing.T) {
	def := NewPolicy(DefaultConsumerID, []string{"EMAIL"})
	r := NewResolver(def, map[string]Policy{"consumer-a": NewPolicy("consumer-a", nil)})

	p, err := r.Resolve("unknown-consumer")
	if !errors.Is(err, ErrUnknownConsumer) {
		t.Fatalf("Resolve error = %v, want ErrUnknownConsumer", err)
	}
	if err != ErrUnknownConsumer {
		t.Errorf("Resolve error = %v, want exact bare sentinel", err)
	}
	if !reflect.DeepEqual(p, Policy{}) {
		t.Errorf("Resolve returned non-zero Policy %+v, want zero", p)
	}
	if p.ConsumerID() == DefaultConsumerID {
		t.Error("unknown identity must not inherit default policy")
	}
	if p.AllowsType("EMAIL") {
		t.Error("unknown identity must not inherit default allowed types")
	}
}

func TestErrUnknownConsumerDoesNotLeakIdentity(t *testing.T) {
	r := NewResolver(NewPolicy(DefaultConsumerID, nil), nil)
	_, err := r.Resolve("secret-identity-value")
	if err == nil {
		t.Fatal("Resolve error = nil, want ErrUnknownConsumer")
	}
	if strings.Contains(err.Error(), "secret-identity-value") {
		t.Errorf("error leaks identity: %q", err.Error())
	}
}

func TestNewResolverCopiesConstructorInputs(t *testing.T) {
	def := NewPolicy(DefaultConsumerID, []string{"EMAIL"})
	consumer := NewPolicy("consumer-a", []string{"EMAIL"})
	r := NewResolver(def, map[string]Policy{"consumer-a": consumer})

	def.AllowDemasking = true
	consumer.AllowDemasking = true
	consumer.AllowRulesOnlyDegraded = true
	// Directly mutate the original private maps to prove they were deep-cloned.
	delete(def.types, "EMAIL")
	def.types["PHONE"] = struct{}{}
	delete(consumer.types, "EMAIL")
	consumer.types["PHONE"] = struct{}{}

	p, err := r.Resolve("")
	if err != nil {
		t.Fatalf("Resolve(\"\") error = %v", err)
	}
	if p.AllowDemasking {
		t.Error("default policy mutated after constructor input mutation")
	}
	if !p.AllowsType("EMAIL") {
		t.Error("default policy lost EMAIL after constructor input map mutation")
	}
	if p.AllowsType("PHONE") {
		t.Error("default policy gained PHONE after constructor input map mutation")
	}

	p2, err := r.Resolve("consumer-a")
	if err != nil {
		t.Fatalf("Resolve(\"consumer-a\") error = %v", err)
	}
	if p2.AllowDemasking || p2.AllowRulesOnlyDegraded {
		t.Error("entry policy mutated after constructor input mutation")
	}
	if !p2.AllowsType("EMAIL") {
		t.Error("entry policy lost EMAIL after constructor input map mutation")
	}
	if p2.AllowsType("PHONE") {
		t.Error("entry policy gained PHONE after constructor input map mutation")
	}
}

func TestResolveResultMutationDoesNotAffectLaterResolve(t *testing.T) {
	def := NewPolicy(DefaultConsumerID, []string{"EMAIL"})
	r := NewResolver(def, nil)

	first, err := r.Resolve("")
	if err != nil {
		t.Fatalf("Resolve error = %v", err)
	}
	first.AllowDemasking = true
	first.AllowRulesOnlyDegraded = true

	second, err := r.Resolve("")
	if err != nil {
		t.Fatalf("Resolve error = %v", err)
	}
	if second.AllowDemasking || second.AllowRulesOnlyDegraded {
		t.Error("later Resolve affected by mutation of earlier result")
	}
}

func TestResolveResultTypesMapIsIsolated(t *testing.T) {
	def := NewPolicy(DefaultConsumerID, []string{"EMAIL"})
	r := NewResolver(def, nil)

	first, err := r.Resolve("")
	if err != nil {
		t.Fatalf("Resolve error = %v", err)
	}
	// Directly mutate the returned private map to prove it is a clone.
	delete(first.types, "EMAIL")
	first.types["PHONE"] = struct{}{}

	second, err := r.Resolve("")
	if err != nil {
		t.Fatalf("Resolve error = %v", err)
	}
	if !second.AllowsType("EMAIL") {
		t.Error("later Resolve lost EMAIL after earlier result map mutation")
	}
	if second.AllowsType("PHONE") {
		t.Error("later Resolve gained PHONE after earlier result map mutation")
	}
}

func TestResolveConcurrentReadsSafe(t *testing.T) {
	def := NewPolicy(DefaultConsumerID, []string{"EMAIL"})
	r := NewResolver(def, map[string]Policy{
		"consumer-a": NewPolicy("consumer-a", []string{"PHONE"}),
	})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Resolve(""); err != nil {
				t.Errorf("Resolve(\"\") error = %v", err)
			}
			if _, err := r.Resolve("consumer-a"); err != nil {
				t.Errorf("Resolve(\"consumer-a\") error = %v", err)
			}
			if _, err := r.Resolve("unknown"); !errors.Is(err, ErrUnknownConsumer) {
				t.Errorf("Resolve(\"unknown\") error = %v, want ErrUnknownConsumer", err)
			}
		}()
	}
	wg.Wait()
}
