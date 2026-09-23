package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/klrushka/llm-proxy/internal/config"
	"github.com/klrushka/llm-proxy/internal/policy"
)

// loadConsumers builds a *config.Consumers from a consumers JSON string via
// config.Load. It requires the vault key and production profile env vars.
func loadConsumers(t *testing.T, consumersJSON string) *config.Consumers {
	t.Helper()
	t.Setenv("PII_VAULT_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("PII_ACCESS_PROFILE", config.AccessProfileProduction)
	t.Setenv("PII_CONSUMERS_JSON", consumersJSON)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	return cfg.Consumers
}

// productionResolver builds a resolver mapping system IDs to policies matching
// the consumers JSON used in tests.
func productionResolver() *policy.Resolver {
	def := policy.NewPolicy(policy.DefaultConsumerID, nil)
	demask := policy.NewPolicy("sys-a", []string{"EMAIL"})
	demask.AllowDemasking = true
	noDemask := policy.NewPolicy("sys-b", []string{"PHONE"})
	return policy.NewResolver(def, map[string]policy.Policy{
		"sys-a": demask,
		"sys-b": noDemask,
	})
}

func productionConsumersJSON() string {
	return `[
		{"system_id":"sys-a","enabled":true,"api_key_sha256":"` + sha256Hex("key-a") + `","enabled_types":["EMAIL"],"allow_demasking":true},
		{"system_id":"sys-b","enabled":true,"api_key_sha256":"` + sha256Hex("key-b") + `","enabled_types":["PHONE"],"allow_demasking":false}
	]`
}

func productionAccess(t *testing.T) *ConsumerAccess {
	t.Helper()
	return NewConsumerAccess(config.AccessProfileProduction, productionResolver(), loadConsumers(t, productionConsumersJSON()))
}

func checkerAccess() *ConsumerAccess {
	return NewConsumerAccess(config.AccessProfileChecker, nil, nil)
}

// captureHandler records whether it was called, the request it received, and
// the policy resolved from context.
type captureHandler struct {
	called    bool
	req       *http.Request
	policy    policy.Policy
	hasPolicy bool
}

func (c *captureHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.called = true
	c.req = r
	c.policy, c.hasPolicy = policy.PolicyFromContext(r.Context())
	w.WriteHeader(http.StatusOK)
}

func doAccessRequest(t *testing.T, access *ConsumerAccess, method, path string, headers map[string]string) (*httptest.ResponseRecorder, *captureHandler) {
	t.Helper()
	capture := &captureHandler{}
	handler := access.Middleware(capture)
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec, capture
}

// --- Checker profile ---

func TestCheckerHealthOpen(t *testing.T) {
	access := checkerAccess()
	for _, path := range []string{"/health/live", "/health/ready"} {
		rec, capture := doAccessRequest(t, access, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want %d", path, rec.Code, http.StatusOK)
		}
		if !capture.called {
			t.Errorf("GET %s did not reach downstream", path)
		}
	}
}

func TestCheckerProcessGetsBenchmarkPolicy(t *testing.T) {
	access := checkerAccess()
	rec, capture := doAccessRequest(t, access, http.MethodPost, "/process", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /process status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !capture.hasPolicy {
		t.Fatal("POST /process did not receive a policy in context")
	}
	if capture.policy.ConsumerID() != policy.DefaultConsumerID {
		t.Errorf("ConsumerID() = %q, want %q", capture.policy.ConsumerID(), policy.DefaultConsumerID)
	}
	if !capture.policy.AllowDemasking {
		t.Error("checker /process policy must have AllowDemasking=true")
	}
}

func TestCheckerDataRoutesForbidden(t *testing.T) {
	access := checkerAccess()
	routes := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1/pii/detect"},
		{http.MethodPost, "/v1/pii/tokenize"},
		{http.MethodPost, "/v1/pii/detokenize"},
		{http.MethodDelete, "/v1/pii/scopes/s1"},
		{http.MethodPost, "/v1/runtime/chat"},
		{http.MethodGet, "/metrics"},
	}
	for _, rt := range routes {
		rec, capture := doAccessRequest(t, access, rt.method, rt.path, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s status = %d, want %d", rt.method, rt.path, rec.Code, http.StatusForbidden)
		}
		if capture.called {
			t.Errorf("%s %s reached downstream, want blocked", rt.method, rt.path)
		}
	}
}

func TestCheckerForbiddenBodyIsSafe(t *testing.T) {
	access := checkerAccess()
	rec, _ := doAccessRequest(t, access, http.MethodGet, "/metrics", nil)
	if !strings.Contains(rec.Body.String(), `"error":"forbidden"`) {
		t.Errorf("body = %q, want safe forbidden error", rec.Body.String())
	}
}

// --- Production: authentication ---

func TestProductionMissingKeyUnauthorized(t *testing.T) {
	access := productionAccess(t)
	rec, capture := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if capture.called {
		t.Error("downstream called on missing key")
	}
}

func TestProductionWrongKeyUnauthorized(t *testing.T) {
	access := productionAccess(t)
	rec, capture := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize",
		map[string]string{"Authorization": "Bearer wrong-key"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if capture.called {
		t.Error("downstream called on wrong key")
	}
}

func TestProductionMalformedKeyUnauthorized(t *testing.T) {
	access := productionAccess(t)
	malformed := []string{
		"Bearer",             // no key
		"Bearer ",            // empty key
		"Bearer key extra",   // extra part
		"Basic dXNlcjpwYXNz", // wrong scheme
		"Bearer key\t",       // trailing tab
		"",                   // empty header
	}
	for _, auth := range malformed {
		rec, capture := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize",
			map[string]string{"Authorization": auth})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q status = %d, want %d", auth, rec.Code, http.StatusUnauthorized)
		}
		if capture.called {
			t.Errorf("Authorization %q reached downstream", auth)
		}
	}
}

