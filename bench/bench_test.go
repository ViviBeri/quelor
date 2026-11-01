package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/coherera/quelor/pkg/resource"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coherera/quelor/pkg/quelor"
)

func TestNormalizeOptions(t *testing.T) {
	tests := []struct {
		name string
		in   Options
		want Options
	}{
		{
			name: "zeros use defaults",
			in:   Options{},
			want: Options{Units: defaultUnits, Seed: defaultSeed, LatencySamples: defaultLatencySamples},
		},
		{
			name: "explicit values stay",
			in:   Options{Units: 10, Seed: 7, LatencySamples: 3},
			want: Options{Units: 10, Seed: 7, LatencySamples: 3},
		},
		{
			name: "negative units and samples default, nonzero seed stays",
			in:   Options{Units: -1, Seed: -5, LatencySamples: -2},
			want: Options{Units: defaultUnits, Seed: -5, LatencySamples: defaultLatencySamples},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeOptions(tt.in)
			if got != tt.want {
				t.Fatalf("got %+v want %+v", got, tt.want)
			}
		})
	}
}

func TestFakeClockAdvance(t *testing.T) {
	start := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		deltas []time.Duration
		want   time.Time
	}{
		{name: "no movement", deltas: nil, want: start},
		{name: "one second", deltas: []time.Duration{time.Second}, want: start.Add(time.Second)},
		{
			name:   "sum",
			deltas: []time.Duration{time.Hour, 30 * time.Minute, -time.Minute},
			want:   start.Add(89 * time.Minute),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewFakeClock(start)
			for _, d := range tt.deltas {
				c.Advance(d)
			}
			if !c.Now().Equal(tt.want) {
				t.Fatalf("now %s want %s", c.Now(), tt.want)
			}
		})
	}
}

func TestFakeClockConcurrent(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewFakeClock(start)
	const goroutines = 8
	const steps = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < steps; i++ {
				c.Advance(time.Millisecond)
				_ = c.Now()
			}
		}()
	}
	wg.Wait()
	want := start.Add(time.Duration(goroutines*steps) * time.Millisecond)
	if !c.Now().Equal(want) {
		t.Fatalf("now %s want %s", c.Now(), want)
	}
}

func TestNewFakeClockZero(t *testing.T) {
	c := NewFakeClock(time.Time{})
	if c.Now().IsZero() {
		t.Fatal("zero start should be replaced")
	}
}

