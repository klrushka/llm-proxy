package policy

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestResolveRegisteredKnownIdentity(t *testing.T) {
	def := NewPolicy(DefaultConsumerID, []string{"EMAIL"})
	consumer := NewPolicy("consumer-a", []string{"PHONE"})
	consumer.AllowDemasking = true
	r := NewResolver(def, map[string]Policy{"consumer-a": consumer})

	p, err := r.ResolveRegistered("consumer-a")
	if err != nil {
		t.Fatalf("ResolveRegistered error = %v, want nil", err)
	}
	if p.ConsumerID() != "consumer-a" {
		t.Errorf("ConsumerID() = %q, want consumer-a", p.ConsumerID())
	}
	if !p.AllowsType("PHONE") || p.AllowsType("EMAIL") {
		t.Error("registered policy lost its type matrix")
	}
	if !p.AllowDemasking {
		t.Error("registered policy lost AllowDemasking")
	}
}

func TestResolveRegisteredEmptyFailsClosed(t *testing.T) {
	def := NewPolicy(DefaultConsumerID, []string{"EMAIL"})
	r := NewResolver(def, map[string]Policy{"consumer-a": NewPolicy("consumer-a", nil)})

	for _, id := range []string{"", " ", "\t", "  \n  "} {
		p, err := r.ResolveRegistered(id)
		if !errors.Is(err, ErrUnknownConsumer) {
			t.Fatalf("ResolveRegistered(%q) error = %v, want ErrUnknownConsumer", id, err)
		}
		if err != ErrUnknownConsumer {
			t.Errorf("ResolveRegistered(%q) error = %v, want exact bare sentinel", id, err)
		}
		if !reflect.DeepEqual(p, Policy{}) {
			t.Errorf("ResolveRegistered(%q) returned non-zero Policy %+v", id, p)
		}
		if p.ConsumerID() == DefaultConsumerID {
			t.Errorf("ResolveRegistered(%q) must not fall back to default", id)
		}
	}
}

func TestResolveRegisteredBenchmarkFailsClosed(t *testing.T) {
	def := NewPolicy(DefaultConsumerID, []string{"EMAIL"})
	def.AllowDemasking = true
	r := NewResolver(def, nil)

	p, err := r.ResolveRegistered(DefaultConsumerID)
	if !errors.Is(err, ErrUnknownConsumer) {
		t.Fatalf("ResolveRegistered(benchmark) error = %v, want ErrUnknownConsumer", err)
	}
	if !reflect.DeepEqual(p, Policy{}) {
		t.Errorf("ResolveRegistered(benchmark) returned non-zero Policy %+v", p)
	}
	if p.AllowDemasking {
		t.Error("benchmark must not inherit default AllowDemasking")
	}
}

func TestResolveRegisteredUnknownFailsClosed(t *testing.T) {
	def := NewPolicy(DefaultConsumerID, []string{"EMAIL"})
	r := NewResolver(def, map[string]Policy{"consumer-a": NewPolicy("consumer-a", nil)})

	p, err := r.ResolveRegistered("unknown-consumer")
	if !errors.Is(err, ErrUnknownConsumer) {
		t.Fatalf("ResolveRegistered error = %v, want ErrUnknownConsumer", err)
	}
	if !reflect.DeepEqual(p, Policy{}) {
		t.Errorf("ResolveRegistered returned non-zero Policy %+v", p)
	}
}

func TestResolveRegisteredDoesNotLeakIdentity(t *testing.T) {
	r := NewResolver(NewPolicy(DefaultConsumerID, nil), nil)
	_, err := r.ResolveRegistered("secret-identity-value")
	if err == nil {
		t.Fatal("ResolveRegistered error = nil, want ErrUnknownConsumer")
	}
	if strings.Contains(err.Error(), "secret-identity-value") {
		t.Errorf("error leaks identity: %q", err.Error())
	}
}

func TestResolveRegisteredResultIsDefensiveCopy(t *testing.T) {
	def := NewPolicy(DefaultConsumerID, nil)
	consumer := NewPolicy("consumer-a", []string{"EMAIL"})
	r := NewResolver(def, map[string]Policy{"consumer-a": consumer})

	first, err := r.ResolveRegistered("consumer-a")
	if err != nil {
		t.Fatalf("ResolveRegistered error = %v", err)
	}
	first.AllowDemasking = true
	first.AllowRulesOnlyDegraded = true
	delete(first.types, "EMAIL")
	first.types["PHONE"] = struct{}{}

	second, err := r.ResolveRegistered("consumer-a")
	if err != nil {
		t.Fatalf("ResolveRegistered error = %v", err)
	}
	if second.AllowDemasking || second.AllowRulesOnlyDegraded {
		t.Error("later ResolveRegistered affected by mutation of earlier result")
	}
	if !second.AllowsType("EMAIL") || second.AllowsType("PHONE") {
		t.Error("later ResolveRegistered lost original type matrix")
	}
}