func TestProductionDisabledKeyUnauthorized(t *testing.T) {
	consumers := loadConsumers(t, `[
		{"system_id":"sys-a","enabled":false,"api_key_sha256":"`+sha256Hex("key-a")+`","enabled_types":["EMAIL"],"allow_demasking":true},
		{"system_id":"sys-b","enabled":true,"api_key_sha256":"`+sha256Hex("key-b")+`","enabled_types":["PHONE"],"allow_demasking":false}
	]`)
	access := NewConsumerAccess(config.AccessProfileProduction, productionResolver(), consumers)
	rec, capture := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize",
		map[string]string{"Authorization": "Bearer key-a"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if capture.called {
		t.Error("downstream called on disabled key")
	}
}

func TestProductionUnauthorizedBodyIsSafe(t *testing.T) {
	access := productionAccess(t)
	rec, _ := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize",
		map[string]string{"Authorization": "Bearer wrong-key"})
	if !strings.Contains(rec.Body.String(), `"error":"unauthorized"`) {
		t.Errorf("body = %q, want safe unauthorized error", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "wrong-key") {
		t.Errorf("body leaks key: %q", rec.Body.String())
	}
}

func TestProductionValidKeyAuthorized(t *testing.T) {
	access := productionAccess(t)
	rec, capture := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize",
		map[string]string{"Authorization": "Bearer key-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !capture.called {
		t.Fatal("downstream not called on valid key")
	}
	if !capture.hasPolicy {
		t.Fatal("valid key did not receive a policy in context")
	}
	if capture.policy.ConsumerID() != "sys-a" {
		t.Errorf("ConsumerID() = %q, want sys-a", capture.policy.ConsumerID())
	}
}

func TestProductionHeaderSpoofingIgnored(t *testing.T) {
	access := productionAccess(t)
	// X-System-ID must never be a source of identity: without a valid key it
	// must still be unauthorized even if X-System-ID names a registered system.
	rec, capture := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize",
		map[string]string{"X-System-ID": "sys-a"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d (X-System-ID must not authenticate)", rec.Code, http.StatusUnauthorized)
	}
	if capture.called {
		t.Error("downstream called on X-System-ID spoofing without key")
	}
}

func TestProductionXSystemIDNotIdentitySource(t *testing.T) {
	access := productionAccess(t)
	// A valid key for sys-a with a spoofed X-System-ID for sys-b must resolve
	// to sys-a's policy, not sys-b's.
	rec, capture := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize",
		map[string]string{
			"Authorization": "Bearer key-a",
			"X-System-ID":   "sys-b",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if capture.policy.ConsumerID() != "sys-a" {
		t.Errorf("ConsumerID() = %q, want sys-a (X-System-ID must not override)", capture.policy.ConsumerID())
	}
}

// --- Production: demasking gates ---

func TestProductionDemaskingDeniedWhenNotAllowed(t *testing.T) {
	access := productionAccess(t)
	// sys-b has AllowDemasking=false.
	for _, path := range []string{"/v1/pii/detokenize", "/v1/runtime/chat"} {
		rec, capture := doAccessRequest(t, access, http.MethodPost, path,
			map[string]string{"Authorization": "Bearer key-b"})
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s status = %d, want %d", path, rec.Code, http.StatusForbidden)
		}
		if capture.called {
			t.Errorf("POST %s reached downstream, want blocked", path)
		}
	}
}

func TestProductionDemaskingAllowedWhenPermitted(t *testing.T) {
	access := productionAccess(t)
	// sys-a has AllowDemasking=true.
	for _, path := range []string{"/v1/pii/detokenize", "/v1/runtime/chat"} {
		rec, capture := doAccessRequest(t, access, http.MethodPost, path,
			map[string]string{"Authorization": "Bearer key-a"})
		if rec.Code != http.StatusOK {
			t.Errorf("POST %s status = %d, want %d", path, rec.Code, http.StatusOK)
		}
		if !capture.called {
			t.Errorf("POST %s did not reach downstream", path)
		}
	}
}

func TestProductionNonDemaskingRoutesAllowed(t *testing.T) {
	access := productionAccess(t)
	// sys-b has AllowDemasking=false but tokenize/detect/revoke are allowed.
	routes := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1/pii/detect"},
		{http.MethodPost, "/v1/pii/tokenize"},
		{http.MethodDelete, "/v1/pii/scopes/s1"},
	}
	for _, rt := range routes {
		rec, capture := doAccessRequest(t, access, rt.method, rt.path,
			map[string]string{"Authorization": "Bearer key-b"})
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s status = %d, want %d", rt.method, rt.path, rec.Code, http.StatusOK)
		}
		if !capture.called {
			t.Errorf("%s %s did not reach downstream", rt.method, rt.path)
		}
	}
}

