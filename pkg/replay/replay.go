// Package replay scores a candidate Decider against a recorded admission trace.
package replay

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/coherera/quelor/pkg/classify"
	"github.com/coherera/quelor/pkg/decider"
	"github.com/coherera/quelor/pkg/quelor"
	"github.com/coherera/quelor/pkg/trace"
)

const (
	opSettle        = "settle"
	reasonFloor     = "engine-floor"
	reasonCandidate = "candidate-"
)

// Options configures an offline replay. A nil Classifier selects classify.Default.
type Options struct {
	Decider    decider.Decider
	Classifier *classify.Ruleset
}

// Report is the aggregate score for one trace.
type Report struct {
	Total                            int     `json:"total"`
	Scored                           int     `json:"scored"`
	Agreement                        float64 `json:"agreement"`
	FalseRed                         float64 `json:"false_red"`
	AdmittedThenFailed               int     `json:"admitted_then_failed"`
	DeferredButWouldHaveFinished     int     `json:"deferred_but_would_have_finished"`
	AdmittedThenFailedRate           float64 `json:"admitted_then_failed_rate"`
	DeferredButWouldHaveFinishedRate float64 `json:"deferred_but_would_have_finished_rate"`
	FailedOutcomes                   int     `json:"failed_outcomes"`
	Skipped                          int     `json:"skipped"`
	Rows                             []Row   `json:"rows"`
}

// Row is one scored acquire, in trace order.
type Row struct {
	DecisionRef string             `json:"decision_ref"`
	Original    quelor.VerdictKind `json:"original"`
	Candidate   quelor.VerdictKind `json:"candidate"`
	Agree       bool               `json:"agree"`
	Outcome     string             `json:"outcome"`
}

// String returns a multi-line human summary.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "total=%d scored=%d skipped=%d\n", r.Total, r.Scored, r.Skipped)
	fmt.Fprintf(&b, "agreement=%s false_red=%s\n", fmtFloat(r.Agreement), fmtFloat(r.FalseRed))
	fmt.Fprintf(&b, "admitted_then_failed=%d rate=%s\n", r.AdmittedThenFailed, fmtFloat(r.AdmittedThenFailedRate))
	fmt.Fprintf(&b, "deferred_but_would_have_finished=%d rate=%s\n", r.DeferredButWouldHaveFinished, fmtFloat(r.DeferredButWouldHaveFinishedRate))
	fmt.Fprintf(&b, "failed_outcomes=%d\n", r.FailedOutcomes)
	for _, row := range r.Rows {
		fmt.Fprintf(&b, "ref=%s original=%s candidate=%s agree=%t outcome=%s\n",
			row.DecisionRef, row.Original, row.Candidate, row.Agree, row.Outcome)
	}
	return b.String()
}

func fmtFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// Run scores opt.Decider against records.
// The decider is required when at least one acquire can be scored.
func Run(ctx context.Context, records []trace.Record, opt Options) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	if opt.Decider == nil {
		for _, rec := range records {
			if scoreable(rec) {
				return Report{}, errors.New("replay: decider is required")
			}
		}
	}
	clf := opt.Classifier
	if clf == nil {
		clf = classify.Default()
	}

	outcomes := make(map[string]*trace.Outcome)
	for _, rec := range records {
		if rec.Op == opSettle && rec.Outcome != nil {
			outcomes[rec.DecisionRef] = rec.Outcome
		}
	}

	rep := Report{Total: len(records)}
	falseRedN := 0
	for _, rec := range records {
		if rec.Op != opSettle || rec.Outcome == nil || !rec.Outcome.Failed {
			continue
		}
		rep.FailedOutcomes++
		if isFalseRed(clf, rec.Outcome) {
			falseRedN++
		}
	}
	if rep.FailedOutcomes > 0 {
		rep.FalseRed = float64(falseRedN) / float64(rep.FailedOutcomes)
	}

	agrees := 0
	admittedWithOutcome := 0
	scoredWithOutcome := 0
	for _, rec := range records {
		if !scoreable(rec) {
			rep.Skipped++
			continue
		}
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		verdict, err := candidateVerdict(ctx, opt.Decider, rec)
		if err != nil {
			return Report{}, fmt.Errorf("replay: decide %q: %w", rec.DecisionRef, err)
		}
		out := outcomes[rec.DecisionRef]
		row := Row{
			DecisionRef: rec.DecisionRef,
			Original:    rec.Verdict.Kind,
			Candidate:   verdict.Kind,
			Agree:       verdict.Kind == rec.Verdict.Kind,
			Outcome:     outcomeLabel(out),
		}
		rep.Rows = append(rep.Rows, row)
		rep.Scored++
		if row.Agree {
			agrees++
		}
		if out == nil {
			continue
		}
		scoredWithOutcome++
		if verdict.Kind == quelor.Admit {
			admittedWithOutcome++
			if out.Failed {
				rep.AdmittedThenFailed++
			}
			continue
		}
		if rec.Verdict.Kind == quelor.Admit && !out.Failed {
			rep.DeferredButWouldHaveFinished++
		}
	}
	if rep.Scored > 0 {
		rep.Agreement = float64(agrees) / float64(rep.Scored)
	}
	if admittedWithOutcome > 0 {
		rep.AdmittedThenFailedRate = float64(rep.AdmittedThenFailed) / float64(admittedWithOutcome)
	}
	if scoredWithOutcome > 0 {
		rep.DeferredButWouldHaveFinishedRate = float64(rep.DeferredButWouldHaveFinished) / float64(scoredWithOutcome)
	}
	return rep, nil
}

func scoreable(rec trace.Record) bool {
	return rec.Op == trace.OpAcquire && rec.Envelope != nil && rec.Verdict != nil
}

func candidateVerdict(ctx context.Context, d decider.Decider, rec trace.Record) (quelor.Verdict, error) {
	resp, err := d.Decide(ctx, decider.Request{
		DeciderAPIVersion: decider.APIVersion,
		Op:                decider.OpAcquire,
		Envelope:          *rec.Envelope,
		Readings:          rec.Readings,
		Meta:              decider.CallMeta{TraceID: rec.TraceID},
	})
	if err != nil {
		return quelor.Verdict{}, err
	}
	// Any status other than answered is DEFER, so an abstain is never an admit.
	v := resp.Verdict
	if resp.Status != decider.StatusAnswered {
		v = quelor.Verdict{Kind: quelor.Defer, Reason: reasonCandidate + string(resp.Status)}
	}
	if v.Kind == quelor.Admit && floored(rec.Envelope.Demands, rec.Readings) {
		v = quelor.Verdict{Kind: quelor.Defer, Reason: reasonFloor}
	}
	return v, nil
}

// floored reports whether an ADMIT must become DEFER.
// Every demands entry is checked. A reading for that resource that is
// unusable or has Available < qty floors the admit. When several readings
// share a resource, any one of them can floor it. A demand with zero
// matching readings keeps the admit.
func floored(demands map[string]int64, readings []quelor.Reading) bool {
	if len(demands) == 0 {
		return false
	}
	for res, qty := range demands {
		for _, rd := range readings {
			if rd.Resource != res {
				continue
			}
			if !rd.Usable() || rd.Available < qty {
				return true
			}
		}
	}
	return false
}

func isFalseRed(clf *classify.Ruleset, o *trace.Outcome) bool {
	if o.Class != quelor.RealDefect {
		return false
	}
	got := clf.Classify(quelor.ExecutionOutcome{
		ExitCode:   o.ExitCode,
		HTTPStatus: o.HTTPStatus,
		Log:        o.LogExcerpt,
	})
	return got.Class == quelor.ResourceExhausted || got.Class == quelor.DependencyUnavailable
}

func outcomeLabel(o *trace.Outcome) string {
	if o == nil {
		return ""
	}
	if o.Class != "" {
		return string(o.Class)
	}
	if o.Failed {
		return "failed"
	}
	return "ok"
}
