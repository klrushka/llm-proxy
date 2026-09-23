// Command pii-smoke runs a reusable live smoke against an already-deployed PII
// service. It checks /health/live, /health/ready and the full
// POST /v1/runtime/chat product flow (mask -> LLM -> demask) using only
// synthetic Russian PII, and verifies that the final response restored the
// synthetic values. It exits non-zero on any non-2xx, malformed/trailing JSON,
// oversized body, wrong contract or missing restored synthetic value.
//
// It never prints request/response bodies, credentials or any sensitive data;
// successful output is short and CI-friendly. It supports http and https base
// URLs. For an https base URL, -allow-self-signed opts in to accepting a
// self-signed TLS certificate; the flag is rejected for http.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/klrushka/llm-proxy/internal/smoke"
)

func main() {
	cfg := smoke.Config{Timeout: smoke.DefaultTimeout}

	flag.StringVar(&cfg.BaseURL, "base-url", "", "base URL of the deployed service (http or https)")
	flag.DurationVar(&cfg.Timeout, "timeout", cfg.Timeout, "overall and per-request timeout")
	flag.BoolVar(&cfg.AllowSelfSigned, "allow-self-signed", false, "accept a self-signed TLS certificate (https only)")
	flag.Parse()

	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "pii-smoke: %v\n", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := smoke.NewClient(cfg.BaseURL, cfg.Timeout, cfg.AllowSelfSigned)
	summary, err := client.Run(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pii-smoke: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(summary)
}
