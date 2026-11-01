package reference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/coherera/quelor/pkg/decider"
	"github.com/coherera/quelor/pkg/quelor"
)

func mustDecide(t *testing.T, d decider.Decider, req decider.Request) decider.Response {
	t.Helper()
	resp, err := d.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	return resp
}

func TestVerdictKinds(t *testing.T) {
	tests := []struct {
		name string
		kind string
		why  string
		wake time.Duration
	}{
		{name: "admit", kind: "ADMIT", why: "ok"},
		{name: "defer", kind: "DEFER", why: "later", wake: 10 * time.Second},
		{name: "shed", kind: "SHED", why: "coalesced"},
		{name: "deny", kind: "DENY", why: "tenant suspended"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wakeLine := ""
			if tt.wake > 0 {
				wakeLine = "\n    wake_after: 10s"
			}
			y := "id: v\nmode: enforce\nrules:\n  - id: r\n    when: \"true\"\n    verdict: " + tt.kind + "\n    reason: " + tt.why + wakeLine + "\n"
			d, rs := mustDecider(t, y)
			for _, op := range []decider.Op{decider.OpAcquire, decider.OpRenew} {
				req := baseReq()
				req.Op = op
				resp := mustDecide(t, d, req)
				assertIdentity(t, resp, "trace-1")
				if resp.Status != decider.StatusAnswered {
					t.Fatalf("status %s", resp.Status)
				}
				if resp.Mode != decider.ModeEnforce || !resp.Enforced() {
					t.Fatalf("mode %s enforced %v", resp.Mode, resp.Enforced())
				}
				if resp.Verdict.Kind != quelor.VerdictKind(tt.kind) || resp.Verdict.Reason != tt.why {
					t.Fatalf("verdict %+v", resp.Verdict)
				}
				if !reflect.DeepEqual(resp.RuleIDs, []string{"r"}) {
					t.Fatalf("rules %+v", resp.RuleIDs)
				}
				if resp.RulesetID != "v" || resp.RulesetHash != rs.Hash() {
					t.Fatalf("ruleset %s %s", resp.RulesetID, resp.RulesetHash)
				}
				if len(resp.Abstains) != 0 {
					t.Fatalf("abstains %+v", resp.Abstains)
				}
				if tt.wake == 0 {
					if resp.Verdict.WakeAt != nil {
						t.Fatalf("wake %v", resp.Verdict.WakeAt)
					}
					continue
				}
				want := fixedNowTime(t).Add(tt.wake)
				if resp.Verdict.WakeAt == nil || !resp.Verdict.WakeAt.Equal(want) {
					t.Fatalf("wake %v want %v", resp.Verdict.WakeAt, want)
				}
			}
		})
	}
}

func TestDefaultAdmit(t *testing.T) {
	resp := mustDecide(t, Default(), baseReq())
	assertIdentity(t, resp, "trace-1")
	if resp.Verdict.Kind != quelor.Admit || resp.Verdict.Reason != "" || len(resp.RuleIDs) != 0 {
		t.Fatalf("%+v rules %+v", resp.Verdict, resp.RuleIDs)
	}
	if resp.Mode != decider.ModeEnforce || !resp.Enforced() {
		t.Fatalf("mode %s", resp.Mode)
	}
	if resp.Status != decider.StatusAnswered {
		t.Fatalf("status %s", resp.Status)
	}
}

