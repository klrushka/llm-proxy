// Command pii-service is the entry point of the PII protection service.
//
// It loads configuration from the environment, wires the real in-process
// detection/ownership/tokenization pipeline, the in-memory demo vault, the
// model-worker client, the /process benchmark adapter, metrics and audit
// logging, and serves the HTTP API until SIGTERM or SIGINT.
//
// The /v1/runtime/chat route is registered but fails closed with 503 because
// no real LLM client is configured yet; a vendor-specific LLM SDK or a
// passthrough fake is intentionally not invented here.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/klrushka/llm-proxy/internal/api"
	"github.com/klrushka/llm-proxy/internal/audit"
	"github.com/klrushka/llm-proxy/internal/config"
	"github.com/klrushka/llm-proxy/internal/detection"
	"github.com/klrushka/llm-proxy/internal/metrics"
	"github.com/klrushka/llm-proxy/internal/modelclient"
	"github.com/klrushka/llm-proxy/internal/policy"
	"github.com/klrushka/llm-proxy/internal/process"
	"github.com/klrushka/llm-proxy/internal/tokenization"
	"github.com/klrushka/llm-proxy/internal/vault"
	"github.com/klrushka/llm-proxy/internal/version"
)

// processScope is the fixed scope used by the /process benchmark adapter. It
// is a single shared scope for the checker loop; the extended /v1/pii/* API
// accepts caller-supplied scopes.
const processScope = "process"

// metricsWindow is the sliding window over which /metrics aggregates latency,
// RPS and TPS.
const metricsWindow = time.Minute

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

	// In-memory demo vault. It does not survive restart and holds plaintext
	// originals in volatile memory; a persistent adapter is a later task.
	v, err := vault.NewMemory(cfg.VaultTTL)
	if err != nil {
		return fmt.Errorf("vault: %w", err)
	}

	issuer, err := tokenization.New()
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
	p := policy.NewPolicy(policy.DefaultConsumerID, allowed)

	client, err := modelclient.New(cfg.ModelWorkerURL, modelclient.Mode(cfg.ModelMode), cfg.ModelClientTimeout)
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

	regMetrics := metrics.NewRegistry(metricsWindow)
	logger := audit.New(os.Stderr)

	// The runtime route is registered but left failing closed with 503 because
	// no real LLM client is configured. WithRuntime is intentionally not wired.
	mux := api.NewRouter(
		func() error { return nil },
		nil,
		api.WithPIIHandlers(handlers),
		api.WithProcess(op.Handle),
		api.WithMetrics(regMetrics),
		api.WithProcessAudit(logger),
	)

	srv := &http.Server{
		Addr:              cfg.APIListenAddress,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stdout, "pii-service %s listening on %s\n", version.Version, cfg.APIListenAddress)
		errCh <- srv.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(stop)

	select {
	case err := <-errCh:
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
		return nil
	}
}

// modelDetector adapts the model-worker client to the pipeline's model
// boundary. It maps worker entities to canonical detection candidates using
// the registry, dropping any entity whose label or source is not canonical.
func modelDetector(client *modelclient.Client, reg *detection.Registry) api.ModelDetector {
	return func(ctx context.Context, text string) ([]detection.Candidate, error) {
		entities, err := client.Infer(ctx, text)
		if err != nil {
			return nil, err
		}
		candidates := make([]detection.Candidate, 0, len(entities))
		for _, e := range entities {
			var src detection.Source
			switch e.Model {
			case "rubert":
				src = detection.SourceRubert
			case "gliner":
				src = detection.SourceGliner
			default:
				continue
			}
			typ, ok := reg.ResolveModelLabel(src, e.Label)
			if !ok {
				continue
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
