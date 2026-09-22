package loadbench

import (
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	valid := Config{
		BaseURL:       "http://127.0.0.1:8080",
		RPS:           1000,
		Duration:      time.Minute,
		PoolSize:      100,
		Concurrency:   50,
		Timeout:       time.Second,
		TargetLatency: time.Second,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cases := []struct {
		name string
		mut  func(*Config)
	}{
		{"empty base URL", func(c *Config) { c.BaseURL = "" }},
		{"zero RPS", func(c *Config) { c.RPS = 0 }},
		{"negative RPS", func(c *Config) { c.RPS = -1 }},
		{"zero duration", func(c *Config) { c.Duration = 0 }},
		{"zero pool", func(c *Config) { c.PoolSize = 0 }},
		{"zero concurrency", func(c *Config) { c.Concurrency = 0 }},
		{"zero timeout", func(c *Config) { c.Timeout = 0 }},
		{"zero target latency", func(c *Config) { c.TargetLatency = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := valid
			tc.mut(&c)
			if err := c.Validate(); err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}
}

func TestBuildReport(t *testing.T) {
	cfg := Config{RPS: 100, TargetLatency: time.Second}
	latencies := []time.Duration{
		10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond,
		40 * time.Millisecond, 50 * time.Millisecond, 60 * time.Millisecond,
		70 * time.Millisecond, 80 * time.Millisecond, 90 * time.Millisecond,
		100 * time.Millisecond,
	}
	r := buildReport(cfg, 10*time.Second, 10, 10, 10, 0, 0, latencies)

	if r.RequestedRPS != 100 {
		t.Errorf("RequestedRPS = %v, want 100", r.RequestedRPS)
	}
	if r.AchievedRPS != 1.0 {
		t.Errorf("AchievedRPS = %v, want 1.0", r.AchievedRPS)
	}
	if r.Planned != 10 || r.Scheduled != 10 || r.Completed != 10 || r.Errors != 0 || r.Unschedulable != 0 {
		t.Errorf("planned/scheduled/completed/errors/unschedulable = %d/%d/%d/%d/%d, want 10/10/10/0/0",
			r.Planned, r.Scheduled, r.Completed, r.Errors, r.Unschedulable)
	}
	if r.P50 != 50*time.Millisecond {
		t.Errorf("P50 = %v, want 50ms", r.P50)
	}
	if r.P95 != 100*time.Millisecond {
		t.Errorf("P95 = %v, want 100ms", r.P95)
	}
	if r.P99 != 100*time.Millisecond {
		t.Errorf("P99 = %v, want 100ms", r.P99)
	}
	if !r.TargetIsAppendix {
		t.Error("TargetIsAppendix = false, want true")
	}
	if !r.TargetMet {
		t.Error("TargetMet = false, want true (P99 <= 1s)")
	}
}

func TestBuildReportTargetNotMet(t *testing.T) {
	cfg := Config{RPS: 100, TargetLatency: time.Second}
	latencies := []time.Duration{2 * time.Second, 3 * time.Second}
	r := buildReport(cfg, time.Second, 2, 2, 2, 0, 0, latencies)
	if r.TargetMet {
		t.Error("TargetMet = true, want false (P99 > 1s)")
	}
}

func TestBuildReportEmptyLatencies(t *testing.T) {
	cfg := Config{RPS: 100, TargetLatency: time.Second}
	r := buildReport(cfg, time.Second, 0, 0, 0, 0, 0, nil)
	if r.P50 != 0 || r.P95 != 0 || r.P99 != 0 {
		t.Errorf("empty latencies percentiles = %v/%v/%v, want 0/0/0", r.P50, r.P95, r.P99)
	}
	if r.TargetMet {
		t.Error("TargetMet = true with no completed requests, want false")
	}
}

func TestBuildReportUnschedulable(t *testing.T) {
	cfg := Config{RPS: 100, TargetLatency: time.Second}
	r := buildReport(cfg, time.Second, 10, 8, 8, 0, 2, nil)
	if r.Planned != 10 || r.Scheduled != 8 || r.Unschedulable != 2 {
		t.Errorf("planned/scheduled/unschedulable = %d/%d/%d, want 10/8/2", r.Planned, r.Scheduled, r.Unschedulable)
	}
}

func TestPercentile(t *testing.T) {
	sorted := []time.Duration{10, 20, 30, 40, 50}
	cases := []struct {
		p    float64
		want time.Duration
	}{
		{0.50, 30},
		{0.95, 50},
		{0.99, 50},
		{0.00, 10},
		{1.00, 50},
	}
	for _, tc := range cases {
		if got := percentile(sorted, tc.p); got != tc.want {
			t.Errorf("percentile(%v) = %v, want %v", tc.p, got, tc.want)
		}
	}
}
