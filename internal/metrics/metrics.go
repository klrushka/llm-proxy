// Package metrics implements safe service metrics for GET /metrics. It records
// only numeric aggregates (request duration and token count) and renders
// latency, RPS and TPS. It never accepts or exposes plaintext PII, secrets,
// prompts, request/response texts or token values; the only labels are the
// fixed metric names and the numeric values themselves.
package metrics

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Registry records safe numeric observations and renders latency, RPS and TPS
// over a sliding window. It is safe for concurrent use.
type Registry struct {
	mu     sync.Mutex
	window time.Duration
	obs    []observation
}

// observation is one recorded request. It carries only safe numeric aggregates:
// the request duration and the number of tokens processed. It never carries the
// payload, the result, a token value or any other plaintext.
type observation struct {
	duration time.Duration
	tokens   int
	at       time.Time
}

// NewRegistry returns a Registry that aggregates observations over the given
// sliding window. A non-positive window defaults to one minute.
func NewRegistry(window time.Duration) *Registry {
	if window <= 0 {
		window = time.Minute
	}
	return &Registry{window: window}
}

// Record adds one observation. duration and tokens are safe numeric aggregates;
// the caller must never pass plaintext, secrets or token values.
func (r *Registry) Record(duration time.Duration, tokens int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.obs = append(r.obs, observation{duration: duration, tokens: tokens, at: now})
	r.prune(now)
}

// prune drops observations older than the sliding window.
func (r *Registry) prune(now time.Time) {
	cutoff := now.Add(-r.window)
	i := 0
	for i < len(r.obs) && r.obs[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		r.obs = append(r.obs[:0], r.obs[i:]...)
	}
}

// Snapshot is a point-in-time view of the safe aggregates over the window.
type Snapshot struct {
	Requests   int
	Tokens     int
	Window     time.Duration
	LatencyAvg time.Duration
	LatencyP50 time.Duration
	LatencyP95 time.Duration
	LatencyP99 time.Duration
	RPS        float64
	TPS        float64
}

// Snapshot returns the current aggregates over the sliding window. It prunes
// expired observations first. With no observations in the window all latency
// values are zero and RPS/TPS are zero.
func (r *Registry) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.prune(now)

	s := Snapshot{Window: r.window, Requests: len(r.obs)}
	if len(r.obs) == 0 {
		return s
	}

	durations := make([]time.Duration, len(r.obs))
	var total time.Duration
	for i, o := range r.obs {
		total += o.duration
		s.Tokens += o.tokens
		durations[i] = o.duration
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })

	s.LatencyAvg = total / time.Duration(len(r.obs))
	s.LatencyP50 = percentile(durations, 0.50)
	s.LatencyP95 = percentile(durations, 0.95)
	s.LatencyP99 = percentile(durations, 0.99)

	windowSec := r.window.Seconds()
	s.RPS = float64(len(r.obs)) / windowSec
	s.TPS = float64(s.Tokens) / windowSec
	return s
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

// Handler returns the http.Handler for GET /metrics. It renders only safe
// numeric aggregates in Prometheus text format; no plaintext, secret, prompt,
// request/response text or token value is ever emitted.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s := r.Snapshot()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "# HELP pii_latency_seconds Average request latency over the window.\n")
		fmt.Fprintf(w, "# TYPE pii_latency_seconds gauge\n")
		fmt.Fprintf(w, "pii_latency_seconds %s\n", formatDuration(s.LatencyAvg))
		fmt.Fprintf(w, "# HELP pii_latency_p50_seconds Median request latency over the window.\n")
		fmt.Fprintf(w, "# TYPE pii_latency_p50_seconds gauge\n")
		fmt.Fprintf(w, "pii_latency_p50_seconds %s\n", formatDuration(s.LatencyP50))
		fmt.Fprintf(w, "# HELP pii_latency_p95_seconds 95th percentile request latency over the window.\n")
		fmt.Fprintf(w, "# TYPE pii_latency_p95_seconds gauge\n")
		fmt.Fprintf(w, "pii_latency_p95_seconds %s\n", formatDuration(s.LatencyP95))
		fmt.Fprintf(w, "# HELP pii_latency_p99_seconds 99th percentile request latency over the window.\n")
		fmt.Fprintf(w, "# TYPE pii_latency_p99_seconds gauge\n")
		fmt.Fprintf(w, "pii_latency_p99_seconds %s\n", formatDuration(s.LatencyP99))
		fmt.Fprintf(w, "# HELP pii_rps Requests per second over the window.\n")
		fmt.Fprintf(w, "# TYPE pii_rps gauge\n")
		fmt.Fprintf(w, "pii_rps %g\n", s.RPS)
		fmt.Fprintf(w, "# HELP pii_tps Tokens per second over the window.\n")
		fmt.Fprintf(w, "# TYPE pii_tps gauge\n")
		fmt.Fprintf(w, "pii_tps %g\n", s.TPS)
	})
}

// formatDuration renders a duration as seconds with microsecond precision.
func formatDuration(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', 6, 64)
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
