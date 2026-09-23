// Command pii-service is the entry point of the PII protection service.
//
// It loads configuration from the environment, wires the real in-process
// detection/ownership/tokenization pipeline, the in-memory demo vault, the
// model-worker client, the /process benchmark adapter, metrics and audit
// logging, and serves the HTTP API until SIGTERM or SIGINT.
//
// The /v1/runtime/chat route runs the full product flow request -> mask ->
// downstream LLM -> demask -> response when the downstream LLM is configured.
// When the LLM configuration group is absent the route stays fail-closed with
// 503 so /process can run alone.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/klrushka/llm-proxy/internal/api"
	"github.com/klrushka/llm-proxy/internal/audit"
	"github.com/klrushka/llm-proxy/internal/config"
	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/llmclient"
	"github.com/klrushka/llm-proxy/internal/metrics"
	"github.com/klrushka/llm-proxy/internal/modelclient"
	"github.com/klrushka/llm-proxy/internal/policy"
	"github.com/klrushka/llm-proxy/internal/process"
	"github.com/klrushka/llm-proxy/internal/rules"
	"github.com/klrushka/llm-proxy/internal/tokenization"
	"github.com/klrushka/llm-proxy/internal/vault"
	"github.com/klrushka/llm-proxy/internal/version"
)

// processScope is the fixed scope used by the /process benchmark adapter. It
// is a single shared scope for the checker loop; the extended /v1/pii/* API
// accepts caller-supplied scopes.
const processScope = "process"

// windowOverlapTokens is the tokenizer-token overlap between adjacent windows
// in the production long-text path. It lets entities spanning a window boundary
// be seen in both windows.
const windowOverlapTokens = 64

// windowConcurrency bounds the number of concurrent /infer calls issued for the
// windows of a single long-text request.
const windowConcurrency = 4

// maxConcurrentRequests is the fixed cap on concurrently executing HTTP
// requests admitted by the admission middleware. When the cap is reached a new
// request is rejected immediately with 429 and no waiter queue is formed.
const maxConcurrentRequests = 64

