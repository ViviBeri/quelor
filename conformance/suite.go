package conformance

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/coherera/quelor/pkg/quelor"
)

const (
	suiteEngine      = "engine"
	callBudget       = 2 * time.Second
	defaultTenant    = "tenant-1"
	defaultHolder    = "holder-1"
	stampUnreachable = "bound-unreachable"
)

// ResourceSpec is one resource the factory must install.
// Unknown asks for a reading that is not known.
// Stale asks for a known reading that is stale; stale is not capacity.
// UnknownPolicy is "defer_short", "deny", or "hold_cursor".
// Spend is a hard budget that does not refill.
type ResourceSpec struct {
	ID            string
	Capacity      int64
	Spend         bool
	Unknown       bool
	Stale         bool
	UnknownPolicy string
}

// Spec is the engine configuration for one case.
// HolderMaxLeases <= 0 means the holder is not quota-limited.
// Now is the clock the factory must use for lease timestamps and freshness.
type Spec struct {
	Resources          []ResourceSpec
	SharedToken        string
	HolderMaxLeases    int
	DeciderUnavailable bool
	Now                func() time.Time
}

// Factory builds a LeaseAPI for one case. Each call is a fresh engine.
type Factory func(t *testing.T, spec Spec) quelor.LeaseAPI

// reporter is the subset of *testing.T the suite drives. The recording
// adapter lets a red-path test observe failures without failing itself.
type reporter interface {
	Run(name string, fn func(r reporter))
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Helper()
}

// RunEngine runs the published engine cases against newEngine.
func RunEngine(t *testing.T, newEngine Factory) Report {
	t.Helper()
	rep := &Report{}
	publishReport(t, rep)
	*rep = runEngine(tAdapter{t: t}, newEngine)
	return *rep
}

func runEngine(r reporter, newEngine Factory) Report {
	r.Helper()
	cases := []namedCase{
		{name: "verdicts/admit", fn: func(r reporter) { caseVerdictAdmit(r, newEngine) }},
		{name: "verdicts/defer", fn: func(r reporter) { caseVerdictDefer(r, newEngine) }},
		{name: "verdicts/shed", fn: func(r reporter) { caseVerdictShed(r, newEngine) }},
		{name: "verdicts/deny", fn: func(r reporter) { caseVerdictDeny(r, newEngine) }},
		{name: "unknown-is-not-available/defer_short", fn: func(r reporter) { caseUnknown(r, newEngine, "defer_short") }},
		{name: "unknown-is-not-available/deny", fn: func(r reporter) { caseUnknown(r, newEngine, "deny") }},
		{name: "fail-static/decider-unreachable", fn: func(r reporter) { caseDeciderUnreachable(r, newEngine) }},
		{name: "fail-static/never-fail-open-on-spend", fn: func(r reporter) { caseNeverFailOpenOnSpend(r, newEngine) }},
		{name: "idempotent-settle", fn: func(r reporter) { caseIdempotentSettle(r, newEngine) }},
		{name: "settle-declared-vs-consumed", fn: func(r reporter) { caseDeclaredVsConsumed(r, newEngine) }},
		{name: "meta-admission/holder-quota", fn: func(r reporter) { caseHolderQuota(r, newEngine) }},
		{name: "meta-admission/authenticated-holder", fn: func(r reporter) { caseAuthenticatedHolder(r, newEngine) }},
		{name: "atomic-multi-resource", fn: func(r reporter) { caseAtomicMultiResource(r, newEngine) }},
		{name: "renew-refusal-hibernate", fn: func(r reporter) { caseRenewHibernate(r, newEngine) }},
	}
	return collect(r, suiteEngine, cases)
}

func caseVerdictAdmit(r reporter, newEngine Factory) {
	r.Helper()
	nowT, now := standardNow()
	api := engineFrom(r, newEngine, Spec{
		Resources: []ResourceSpec{{ID: "slots", Capacity: 4}},
		Now:       now,
	})
	e := makeUnit("u-admit", "", map[string]int64{"slots": 1}, "", "", 0).envelope()
	res := mustAcquire(r, api, e, "")
	assertAdmit(r, res, e, nowT)
}