func TestWithPolicyStoresDefensiveCopy(t *testing.T) {
	p := NewPolicy("consumer-a", []string{"EMAIL"})
	p.AllowDemasking = true
	ctx := WithPolicy(context.Background(), p)

	// Mutate the original after storing.
	p.AllowDemasking = false
	delete(p.types, "EMAIL")
	p.types["PHONE"] = struct{}{}

	got, ok := PolicyFromContext(ctx)
	if !ok {
		t.Fatal("PolicyFromContext not found")
	}
	if !got.AllowDemasking {
		t.Error("stored policy lost AllowDemasking after original mutation")
	}
	if !got.AllowsType("EMAIL") || got.AllowsType("PHONE") {
		t.Error("stored policy type matrix changed after original mutation")
	}
}

func TestPolicyFromContextReturnsDefensiveCopy(t *testing.T) {
	p := NewPolicy("consumer-a", []string{"EMAIL"})
	ctx := WithPolicy(context.Background(), p)

	first, ok := PolicyFromContext(ctx)
	if !ok {
		t.Fatal("PolicyFromContext not found")
	}
	first.AllowDemasking = true
	delete(first.types, "EMAIL")
	first.types["PHONE"] = struct{}{}

	second, ok := PolicyFromContext(ctx)
	if !ok {
		t.Fatal("PolicyFromContext not found on second read")
	}
	if second.AllowDemasking {
		t.Error("second read affected by mutation of first read result")
	}
	if !second.AllowsType("EMAIL") || second.AllowsType("PHONE") {
		t.Error("second read lost original type matrix")
	}
}

func TestPolicyFromContextMissing(t *testing.T) {
	if _, ok := PolicyFromContext(context.Background()); ok {
		t.Error("PolicyFromContext on empty context found a policy, want not found")
	}
}

func TestPolicyFromContextWrongType(t *testing.T) {
	ctx := context.WithValue(context.Background(), policyContextKey{}, "not-a-policy")
	if _, ok := PolicyFromContext(ctx); ok {
		t.Error("PolicyFromContext with wrong value type found a policy, want not found")
	}
}

func TestPolicyContextKeyIsPrivate(t *testing.T) {
	// A string key must not collide with the private policyContextKey type.
	ctx := context.WithValue(context.Background(), "policy", "spoofed")
	if _, ok := PolicyFromContext(ctx); ok {
		t.Error("string-keyed value leaked into PolicyFromContext")
	}
}

func TestResolveRegisteredConcurrentSafe(t *testing.T) {
	def := NewPolicy(DefaultConsumerID, []string{"EMAIL"})
	r := NewResolver(def, map[string]Policy{
		"consumer-a": NewPolicy("consumer-a", []string{"PHONE"}),
	})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.ResolveRegistered("consumer-a"); err != nil {
				t.Errorf("ResolveRegistered(consumer-a) error = %v", err)
			}
			if _, err := r.ResolveRegistered(""); !errors.Is(err, ErrUnknownConsumer) {
				t.Errorf("ResolveRegistered(\"\") error = %v, want ErrUnknownConsumer", err)
			}
			if _, err := r.ResolveRegistered("unknown"); !errors.Is(err, ErrUnknownConsumer) {
				t.Errorf("ResolveRegistered(unknown) error = %v, want ErrUnknownConsumer", err)
			}
		}()
	}
	wg.Wait()
}

func TestWithPolicyConcurrentSafe(t *testing.T) {
	p := NewPolicy("consumer-a", []string{"EMAIL"})
	ctx := WithPolicy(context.Background(), p)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, ok := PolicyFromContext(ctx)
			if !ok {
				t.Error("PolicyFromContext not found")
				return
			}
			got.AllowDemasking = true
			delete(got.types, "EMAIL")
		}()
	}
	wg.Wait()

	got, ok := PolicyFromContext(ctx)
	if !ok {
		t.Fatal("PolicyFromContext not found after concurrent reads")
	}
	if got.AllowDemasking {
		t.Error("concurrent mutation of read results corrupted stored policy")
	}
	if !got.AllowsType("EMAIL") {
		t.Error("concurrent mutation of read results corrupted stored type matrix")
	}
}