const (
	serverReadHeaderTimeout = 10 * time.Second
	serverReadTimeout       = 30 * time.Second
	serverIdleTimeout       = 60 * time.Second
	serverResponseMargin    = 10 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "pii-service: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Encrypted volatile vault. It stores only AES-256-GCM ciphertext of the
	// original values in volatile memory and derives its AES/HMAC keys from the
	// configured master key. The plaintext Memory adapter is intentionally not
	// used in production wiring.
	v, err := vault.NewEncrypted(cfg.VaultKey.Bytes(), cfg.VaultTTL)
	if err != nil {
		return fmt.Errorf("vault: %w", err)
	}

	// Keyed TTL token issuer. It derives its HMAC key from the configured
	// master key and reuses tokens only within the configured TTL.
	issuer, err := tokenization.NewKeyed(cfg.VaultKey.Bytes(), cfg.VaultTTL)
	if err != nil {
		return fmt.Errorf("token issuer: %w", err)
	}

	// The benchmark/default consumer is allowed every canonical PII type.
	reg, err := detection.New()
	if err != nil {
		return fmt.Errorf("detection registry: %w", err)
	}
	allowed := make([]string, 0, len(reg.Types()))
	for _, t := range reg.Types() {
		allowed = append(allowed, string(t))
	}
	p := policy.NewPolicy(allowed)

	regMetrics := metrics.New(metrics.Options{Version: version.Version, ModelMode: cfg.ModelMode, EntityTypes: allowed})
	regMetrics.RegisterVaultMappings(v.Len)

	client, err := modelclient.New(cfg.ModelWorkerURL, modelclient.Mode(cfg.ModelMode), cfg.ModelClientTimeout,
		modelclient.WithTransport(regMetrics.InstrumentTransport("model_worker", modelclient.Operation, nil)))
	if err != nil {
		return fmt.Errorf("model client: %w", err)
	}

	pipe := api.NewPipeline(modelDetector(client, reg), p, issuer, v)
	handlers := pipe.Handlers()

	// The /process masker is the real pipeline tokenize path. A model-worker
	// unavailability is classified to the process-level sentinel so the
	// operation fails closed with 503 instead of a generic 500.
	mask := func(ctx context.Context, payload string) (string, error) {
		res, err := handlers.Tokenize(ctx, api.TokenizeRequest{Text: payload, ScopeID: processScope})
		if err != nil {
			if errors.Is(err, modelclient.ErrModelUnavailable) {
				return "", process.ErrModelUnavailable
			}
			return "", err
		}
		return res.TokenizedText, nil
	}
	op := process.NewOperation(process.NewStore(), mask)

	logger := audit.New(os.Stderr)

	handler, err := buildRouter(cfg, pipe, handlers, op, regMetrics)
	if err != nil {
		return err
	}

	// Audit surrounds admission so every admitted data request has exactly one
	// safe event, including requests rejected while the service is overloaded.
	// The metrics middleware sits inside audit, so it can read the request's
	// audit collector, and outside admission, so 429 responses are measured.
	auditHandler := composeHandler(cfg, logger, handler, regMetrics, maxConcurrentRequests)

	// Runtime executes model protection and the downstream LLM sequentially.
	// Keep the socket write deadline above both configured upstream budgets so
	// the server does not cut off an otherwise valid response mid-flight.
	writeTimeout := serverWriteTimeout(cfg)
	srv := newServer(cfg.APIListenAddress, auditHandler, writeTimeout)
	metricsSrv := newMetricsServer(cfg.MetricsListenAddress, regMetrics.Handler())

	errCh := make(chan error, 2)
	go func() {
		fmt.Fprintf(os.Stdout, "pii-service %s listening on %s\n", version.Version, cfg.APIListenAddress)
		errCh <- srv.ListenAndServe()
	}()
	go func() {
		fmt.Fprintf(os.Stdout, "pii-service metrics listening on %s\n", cfg.MetricsListenAddress)
		errCh <- metricsSrv.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(stop)

	select {
	case err := <-errCh:
		// Either listener stopping takes the whole service down so a dead
		// metrics listener is never silently ignored.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = metricsSrv.Shutdown(ctx)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case sig := <-stop:
		fmt.Fprintf(os.Stdout, "pii-service: received %s, shutting down\n", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			return fmt.Errorf("http server shutdown: %w", err)
		}
		if err := metricsSrv.Shutdown(ctx); err != nil {
			return fmt.Errorf("metrics server shutdown: %w", err)
		}
		return nil
	}
}

// newServer builds the production http.Server with non-zero read, write, idle
// and read-header timeouts so no connection can be held open indefinitely. It
// is a small testable helper; graceful shutdown is handled by the caller.
func newServer(addr string, handler http.Handler, writeTimeout time.Duration) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		ReadTimeout:       serverReadTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       serverIdleTimeout,
	}
}

// composeHandler builds the public API middleware chain:
// audit -> metrics -> admission -> router.
func composeHandler(cfg config.Config, logger *audit.Logger, mux *http.ServeMux, m *metrics.Metrics, admissionLimit int) http.Handler {
	admission := newAdmissionMiddleware(admissionLimit)
	instrument := m.Middleware(routeTemplate(mux))
	return audit.Middleware(logger, audit.ModelMode(cfg.ModelMode))(instrument(admission(mux)))
}

// routeTemplate returns the metrics route resolver for mux. It reports the
// matched ServeMux pattern without its method, for example
// /v1/pii/scopes/{scope_id}, and never the actual request path. An unmatched
// request resolves to "".
func routeTemplate(mux *http.ServeMux) func(*http.Request) string {
	return func(r *http.Request) string {
		_, pattern := mux.Handler(r)
		if i := strings.IndexByte(pattern, ' '); i >= 0 {
			pattern = pattern[i+1:]
		}
		return pattern
	}
}