func caseVerdictDefer(r reporter, newEngine Factory) {
	r.Helper()
	_, now := standardNow()
	api := engineFrom(r, newEngine, Spec{
		Resources: []ResourceSpec{{ID: "slots", Capacity: 1}},
		Now:       now,
	})
	first := makeUnit("u-hold", "", map[string]int64{"slots": 1}, "", "", 0).envelope()
	held := mustAcquire(r, api, first, "")
	assertAdmit(r, held, first, now())
	second := makeUnit("u-wait", "", map[string]int64{"slots": 1}, "", "", 0).envelope()
	res := mustAcquire(r, api, second, "")
	if res.Verdict.Kind != quelor.Defer {
		r.Fatalf("exhausted capacity: verdict %s (%s), want DEFER", res.Verdict.Kind, res.Verdict.Reason)
	}
	if res.Lease != nil {
		r.Fatalf("DEFER issued lease %+v", res.Lease)
	}
	if res.Verdict.WakeAt == nil && res.Verdict.WakeToken == "" {
		r.Fatalf("DEFER missing WakeAt and WakeToken")
	}
}

func caseVerdictShed(r reporter, newEngine Factory) {
	r.Helper()
	nowT, now := standardNow()
	api := engineFrom(r, newEngine, Spec{
		Resources: []ResourceSpec{{ID: "slots", Capacity: 8}},
		Now:       now,
	})
	old := makeUnit("u-keep", "", map[string]int64{"slots": 1}, "k1", quelor.CoalesceDropNew, 1).envelope()
	kept := mustAcquire(r, api, old, "")
	assertAdmit(r, kept, old, nowT)
	dup := makeUnit("u-drop", "", map[string]int64{"slots": 1}, "k1", quelor.CoalesceDropNew, 2).envelope()
	res := mustAcquire(r, api, dup, "")
	if res.Verdict.Kind != quelor.Shed {
		r.Fatalf("DROP_NEW duplicate: verdict %s (%s), want SHED", res.Verdict.Kind, res.Verdict.Reason)
	}
	if res.Lease != nil {
		r.Fatalf("SHED issued lease %+v", res.Lease)
	}
	settled := mustSettle(r, api, kept.Lease, map[string]int64{"slots": 1})
	if settled.Duplicate {
		r.Fatalf("kept unit was already settled")
	}
}

func caseVerdictDeny(r reporter, newEngine Factory) {
	r.Helper()
	_, now := standardNow()
	authed := engineFrom(r, newEngine, Spec{
		Resources:   []ResourceSpec{{ID: "slots", Capacity: 4}},
		SharedToken: "secret",
		Now:         now,
	})
	unauth := makeUnit("u-anon", "holder-anon", map[string]int64{"slots": 1}, "", "", 0).envelope()
	denied := mustAcquire(r, authed, unauth, "")
	assertKind(r, denied, quelor.Deny)

	capped := engineFrom(r, newEngine, Spec{
		Resources:       []ResourceSpec{{ID: "slots", Capacity: 8}},
		HolderMaxLeases: 1,
		Now:             now,
	})
	ok := makeUnit("u-q1", "holder-q", map[string]int64{"slots": 1}, "", "", 0).envelope()
	first := mustAcquire(r, capped, ok, "")
	assertAdmit(r, first, ok, now())
	over := makeUnit("u-q2", "holder-q", map[string]int64{"slots": 1}, "", "", 0).envelope()
	quota := mustAcquire(r, capped, over, "")
	assertKind(r, quota, quelor.Deny)
}

func caseUnknown(r reporter, newEngine Factory, policy string) {
	r.Helper()
	_, now := standardNow()
	unknown := assertUnusable(r, newEngine, ResourceSpec{
		ID:            "slots",
		Capacity:      10,
		Unknown:       true,
		UnknownPolicy: policy,
	}, now)
	stale := assertUnusable(r, newEngine, ResourceSpec{
		ID:            "slots",
		Capacity:      10,
		Stale:         true,
		UnknownPolicy: policy,
	}, now)
	if unknown.Kind != stale.Kind {
		r.Fatalf("stale kind %s, unknown kind %s", stale.Kind, unknown.Kind)
	}
}