func TestMatchOrder(t *testing.T) {
	d, _ := mustDecider(t, `
id: mix
mode: enforce
rules:
  - id: renew-only
    when: "true"
    verdict: SHED
    reason: renew
    ops: [renew]
  - id: boom
    when: "readings['missing'].available > 0"
    verdict: DENY
    reason: missing
  - id: banned
    when: "tenant_id == 'acme'"
    verdict: DENY
    reason: banned
  - id: later
    when: "true"
    verdict: ADMIT
    reason: later
`)
	acme := baseReq()
	acme.Envelope.TenantID = "acme"
	resp := mustDecide(t, d, acme)
	if resp.Verdict.Kind != quelor.Deny || resp.Verdict.Reason != "banned" {
		t.Fatalf("%+v", resp.Verdict)
	}
	if !reflect.DeepEqual(resp.RuleIDs, []string{"banned"}) {
		t.Fatalf("rules %+v", resp.RuleIDs)
	}
	if !reflect.DeepEqual(resp.Abstains, []string{"boom:eval-error"}) {
		t.Fatalf("abstains %+v", resp.Abstains)
	}
	if resp.Status != decider.StatusAnswered || !resp.Enforced() {
		t.Fatalf("status %s enforced %v", resp.Status, resp.Enforced())
	}

	other := baseReq()
	other.Envelope.TenantID = "other"
	resp = mustDecide(t, d, other)
	if resp.Verdict.Kind != quelor.Admit || resp.Verdict.Reason != "later" {
		t.Fatalf("%+v", resp.Verdict)
	}
	if !reflect.DeepEqual(resp.RuleIDs, []string{"later"}) {
		t.Fatalf("rules %+v", resp.RuleIDs)
	}
	if !reflect.DeepEqual(resp.Abstains, []string{"boom:eval-error"}) {
		t.Fatalf("abstains %+v", resp.Abstains)
	}

	renew := acme
	renew.Op = decider.OpRenew
	resp = mustDecide(t, d, renew)
	if resp.Verdict.Kind != quelor.Shed || resp.Verdict.Reason != "renew" {
		t.Fatalf("%+v", resp.Verdict)
	}
	if !reflect.DeepEqual(resp.RuleIDs, []string{"renew-only"}) || len(resp.Abstains) != 0 {
		t.Fatalf("rules %+v abstains %+v", resp.RuleIDs, resp.Abstains)
	}
	if resp.Verdict.WakeAt != nil {
		t.Fatalf("wake %v", resp.Verdict.WakeAt)
	}
}

func TestDeferWake(t *testing.T) {
	d, _ := mustDecider(t, `
id: w
mode: enforce
rules:
  - id: wait
    when: "true"
    verdict: DEFER
    reason: later
    wake_after: 30s
`)
	resp := mustDecide(t, d, baseReq())
	want := fixedNowTime(t).Add(30 * time.Second)
	if resp.Verdict.WakeAt == nil || !resp.Verdict.WakeAt.Equal(want) {
		t.Fatalf("wake %v", resp.Verdict.WakeAt)
	}

	nano := baseReq()
	nano.Meta.Now = "2026-10-31T12:00:00.5Z"
	resp = mustDecide(t, d, nano)
	wantNano := time.Date(2026, 10, 31, 12, 0, 30, int(500*time.Millisecond), time.UTC)
	if resp.Verdict.WakeAt == nil || !resp.Verdict.WakeAt.Equal(wantNano) {
		t.Fatalf("wake %v want %v", resp.Verdict.WakeAt, wantNano)
	}

	bad := baseReq()
	bad.Meta.Now = "not-a-time"
	before := time.Now()
	resp = mustDecide(t, d, bad)
	after := time.Now()
	if resp.Verdict.WakeAt == nil {
		t.Fatal("missing wake")
	}
	got := *resp.Verdict.WakeAt
	if got.Before(before.Add(30*time.Second)) || got.After(after.Add(30*time.Second)) {
		t.Fatalf("fallback wake %v not in [%v, %v]", got, before, after)
	}

	zero, _ := mustDecider(t, `
id: z
mode: enforce
rules:
  - id: wait
    when: "true"
    verdict: DEFER
    reason: nowake
    wake_after: 0s
`)
	resp = mustDecide(t, zero, baseReq())
	if resp.Verdict.Kind != quelor.Defer || resp.Verdict.WakeAt != nil {
		t.Fatalf("%+v", resp.Verdict)
	}
}