// newMetricsServer builds the internal metrics listener. It serves only
// GET /metrics, bypasses admission and audit, and must be
// reachable only by the metrics collector.
func newMetricsServer(addr string, metrics http.Handler) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics)
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		ReadTimeout:       serverReadTimeout,
		WriteTimeout:      serverReadTimeout,
		IdleTimeout:       serverIdleTimeout,
	}
}

// serverWriteTimeout covers the complete post-header request budget: reading
// the bounded body, model protection, the downstream LLM and response writing.
func serverWriteTimeout(cfg config.Config) time.Duration {
	return serverReadTimeout + cfg.ModelClientTimeout + cfg.LLM.Timeout + serverResponseMargin
}

// admissionMiddleware bounds the number of concurrently executing HTTP
// requests. Accepted requests hold a permit for the full wrapped call and
// always release it, including success, error and panic paths. When capacity
// is full a request is rejected immediately with a fixed safe 429 and no
// waiter queue is formed.
type admissionMiddleware struct {
	permits chan struct{}
	next    http.Handler
}

// newAdmissionMiddleware returns an admissionMiddleware that allows at most
// limit concurrent requests to next. limit must be positive and next must be
// non-nil.
func newAdmissionMiddleware(limit int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return &admissionMiddleware{
			permits: make(chan struct{}, limit),
			next:    next,
		}
	}
}

