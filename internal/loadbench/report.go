package loadbench

import (
	"math"
	"sort"
	"time"
)

// Report is the machine-readable JSON output of a load run.
type Report struct {
	RequestedRPS     float64       `json:"requested_rps"`
	AchievedRPS      float64       `json:"achieved_rps"`
	Duration         time.Duration `json:"duration"`
	Planned          int           `json:"planned"`
	Scheduled        int           `json:"scheduled"`
	Completed        int           `json:"completed"`
	Errors           int           `json:"errors"`
	Unschedulable    int           `json:"unschedulable"`
	P50              time.Duration `json:"p50"`
	P95              time.Duration `json:"p95"`
	P99              time.Duration `json:"p99"`
	TargetLatency    time.Duration `json:"target_latency"`
	TargetMet        bool          `json:"target_met"`
	TargetIsAppendix bool          `json:"target_is_appendix"`
}

// buildReport computes the aggregate report from the collected observations.
// planned is the number of target slots derived from RPS and duration;
// scheduled is how many of those slots actually launched a request;
// unschedulable is how many planned slots could not launch because no worker
// slot was free. latencies are the per-request round-trip durations of
// completed requests.
func buildReport(cfg Config, duration time.Duration, planned, scheduled, completed, errors, unschedulable int, latencies []time.Duration) Report {
	r := Report{
		RequestedRPS:     cfg.RPS,
		Duration:         duration,
		Planned:          planned,
		Scheduled:        scheduled,
		Completed:        completed,
		Errors:           errors,
		Unschedulable:    unschedulable,
		TargetLatency:    cfg.TargetLatency,
		TargetIsAppendix: true,
	}
	if duration > 0 {
		r.AchievedRPS = float64(completed) / duration.Seconds()
	}
	if len(latencies) > 0 {
		sorted := append([]time.Duration(nil), latencies...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		r.P50 = percentile(sorted, 0.50)
		r.P95 = percentile(sorted, 0.95)
		r.P99 = percentile(sorted, 0.99)
		r.TargetMet = r.P99 <= cfg.TargetLatency
	}
	return r
}

// percentile returns the p-th percentile of a sorted duration slice using the
// nearest-rank method. sorted must be non-empty.
func percentile(sorted []time.Duration, p float64) time.Duration {
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