func TestPercentile(t *testing.T) {
	tests := []struct {
		name    string
		samples []time.Duration
		p       int
		want    time.Duration
	}{
		{name: "empty", samples: nil, p: 50, want: 0},
		{name: "single p50", samples: []time.Duration{5}, p: 50, want: 5},
		{name: "single p99", samples: []time.Duration{5}, p: 99, want: 5},
		{name: "ten p50", samples: []time.Duration{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, p: 50, want: 5},
		{name: "ten p99", samples: []time.Duration{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, p: 99, want: 9},
		{name: "ten p100", samples: []time.Duration{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, p: 100, want: 9},
		{name: "unsorted p50", samples: []time.Duration{9, 0, 5}, p: 50, want: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := percentile(tt.samples, tt.p)
			if got != tt.want {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestMeanCorrectness(t *testing.T) {
	tests := []struct {
		name string
		in   []ScenarioResult
		want float64
	}{
		{name: "empty", in: nil, want: 0},
		{
			name: "half and one",
			in: []ScenarioResult{
				{CorrectDefers: 1, TotalEmpty: 2},
				{CorrectDefers: 1, TotalEmpty: 1},
			},
			want: 0.75,
		},
		{
			name: "zero denominator",
			in:   []ScenarioResult{{CorrectDefers: 5, TotalEmpty: 0}},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := meanCorrectness(tt.in)
			if got != tt.want {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestReportStringLabels(t *testing.T) {
	labels := []string{
		"p50/p99 acquire",
		"throughput",
		"correctness under scarcity",
		"false-red rate",
	}
	cases := []Report{
		{},
		{
			AcquireP50:  time.Microsecond,
			AcquireP99:  3 * time.Microsecond,
			Throughput:  12.5,
			Correctness: 1,
			FalseRed:    0.25,
			Scenarios:   []ScenarioResult{{Name: scenarioBucket, Units: 1}},
		},
	}
	for i, rep := range cases {
		text := rep.String()
		lines := strings.Split(text, "\n")
		if len(lines) < 4 {
			t.Fatalf("case %d too short: %q", i, text)
		}
		for j, label := range labels {
			if !strings.Contains(lines[j], label) {
				t.Errorf("case %d line %d %q missing %q", i, j, lines[j], label)
			}
		}
	}
}

func TestReportJSONRoundTrip(t *testing.T) {
	in := Report{
		AcquireP50:  1500,
		AcquireP99:  9000,
		Throughput:  42.5,
		Correctness: 1,
		FalseRed:    0,
		Scenarios: []ScenarioResult{{
			Name:          scenarioBucket,
			Units:         3,
			CorrectDefers: 2,
			TotalEmpty:    2,
		}},
	}
	var out Report
	if err := json.Unmarshal([]byte(in.JSON()), &out); err != nil {
		t.Fatal(err)
	}
	if out.Throughput != in.Throughput || out.FalseRed != in.FalseRed || out.AcquireP99 != in.AcquireP99 {
		t.Fatalf("scalars %+v", out)
	}
	if len(out.Scenarios) != 1 || out.Scenarios[0].Name != scenarioBucket || out.Scenarios[0].TotalEmpty != 2 {
		t.Fatalf("scenarios %+v", out.Scenarios)
	}
}

func TestScenarioInvariants(t *testing.T) {
	opt := Options{Units: 3000, Seed: 1, LatencySamples: 512}
	var rep Report
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(rep.String())
		}
	})
	rep = RunAll(opt)

	text := rep.String()
	lines := strings.Split(text, "\n")
	labels := []string{
		"p50/p99 acquire",
		"throughput",
		"correctness under scarcity",
		"false-red rate",
	}
	if len(lines) < 4 {
		t.Fatalf("report too short:\n%s", text)
	}
	for i, label := range labels {
		if !strings.Contains(lines[i], label) {
			t.Errorf("line %d %q missing %q", i, lines[i], label)
		}
	}
	if rep.FalseRed != 0 {
		t.Fatalf("FalseRed = %v", rep.FalseRed)
	}
	if rep.Throughput <= 0 {
		t.Fatal("throughput not measured")
	}
	if rep.AcquireP99 < rep.AcquireP50 {
		t.Fatalf("p99 %s < p50 %s", rep.AcquireP99, rep.AcquireP50)
	}
	var decoded Report
	if err := json.Unmarshal([]byte(rep.JSON()), &decoded); err != nil {
		t.Fatalf("json: %v", err)
	}
	if decoded.FalseRed != 0 || len(decoded.Scenarios) != 3 {
		t.Fatalf("json report scenarios=%d false_red=%v", len(decoded.Scenarios), decoded.FalseRed)
	}

	byName := make(map[string]ScenarioResult, len(rep.Scenarios))
	for _, s := range rep.Scenarios {
		byName[s.Name] = s
	}

	bucket, ok := byName[scenarioBucket]
	if !ok {
		t.Fatalf("missing %s", scenarioBucket)
	}
	if bucket.AdmittedIntoEmpty != 0 {
		t.Fatalf("admitted into empty bucket: %d", bucket.AdmittedIntoEmpty)
	}
	if bucket.TotalEmpty == 0 || bucket.CorrectDefers != bucket.TotalEmpty {
		t.Fatalf("empty-bucket defers: correct=%d total=%d", bucket.CorrectDefers, bucket.TotalEmpty)
	}
	resumed, err := notesInt(bucket.Notes, "resumed_admits")
	if err != nil || resumed <= 0 {
		t.Fatalf("admissions did not resume: %s", bucket.Notes)
	}

	storm, ok := byName[scenarioStorm]
	if !ok {
		t.Fatalf("missing %s", scenarioStorm)
	}
	counts, err := parseCSVInts(notesValue(storm.Notes, "w429"))
	if err != nil {
		t.Fatalf("w429: %v (%s)", err, storm.Notes)
	}
	if len(counts) < stormWindows {
		t.Fatalf("windows %v", counts)
	}
	if counts[0] <= 0 {
		t.Fatalf("window 1 recorded no 429s: %v", counts)
	}
	for i := 1; i <= 5; i++ {
		if counts[i] >= counts[0] {
			t.Fatalf("window %d count %d not < window 1 %d (%v)", i+1, counts[i], counts[0], counts)
		}
	}
	if notesValue(storm.Notes, "real_defect_ok") != "true" {
		t.Fatalf("real-defect classification: %s", storm.Notes)
	}

	flap, ok := byName[scenarioFlap]
	if !ok {
		t.Fatalf("missing %s", scenarioFlap)
	}
	down, err := notesInt(flap.Notes, "admitted_while_down")
	if err != nil {
		t.Fatal(err)
	}
	wantBound := flapThreshold + int(flapDownFor/flapOpenFor)
	gotBound, err := notesInt(flap.Notes, "bound")
	if err != nil {
		t.Fatal(err)
	}
	if gotBound != wantBound {
		t.Fatalf("recorded bound %d want %d", gotBound, wantBound)
	}
	if down > wantBound {
		t.Fatalf("admitted while down %d exceeds bound %d (%s)", down, wantBound, flap.Notes)
	}
	if notesValue(flap.Notes, "resumed") != "true" {
		t.Fatalf("circuit did not resume: %s", flap.Notes)
	}
	if notesValue(flap.Notes, "resume_within_open_for") != "true" {
		t.Fatalf("resume outside OpenFor: %s", flap.Notes)
	}
}

func TestProvesRedAlwaysAdmit(t *testing.T) {
	var _ quelor.LeaseAPI = (*alwaysAdmit)(nil)
	tests := []struct {
		name          string
		units         int
		wantIntoEmpty int
	}{
		{name: "below bucket capacity", units: 100, wantIntoEmpty: 0},
		{name: "above bucket capacity", units: 500, wantIntoEmpty: holdersN*500 - bucketCapacity},
		{name: "far above", units: 1000, wantIntoEmpty: holdersN*1000 - bucketCapacity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := RunSharedTokenBucket(Options{Units: tt.units, Seed: 1}, func(clock *FakeClock) (quelor.LeaseAPI, AdvanceFunc, error) {
				if clock == nil {
					return nil, nil, fmt.Errorf("nil clock")
				}
				return &alwaysAdmit{}, nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.AdmittedIntoEmpty != tt.wantIntoEmpty {
				t.Fatalf("AdmittedIntoEmpty=%d want %d (%+v)", res.AdmittedIntoEmpty, tt.wantIntoEmpty, res)
			}
			if tt.wantIntoEmpty > 0 && res.AdmittedIntoEmpty == 0 {
				t.Fatal("broken stub must admit into an empty bucket")
			}
		})
	}
}

type alwaysAdmit struct {
	seq atomic.Int64
}

func (a *alwaysAdmit) AcquireLease(_ context.Context, r quelor.AcquireRequest) (quelor.AcquireResult, error) {
	n := a.seq.Add(1)
	now := time.Unix(0, 0).UTC()
	lease := &quelor.Lease{
		ID:        fmt.Sprintf("stub-%d", n),
		UnitID:    r.Envelope.UnitID,
		TenantID:  r.Envelope.TenantID,
		HolderID:  r.Envelope.HolderID,
		Declared:  copyIntMap(r.Envelope.Demands),
		GrantedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}
	return quelor.AcquireResult{
		Verdict:     quelor.Verdict{Kind: quelor.Admit, Reason: "always"},
		Lease:       lease,
		DecisionRef: lease.ID,
	}, nil
}

func (a *alwaysAdmit) RenewLease(_ context.Context, r quelor.RenewRequest) (quelor.RenewResult, error) {
	return quelor.RenewResult{
		Verdict:     quelor.Verdict{Kind: quelor.Admit, Reason: "always"},
		DecisionRef: r.LeaseID,
	}, nil
}

func (a *alwaysAdmit) SettleLease(_ context.Context, r quelor.SettleRequest) (quelor.SettleResult, error) {
	return quelor.SettleResult{
		LeaseID:  r.LeaseID,
		Consumed: copyIntMap(r.Consumed),
	}, nil
}

func notesValue(notes, key string) string {
	prefix := key + "="
	for _, f := range strings.Fields(notes) {
		if strings.HasPrefix(f, prefix) {
			return strings.TrimPrefix(f, prefix)
		}
	}
	return ""
}

func notesInt(notes, key string) (int, error) {
	v := notesValue(notes, key)
	if v == "" {
		return 0, fmt.Errorf("missing %s in %q", key, notes)
	}
	return strconv.Atoi(v)
}

func parseCSVInts(s string) ([]int, error) {
	if s == "" {
		return nil, fmt.Errorf("empty")
	}
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func mustOpen(b *testing.B, clock func() time.Time, ttl time.Duration, ms ...resource.Manifest) quelor.LeaseAPI {
	b.Helper()
	api, err := openAPI(clock, ttl, ms...)
	if err != nil {
		b.Fatalf("engine: %v", err)
	}
	if api == nil {
		b.Fatal("nil api")
	}
	return api
}

func BenchmarkAcquireSettle(b *testing.B) {
	api := mustOpen(b, time.Now, time.Minute, leaseManifest(resSlots, 1<<62))
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := strconv.Itoa(i)
		env := baseEnv("as-"+id, "ash-"+id, resSlots, 1)
		res, err := api.AcquireLease(ctx, quelor.AcquireRequest{Envelope: env})
		if err != nil {
			b.Fatal(err)
		}
		if res.Verdict.Kind != quelor.Admit || res.Lease == nil {
			b.Fatalf("acquire: %+v", res.Verdict)
		}
		if err := settleGranted(ctx, api, env.HolderID, res.Lease, env.Demands, nil, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAcquireParallel(b *testing.B) {
	api := mustOpen(b, time.Now, time.Minute, leaseManifest(resSlots, 1<<62))
	ctx := context.Background()
	var seq atomic.Uint64
	var failures atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := seq.Add(1)
			id := strconv.FormatUint(n, 10)
			env := baseEnv("p-"+id, "h-"+id, resSlots, 1)
			res, err := api.AcquireLease(ctx, quelor.AcquireRequest{Envelope: env})
			if err != nil || res.Verdict.Kind != quelor.Admit || res.Lease == nil {
				failures.Add(1)
				continue
			}
			if serr := settleGranted(ctx, api, env.HolderID, res.Lease, env.Demands, nil, nil); serr != nil {
				failures.Add(1)
			}
		}
	})
	if failures.Load() > 0 {
		b.Fatalf("parallel failures: %d", failures.Load())
	}
}

func BenchmarkAcquireDeferPath(b *testing.B) {
	api := mustOpen(b, time.Now, time.Minute, leaseManifest(resSlots, 1))
	ctx := context.Background()
	pin := baseEnv("pin", "pin-holder", resSlots, 1)
	res, err := api.AcquireLease(ctx, quelor.AcquireRequest{Envelope: pin})
	if err != nil {
		b.Fatal(err)
	}
	if res.Verdict.Kind != quelor.Admit || res.Lease == nil {
		b.Fatalf("pin: %+v", res.Verdict)
	}
	probe := baseEnv("probe", "probe-holder", resSlots, 1)
	pres, err := api.AcquireLease(ctx, quelor.AcquireRequest{Envelope: probe})
	if err != nil {
		b.Fatal(err)
	}
	if pres.Verdict.Kind != quelor.Defer {
		b.Fatalf("exhausted path: got %s", pres.Verdict.Kind)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := strconv.Itoa(i)
		env := baseEnv("d-"+id, "dh-"+id, resSlots, 1)
		if _, aerr := api.AcquireLease(ctx, quelor.AcquireRequest{Envelope: env}); aerr != nil {
			b.Fatal(aerr)
		}
	}
}

func BenchmarkMultiResourceAcquire(b *testing.B) {
	api := mustOpen(b, time.Now, time.Minute,
		leaseManifest("r0", 1<<62),
		leaseManifest("r1", 1<<62),
		leaseManifest("r2", 1<<62),
	)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := strconv.Itoa(i)
		env := quelor.Envelope{
			UnitID:   "m-" + id,
			TenantID: tenantID,
			HolderID: "mh-" + id,
			Demands: map[string]int64{
				"r0": 1,
				"r1": 1,
				"r2": 1,
			},
			Priority: quelor.PriorityChange,
		}
		res, err := api.AcquireLease(ctx, quelor.AcquireRequest{Envelope: env})
		if err != nil {
			b.Fatal(err)
		}
		if res.Verdict.Kind != quelor.Admit || res.Lease == nil {
			b.Fatalf("multi acquire: %+v", res.Verdict)
		}
		if err := settleGranted(ctx, api, env.HolderID, res.Lease, env.Demands, nil, nil); err != nil {
			b.Fatal(err)
		}
	}
}