func caseDeciderUnreachable(r reporter, newEngine Factory) {
	r.Helper()
	_, now := standardNow()
	spendAPI := engineFrom(r, newEngine, Spec{
		Resources: []ResourceSpec{{
			ID:       "spend",
			Capacity: 100,
			Spend:    true,
		}},
		DeciderUnavailable: true,
		Now:                now,
	})
	spendEnv := makeUnit("u-spend", "", map[string]int64{"spend": 1}, "", "", 0).envelope()
	spend := mustAcquire(r, spendAPI, spendEnv, "")
	assertRefused(r, spend)
	if !contains(spend.Verdict.Stamps, stampUnreachable) {
		r.Fatalf("stamps %v, want %s", spend.Verdict.Stamps, stampUnreachable)
	}

	localAPI := engineFrom(r, newEngine, Spec{
		Resources: []ResourceSpec{{
			ID:            "slots",
			Capacity:      4,
			UnknownPolicy: "hold_cursor",
		}},
		DeciderUnavailable: true,
		Now:                now,
	})
	localEnv := makeUnit("u-local", "", map[string]int64{"slots": 1}, "", "", 0).envelope()
	local := mustAcquire(r, localAPI, localEnv, "")
	assertAdmit(r, local, localEnv, now())
}

func caseNeverFailOpenOnSpend(r reporter, newEngine Factory) {
	r.Helper()
	_, now := standardNow()
	api := engineFrom(r, newEngine, Spec{
		Resources: []ResourceSpec{{
			ID:            "spend",
			Capacity:      80,
			Spend:         true,
			UnknownPolicy: "hold_cursor",
		}},
		DeciderUnavailable: true,
		Now:                now,
	})
	e := makeUnit("u-closed", "", map[string]int64{"spend": 1}, "", "", 0).envelope()
	res := mustAcquire(r, api, e, "")
	assertRefused(r, res)
}

func caseIdempotentSettle(r reporter, newEngine Factory) {
	r.Helper()
	const cap int64 = 4
	nowT, now := standardNow()
	api := engineFrom(r, newEngine, Spec{
		Resources: []ResourceSpec{{ID: "slots", Capacity: cap}},
		Now:       now,
	})
	demands := map[string]int64{"slots": cap}
	e := makeUnit("u-once", "", demands, "", "", 0).envelope()
	got := mustAcquire(r, api, e, "")
	assertAdmit(r, got, e, nowT)
	consumed := map[string]int64{"slots": cap / 2}
	first := mustSettle(r, api, got.Lease, consumed)
	if first.Duplicate {
		r.Fatalf("first settle marked duplicate")
	}
	second := mustSettle(r, api, got.Lease, consumed)
	if !second.Duplicate {
		r.Fatalf("second settle Duplicate false")
	}
	if !mapsEqual(first.Declared, second.Declared) || !mapsEqual(first.Consumed, second.Consumed) {
		r.Fatalf("duplicate settle diverged declared %v/%v consumed %v/%v", first.Declared, second.Declared, first.Consumed, second.Consumed)
	}
	if !mapsEqual(first.Declared, demands) || !mapsEqual(first.Consumed, consumed) {
		r.Fatalf("settle declared %v consumed %v", first.Declared, first.Consumed)
	}
	againEnv := makeUnit("u-again", "", demands, "", "", 0).envelope()
	again := mustAcquire(r, api, againEnv, "")
	assertAdmit(r, again, againEnv, nowT)
	overEnv := makeUnit("u-over", "", demands, "", "", 0).envelope()
	over := mustAcquire(r, api, overEnv, "")
	if over.Verdict.Kind != quelor.Defer {
		r.Fatalf("after exact re-acquire: verdict %s, want DEFER", over.Verdict.Kind)
	}
	if over.Lease != nil {
		r.Fatalf("over-acquire issued lease")
	}
}

func caseDeclaredVsConsumed(r reporter, newEngine Factory) {
	r.Helper()
	nowT, now := standardNow()
	api := engineFrom(r, newEngine, Spec{
		Resources: []ResourceSpec{{ID: "slots", Capacity: 9}},
		Now:       now,
	})
	demands := map[string]int64{"slots": 5}
	e := makeUnit("u-delta", "", demands, "", "", 0).envelope()
	got := mustAcquire(r, api, e, "")
	assertAdmit(r, got, e, nowT)
	consumed := map[string]int64{"slots": 3}
	res := mustSettle(r, api, got.Lease, consumed)
	if res.Duplicate {
		r.Fatalf("settle marked duplicate")
	}
	if !mapsEqual(res.Declared, demands) {
		r.Fatalf("declared %v, want %v", res.Declared, demands)
	}
	if !mapsEqual(res.Consumed, consumed) {
		r.Fatalf("consumed %v, want %v", res.Consumed, consumed)
	}
	assertDelta(r, res.Declared, res.Consumed, res.Delta)
}

