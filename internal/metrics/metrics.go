// Package metrics exposes the service metrics in the Prometheus format. It
// records only numeric aggregates labelled with values from closed sets (fixed
// build metadata, route templates, status codes, dependency names and canonical
// PII type names). It never accepts or exposes plaintext PII, secrets, prompts,
// request/response texts, scope IDs, payload IDs or token values.
package metrics

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/klrushka/llm-proxy/internal/audit"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Options describes the fixed build metadata published by pii_build_info.
type Options struct {
	// Version is the service version.
	Version string
	// ModelMode is the configured model mode (full or fast).
	ModelMode string
	// EntityTypes is the canonical PII type registry. Only these names are
	// used as type label values; anything else is recorded as other.
	EntityTypes []string
}

// Metrics owns a private Prometheus registry with the Go runtime and process
// collectors and the service metrics. A nil *Metrics is valid: every recording
// method is a no-op, so optional wiring and tests need no guards.
type Metrics struct {
	reg            *prometheus.Registry
	serverDuration *prometheus.HistogramVec
	serverActive   *prometheus.GaugeVec
	serverBodySize *prometheus.HistogramVec
	clientDuration *prometheus.HistogramVec
	entities       *prometheus.CounterVec
	inputTokens    prometheus.Counter
	nerCache       *prometheus.CounterVec
	rulesFallback  *prometheus.CounterVec
	entityTypes    map[string]struct{}
}

// DurationBuckets are the OpenTelemetry-recommended HTTP duration buckets in
// seconds, extended with 30s and 60s for downstream LLM calls.
var DurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10, 30, 60}

// bodySizeBuckets cover request bodies from 64 B to 16 MiB.
var bodySizeBuckets = prometheus.ExponentialBuckets(64, 4, 10)

// knownMethods is the closed set of HTTP methods used as label values. Any
// other method is recorded as _OTHER, following OpenTelemetry conventions.
var knownMethods = map[string]struct{}{
	http.MethodGet: {}, http.MethodHead: {}, http.MethodPost: {}, http.MethodPut: {},
	http.MethodPatch: {}, http.MethodDelete: {}, http.MethodOptions: {},
}

// New returns Metrics with the Go runtime, process and build info collectors
// registered.
func New(opts Options) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "pii_build_info",
			Help: "Build metadata of the PII service; the value is always 1.",
			ConstLabels: prometheus.Labels{
				"version":    opts.Version,
				"model_mode": opts.ModelMode,
			},
		}, func() float64 { return 1 }),
	)
	m := &Metrics{
		reg: reg,
		serverDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_server_request_duration_seconds",
			Help:    "Duration of inbound HTTP requests.",
			Buckets: DurationBuckets,
		}, []string{"http_request_method", "http_route", "http_response_status_code"}),
		serverActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "http_server_active_requests",
			Help: "Number of inbound HTTP requests currently in flight.",
		}, []string{"http_request_method"}),
		serverBodySize: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_server_request_body_size_bytes",
			Help:    "Declared size of inbound HTTP request bodies.",
			Buckets: bodySizeBuckets,
		}, []string{"http_request_method", "http_route"}),
	}
	m.clientDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_client_request_duration_seconds",
		Help:    "Duration of outbound HTTP requests to service dependencies.",
		Buckets: DurationBuckets,
	}, []string{"server", "operation", "http_request_method", "http_response_status_code", "error_type"})
	m.entities = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pii_entities_detected_total",
		Help: "Personal data entities confirmed by the detection pipeline, by canonical type.",
	}, []string{"type"})
	m.inputTokens = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pii_input_tokens_total",
		Help: "Whitespace-separated tokens submitted to the detection pipeline.",
	})
	m.nerCache = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pii_ner_cache_requests_total",
		Help: "Full NER requests by bounded cache outcome.",
	}, []string{"outcome"})
	for _, outcome := range []string{"hit", "miss", "coalesced", "uncached"} {
		m.nerCache.WithLabelValues(outcome)
	}
	m.rulesFallback = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pii_rules_only_fallback_attempts_total",
		Help: "Model-unavailable process requests attempted with the explicitly enabled rules-only fallback.",
	}, []string{"outcome"})
	for _, outcome := range []string{"success", "review", "error"} {
		m.rulesFallback.WithLabelValues(outcome)
	}
	m.entityTypes = make(map[string]struct{}, len(opts.EntityTypes))
	for _, t := range opts.EntityTypes {
		m.entityTypes[t] = struct{}{}
		m.entities.WithLabelValues(t)
	}
	reg.MustRegister(m.serverDuration, m.serverActive, m.serverBodySize, m.clientDuration, m.entities, m.inputTokens, m.nerCache, m.rulesFallback)
	return m
}

// RecordNERCache counts only fixed outcome labels, never text or IDs.
func (m *Metrics) RecordNERCache(outcome string) {
	if m == nil {
		return
	}
	switch outcome {
	case "hit", "miss", "coalesced", "uncached":
		m.nerCache.WithLabelValues(outcome).Inc()
	}
}

