// Package loadbench implements a small standard-library load benchmark for the
// POST /process benchmark adapter. It paces requests as an open-loop target
// rate with bounded concurrency and an explicit request timeout, exercises both
// the mask and restore paths over a bounded pool of synthetic records, and
// emits a machine-readable JSON report. It never sends real PII: every payload
// is synthetic.
package loadbench

import (
	"errors"
	"fmt"
	"time"
)

// Defaults for the canonical 1000 RPS / 5 minute run.
const (
	DefaultRPS           = 1000.0
	DefaultDuration      = 5 * time.Minute
	DefaultPoolSize      = 1000
	DefaultConcurrency   = 200
	DefaultTimeout       = 10 * time.Second
	DefaultTargetLatency = time.Second
)

// Config configures a load run.
type Config struct {
	// BaseURL is the base URL of the service, e.g. http://127.0.0.1:8080.
	BaseURL string
	// RPS is the open-loop target request rate.
	RPS float64
	// Duration is the timed run length.
	Duration time.Duration
	// PoolSize is the number of synthetic payload_id/original/mask records
	// prepared before timing. It bounds server-side record growth.
	PoolSize int
	// Concurrency bounds the number of in-flight requests.
	Concurrency int
	// Timeout is the per-request HTTP timeout.
	Timeout time.Duration
	// TargetLatency is the product latency target. It is an Appendix target
	// (a goal), not a hard rule of the official checker.
	TargetLatency time.Duration
}

// Validate checks the configuration for a runnable load test.
func (c Config) Validate() error {
	if c.BaseURL == "" {
		return errors.New("base URL is required")
	}
	if c.RPS <= 0 {
		return errors.New("RPS must be positive")
	}
	if c.Duration <= 0 {
		return errors.New("duration must be positive")
	}
	if c.PoolSize <= 0 {
		return errors.New("pool size must be positive")
	}
	if c.Concurrency <= 0 {
		return errors.New("concurrency must be positive")
	}
	if c.Timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	if c.TargetLatency <= 0 {
		return errors.New("target latency must be positive")
	}
	return nil
}

// String renders the config for a human-readable header line.
func (c Config) String() string {
	return fmt.Sprintf("rps=%g duration=%s pool=%d concurrency=%d timeout=%s target=%s",
		c.RPS, c.Duration, c.PoolSize, c.Concurrency, c.Timeout, c.TargetLatency)
}
