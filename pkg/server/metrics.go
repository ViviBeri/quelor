package server

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/coherera/quelor/pkg/engine"
)

// acquireBuckets are Prometheus histogram bounds in seconds.
// The +Inf bucket is implicit.
var acquireBuckets = []bucket{
	{0.0001, "0.0001"},
	{0.0005, "0.0005"},
	{0.001, "0.001"},
	{0.005, "0.005"},
	{0.01, "0.01"},
	{0.05, "0.05"},
	{0.1, "0.1"},
	{0.5, "0.5"},
	{1, "1"},
}

type bucket struct {
	bound float64
	le    string
}

// histogram is handler-local acquire latency. Other series come from Stats.
type histogram struct {
	mu     sync.Mutex
	bounds []bucket
	counts []uint64
	sum    float64
	n      uint64
}

type histSnap struct {
	bounds []bucket
	counts []uint64
	sum    float64
	n      uint64
}

func newHistogram(bounds []bucket) *histogram {
	return &histogram{
		bounds: bounds,
		counts: make([]uint64, len(bounds)+1),
	}
}

func (h *histogram) observe(v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sum += v
	h.n++
	for i := range h.bounds {
		if v <= h.bounds[i].bound {
			h.counts[i]++
			return
		}
	}
	h.counts[len(h.bounds)]++
}

func (h *histogram) snapshot() histSnap {
	h.mu.Lock()
	defer h.mu.Unlock()
	return histSnap{
		bounds: h.bounds,
		counts: append([]uint64(nil), h.counts...),
		sum:    h.sum,
		n:      h.n,
	}
}

func renderMetrics(st engine.Stats, h histSnap) string {
	var b strings.Builder
	writeLabeledCounter(&b, "quelor_acquire_total", "Admission decisions by verdict.", "verdict", []labeledValue{
		{"admit", st.AcquireAdmit},
		{"defer", st.AcquireDefer},
		{"shed", st.AcquireShed},
		{"deny", st.AcquireDeny},
	})
	writeCounter(&b, "quelor_renew_total", "Lease renewals.", st.Renews)
	writeCounter(&b, "quelor_renew_refused_total", "Refused lease renewals.", st.RenewRefused)
	writeCounter(&b, "quelor_settle_total", "Lease settlements.", st.Settles)
	writeCounter(&b, "quelor_settle_duplicate_total", "Duplicate lease settlements.", st.DuplicateSettles)
	writeCounter(&b, "quelor_bound_unreachable_total", "Decisions stamped bound-unreachable.", st.BoundUnreachable)
	writeCounter(&b, "quelor_aimd_decrease_total", "AIMD decreases applied from upstream observations.", st.AIMDDecreases)
	writeCounter(&b, "quelor_lease_expired_total", "Leases expired by the engine.", st.Expired)
	writeCounter(&b, "quelor_trace_errors_total", "Trace write errors.", st.TraceErrors)
	writeGauge(&b, "quelor_active_leases", "Leases currently held.", st.ActiveLeases)
	writeHistogram(&b, "quelor_acquire_duration_seconds", "Acquire handler latency in seconds.", h)
	return b.String()
}

type labeledValue struct {
	label string
	value int64
}

func writeLabeledCounter(b *strings.Builder, name, help, label string, values []labeledValue) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	for _, v := range values {
		fmt.Fprintf(b, "%s{%s=%q} %d\n", name, label, v.label, v.value)
	}
}

func writeCounter(b *strings.Builder, name, help string, v int64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
}

func writeGauge(b *strings.Builder, name, help string, v int64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, v)
}

func writeHistogram(b *strings.Builder, name, help string, h histSnap) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	var cum uint64
	for i, bk := range h.bounds {
		cum += h.counts[i]
		fmt.Fprintf(b, "%s_bucket{le=%q} %d\n", name, bk.le, cum)
	}
	if len(h.counts) > len(h.bounds) {
		cum += h.counts[len(h.bounds)]
	}
	fmt.Fprintf(b, "%s_bucket{le=\"+Inf\"} %d\n", name, cum)
	fmt.Fprintf(b, "%s_sum %s\n", name, strconv.FormatFloat(h.sum, 'f', -1, 64))
	fmt.Fprintf(b, "%s_count %d\n", name, h.n)
}
