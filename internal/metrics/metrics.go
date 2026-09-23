// Package metrics exposes the service metrics in the Prometheus format. It
// records only numeric aggregates labelled with values from closed sets (fixed
// build metadata, route templates, status codes, dependency names and canonical
// PII type names). It never accepts or exposes plaintext PII, secrets, prompts,
// request/response texts, scope IDs, payload IDs or token values.
package metrics

import (
	"net/http"
	"strings"

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
}

// Metrics owns a private Prometheus registry with the Go runtime and process
// collectors and the service metrics. A nil *Metrics is valid: every recording
// method is a no-op, so optional wiring and tests need no guards.
type Metrics struct {
	reg *prometheus.Registry
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
	return &Metrics{reg: reg}
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