func TestReadingBudget(t *testing.T) {
	d, _ := mustDecider(t, `
id: budget
mode: enforce
rules:
  - id: budget
    when: "readings['github_api'].available < demands['github_api']"
    verdict: DEFER
    reason: rate budget
    wake_after: 15s
`)
	req := baseReq()
	req.Envelope.Demands = map[string]int64{"github_api": 10}
	req.Readings = []quelor.Reading{{
		Resource:   "github_api",
		Available:  3,
		Capacity:   100,
		Known:      true,
		Stale:      false,
		AgeMS:      15,
		ObservedAt: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC),
	}}
	resp := mustDecide(t, d, req)
	if resp.Verdict.Kind != quelor.Defer || resp.Verdict.Reason != "rate budget" {
		t.Fatalf("%+v", resp.Verdict)
	}
	want := fixedNowTime(t).Add(15 * time.Second)
	if resp.Verdict.WakeAt == nil || !resp.Verdict.WakeAt.Equal(want) {
		t.Fatalf("wake %v", resp.Verdict.WakeAt)
	}
	if !reflect.DeepEqual(resp.RuleIDs, []string{"budget"}) || len(resp.Abstains) != 0 {
		t.Fatalf("rules %+v abstains %+v", resp.RuleIDs, resp.Abstains)
	}

	enough := req
	enough.Readings = append([]quelor.Reading(nil), req.Readings...)
	enough.Readings[0].Available = 10
	resp = mustDecide(t, d, enough)
	if resp.Verdict.Kind != quelor.Admit || len(resp.RuleIDs) != 0 || len(resp.Abstains) != 0 {
		t.Fatalf("kind %s rules %+v abstains %+v", resp.Verdict.Kind, resp.RuleIDs, resp.Abstains)
	}

	missingReading := req
	missingReading.Readings = nil
	resp = mustDecide(t, d, missingReading)
	if resp.Status != decider.StatusAnswered || resp.Verdict.Kind != quelor.Admit || len(resp.RuleIDs) != 0 {
		t.Fatalf("status %s %+v rules %+v", resp.Status, resp.Verdict, resp.RuleIDs)
	}
	if !reflect.DeepEqual(resp.Abstains, []string{"budget:eval-error"}) {
		t.Fatalf("abstains %+v", resp.Abstains)
	}

	missingDemand := req
	missingDemand.Envelope.Demands = map[string]int64{}
	resp = mustDecide(t, d, missingDemand)
	if !reflect.DeepEqual(resp.Abstains, []string{"budget:eval-error"}) || resp.Verdict.Kind != quelor.Admit {
		t.Fatalf("abstains %+v kind %s", resp.Abstains, resp.Verdict.Kind)
	}
}

func TestReadingFields(t *testing.T) {
	d, _ := mustDecider(t, `
id: fields
mode: enforce
rules:
  - id: fields
    when: "readings['github_api'].available == 3 && readings['github_api'].capacity == 100 && readings['github_api'].age_ms == 15 && readings['github_api'].known == true && readings['github_api'].stale == false && readings['github_api'].usable == true"
    verdict: DENY
    reason: fields
`)
	req := baseReq()
	req.Readings = []quelor.Reading{{
		Resource:  "github_api",
		Available: 3,
		Capacity:  100,
		Known:     true,
		Stale:     false,
		AgeMS:     15,
	}}
	resp := mustDecide(t, d, req)
	if resp.Verdict.Kind != quelor.Deny || len(resp.Abstains) != 0 {
		t.Fatalf("%+v abstains %+v", resp.Verdict, resp.Abstains)
	}

	stale, _ := mustDecider(t, `
id: stale
mode: enforce
rules:
  - id: live
    when: "readings['github_api'].usable == true"
    verdict: DENY
    reason: live
`)
	req.Readings[0].Stale = true
	resp = mustDecide(t, stale, req)
	if resp.Verdict.Kind != quelor.Admit || len(resp.Abstains) != 0 || len(resp.RuleIDs) != 0 {
		t.Fatalf("kind %s abstains %+v rules %+v", resp.Verdict.Kind, resp.Abstains, resp.RuleIDs)
	}
}

func TestReadingsCopied(t *testing.T) {
	d := Default()
	req := baseReq()
	req.Readings = []quelor.Reading{
		{Resource: "github_api", Available: 1, Known: true, AgeMS: 4},
		{Resource: "executor", Available: 2, Stale: true, AgeMS: 9},
	}
	resp := mustDecide(t, d, req)
	req.Readings[0].Available = 99
	if resp.Readings[0].Available != 1 || resp.Readings[1].Resource != "executor" || resp.Readings[1].AgeMS != 9 {
		t.Fatalf("readings %+v", resp.Readings)
	}
}

