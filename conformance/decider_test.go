package conformance

import (
	"context"
	"testing"

	"github.com/coherera/quelor/pkg/decider"
	"github.com/coherera/quelor/pkg/decider/reference"
	"github.com/coherera/quelor/pkg/quelor"
)

func TestDeciderConformance(t *testing.T) {
	tests := []struct {
		name        string
		newDecider  DeciderFactory
		opts        DeciderOptions
		wantRuleset bool
	}{
		{
			name:        "reference",
			newDecider:  func(*testing.T) decider.Decider { return reference.Default() },
			opts:        DeciderOptions{RulesetAware: true},
			wantRuleset: true,
		},
		{
			name: "static",
			newDecider: func(*testing.T) decider.Decider {
				return decider.Func(func(context.Context, decider.Request) (decider.Response, error) { return staticResponse(), nil })
			},
			opts:        DeciderOptions{RulesetAware: false},
			wantRuleset: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := RunDecider(t, tc.newDecider, tc.opts)
			if report.Suite != suiteDecider {
				t.Fatalf("suite %q", report.Suite)
			}
			if report.Failed > 0 {
				t.Fatalf("%s decider conformance failed %d: %s", tc.name, report.Failed, failedList(report))
			}
			saw := false
			for _, c := range report.Cases {
				if c.Name == "ruleset-moved" {
					saw = true
				}
			}
			if saw != tc.wantRuleset {
				t.Fatalf("ruleset-moved present=%v want %v", saw, tc.wantRuleset)
			}
		})
	}
}

func staticResponse() decider.Response {
	var resp decider.Response
	resp.DeciderAPIVersion = decider.APIVersion
	resp.Status = decider.StatusAnswered
	resp.Mode = decider.ModeEnforce
	resp.Verdict = quelor.Verdict{Kind: quelor.Admit, Reason: "static"}
	resp.DecisionRef = "static-decision-ref"
	resp.DeciderID = "static"
	resp.Provenance = decider.Provenance{Step: "static-step", Tier: "static-tier", EvidenceClass: "static-evidence"}
	resp.RulesetID = "static-ruleset"
	resp.RulesetHash = "static-hash"
	resp.RuleIDs = []string{"static-rule"}
	resp.TraceID = "trace-static"
	return resp
}
