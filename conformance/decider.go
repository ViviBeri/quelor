package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"testing"
	"time"

	"github.com/coherera/quelor/pkg/decider"
	"github.com/coherera/quelor/pkg/quelor"
)

const (
	suiteDecider  = "decider"
	deciderBudget = 250 * time.Millisecond
)

// DeciderFactory builds the Decider under test.
type DeciderFactory func(t *testing.T) decider.Decider

// DeciderOptions selects optional cases.
// RulesetAware runs ruleset-moved.
type DeciderOptions struct {
	RulesetAware bool
}

// RunDecider runs the published decider cases.
func RunDecider(t *testing.T, newDecider DeciderFactory, opts DeciderOptions) Report {
	t.Helper()
	rep := &Report{}
	publishReport(t, rep)
	*rep = runDecider(tAdapter{t: t}, newDecider, opts)
	return *rep
}

func runDecider(r reporter, newDecider DeciderFactory, opts DeciderOptions) Report {
	r.Helper()
	cases := []namedCase{
		{name: "answers-with-valid-status", fn: func(r reporter) { caseValidStatus(r, newDecider) }},
		{name: "decision-ref-present-and-idempotent", fn: func(r reporter) { caseDecisionRef(r, newDecider) }},
		{name: "decider-id-set", fn: func(r reporter) { caseDeciderID(r, newDecider) }},
		{name: "api-version-echoed", fn: func(r reporter) { caseAPIVersion(r, newDecider) }},
		{name: "does-not-mutate-request", fn: func(r reporter) { caseNoMutate(r, newDecider) }},
		{name: "provenance-opaque", fn: func(r reporter) { caseProvenance(r, newDecider) }},
		{name: "json-roundtrip", fn: func(r reporter) { caseJSONRoundTrip(r, newDecider) }},
		{name: "not-slower-than", fn: func(r reporter) { caseLatency(r, newDecider) }},
	}
	if opts.RulesetAware {
		cases = append(cases, namedCase{
			name: "ruleset-moved",
			fn:   func(r reporter) { caseRulesetMoved(r, newDecider) },
		})
	}
	return collect(r, suiteDecider, cases)
}

func caseValidStatus(r reporter, newDecider DeciderFactory) {
	r.Helper()
	d := deciderFrom(r, newDecider)
	resp, err := d.Decide(context.Background(), sampleRequest())
	if err != nil && !validStatus(resp.Status) {
		r.Fatalf("decide: %v", err)
	}
	if !validStatus(resp.Status) {
		r.Fatalf("status %q", resp.Status)
	}
	if resp.Status == decider.StatusAnswered && !validKind(resp.Verdict.Kind) {
		r.Fatalf("answered verdict kind %q", resp.Verdict.Kind)
	}
}

func caseDecisionRef(r reporter, newDecider DeciderFactory) {
	r.Helper()
	d := deciderFrom(r, newDecider)
	firstReq := sampleRequest()
	secondReq := cloneRequest(firstReq)
	first, err := d.Decide(context.Background(), firstReq)
	if err != nil {
		r.Fatalf("first decide: %v", err)
	}
	second, err := d.Decide(context.Background(), secondReq)
	if err != nil {
		r.Fatalf("second decide: %v", err)
	}
	if first.DecisionRef == "" {
		r.Fatalf("decision_ref empty")
	}
	if first.DecisionRef != second.DecisionRef {
		r.Fatalf("decision_ref %q then %q", first.DecisionRef, second.DecisionRef)
	}
}

func caseDeciderID(r reporter, newDecider DeciderFactory) {
	r.Helper()
	d := deciderFrom(r, newDecider)
	resp, err := d.Decide(context.Background(), sampleRequest())
	if err != nil {
		r.Fatalf("decide: %v", err)
	}
	if resp.DeciderID == "" {
		r.Fatalf("decider_id empty")
	}
}

func caseAPIVersion(r reporter, newDecider DeciderFactory) {
	r.Helper()
	d := deciderFrom(r, newDecider)
	req := sampleRequest()
	resp, err := d.Decide(context.Background(), req)
	if err != nil {
		r.Fatalf("decide: %v", err)
	}
	if resp.DeciderAPIVersion == "" {
		r.Fatalf("decider_api_version empty")
	}
	if resp.DeciderAPIVersion != req.DeciderAPIVersion {
		r.Fatalf("decider_api_version %q, request %q", resp.DeciderAPIVersion, req.DeciderAPIVersion)
	}
}

func caseNoMutate(r reporter, newDecider DeciderFactory) {
	r.Helper()
	d := deciderFrom(r, newDecider)
	req := sampleRequest()
	req.Lease = sampleLease()
	before := cloneRequest(req)
	if !reflect.DeepEqual(before, req) {
		r.Fatalf("request clone diverged before decide")
	}
	_, err := d.Decide(context.Background(), req)
	if !reflect.DeepEqual(before, req) {
		r.Fatalf("decider mutated the request")
	}
	if err != nil {
		r.Fatalf("decide: %v", err)
	}
}