func TestVariables(t *testing.T) {
	d, _ := mustDecider(t, `
id: vars
mode: enforce
rules:
  - id: vars
    when: "op == 'renew' && unit_id == 'u' && tenant_id == 'ten' && holder_id == 'h' && affinity_key == 'aff' && coalesce_key == 'coa' && priority == 'release-critical' && priority_rank == 1 && generation == 7 && demands['slots'] == 2 && spend_delta['usd'] == 5"
    verdict: DENY
    reason: vars
`)
	req := baseReq()
	req.Op = decider.OpRenew
	req.Envelope.UnitID = "u"
	req.Envelope.TenantID = "ten"
	req.Envelope.HolderID = "h"
	req.Envelope.AffinityKey = "aff"
	req.Envelope.CoalesceKey = "coa"
	req.Envelope.Priority = quelor.PriorityReleaseCritical
	req.Envelope.Generation = 7
	req.Envelope.Demands = map[string]int64{"slots": 2}
	req.SpendDelta = map[string]int64{"usd": 5}
	resp := mustDecide(t, d, req)
	if resp.Verdict.Kind != quelor.Deny || resp.Verdict.Reason != "vars" || len(resp.Abstains) != 0 {
		t.Fatalf("%+v abstains %+v", resp.Verdict, resp.Abstains)
	}

	req.Op = decider.OpAcquire
	resp = mustDecide(t, d, req)
	if resp.Verdict.Kind != quelor.Admit || len(resp.Abstains) != 0 || len(resp.RuleIDs) != 0 {
		t.Fatalf("kind %s abstains %+v", resp.Verdict.Kind, resp.Abstains)
	}
}

func TestPriorityRank(t *testing.T) {
	tests := []struct {
		name string
		pri  quelor.Priority
		when string
	}{
		{name: "release", pri: quelor.PriorityReleaseCritical, when: "priority == 'release-critical' && priority_rank == 1"},
		{name: "maint", pri: quelor.PriorityMaintenance, when: "priority == 'maintenance' && priority_rank == 4"},
		{name: "empty", pri: "", when: "priority == '' && priority_rank == 2"},
		{name: "unknown", pri: quelor.Priority("nope"), when: "priority_rank == 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, _ := mustDecider(t, "id: p\nmode: enforce\nrules:\n  - id: p\n    when: \""+tt.when+"\"\n    verdict: SHED\n    reason: p\n")
			req := baseReq()
			req.Envelope.Priority = tt.pri
			resp := mustDecide(t, d, req)
			if resp.Verdict.Kind != quelor.Shed || len(resp.Abstains) != 0 {
				t.Fatalf("%+v abstains %+v", resp.Verdict, resp.Abstains)
			}
		})
	}
}

func TestRulesetMoved(t *testing.T) {
	d, rs := mustDecider(t, `
id: default-v1
mode: enforce
rules:
  - id: deny-banned-tenant
    when: "tenant_id == 'acme-banned'"
    verdict: DENY
    reason: tenant suspended
    ops: [acquire]
`)
	req := baseReq()
	req.Envelope.TenantID = "acme-banned"
	req.Readings = []quelor.Reading{{Resource: "github_api", Available: 1, Known: true}}
	req.ExpectRulesetHash = "deadbeef"
	resp := mustDecide(t, d, req)
	assertIdentity(t, resp, "trace-1")
	if resp.Status != decider.StatusAbstained || resp.Enforced() {
		t.Fatalf("status %s enforced %v", resp.Status, resp.Enforced())
	}
	if !reflect.DeepEqual(resp.Abstains, []string{"ruleset-moved"}) {
		t.Fatalf("abstains %+v", resp.Abstains)
	}
	if resp.RulesetID != "default-v1" || resp.RulesetHash != rs.Hash() {
		t.Fatalf("ruleset %s %s", resp.RulesetID, resp.RulesetHash)
	}
	if resp.Verdict.Kind != "" || len(resp.RuleIDs) != 0 {
		t.Fatalf("verdict %+v rules %+v", resp.Verdict, resp.RuleIDs)
	}
	if len(resp.Readings) != 1 || resp.Readings[0].Available != 1 {
		t.Fatalf("readings %+v", resp.Readings)
	}
	if len(resp.DecisionRef) != 24 {
		t.Fatalf("ref %q", resp.DecisionRef)
	}

	req.ExpectRulesetHash = rs.Hash()
	resp = mustDecide(t, d, req)
	if resp.Status != decider.StatusAnswered || resp.Verdict.Kind != quelor.Deny {
		t.Fatalf("status %s %+v", resp.Status, resp.Verdict)
	}

	req.ExpectRulesetHash = ""
	req.Op = decider.OpRenew
	resp = mustDecide(t, d, req)
	if resp.Verdict.Kind != quelor.Admit || len(resp.RuleIDs) != 0 {
		t.Fatalf("renew %+v rules %+v", resp.Verdict, resp.RuleIDs)
	}
}

