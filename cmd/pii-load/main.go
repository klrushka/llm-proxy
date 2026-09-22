// Command pii-load runs a small standard-library load benchmark against the
// POST /process benchmark adapter. It prewarms a bounded pool of synthetic
// payload_id/original/mask records, then paces requests as an open-loop target
// rate with bounded concurrency and an explicit request timeout, exercising
// both the mask and restore paths. It prints a machine-readable JSON report
// with requested/achieved RPS, duration, scheduled/completed requests, errors,
// p50/p95/p99, the 1 second product target and whether that target was met.
//
// The 1 second latency target is an Appendix target (a goal), not an invented
// hard rule of the official checker. The command exits non-zero on any
// request/contract/round-trip error or an inability to schedule or complete the
// requested load.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/klrushka/llm-proxy/internal/loadbench"
)

func main() {
	cfg := loadbench.Config{
		RPS:           loadbench.DefaultRPS,
		Duration:      loadbench.DefaultDuration,
		PoolSize:      loadbench.DefaultPoolSize,
		Concurrency:   loadbench.DefaultConcurrency,
		Timeout:       loadbench.DefaultTimeout,
		TargetLatency: loadbench.DefaultTargetLatency,
	}

	flag.StringVar(&cfg.BaseURL, "base-url", "http://127.0.0.1:8080", "base URL of the service")
	flag.Float64Var(&cfg.RPS, "rps", cfg.RPS, "open-loop target request rate")
	flag.DurationVar(&cfg.Duration, "duration", cfg.Duration, "timed run length")
	flag.IntVar(&cfg.PoolSize, "pool", cfg.PoolSize, "number of synthetic records prepared before timing")
	flag.IntVar(&cfg.Concurrency, "concurrency", cfg.Concurrency, "max in-flight requests")
	flag.DurationVar(&cfg.Timeout, "timeout", cfg.Timeout, "per-request HTTP timeout")
	flag.DurationVar(&cfg.TargetLatency, "target-latency", cfg.TargetLatency, "product latency target (Appendix goal)")
	flag.Parse()

	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "pii-load: %v\n", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := loadbench.NewClient(cfg.BaseURL, cfg.Timeout)
	pool := loadbench.NewPool(cfg.PoolSize)

	fmt.Fprintf(os.Stderr, "pii-load: prewarming %d records against %s\n", cfg.PoolSize, cfg.BaseURL)
	report, err := loadbench.Run(ctx, cfg, client, pool)
	if err != nil {
		// The report is still printed so the orchestrator can inspect partial
		// results; the non-zero exit signals the failure.
		printReport(report)
		fmt.Fprintf(os.Stderr, "pii-load: %v\n", err)
		os.Exit(1)
	}

	printReport(report)
}

func printReport(report loadbench.Report) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		fmt.Fprintf(os.Stderr, "pii-load: encode report: %v\n", err)
		os.Exit(1)
	}
}