func caseHolderQuota(r reporter, newEngine Factory) {
	r.Helper()
	nowT, now := standardNow()
	api := engineFrom(r, newEngine, Spec{
		Resources:       []ResourceSpec{{ID: "slots", Capacity: 10}},
		HolderMaxLeases: 2,
		Now:             now,
	})
	a1 := makeUnit("u-a1", "holder-a", map[string]int64{"slots": 1}, "", "", 0).envelope()
	a2 := makeUnit("u-a2", "holder-a", map[string]int64{"slots": 1}, "", "", 0).envelope()
	a3 := makeUnit("u-a3", "holder-a", map[string]int64{"slots": 1}, "", "", 0).envelope()
	b1 := makeUnit("u-b1", "holder-b", map[string]int64{"slots": 1}, "", "", 0).envelope()
	first := mustAcquire(r, api, a1, "")
	assertAdmit(r, first, a1, nowT)
	second := mustAcquire(r, api, a2, "")
	assertAdmit(r, second, a2, nowT)
	third := mustAcquire(r, api, a3, "")
	assertKind(r, third, quelor.Deny)
	other := mustAcquire(r, api, b1, "")
	assertAdmit(r, other, b1, nowT)
}

func caseAuthenticatedHolder(r reporter, newEngine Factory) {
	r.Helper()
	nowT, now := standardNow()
	api := engineFrom(r, newEngine, Spec{
		Resources:   []ResourceSpec{{ID: "slots", Capacity: 4}},
		SharedToken: "token-ok",
		Now:         now,
	})
	bad := makeUnit("u-bad", "holder-1", map[string]int64{"slots": 1}, "", "", 0).envelope()
	denied := mustAcquire(r, api, bad, "nope")
	assertKind(r, denied, quelor.Deny)
	good := makeUnit("u-good", "holder-1", map[string]int64{"slots": 1}, "", "", 0).envelope()
	ok := mustAcquire(r, api, good, "token-ok")
	assertAdmit(r, ok, good, nowT)
}

func caseAtomicMultiResource(r reporter, newEngine Factory) {
	r.Helper()
	nowT, now := standardNow()
	api := engineFrom(r, newEngine, Spec{
		Resources: []ResourceSpec{
			{ID: "alpha", Capacity: 10},
			{ID: "beta", Capacity: 2},
		},
		Now: now,
	})
	both := makeUnit("u-both", "", map[string]int64{"alpha": 10, "beta": 5}, "", "", 0).envelope()
	res, err := acquire(r, api, both, "")
	if err != nil {
		r.Fatalf("short multi-resource acquire: %v", err)
	}
	assertRefused(r, res)
	alpha := makeUnit("u-alpha", "", map[string]int64{"alpha": 10}, "", "", 0).envelope()
	got := mustAcquire(r, api, alpha, "")
	assertAdmit(r, got, alpha, nowT)
	beta := makeUnit("u-beta", "holder-2", map[string]int64{"beta": 2}, "", "", 0).envelope()
	gotBeta := mustAcquire(r, api, beta, "")
	assertAdmit(r, gotBeta, beta, nowT)
}

func caseRenewHibernate(r reporter, newEngine Factory) {
	r.Helper()
	nowT, now := standardNow()
	api := engineFrom(r, newEngine, Spec{
		Resources: []ResourceSpec{{ID: "slots", Capacity: 1}},
		Now:       now,
	})
	e := makeUnit("u-prog", "", map[string]int64{"slots": 1}, "", "", 0).envelope()
	got := mustAcquire(r, api, e, "")
	assertAdmit(r, got, e, nowT)
	res := mustRenew(r, api, got.Lease, map[string]int64{"slots": 1})
	if res.Verdict.Kind == quelor.Admit || !res.Verdict.Hibernate {
		r.Fatalf("renew when empty: kind=%s hibernate=%v reason=%s", res.Verdict.Kind, res.Verdict.Hibernate, res.Verdict.Reason)
	}
}

func assertUnusable(r reporter, newEngine Factory, rs ResourceSpec, now func() time.Time) quelor.Verdict {
	r.Helper()
	api := engineFrom(r, newEngine, Spec{Resources: []ResourceSpec{rs}, Now: now})
	e := makeUnit("u-unusable", "", map[string]int64{rs.ID: 1}, "", "", 0).envelope()
	res := mustAcquire(r, api, e, "")
	assertRefused(r, res)
	return res.Verdict
}