func TestProductionDemaskingForbiddenBodyIsSafe(t *testing.T) {
	access := productionAccess(t)
	rec, _ := doAccessRequest(t, access, http.MethodPost, "/v1/pii/detokenize",
		map[string]string{"Authorization": "Bearer key-b"})
	if !strings.Contains(rec.Body.String(), `"error":"forbidden"`) {
		t.Errorf("body = %q, want safe forbidden error", rec.Body.String())
	}
}

// --- Production: /process ---

func TestProductionProcessAlwaysForbidden(t *testing.T) {
	access := productionAccess(t)
	for _, auth := range []string{"", "Bearer key-a", "Bearer key-b"} {
		headers := map[string]string{}
		if auth != "" {
			headers["Authorization"] = auth
		}
		rec, capture := doAccessRequest(t, access, http.MethodPost, "/process", headers)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Authorization %q status = %d, want %d", auth, rec.Code, http.StatusForbidden)
		}
		if capture.called {
			t.Errorf("Authorization %q reached downstream /process", auth)
		}
	}
}

// --- Production: header stripping ---

func TestProductionStripsAuthorizationAndSystemID(t *testing.T) {
	access := productionAccess(t)
	rec, capture := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize",
		map[string]string{
			"Authorization": "Bearer key-a",
			"X-System-ID":   "sys-a",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if capture.req == nil {
		t.Fatal("downstream request not captured")
	}
	if got := capture.req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization leaked downstream: %q", got)
	}
	if got := capture.req.Header.Get("X-System-ID"); got != "" {
		t.Errorf("X-System-ID leaked downstream: %q", got)
	}
}