func (m *Metrics) RecordRulesFallback(outcome string) {
	if m == nil {
		return
	}
	switch outcome {
	case "success", "review", "error":
		m.rulesFallback.WithLabelValues(outcome).Inc()
	}
}

// Middleware returns the outer HTTP instrumentation. route maps a request to
// its route template (for example /v1/pii/scopes/{scope_id}); it must never
// return the actual path, and an unknown request maps to "". The middleware
// observes every response, including admission rejections,
// and a panic is recorded as 500 before being re-raised unchanged. A nil
// *Metrics returns next unchanged.
func (m *Metrics) Middleware(route func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if m == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			method := methodLabel(r.Method)
			tmpl := route(r)
			active := m.serverActive.WithLabelValues(method)
			active.Inc()
			if r.ContentLength >= 0 {
				m.serverBodySize.WithLabelValues(method, tmpl).Observe(float64(r.ContentLength))
			}
			col, ok := audit.CollectorFromContext(r.Context())
			if !ok {
				col = audit.NewCollector()
				r = r.WithContext(audit.WithCollector(r.Context(), col))
			}
			rec := &statusRecorder{ResponseWriter: w}
			start := time.Now()
			defer func() {
				active.Dec()
				m.recordDetection(col)
				status := rec.status
				p := recover()
				if p != nil {
					status = http.StatusInternalServerError
				} else if status == 0 {
					status = http.StatusOK
				}
				m.serverDuration.WithLabelValues(method, tmpl, strconv.Itoa(status)).Observe(time.Since(start).Seconds())
				if p != nil {
					panic(p)
				}
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

// recordDetection adds the request's confirmed personal entities and input
// token count. Only canonical type names become label values.
func (m *Metrics) recordDetection(col *audit.Collector) {
	entities, _ := col.Snapshot()
	for _, e := range entities {
		if !e.Personal {
			continue
		}
		t := e.Type
		if _, ok := m.entityTypes[t]; !ok {
			t = "other"
		}
		m.entities.WithLabelValues(t).Inc()
	}
	if n := col.InputTokens(); n > 0 {
		m.inputTokens.Add(float64(n))
	}
}

// RegisterVaultMappings publishes pii_vault_mappings, the number of live vault
// mappings, read from count at scrape time. A nil *Metrics ignores the call.
func (m *Metrics) RegisterVaultMappings(count func() int) {
	if m == nil {
		return
	}
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "pii_vault_mappings",
		Help: "Number of live token mappings held in the vault.",
	}, func() float64 { return float64(count()) }))
}

// methodLabel returns method when it is a known HTTP method and _OTHER
// otherwise, so a client cannot create unbounded label values.
func methodLabel(method string) string {
	if _, ok := knownMethods[method]; ok {
		return method
	}
	return "_OTHER"
}

// statusRecorder records the first status written by the wrapped handler. It
// never inspects or stores the response body.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

// InstrumentTransport wraps next so every outbound request is observed in
// http_client_request_duration_seconds. server names the dependency (for
// example model_worker or llm) and operation maps a request to a fixed
// operation name; neither may carry request data. A transport failure is
// recorded with an empty status code and an error_type of timeout, canceled
// or transport. A nil next uses http.DefaultTransport; a nil *Metrics returns
// next unchanged.
func (m *Metrics) InstrumentTransport(server string, operation func(*http.Request) string, next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	if m == nil {
		return next
	}
	return roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		start := time.Now()
		resp, err := next.RoundTrip(r)
		status, errType := "", ""
		if err != nil {
			errType = transportErrorType(r.Context(), err)
		} else {
			status = strconv.Itoa(resp.StatusCode)
		}
		m.clientDuration.WithLabelValues(server, operation(r), methodLabel(r.Method), status, errType).Observe(time.Since(start).Seconds())
		return resp, err
	})
}

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// transportErrorType classifies a transport error into a fixed label value.
// It never uses the error text. http.Client.Timeout may cancel the request
// just before its context reports DeadlineExceeded, so a reached context
// deadline also counts as a timeout.
func transportErrorType(ctx context.Context, err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded),
		errors.As(err, &netErr) && netErr.Timeout(), deadlineReached(ctx):
		return "timeout"
	case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		return "canceled"
	default:
		return "transport"
	}
}

// deadlineReached reports whether ctx has a deadline that is not in the future.
func deadlineReached(ctx context.Context) bool {
	dl, ok := ctx.Deadline()
	return ok && !time.Now().Before(dl)
}

// Handler returns the http.Handler for GET /metrics. A nil *Metrics serves a
// safe 503.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
		})
	}
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// CountTokens returns the number of tokens in text as a safe numeric aggregate.
// It counts whitespace-separated UTF-8 tokens and never returns or stores the
// text itself. This is a lightweight local estimate used only for the TPS
// metric; it does not expose any token value or plaintext.
func CountTokens(text string) int {
	if text == "" {
		return 0
	}
	return len(strings.Fields(text))
}