func assertAdmit(r reporter, res quelor.AcquireResult, e quelor.Envelope, now time.Time) {
	r.Helper()
	if res.Verdict.Kind != quelor.Admit {
		r.Fatalf("verdict %s (%s), want ADMIT", res.Verdict.Kind, res.Verdict.Reason)
	}
	if res.Lease == nil {
		r.Fatalf("ADMIT without lease")
	}
	l := res.Lease
	if l.ID == "" || l.UnitID != e.UnitID || l.HolderID != e.HolderID || l.TenantID != e.TenantID {
		r.Fatalf("lease identity id=%q unit=%q holder=%q tenant=%q", l.ID, l.UnitID, l.HolderID, l.TenantID)
	}
	if !mapsEqual(l.Declared, e.Demands) {
		r.Fatalf("declared %v demands %v", l.Declared, e.Demands)
	}
	if !l.GrantedAt.Equal(now) {
		r.Fatalf("granted_at %s, clock %s", l.GrantedAt.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	}
	if !l.ExpiresAt.After(l.GrantedAt) {
		r.Fatalf("expires_at %s not after grant", l.ExpiresAt.Format(time.RFC3339Nano))
	}
}

func assertKind(r reporter, res quelor.AcquireResult, kind quelor.VerdictKind) {
	r.Helper()
	if res.Verdict.Kind != kind {
		r.Fatalf("verdict %s (%s), want %s", res.Verdict.Kind, res.Verdict.Reason, kind)
	}
	if res.Lease != nil {
		r.Fatalf("%s issued lease %+v", kind, res.Lease)
	}
}

func assertRefused(r reporter, res quelor.AcquireResult) {
	r.Helper()
	switch res.Verdict.Kind {
	case quelor.Defer, quelor.Deny, quelor.Shed:
	default:
		r.Fatalf("verdict %q (%s), want a non-admit", res.Verdict.Kind, res.Verdict.Reason)
	}
	if res.Lease != nil {
		r.Fatalf("refusal issued lease %+v", res.Lease)
	}
}

func assertDelta(r reporter, declared, consumed, delta map[string]int64) {
	r.Helper()
	keys := map[string]struct{}{}
	for k := range declared {
		keys[k] = struct{}{}
	}
	for k := range consumed {
		keys[k] = struct{}{}
	}
	for k := range keys {
		want := declared[k] - consumed[k]
		if delta[k] != want {
			r.Fatalf("delta[%s]=%d, want %d", k, delta[k], want)
		}
	}
	for k, v := range delta {
		if _, ok := keys[k]; !ok && v != 0 {
			r.Fatalf("unexpected delta[%s]=%d", k, v)
		}
	}
}

type unit struct {
	id         string
	holder     string
	demands    map[string]int64
	coalesce   string
	policy     quelor.CoalescePolicy
	generation int64
}

func makeUnit(id, holder string, demands map[string]int64, coalesce string, policy quelor.CoalescePolicy, gen int64) unit {
	return unit{id: id, holder: holder, demands: demands, coalesce: coalesce, policy: policy, generation: gen}
}

func (u unit) envelope() quelor.Envelope {
	h := u.holder
	if h == "" {
		h = defaultHolder
	}
	var e quelor.Envelope
	e.UnitID = u.id
	e.TenantID = defaultTenant
	e.HolderID = h
	e.Demands = u.demands
	e.CoalesceKey = u.coalesce
	e.CoalescePolicy = u.policy
	e.Generation = u.generation
	e.Priority = quelor.PriorityChange
	return e
}

func engineFrom(r reporter, newEngine Factory, spec Spec) quelor.LeaseAPI {
	r.Helper()
	if newEngine == nil {
		r.Fatalf("nil factory")
	}
	api := newEngine(reporterT(r), spec)
	if api == nil {
		r.Fatalf("factory returned nil LeaseAPI")
	}
	return api
}

func acquire(r reporter, api quelor.LeaseAPI, e quelor.Envelope, token string) (quelor.AcquireResult, error) {
	r.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callBudget)
	defer cancel()
	return api.AcquireLease(ctx, quelor.AcquireRequest{Envelope: e, Token: token})
}

func mustAcquire(r reporter, api quelor.LeaseAPI, e quelor.Envelope, token string) quelor.AcquireResult {
	r.Helper()
	res, err := acquire(r, api, e, token)
	if err != nil {
		r.Fatalf("acquire %s: %v", e.UnitID, err)
	}
	return res
}