func TestProductionStripsHeadersWithoutMutatingOriginal(t *testing.T) {
	access := productionAccess(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/pii/tokenize", nil)
	req.Header.Set("Authorization", "Bearer key-a")
	req.Header.Set("X-System-ID", "sys-a")
	rec := httptest.NewRecorder()
	capture := &captureHandler{}
	access.Middleware(capture).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	// The original request must be untouched.
	if req.Header.Get("Authorization") != "Bearer key-a" {
		t.Error("original request Authorization was mutated")
	}
	if req.Header.Get("X-System-ID") != "sys-a" {
		t.Error("original request X-System-ID was mutated")
	}
}

// --- Production: distinct policies ---

func TestProductionTwoKeysGetDifferentPolicies(t *testing.T) {
	access := productionAccess(t)
	_, capA := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize",
		map[string]string{"Authorization": "Bearer key-a"})
	_, capB := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize",
		map[string]string{"Authorization": "Bearer key-b"})
	if capA.policy.ConsumerID() == capB.policy.ConsumerID() {
		t.Error("two keys resolved to the same consumer")
	}
	if capA.policy.AllowDemasking == capB.policy.AllowDemasking {
		t.Error("two keys resolved to the same demasking capability")
	}
	if !capA.policy.AllowDemasking || capB.policy.AllowDemasking {
		t.Error("sys-a must allow demasking, sys-b must not")
	}
}

// --- Production: health open ---

func TestProductionHealthOpen(t *testing.T) {
	access := productionAccess(t)
	for _, path := range []string{"/health/live", "/health/ready"} {
		rec, capture := doAccessRequest(t, access, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want %d", path, rec.Code, http.StatusOK)
		}
		if !capture.called {
			t.Errorf("GET %s did not reach downstream", path)
		}
	}
}

// --- Race safety ---

func TestProductionConcurrentAuthRaceSafe(t *testing.T) {
	access := productionAccess(t)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "key-a"
			if i%2 == 1 {
				key = "key-b"
			}
			rec, capture := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize",
				map[string]string{"Authorization": "Bearer " + key})
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
				return
			}
			if !capture.hasPolicy {
				t.Error("no policy in context")
			}
		}(i)
	}
	wg.Wait()
}

func TestProductionConcurrentUnauthorizedRaceSafe(t *testing.T) {
	access := productionAccess(t)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec, capture := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize",
				map[string]string{"Authorization": "Bearer wrong-key"})
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			if capture.called {
				t.Error("downstream called on unauthorized")
			}
		}()
	}
	wg.Wait()
}

func TestCheckerConcurrentRaceSafe(t *testing.T) {
	access := checkerAccess()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec, capture := doAccessRequest(t, access, http.MethodPost, "/process", nil)
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			if !capture.hasPolicy || !capture.policy.AllowDemasking {
				t.Error("checker /process policy missing or wrong")
			}
		}()
	}
	wg.Wait()
}

func TestProductionContextPolicyIsDefensiveCopy(t *testing.T) {
	access := productionAccess(t)
	_, capture := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize",
		map[string]string{"Authorization": "Bearer key-a"})
	if !capture.hasPolicy {
		t.Fatal("no policy in context")
	}
	// Mutate the captured policy; a later request must be unaffected.
	capture.policy.AllowDemasking = false
	_, cap2 := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize",
		map[string]string{"Authorization": "Bearer key-a"})
	if !cap2.policy.AllowDemasking {
		t.Error("later request affected by mutation of earlier captured policy")
	}
}

func TestProductionUnknownProfileFailsClosed(t *testing.T) {
	access := NewConsumerAccess("bogus", nil, nil)
	rec, capture := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if capture.called {
		t.Error("downstream called on unknown profile")
	}
}

func TestProductionNilConsumersFailsClosed(t *testing.T) {
	access := NewConsumerAccess(config.AccessProfileProduction, productionResolver(), nil)
	rec, capture := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize",
		map[string]string{"Authorization": "Bearer key-a"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if capture.called {
		t.Error("downstream called with nil consumers")
	}
}

func TestProductionNilResolverFailsClosed(t *testing.T) {
	consumers := loadConsumers(t, productionConsumersJSON())
	access := NewConsumerAccess(config.AccessProfileProduction, nil, consumers)
	rec, capture := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize",
		map[string]string{"Authorization": "Bearer key-a"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if capture.called {
		t.Error("downstream called with nil resolver")
	}
}

func TestProductionContextCarriesPolicy(t *testing.T) {
	access := productionAccess(t)
	_, capture := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize",
		map[string]string{"Authorization": "Bearer key-a"})
	if !capture.hasPolicy {
		t.Fatal("policy not present in downstream context")
	}
	if capture.policy.ConsumerID() != "sys-a" {
		t.Errorf("ConsumerID() = %q, want sys-a", capture.policy.ConsumerID())
	}
}

func TestProductionContextPolicyReadableWithoutHeaders(t *testing.T) {
	access := productionAccess(t)
	_, capture := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize",
		map[string]string{"Authorization": "Bearer key-a"})
	// Downstream reads the policy from context, not from headers.
	if capture.req == nil {
		t.Fatal("downstream request not captured")
	}
	if capture.req.Header.Get("Authorization") != "" {
		t.Error("downstream still has Authorization header")
	}
	if !capture.hasPolicy {
		t.Error("downstream could not read policy from context")
	}
}