func TestAdvisoryModes(t *testing.T) {
	logOnly, _ := mustDecider(t, `
id: log
mode: log-only
rules:
  - id: no
    when: "true"
    verdict: DENY
    reason: no
`)
	resp := mustDecide(t, logOnly, baseReq())
	if resp.Status != decider.StatusAnswered || resp.Mode != decider.ModeLogOnly || resp.Enforced() {
		t.Fatalf("status %s mode %s enforced %v", resp.Status, resp.Mode, resp.Enforced())
	}
	if resp.Verdict.Kind != quelor.Deny || resp.Verdict.Reason != "no" {
		t.Fatalf("%+v", resp.Verdict)
	}

	advise, _ := mustDecider(t, `
id: adv
mode: advise
rules:
  - id: wait
    when: "true"
    verdict: DEFER
    reason: later
    wake_after: 5s
`)
	resp = mustDecide(t, advise, baseReq())
	if resp.Mode != decider.ModeAdvise || resp.Enforced() || resp.Verdict.Kind != quelor.Defer {
		t.Fatalf("mode %s enforced %v %+v", resp.Mode, resp.Enforced(), resp.Verdict)
	}
	want := fixedNowTime(t).Add(5 * time.Second)
	if resp.Verdict.WakeAt == nil || !resp.Verdict.WakeAt.Equal(want) {
		t.Fatalf("wake %v", resp.Verdict.WakeAt)
	}
}

func TestDecisionRef(t *testing.T) {
	d, rs := mustDecider(t, `
id: ref
mode: enforce
rules:
  - id: no
    when: "true"
    verdict: DENY
    reason: no
`)
	req := baseReq()
	first := mustDecide(t, d, req)
	second := mustDecide(t, d, req)
	if first.DecisionRef != second.DecisionRef || len(first.DecisionRef) != 24 {
		t.Fatalf("ref %q %q", first.DecisionRef, second.DecisionRef)
	}
	sum := sha256.Sum256([]byte(rs.Hash() + "trace-1" + "unit-1" + "acquire"))
	want := hex.EncodeToString(sum[:])[:24]
	if first.DecisionRef != want {
		t.Fatalf("ref %s want %s", first.DecisionRef, want)
	}

	otherTrace := req
	otherTrace.Meta.TraceID = "trace-2"
	if mustDecide(t, d, otherTrace).DecisionRef == first.DecisionRef {
		t.Fatal("trace did not change ref")
	}
	otherUnit := req
	otherUnit.Envelope.UnitID = "unit-2"
	if mustDecide(t, d, otherUnit).DecisionRef == first.DecisionRef {
		t.Fatal("unit did not change ref")
	}
	otherOp := req
	otherOp.Op = decider.OpRenew
	if mustDecide(t, d, otherOp).DecisionRef == first.DecisionRef {
		t.Fatal("op did not change ref")
	}

	sameRules, otherRS := mustDecider(t, `
id: ref
mode: advise
rules:
  - id: no
    when: "true"
    verdict: DENY
    reason: no
`)
	if otherRS.Hash() != rs.Hash() {
		t.Fatal("mode changed hash")
	}
	if mustDecide(t, sameRules, req).DecisionRef != first.DecisionRef {
		t.Fatal("mode changed decision ref")
	}

	different, _ := mustDecider(t, `
id: ref
mode: enforce
rules:
  - id: no
    when: "false"
    verdict: DENY
    reason: no
`)
	if mustDecide(t, different, req).DecisionRef == first.DecisionRef {
		t.Fatal("ruleset content kept decision ref")
	}
}

func TestCancelledContext(t *testing.T) {
	d, _ := mustDecider(t, `
id: c
mode: enforce
rules:
  - id: no
    when: "true"
    verdict: DENY
    reason: no
`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := d.Decide(ctx, baseReq())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

func TestDeciderConcurrent(t *testing.T) {
	d, _ := mustDecider(t, `
id: conc
mode: enforce
rules:
  - id: all
    when: "true"
    verdict: DENY
    reason: stop
`)
	want := mustDecide(t, d, baseReq())
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := mustDecide(t, d, baseReq())
			if got.Verdict.Kind != quelor.Deny || got.DecisionRef != want.DecisionRef || len(got.RuleIDs) != 1 || got.RuleIDs[0] != "all" {
				t.Errorf("got %+v ref %s rules %+v", got.Verdict, got.DecisionRef, got.RuleIDs)
			}
		}()
	}
	wg.Wait()
}