func mustSettle(r reporter, api quelor.LeaseAPI, lease *quelor.Lease, consumed map[string]int64) quelor.SettleResult {
	r.Helper()
	if lease == nil {
		r.Fatalf("settle without lease")
	}
	ctx, cancel := context.WithTimeout(context.Background(), callBudget)
	defer cancel()
	res, err := api.SettleLease(ctx, quelor.SettleRequest{
		LeaseID:  lease.ID,
		HolderID: lease.HolderID,
		Consumed: consumed,
	})
	if err != nil {
		r.Fatalf("settle %s: %v", lease.ID, err)
	}
	return res
}

func mustRenew(r reporter, api quelor.LeaseAPI, lease *quelor.Lease, delta map[string]int64) quelor.RenewResult {
	r.Helper()
	if lease == nil {
		r.Fatalf("renew without lease")
	}
	ctx, cancel := context.WithTimeout(context.Background(), callBudget)
	defer cancel()
	res, err := api.RenewLease(ctx, quelor.RenewRequest{
		LeaseID:  lease.ID,
		HolderID: lease.HolderID,
		Delta:    delta,
	})
	if err != nil {
		r.Fatalf("renew %s: %v", lease.ID, err)
	}
	return res
}

func standardNow() (time.Time, func() time.Time) {
	t := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	return t, func() time.Time { return t }
}

func mapsEqual(a, b map[string]int64) bool {
	keys := make(map[string]struct{})
	for k := range a {
		keys[k] = struct{}{}
	}
	for k := range b {
		keys[k] = struct{}{}
	}
	for k := range keys {
		if a[k] != b[k] {
			return false
		}
	}
	return true
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

type namedCase struct {
	name string
	fn   func(r reporter)
}

func collect(r reporter, suite string, cases []namedCase) Report {
	r.Helper()
	results := make([]CaseResult, 0, len(cases))
	for _, tc := range cases {
		passed := true
		r.Run(tc.name, func(sub reporter) {
			cr := &captureReporter{inner: sub}
			defer func() {
				if p := recover(); p != nil {
					if _, ok := p.(fatalStop); !ok {
						panic(p)
					}
					cr.failed = true
				}
				if cr.failed || reporterFailed(sub) {
					passed = false
				}
			}()
			tc.fn(cr)
		})
		results = append(results, CaseResult{Name: tc.name, Passed: passed})
	}
	return summarize(suite, results)
}

type fatalStop struct{}

type captureReporter struct {
	inner  reporter
	failed bool
}

func (c *captureReporter) Helper() {
	if t := c.testingT(); t != nil {
		t.Helper()
	}
	c.inner.Helper()
}

func (c *captureReporter) Errorf(format string, args ...any) {
	if t := c.testingT(); t != nil {
		t.Helper()
	}
	c.failed = true
	c.inner.Errorf(format, args...)
}

func (c *captureReporter) Fatalf(format string, args ...any) {
	if t := c.testingT(); t != nil {
		t.Helper()
	}
	c.failed = true
	c.inner.Fatalf(format, args...)
}

func (c *captureReporter) Run(name string, fn func(r reporter)) {
	c.inner.Run(name, fn)
}

func (c *captureReporter) testingT() *testing.T { return reporterT(c.inner) }

type tAdapter struct{ t *testing.T }

func (a tAdapter) Helper() {
	if a.t != nil {
		a.t.Helper()
	}
}

func (a tAdapter) Errorf(format string, args ...any) {
	a.t.Helper()
	a.t.Errorf(format, args...)
}

func (a tAdapter) Fatalf(format string, args ...any) {
	a.t.Helper()
	a.t.Fatalf(format, args...)
}

func (a tAdapter) Run(name string, fn func(r reporter)) {
	a.t.Run(name, func(t *testing.T) {
		fn(tAdapter{t: t})
	})
}

func (a tAdapter) testingT() *testing.T { return a.t }

func (a tAdapter) failed() bool { return a.t != nil && a.t.Failed() }

func reporterT(r reporter) *testing.T {
	type hasT interface{ testingT() *testing.T }
	if h, ok := r.(hasT); ok {
		return h.testingT()
	}
	return nil
}

func reporterFailed(r reporter) bool {
	type hasFailed interface{ failed() bool }
	if h, ok := r.(hasFailed); ok {
		return h.failed()
	}
	return false
}

// recordingReporter stores assertion failures and does not touch *testing.T.
type recordingReporter struct {
	errs []string
}

func (r *recordingReporter) Helper() {}

func (r *recordingReporter) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func (r *recordingReporter) Fatalf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
	panic(fatalStop{})
}

func (r *recordingReporter) Run(name string, fn func(reporter)) {
	fn(r)
}
