package loadbench

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

// Run executes a load run against the service at cfg.BaseURL. It prewarms the
// pool (masking every original), then paces requests as an open-loop target
// rate for cfg.Duration with bounded concurrency and an explicit per-request
// timeout.
//
// The open-loop target is derived from RPS and duration as an explicit planned
// request count, and each planned slot is scheduled against its target
// timestamp. A Go ticker may drop ticks when the scheduler falls behind, so the
// run never relies on ticker events: every planned slot is accounted for either
// as scheduled or as unschedulable. If no worker slot is free when a planned
// slot fires, the slot is counted as unschedulable rather than silently turning
// the test into a closed-loop one.
//
// Run returns a non-nil error if the configuration is invalid, the pool cannot
// be prewarmed, the run context is cancelled, or the run could not schedule or
// complete the requested load (any unschedulable slot or any
// request/contract/round-trip error). On cancellation it stops scheduling,
// waits for already launched requests to finish or cancel, and returns a
// partial report together with the cancellation error.
func Run(ctx context.Context, cfg Config, client *Client, pool *Pool) (Report, error) {
	if err := cfg.Validate(); err != nil {
		return Report{}, fmt.Errorf("config: %w", err)
	}
	if err := pool.Prewarm(ctx, client); err != nil {
		return Report{}, err
	}

	interval := time.Duration(float64(time.Second) / cfg.RPS)
	if interval <= 0 {
		interval = time.Nanosecond
	}
	planned := int(math.Ceil(cfg.Duration.Seconds() * cfg.RPS))
	if planned <= 0 {
		planned = 1
	}

	start := time.Now()

	sem := make(chan struct{}, cfg.Concurrency)

	var mu sync.Mutex
	var wg sync.WaitGroup
	var latencies []time.Duration
	var scheduled, completed, errors, unschedulable int

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	timer := time.NewTimer(0)
	defer timer.Stop()

	var cancelErr error
	for i := 0; i < planned; i++ {
		target := start.Add(time.Duration(i) * interval)
		// Drain any stale timer value before resetting to the next target.
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(time.Until(target))
		select {
		case <-ctx.Done():
			cancelErr = ctx.Err()
			goto done
		case <-timer.C:
		}
		select {
		case sem <- struct{}{}:
			scheduled++
			wg.Add(1)
			go func() {
				defer func() { <-sem }()
				defer wg.Done()
				req, expect := pool.Next()
				reqStart := time.Now()
				result, err := client.Do(ctx, req)
				dur := time.Since(reqStart)
				mu.Lock()
				latencies = append(latencies, dur)
				if err != nil {
					errors++
				} else if result != expect {
					errors++
				} else {
					completed++
				}
				mu.Unlock()
			}()
		default:
			// No worker slot free: this planned slot cannot be met. Count it as
			// unschedulable rather than silently reducing the rate into a
			// closed-loop test.
			mu.Lock()
			unschedulable++
			mu.Unlock()
		}
	}

done:
	// Wait for all launched requests to finish or cancel so the report reflects
	// a complete run and no worker goroutine mutates run state afterwards.
	wg.Wait()
	duration := time.Since(start)

	mu.Lock()
	report := buildReport(cfg, duration, planned, scheduled, completed, errors, unschedulable, latencies)
	mu.Unlock()

	if cancelErr != nil {
		return report, cancelErr
	}
	if unschedulable > 0 {
		return report, fmt.Errorf("could not schedule %d of %d planned request(s): concurrency %d too low for %g RPS",
			unschedulable, planned, cfg.Concurrency, cfg.RPS)
	}
	if errors > 0 {
		return report, fmt.Errorf("%d request(s) failed or returned an unexpected result", errors)
	}
	return report, nil
}