func caseProvenance(r reporter, newDecider DeciderFactory) {
	r.Helper()
	d := deciderFrom(r, newDecider)
	resp, err := d.Decide(context.Background(), sampleRequest())
	if err != nil {
		r.Fatalf("decide: %v", err)
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		r.Fatalf("marshal: %v", err)
	}
	var again decider.Response
	if err := json.Unmarshal(raw, &again); err != nil {
		r.Fatalf("unmarshal: %v", err)
	}
	if again.Provenance != resp.Provenance {
		r.Fatalf("provenance %v became %v", resp.Provenance, again.Provenance)
	}
}

func caseJSONRoundTrip(r reporter, newDecider DeciderFactory) {
	r.Helper()
	d := deciderFrom(r, newDecider)
	resp, err := d.Decide(context.Background(), sampleRequest())
	if err != nil {
		r.Fatalf("decide: %v", err)
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		r.Fatalf("marshal: %v", err)
	}
	var again decider.Response
	if err := json.Unmarshal(raw, &again); err != nil {
		r.Fatalf("unmarshal: %v", err)
	}
	raw2, err := json.Marshal(again)
	if err != nil {
		r.Fatalf("remarshal: %v", err)
	}
	if !bytes.Equal(raw, raw2) {
		r.Fatalf("json round trip\n%s\n%s", raw, raw2)
	}
}

func caseRulesetMoved(r reporter, newDecider DeciderFactory) {
	r.Helper()
	d := deciderFrom(r, newDecider)
	req := sampleRequest()
	req.ExpectRulesetHash = "conformance-not-the-loaded-ruleset"
	resp, err := d.Decide(context.Background(), req)
	if err != nil && resp.Status != decider.StatusAbstained {
		r.Fatalf("decide: %v", err)
	}
	if resp.Status != decider.StatusAbstained {
		r.Fatalf("status %s, want abstained", resp.Status)
	}
	if !contains(resp.Abstains, "ruleset-moved") {
		r.Fatalf("abstains %v, want ruleset-moved", resp.Abstains)
	}
}

func caseLatency(r reporter, newDecider DeciderFactory) {
	r.Helper()
	d := deciderFrom(r, newDecider)
	ctx, cancel := context.WithTimeout(context.Background(), deciderBudget)
	defer cancel()
	start := time.Now()
	_, err := d.Decide(ctx, sampleRequest())
	elapsed := time.Since(start)
	if elapsed > deciderBudget || errors.Is(err, context.DeadlineExceeded) {
		r.Fatalf("decide took %s (budget %s): %v", elapsed, deciderBudget, err)
	}
	if err != nil {
		r.Fatalf("decide: %v", err)
	}
}

func deciderFrom(r reporter, newDecider DeciderFactory) decider.Decider {
	r.Helper()
	if newDecider == nil {
		r.Fatalf("nil decider factory")
	}
	d := newDecider(reporterT(r))
	if d == nil {
		r.Fatalf("factory returned nil Decider")
	}
	return d
}

func sampleRequest() decider.Request {
	deadline := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	observed := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	var req decider.Request
	req.DeciderAPIVersion = decider.APIVersion
	req.Op = decider.OpAcquire
	req.Envelope.UnitID = "unit-1"
	req.Envelope.TenantID = defaultTenant
	req.Envelope.HolderID = defaultHolder
	req.Envelope.Demands = map[string]int64{"slots": 1}
	req.Envelope.Priority = quelor.PriorityChange
	req.Envelope.Generation = 1
	req.Envelope.Deadline = &deadline
	req.SpendDelta = map[string]int64{"slots": 1}
	req.Readings = []quelor.Reading{{
		Resource:   "slots",
		Available:  8,
		Capacity:   8,
		Known:      true,
		ObservedAt: observed,
	}}
	req.Meta.TraceID = "trace-conformance"
	req.Meta.Now = observed.Format(time.RFC3339Nano)
	return req
}

func sampleLease() *quelor.Lease {
	granted := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	var l quelor.Lease
	l.ID = "lease-1"
	l.UnitID = "unit-1"
	l.TenantID = defaultTenant
	l.HolderID = defaultHolder
	l.Declared = map[string]int64{"slots": 1}
	l.GrantedAt = granted
	l.ExpiresAt = granted.Add(time.Minute)
	return &l
}

func cloneRequest(in decider.Request) decider.Request {
	out := in
	if in.Envelope.Demands != nil {
		out.Envelope.Demands = maps.Clone(in.Envelope.Demands)
	}
	if in.Envelope.Deadline != nil {
		d := *in.Envelope.Deadline
		out.Envelope.Deadline = &d
	}
	if in.SpendDelta != nil {
		out.SpendDelta = maps.Clone(in.SpendDelta)
	}
	if in.Readings != nil {
		out.Readings = append([]quelor.Reading(nil), in.Readings...)
	}
	if in.Lease != nil {
		l := *in.Lease
		if in.Lease.Declared != nil {
			l.Declared = maps.Clone(in.Lease.Declared)
		}
		out.Lease = &l
	}
	return out
}

func validStatus(s decider.Status) bool {
	switch s {
	case decider.StatusAnswered, decider.StatusAbstained, decider.StatusUnavailable, decider.StatusKilled:
		return true
	default:
		return false
	}
}

func validKind(k quelor.VerdictKind) bool {
	switch k {
	case quelor.Admit, quelor.Defer, quelor.Shed, quelor.Deny:
		return true
	default:
		return false
	}
}