func TestProductionDuplicateAuthorizationRejected(t *testing.T) {
	access := productionAccess(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/pii/tokenize", nil)
	req.Header.Add("Authorization", "Bearer key-a")
	req.Header.Add("Authorization", "Bearer key-a")
	rec := httptest.NewRecorder()
	capture := &captureHandler{}
	access.Middleware(capture).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d (ambiguous duplicate Authorization must be rejected)", rec.Code, http.StatusUnauthorized)
	}
	if capture.called {
		t.Error("downstream called on ambiguous duplicate Authorization")
	}
}

func TestCheckerProcessPolicyHasFullTypeSet(t *testing.T) {
	access := checkerAccess()
	_, capture := doAccessRequest(t, access, http.MethodPost, "/process", nil)
	if !capture.hasPolicy {
		t.Fatal("POST /process did not receive a policy in context")
	}
	// The checker /process policy must carry the full canonical benchmark set,
	// not an empty type set.
	if !capture.policy.AllowsType("EMAIL") || !capture.policy.AllowsType("PHONE") {
		t.Error("checker /process policy must allow canonical types")
	}
	if capture.policy.AllowsType("BOGUS_TYPE") {
		t.Error("checker /process policy must not allow an unknown type")
	}
}

// --- Production: resolver/consumer consistency ---

// mismatchedResolver builds a resolver whose sys-a policy deliberately does not
// match the authenticated sys-a consumer (EMAIL, AllowDemasking=true). The
// mismatch is applied by the build function, which returns the sys-a policy.
func mismatchedResolver(build func() policy.Policy) *policy.Resolver {
	def := policy.NewPolicy(policy.DefaultConsumerID, nil)
	sysB := policy.NewPolicy("sys-b", []string{"PHONE"})
	return policy.NewResolver(def, map[string]policy.Policy{
		"sys-a": build(),
		"sys-b": sysB,
	})
}

func TestProductionConsumerIDMismatchUnauthorized(t *testing.T) {
	access := NewConsumerAccess(config.AccessProfileProduction,
		mismatchedResolver(func() policy.Policy {
			p := policy.NewPolicy("other", []string{"EMAIL"})
			p.AllowDemasking = true
			return p
		}),
		loadConsumers(t, productionConsumersJSON()))
	rec, capture := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize",
		map[string]string{"Authorization": "Bearer key-a"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d (consumer ID mismatch must fail closed)", rec.Code, http.StatusUnauthorized)
	}
	if capture.called {
		t.Error("downstream called on consumer ID mismatch")
	}
}

func TestProductionEnabledTypesMismatchUnauthorized(t *testing.T) {
	access := NewConsumerAccess(config.AccessProfileProduction,
		mismatchedResolver(func() policy.Policy {
			p := policy.NewPolicy("sys-a", []string{"PHONE"})
			p.AllowDemasking = true
			return p
		}),
		loadConsumers(t, productionConsumersJSON()))
	rec, capture := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize",
		map[string]string{"Authorization": "Bearer key-a"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d (enabled types mismatch must fail closed)", rec.Code, http.StatusUnauthorized)
	}
	if capture.called {
		t.Error("downstream called on enabled types mismatch")
	}
}

func TestProductionAllowDemaskingMismatchUnauthorized(t *testing.T) {
	access := NewConsumerAccess(config.AccessProfileProduction,
		mismatchedResolver(func() policy.Policy {
			return policy.NewPolicy("sys-a", []string{"EMAIL"})
		}),
		loadConsumers(t, productionConsumersJSON()))
	rec, capture := doAccessRequest(t, access, http.MethodPost, "/v1/pii/tokenize",
		map[string]string{"Authorization": "Bearer key-a"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d (AllowDemasking mismatch must fail closed)", rec.Code, http.StatusUnauthorized)
	}
	if capture.called {
		t.Error("downstream called on AllowDemasking mismatch")
	}
}
