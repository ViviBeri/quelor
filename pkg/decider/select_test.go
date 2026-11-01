package decider

import (
	"context"
	"testing"

	"github.com/coherera/quelor/pkg/quelor"
)

type stubDecider struct {
	id   string
	kind quelor.VerdictKind
}

func (s *stubDecider) Decide(context.Context, Request) (Response, error) {
	var r Response
	r.DeciderAPIVersion = APIVersion
	r.Status = StatusAnswered
	r.Mode = ModeEnforce
	r.Verdict.Kind = s.kind
	r.DeciderID = s.id
	return r, nil
}

func TestForceStatic(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want bool
	}{
		{name: "empty", env: "", want: false},
		{name: "static", env: "static", want: true},
		{name: "Static", env: "Static", want: false},
		{name: "padded", env: " static", want: false},
		{name: "suffix", env: "static ", want: false},
		{name: "other", env: "sidecar", want: false},
		{name: "http", env: "http", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvDecider, tc.env)
			if got := ForceStatic(); got != tc.want {
				t.Fatalf("ForceStatic() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSelect(t *testing.T) {
	cfg := Static(quelor.Verdict{Kind: quelor.Admit, Reason: "go"}, "from-static")
	ref := &stubDecider{id: "from-ref", kind: quelor.Deny}
	req := Request{Op: OpRenew, Envelope: quelor.Envelope{UnitID: "unit-7"}}
	cases := []struct {
		name    string
		env     string
		useCfg  bool
		wantID  string
		want    quelor.VerdictKind
		refOnly bool
	}{
		{name: "configured", env: "", useCfg: true, wantID: "from-static", want: quelor.Admit},
		{name: "kill-switch", env: "static", useCfg: true, wantID: "from-ref", want: quelor.Deny},
		{name: "nil-configured", env: "", useCfg: false, wantID: "from-ref", want: quelor.Deny},
		{name: "nil-and-switch", env: "static", useCfg: false, wantID: "from-ref", want: quelor.Deny},
		{name: "other-env", env: "http", useCfg: true, wantID: "from-static", want: quelor.Admit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvDecider, tc.env)
			var configured Decider
			if tc.useCfg {
				configured = cfg
			}
			got := Select(configured, ref)
			if !tc.useCfg || tc.env == "static" {
				if got != Decider(ref) {
					t.Fatal("expected reference identity")
				}
			}
			resp, err := got.Decide(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.DeciderID != tc.wantID || resp.Verdict.Kind != tc.want {
				t.Fatalf("id %q kind %q", resp.DeciderID, resp.Verdict.Kind)
			}
			if tc.wantID == "from-static" {
				if resp.DecisionRef != decisionRef(OpRenew, "unit-7", quelor.Admit) {
					t.Fatalf("ref %q", resp.DecisionRef)
				}
				if resp.Verdict.Reason != "go" {
					t.Fatalf("reason %q", resp.Verdict.Reason)
				}
			}
		})
	}
}

func TestSelectNilReference(t *testing.T) {
	cases := []struct {
		name   string
		env    string
		hasCfg bool
	}{
		{name: "switch-drops-configured", env: "static", hasCfg: true},
		{name: "both-nil", env: "", hasCfg: false},
		{name: "switch-both-nil", env: "static", hasCfg: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvDecider, tc.env)
			var configured Decider
			if tc.hasCfg {
				configured = &stubDecider{id: "cfg", kind: quelor.Admit}
			}
			if got := Select(configured, nil); got != nil {
				t.Fatalf("got %#v", got)
			}
		})
	}
}
