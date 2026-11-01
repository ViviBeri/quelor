package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/coherera/quelor/pkg/engine"
)

func TestHistogramBuckets(t *testing.T) {
	h := newHistogram(acquireBuckets)
	samples := []float64{0.0001, 0.0005, 1, 1.1}
	for _, s := range samples {
		h.observe(s)
	}
	text := renderMetrics(engine.Stats{}, h.snapshot())

	want := []struct {
		le string
		n  uint64
	}{
		{"0.0001", 1},
		{"0.0005", 2},
		{"0.001", 2},
		{"0.005", 2},
		{"0.01", 2},
		{"0.05", 2},
		{"0.1", 2},
		{"0.5", 2},
		{"1", 3},
		{"+Inf", 4},
	}
	for _, tt := range want {
		got := bucketCount(t, text, tt.le)
		if got != tt.n {
			t.Errorf("le=%s count=%d want %d", tt.le, got, tt.n)
		}
	}
	if !strings.Contains(text, "quelor_acquire_duration_seconds_count 4\n") {
		t.Fatalf("count line missing:\n%s", text)
	}
	var prev uint64
	for i, tt := range want {
		got := bucketCount(t, text, tt.le)
		if i > 0 && got < prev {
			t.Fatalf("bucket %s went backwards", tt.le)
		}
		prev = got
	}
}

func TestRenderMetricsStats(t *testing.T) {
	text := renderMetrics(engine.Stats{
		AcquireAdmit:     11,
		AcquireDefer:     12,
		AcquireShed:      13,
		AcquireDeny:      14,
		Renews:           15,
		RenewRefused:     16,
		Settles:          17,
		DuplicateSettles: 18,
		BoundUnreachable: 19,
		AIMDDecreases:    20,
		Expired:          21,
		TraceErrors:      22,
		ActiveLeases:     23,
	}, newHistogram(acquireBuckets).snapshot())

	checks := []string{
		"# HELP quelor_acquire_total ",
		"# TYPE quelor_acquire_total counter",
		"quelor_acquire_total{verdict=\"admit\"} 11\n",
		"quelor_acquire_total{verdict=\"defer\"} 12\n",
		"quelor_acquire_total{verdict=\"shed\"} 13\n",
		"quelor_acquire_total{verdict=\"deny\"} 14\n",
		"# TYPE quelor_renew_total counter",
		"quelor_renew_total 15\n",
		"quelor_renew_refused_total 16\n",
		"quelor_settle_total 17\n",
		"quelor_settle_duplicate_total 18\n",
		"quelor_bound_unreachable_total 19\n",
		"quelor_aimd_decrease_total 20\n",
		"quelor_lease_expired_total 21\n",
		"quelor_trace_errors_total 22\n",
		"# TYPE quelor_active_leases gauge",
		"quelor_active_leases 23\n",
		"# TYPE quelor_acquire_duration_seconds histogram",
		"quelor_acquire_duration_seconds_bucket{le=\"+Inf\"} 0\n",
		"quelor_acquire_duration_seconds_count 0\n",
	}
	for _, c := range checks {
		if !strings.Contains(text, c) {
			t.Errorf("missing %q", c)
		}
	}
}

func bucketCount(t *testing.T, text, le string) uint64 {
	t.Helper()
	prefix := `quelor_acquire_duration_seconds_bucket{le="` + le + `"} `
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			var n uint64
			if _, err := fmt.Sscanf(strings.TrimPrefix(line, prefix), "%d", &n); err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatalf("missing bucket %s\n%s", le, text)
	return 0
}