// ServeHTTP acquires a permit or rejects the request immediately with a fixed
// safe 429 when capacity is full. The permit is released on success, error and
// propagated panic.
func (m *admissionMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Operational endpoints must remain observable during data-plane
	// saturation; access control still runs inside this middleware.
	if r.Method == http.MethodGet && (r.URL.Path == "/health/live" || r.URL.Path == "/health/ready") {
		m.next.ServeHTTP(w, r)
		return
	}
	select {
	case m.permits <- struct{}{}:
	default:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"service overloaded"}`))
		return
	}
	defer func() { <-m.permits }()
	m.next.ServeHTTP(w, r)
}

// buildRouter wires the HTTP router. The runtime route runs the full product
// flow when the downstream LLM is configured; when the LLM configuration group
// is absent the route stays fail-closed with 503 so /process can run alone.
// WithRuntime is appended only when the group is complete: a nil option would
// panic in NewRouter, which calls every option unconditionally. It returns the
// router mux for the server middleware chain.
func buildRouter(cfg config.Config, pipe *api.Pipeline, handlers api.PIIHandlers, op *process.Operation, m *metrics.Metrics) (*http.ServeMux, error) {
	opts := []api.Option{
		api.WithPIIHandlers(handlers),
		api.WithProcess(op.Handle),
	}
	if cfg.LLM.Enabled() {
		llm, err := llmclient.New(llmclient.Config{
			URL:       cfg.LLM.URL,
			Model:     cfg.LLM.Model,
			APIKey:    cfg.LLM.APIKey,
			Timeout:   cfg.LLM.Timeout,
			Transport: m.InstrumentTransport("llm", func(*http.Request) string { return "chat" }, nil),
		})
		if err != nil {
			return nil, fmt.Errorf("llm client: %w", err)
		}
		coord := pipe.RuntimeCoordinator(llm.Complete)
		opts = append(opts, api.WithRuntime(api.RuntimeFuncFromCoordinator(coord)))
	}
	return api.NewRouter(
		func() error { return nil },
		opts...,
	), nil
}

// modelDetector adapts the model-worker client to the pipeline's model
// boundary. It maps worker entities to canonical or intermediate detection
// candidates using the registry. The contract is strictly source-specific: any
// syntactically valid entity with an unknown model source or a label outside
// the source's explicit allowlist is a contract violation of the whole
// inference response and fails closed with a safe modelclient.ErrInvalidResponse
// that never embeds the request text, label or value. RuBERT document labels
// (PASSPORT, INN, CREDIT_CARD, DRIVER_LICENSE) become canonical only on an exact
// UTF-8 byte span match against the Go document validators; invalid structural
// values are dropped rather than promoted.
func modelDetector(client *modelclient.Client, reg *detection.Registry) api.ModelDetector {
	return func(ctx context.Context, text string) ([]detection.Candidate, error) {
		entities, err := client.InferBounded(ctx, text, windowOverlapTokens, windowConcurrency)
		if err != nil {
			// Keep the modelclient classification for /process and add the
			// API sentinel so /v1/pii/* fail closed with 503.
			if errors.Is(err, modelclient.ErrModelUnavailable) {
				return nil, fmt.Errorf("%w: %w", api.ErrModelUnavailable, err)
			}
			return nil, err
		}
		validated := validatedDocumentIndex(text)
		candidates := make([]detection.Candidate, 0, len(entities))
		for _, e := range entities {
			src, ok := sourceFor(e.Model)
			if !ok {
				return nil, fmt.Errorf("%w: unknown model source", modelclient.ErrInvalidResponse)
			}
			if src == detection.SourceRubert {
				if t, isDoc := documentLabelType(e.Label); isDoc {
					if validated.contains(t, e.Start, e.End) {
						candidates = append(candidates, detection.Candidate{
							Type:       t,
							Start:      e.Start,
							End:        e.End,
							Confidence: e.Confidence,
							Sources:    []detection.Source{src},
						})
					}
					continue
				}
			}
			typ, ok := reg.ResolveModelLabel(src, e.Label)
			if !ok {
				return nil, fmt.Errorf("%w: unknown label", modelclient.ErrInvalidResponse)
			}
			candidates = append(candidates, detection.Candidate{
				Type:       typ,
				Start:      e.Start,
				End:        e.End,
				Confidence: e.Confidence,
				Sources:    []detection.Source{src},
			})
		}
		return candidates, nil
	}
}

// sourceFor maps a worker model string to a detection source.
func sourceFor(model string) (detection.Source, bool) {
	switch model {
	case "rubert":
		return detection.SourceRubert, true
	case "gliner":
		return detection.SourceGliner, true
	}
	return "", false
}

// documentLabelType maps a RuBERT document label to its canonical type.
func documentLabelType(label string) (detection.Type, bool) {
	switch label {
	case "PASSPORT":
		return detection.TypePassportNumber, true
	case "INN":
		return detection.TypeINNPerson, true
	case "CREDIT_CARD":
		return detection.TypeBankCardNumber, true
	case "DRIVER_LICENSE":
		return detection.TypeDriverLicenseNumber, true
	}
	return "", false
}

// docIndexKey identifies a validated document candidate by type and exact span.
type docIndexKey struct {
	t     detection.Type
	start int
	end   int
}

// docIndex is an O(1) membership index over Go-validated document candidates.
type docIndex map[docIndexKey]struct{}

// contains reports whether a validated candidate of type t exactly matches the
// span [start,end).
func (d docIndex) contains(t detection.Type, start, end int) bool {
	_, ok := d[docIndexKey{t: t, start: start, end: end}]
	return ok
}

// validatedDocumentIndex runs the Go document validators once and indexes the
// resulting candidates by (type,start,end) so model document labels are
// confirmed against the same checksums and context rules the Go pipeline
// applies, without duplicating them or scanning per model span.
func validatedDocumentIndex(text string) docIndex {
	idx := make(docIndex)
	for _, c := range validatedDocumentCandidates(text) {
		idx[docIndexKey{t: c.Type, start: c.Start, end: c.End}] = struct{}{}
	}
	return idx
}

// validatedDocumentCandidates runs the Go document validators once so model
// document labels are confirmed against the same checksums and context rules
// the Go pipeline applies, without duplicating them.
func validatedDocumentCandidates(text string) []detection.Candidate {
	var out []detection.Candidate
	out = append(out, rules.DetectIdentityDocuments(text)...)
	out = append(out, rules.DetectINN(text)...)
	out = append(out, rules.DetectBankCards(text)...)
	return out
}
