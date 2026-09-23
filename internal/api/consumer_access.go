// Package api provides the access-control middleware that enforces the
// checker and production profiles over the HTTP router. It authenticates
// production consumers by API key, resolves their registered policy, and
// applies demasking and route gates before any downstream handler runs.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/klrushka/llm-proxy/internal/config"
	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/policy"
)

// ConsumerAccess enforces the access-control profile over the wrapped router.
// It is immutable after construction and safe for concurrent use.
type ConsumerAccess struct {
	profile   string
	resolver  *policy.Resolver
	consumers *config.Consumers
	benchmark policy.Policy
}

// NewConsumerAccess builds the access-control middleware for the given
// profile. resolver maps a registered system ID to its policy; consumers maps
// an API key SHA-256 hash to the registered system. The checker profile does
// not use resolver or consumers. The benchmark policy carries the full
// canonical type set so the checker /process route never receives an empty
// policy.
func NewConsumerAccess(profile string, resolver *policy.Resolver, consumers *config.Consumers) *ConsumerAccess {
	return &ConsumerAccess{
		profile:   profile,
		resolver:  resolver,
		consumers: consumers,
		benchmark: benchmarkPolicy(),
	}
}

// benchmarkPolicy builds the full benchmark/default policy: every canonical
// PII type allowed and demasking enabled. It is the policy injected into the
// checker /process route so the benchmark loop processes the complete type
// set. The canonical registry always builds; on an unexpected failure it fails
// closed with an empty policy rather than panicking.
func benchmarkPolicy() policy.Policy {
	reg, err := detection.New()
	if err != nil {
		return policy.NewPolicy(policy.DefaultConsumerID, nil)
	}
	allowed := make([]string, 0, len(reg.Types()))
	for _, t := range reg.Types() {
		allowed = append(allowed, string(t))
	}
	p := policy.NewPolicy(policy.DefaultConsumerID, allowed)
	p.AllowDemasking = true
	return p
}

// NewProductionResolver builds a policy.Resolver from the validated consumer
// settings. Each registered system maps to a policy allowing exactly its
// enabled types with its demasking capability. The default policy is the
// benchmark/default consumer with no types, so a blank identity never grants
// production access through the strict ResolveRegistered path.
func NewProductionResolver(consumers *config.Consumers) *policy.Resolver {
	entries := make(map[string]policy.Policy)
	for _, c := range consumers.All() {
		p := policy.NewPolicy(c.SystemID, c.EnabledTypes)
		p.AllowDemasking = c.AllowDemasking
		entries[c.SystemID] = p
	}
	def := policy.NewPolicy(policy.DefaultConsumerID, nil)
	return policy.NewResolver(def, entries)
}

// Middleware wraps next with the access-control enforcement for the configured
// profile. Health endpoints are always open. All other routes are gated per
// profile. It never logs or reflects secrets, headers, bodies or system IDs.
func (a *ConsumerAccess) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isHealth(r) {
			next.ServeHTTP(w, r)
			return
		}
		switch a.profile {
		case config.AccessProfileChecker:
			a.serveChecker(w, r, next)
		case config.AccessProfileProduction:
			a.serveProduction(w, r, next)
		default:
			writeJSONError(w, http.StatusForbidden, "forbidden")
		}
	})
}

// serveChecker enforces the checker profile. Only POST /process is allowed and
// receives the full benchmark policy with AllowDemasking=true. Every other
// functional route and /metrics fails closed with a safe 403.
func (a *ConsumerAccess) serveChecker(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if isProcess(r) {
		ctx := policy.WithPolicy(r.Context(), a.benchmark)
		next.ServeHTTP(w, r.WithContext(ctx))
		return
	}
	writeJSONError(w, http.StatusForbidden, "forbidden")
}

// serveProduction enforces the production profile. POST /process is always
// forbidden. Every other route requires a valid enabled API key; after
// authentication the registered system policy is resolved into the request
// context and the Authorization and X-System-ID headers are stripped from the
// cloned request before downstream. Demasking routes are gated on the policy's
// AllowDemasking capability.
func (a *ConsumerAccess) serveProduction(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if isProcess(r) {
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	key, ok := bearerKey(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if a.consumers == nil || a.resolver == nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	hash := sha256Hex(key)
	cons, ok := a.consumers.LookupByHash(hash)
	if !ok || !cons.Enabled {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	p, err := a.resolver.ResolveRegistered(cons.SystemID)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	// The resolver and the authenticated consumer are two sources of authority.
	// Fail closed unless the resolved policy exactly matches the consumer: the
	// consumer ID, the exact enabled type set and the demasking capability must
	// agree. Any mismatch is a misconfiguration and must not reach downstream.
	if !policyMatchesConsumer(p, cons) {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !p.AllowDemasking && isDemaskingRoute(r) {
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	ctx := policy.WithPolicy(r.Context(), p)
	next.ServeHTTP(w, stripIdentityHeaders(r).WithContext(ctx))
}

// policyMatchesConsumer reports whether the resolved policy exactly matches the
// authenticated consumer: the consumer ID, the exact enabled type set and the
// AllowDemasking capability must all agree. It never logs or reflects the
// consumer ID, types or any secret.
func policyMatchesConsumer(p policy.Policy, cons config.Consumer) bool {
	if p.ConsumerID() != cons.SystemID {
		return false
	}
	if p.AllowDemasking != cons.AllowDemasking {
		return false
	}
	return sameStringSet(p.Types(), cons.EnabledTypes)
}

// sameStringSet reports whether a and b contain exactly the same strings,
// ignoring order.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, s := range a {
		counts[s]++
	}
	for _, s := range b {
		counts[s]--
		if counts[s] < 0 {
			return false
		}
	}
	return true
}

// bearerKey extracts the exact "Bearer <key>" credential from the
// Authorization header. It accepts only a single non-empty key with no
// additional whitespace or parts; any other form is rejected. A request with
// more than one Authorization header value is ambiguous and rejected.
func bearerKey(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	auth := values[0]
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return "", false
	}
	key := strings.TrimPrefix(auth, prefix)
	if key == "" || strings.ContainsAny(key, " \t") {
		return "", false
	}
	return key, true
}

// sha256Hex returns the lowercase hex SHA-256 digest of s.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// stripIdentityHeaders returns a clone of r with the Authorization and
// X-System-ID headers removed. The clone shares no mutable header state with
// the original request.
func stripIdentityHeaders(r *http.Request) *http.Request {
	clone := r.Clone(r.Context())
	clone.Header.Del("Authorization")
	clone.Header.Del("X-System-ID")
	return clone
}

// isHealth reports whether r targets an open health endpoint.
func isHealth(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		(r.URL.Path == "/health/live" || r.URL.Path == "/health/ready")
}

// isProcess reports whether r targets the POST /process benchmark route.
func isProcess(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == "/process"
}

// isDemaskingRoute reports whether r targets a route that restores original
// values and is therefore gated on the AllowDemasking capability.
func isDemaskingRoute(r *http.Request) bool {
	return r.Method == http.MethodPost &&
		(r.URL.Path == "/v1/pii/detokenize" || r.URL.Path == "/v1/runtime/chat")
}
