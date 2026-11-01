package replay

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/coherera/quelor/pkg/classify"
	"github.com/coherera/quelor/pkg/decider"
	"github.com/coherera/quelor/pkg/quelor"
	"github.com/coherera/quelor/pkg/trace"
)

const rateLimitLog = "429 Too Many Requests: rate limit exceeded"

func TestPerfectAgreement(t *testing.T) {
	usable := []quelor.Reading{reading("cpu", true, false, 8)}
	demand := map[string]int64{"cpu": 2}
	recs := []trace.Record{
		acq("a", "u1", quelor.Admit, demand, usable),
		settle("a", okOutcome()),
		acq("b", "u2", quelor.Defer, demand, usable),
		settle("b", okOutcome()),
	}
	rep := mustRun(t, recs, Options{Decider: byUnit(map[string]quelor.VerdictKind{
		"u1": quelor.Admit,
		"u2": quelor.Defer,
	})})
	assertReport(t, rep, Report{
		Total: 4, Scored: 2, Skipped: 2, Agreement: 1,
		Rows: []Row{
			row("a", quelor.Admit, quelor.Admit, "ok"),
			row("b", quelor.Defer, quelor.Defer, "ok"),
		},
	})
}

func TestDisagreement(t *testing.T) {
	recs := []trace.Record{
		acq("b", "u1", quelor.Admit, nil, nil),
		acq("a", "u2", quelor.Defer, nil, nil),
	}
	rep := mustRun(t, recs, Options{Decider: byUnit(map[string]quelor.VerdictKind{
		"u1": quelor.Shed,
		"u2": quelor.Admit,
	})})
	assertReport(t, rep, Report{
		Total: 2, Scored: 2, Agreement: 0,
		Rows: []Row{
			row("b", quelor.Admit, quelor.Shed, ""),
			row("a", quelor.Defer, quelor.Admit, ""),
		},
	})
}

func TestEngineFloor(t *testing.T) {
	cases := []struct {
		name     string
		demands  map[string]int64
		readings []quelor.Reading
		fromDec  quelor.VerdictKind
		original quelor.VerdictKind
		want     quelor.VerdictKind
	}{
		{
			name:     "unknown",
			demands:  map[string]int64{"cpu": 1},
			readings: []quelor.Reading{reading("cpu", false, false, 100)},
			fromDec:  quelor.Admit,
			original: quelor.Admit,
			want:     quelor.Defer,
		},
		{
			name:     "stale",
			demands:  map[string]int64{"cpu": 1},
			readings: []quelor.Reading{reading("cpu", true, true, 100)},
			fromDec:  quelor.Admit,
			original: quelor.Admit,
			want:     quelor.Defer,
		},
		{
			name:     "short",
			demands:  map[string]int64{"cpu": 5},
			readings: []quelor.Reading{reading("cpu", true, false, 4)},
			fromDec:  quelor.Admit,
			original: quelor.Admit,
			want:     quelor.Defer,
		},
		{
			name:     "equal-available",
			demands:  map[string]int64{"cpu": 4},
			readings: []quelor.Reading{reading("cpu", true, false, 4)},
			fromDec:  quelor.Admit,
			original: quelor.Admit,
			want:     quelor.Admit,
		},
		{
			name:     "no-reading-for-demand",
			demands:  map[string]int64{"gpu": 3},
			readings: []quelor.Reading{reading("cpu", false, true, 0)},
			fromDec:  quelor.Admit,
			original: quelor.Admit,
			want:     quelor.Admit,
		},
		{
			name:    "unusable-unrelated",
			demands: map[string]int64{"cpu": 1},
			readings: []quelor.Reading{
				reading("cpu", true, false, 2),
				reading("gpu", false, false, 0),
			},
			fromDec:  quelor.Admit,
			original: quelor.Admit,
			want:     quelor.Admit,
		},
		{
			name:    "one-of-two-short",
			demands: map[string]int64{"cpu": 1, "mem": 8},
			readings: []quelor.Reading{
				reading("cpu", true, false, 4),
				reading("mem", true, false, 7),
			},
			fromDec:  quelor.Admit,
			original: quelor.Admit,
			want:     quelor.Defer,
		},
		{
			name:    "any-bad-reading",
			demands: map[string]int64{"cpu": 1},
			readings: []quelor.Reading{
				reading("cpu", true, false, 10),
				reading("cpu", true, true, 10),
			},
			fromDec:  quelor.Admit,
			original: quelor.Admit,
			want:     quelor.Defer,
		},
		{
			name:    "earlier-bad-reading",
			demands: map[string]int64{"cpu": 1},
			readings: []quelor.Reading{
				reading("cpu", true, true, 10),
				reading("cpu", true, false, 10),
			},
			fromDec:  quelor.Admit,
			original: quelor.Admit,
			want:     quelor.Defer,
		},
		{
			name:    "any-short-reading",
			demands: map[string]int64{"cpu": 5},
			readings: []quelor.Reading{
				reading("cpu", true, false, 9),
				reading("cpu", true, false, 4),
			},
			fromDec:  quelor.Admit,
			original: quelor.Admit,
			want:     quelor.Defer,
		},
		{
			name:    "ignores-capacity",
			demands: map[string]int64{"cpu": 1},
			readings: []quelor.Reading{{
				Resource:  "cpu",
				Known:     true,
				Stale:     false,
				Available: 5,
				Capacity:  0,
			}},
			fromDec:  quelor.Admit,
			original: quelor.Admit,
			want:     quelor.Admit,
		},
		{
			name:     "does-not-override-deny",
			demands:  map[string]int64{"cpu": 1},
			readings: []quelor.Reading{reading("cpu", false, false, 0)},
			fromDec:  quelor.Deny,
			original: quelor.Admit,
			want:     quelor.Deny,
		},
		{
			name:     "does-not-override-shed",
			demands:  map[string]int64{"cpu": 1},
			readings: []quelor.Reading{reading("cpu", false, false, 0)},
			fromDec:  quelor.Shed,
			original: quelor.Admit,
			want:     quelor.Shed,
		},
		{
			name:     "unknown-agrees-with-recorded-defer",
			demands:  map[string]int64{"cpu": 1},
			readings: []quelor.Reading{reading("cpu", false, false, 50)},
			fromDec:  quelor.Admit,
			original: quelor.Defer,
			want:     quelor.Defer,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recs := []trace.Record{acq("d", "u", tc.original, tc.demands, tc.readings)}
			rep := mustRun(t, recs, Options{Decider: fixed(decider.StatusAnswered, tc.fromDec)})
			got := rep.Rows[0]
			if got.Candidate != tc.want || got.Original != tc.original || got.Agree != (tc.original == tc.want) {
				t.Fatalf("row %+v want candidate %s", got, tc.want)
			}
		})
	}
}

func TestFloorChangesMetrics(t *testing.T) {
	bad := []quelor.Reading{reading("cpu", false, false, 100)}
	demand := map[string]int64{"cpu": 1}
	recs := []trace.Record{
		acq("fail", "u1", quelor.Admit, demand, bad),
		settle("fail", failedOutcome("unknown")),
		acq("ok", "u2", quelor.Admit, demand, bad),
		settle("ok", okOutcome()),
	}
	rep := mustRun(t, recs, Options{Decider: fixed(decider.StatusAnswered, quelor.Admit)})
	assertReport(t, rep, Report{
		Total: 4, Scored: 2, Skipped: 2, Agreement: 0,
		DeferredButWouldHaveFinished:     1,
		DeferredButWouldHaveFinishedRate: 0.5,
		FailedOutcomes:                   1,
		Rows: []Row{
			row("fail", quelor.Admit, quelor.Defer, "unknown"),
			row("ok", quelor.Admit, quelor.Defer, "ok"),
		},
	})
}

func TestAdmittedThenFailed(t *testing.T) {
	recs := []trace.Record{
		acq("a", "u", quelor.Admit, nil, nil),
		settle("a", failedOutcome("unknown")),
		settle("b", failedOutcome("unknown")),
	}
	rep := mustRun(t, recs, Options{Decider: fixed(decider.StatusAnswered, quelor.Admit)})
	assertReport(t, rep, Report{
		Total: 3, Scored: 1, Skipped: 2, Agreement: 1,
		AdmittedThenFailed:     1,
		AdmittedThenFailedRate: 1,
		FailedOutcomes:         2,
		Rows:                   []Row{row("a", quelor.Admit, quelor.Admit, "unknown")},
	})
}

func TestDeferredButWouldHaveFinished(t *testing.T) {
	usable := []quelor.Reading{reading("cpu", true, false, 2)}
	demand := map[string]int64{"cpu": 1}
	recs := []trace.Record{
		acq("a", "u1", quelor.Admit, demand, usable),
		settle("a", okOutcome()),
		acq("b", "u2", quelor.Defer, demand, usable),
		settle("b", okOutcome()),
	}
	rep := mustRun(t, recs, Options{Decider: byUnit(map[string]quelor.VerdictKind{
		"u1": quelor.Shed,
		"u2": quelor.Defer,
	})})
	assertReport(t, rep, Report{
		Total: 4, Scored: 2, Skipped: 2, Agreement: 0.5,
		DeferredButWouldHaveFinished:     1,
		DeferredButWouldHaveFinishedRate: 0.5,
		Rows: []Row{
			row("a", quelor.Admit, quelor.Shed, "ok"),
			row("b", quelor.Defer, quelor.Defer, "ok"),
		},
	})
}

func TestRates(t *testing.T) {
	recs := []trace.Record{
		acq("1", "u1", quelor.Admit, nil, nil),
		settle("1", failedOutcome("unknown")),
		acq("2", "u2", quelor.Admit, nil, nil),
		settle("2", okOutcome()),
		acq("3", "u3", quelor.Admit, nil, nil),
		settle("3", okOutcome()),
		acq("4", "u4", quelor.Admit, nil, nil),
	}
	rep := mustRun(t, recs, Options{Decider: byUnit(map[string]quelor.VerdictKind{
		"u1": quelor.Admit,
		"u2": quelor.Admit,
		"u3": quelor.Defer,
		"u4": quelor.Admit,
	})})
	assertReport(t, rep, Report{
		Total: 7, Scored: 4, Skipped: 3,
		Agreement:                        float64(3) / float64(4),
		AdmittedThenFailed:               1,
		AdmittedThenFailedRate:           float64(1) / float64(2),
		DeferredButWouldHaveFinished:     1,
		DeferredButWouldHaveFinishedRate: float64(1) / float64(3),
		FailedOutcomes:                   1,
		Rows: []Row{
			row("1", quelor.Admit, quelor.Admit, "unknown"),
			row("2", quelor.Admit, quelor.Admit, "ok"),
			row("3", quelor.Admit, quelor.Defer, "ok"),
			row("4", quelor.Admit, quelor.Admit, ""),
		},
	})
}

func TestFalseRedRateLimitLog(t *testing.T) {
	evidence := quelor.ExecutionOutcome{ExitCode: 1, HTTPStatus: 429, Log: rateLimitLog}
	cls := classify.Default().Classify(evidence)
	if cls.Class != quelor.ResourceExhausted && cls.Class != quelor.DependencyUnavailable {
		t.Fatalf("classify.Default() => class %q rule %q failed=%v; rate-limit evidence must reclassify as resource-exhausted or dependency-unavailable", cls.Class, cls.RuleID, cls.Failed)
	}
	red := &trace.Outcome{
		Failed:     true,
		Class:      quelor.RealDefect,
		ExitCode:   1,
		HTTPStatus: 429,
		LogExcerpt: rateLimitLog,
	}
	recs := []trace.Record{
		{Op: "settle", DecisionRef: "a", Outcome: red},
		{Op: "settle", DecisionRef: "a", Outcome: red},
		{Op: "settle", DecisionRef: "b", Outcome: &trace.Outcome{
			Failed: true, Class: quelor.Flaky, ExitCode: 1, HTTPStatus: 429, LogExcerpt: rateLimitLog,
		}},
		{Op: "settle", DecisionRef: "c", Outcome: &trace.Outcome{
			Failed: false, Class: quelor.RealDefect, ExitCode: 1, HTTPStatus: 429, LogExcerpt: rateLimitLog,
		}},
	}
	rep := mustRun(t, recs, Options{})
	assertReport(t, rep, Report{
		Total:          4,
		Skipped:        4,
		FalseRed:       float64(2) / float64(3),
		FailedOutcomes: 3,
	})
}

func TestAbstainNeverAdmits(t *testing.T) {
	d := decider.Func(func(context.Context, decider.Request) (decider.Response, error) {
		return decider.Response{
			Status:  decider.StatusAbstained,
			Verdict: quelor.Verdict{Kind: quelor.Admit, Reason: "ignore-me"},
		}, nil
	})
	recs := []trace.Record{
		acq("f", "u1", quelor.Admit, nil, nil),
		settle("f", failedOutcome("unknown")),
		acq("s", "u2", quelor.Admit, nil, nil),
		settle("s", okOutcome()),
	}
	rep := mustRun(t, recs, Options{Decider: d})
	assertReport(t, rep, Report{
		Total: 4, Scored: 2, Skipped: 2, Agreement: 0,
		DeferredButWouldHaveFinished:     1,
		DeferredButWouldHaveFinishedRate: 0.5,
		FailedOutcomes:                   1,
		Rows: []Row{
			row("f", quelor.Admit, quelor.Defer, "unknown"),
			row("s", quelor.Admit, quelor.Defer, "ok"),
		},
	})
}

func TestNonAnsweredStatuses(t *testing.T) {
	cases := []struct {
		name   string
		status decider.Status
	}{
		{name: "abstained", status: decider.StatusAbstained},
		{name: "unavailable", status: decider.StatusUnavailable},
		{name: "killed", status: decider.StatusKilled},
		{name: "empty", status: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := decider.Func(func(context.Context, decider.Request) (decider.Response, error) {
				return decider.Response{Status: tc.status, Verdict: quelor.Verdict{Kind: quelor.Admit}}, nil
			})
			rep := mustRun(t, []trace.Record{
				acq("d", "u", quelor.Admit, nil, nil),
				settle("d", failedOutcome("unknown")),
			}, Options{Decider: d})
			if rep.Rows[0].Candidate != quelor.Defer || rep.AdmittedThenFailed != 0 {
				t.Fatalf("candidate %s atf %d", rep.Rows[0].Candidate, rep.AdmittedThenFailed)
			}
		})
	}
}

func TestModeIgnored(t *testing.T) {
	d := decider.Func(func(context.Context, decider.Request) (decider.Response, error) {
		return decider.Response{
			Status:  decider.StatusAnswered,
			Mode:    decider.ModeLogOnly,
			Verdict: quelor.Verdict{Kind: quelor.Deny},
		}, nil
	})
	rep := mustRun(t, []trace.Record{acq("d", "u", quelor.Admit, nil, nil)}, Options{Decider: d})
	if rep.Rows[0].Candidate != quelor.Deny || rep.Rows[0].Agree {
		t.Fatalf("%+v", rep.Rows[0])
	}
}

func TestSkipped(t *testing.T) {
	env := &quelor.Envelope{UnitID: "u"}
	verdict := &quelor.Verdict{Kind: quelor.Admit}
	var calls int
	d := decider.Func(func(context.Context, decider.Request) (decider.Response, error) {
		calls++
		return decider.Response{Status: decider.StatusAnswered, Verdict: quelor.Verdict{Kind: quelor.Admit}}, nil
	})
	recs := []trace.Record{
		{Op: "acquire"},
		{Op: "acquire", Envelope: env},
		{Op: "acquire", Verdict: verdict},
		{Op: "ACQUIRE", Envelope: env, Verdict: verdict},
		{Op: "renew", Envelope: env, Verdict: verdict},
		{Op: "settle", DecisionRef: "s", Outcome: okOutcome()},
		acq("keep", "u", quelor.Admit, nil, nil),
	}
	rep := mustRun(t, recs, Options{Decider: d})
	if calls != 1 {
		t.Fatalf("decider calls %d", calls)
	}
	assertReport(t, rep, Report{
		Total: 7, Scored: 1, Skipped: 6, Agreement: 1,
		Rows: []Row{row("keep", quelor.Admit, quelor.Admit, "")},
	})
}

func TestOutcomeJoin(t *testing.T) {
	t.Run("last-wins", func(t *testing.T) {
		recs := []trace.Record{
			acq("d", "u", quelor.Admit, nil, nil),
			settle("d", failedOutcome("unknown")),
			settle("d", &trace.Outcome{}),
		}
		rep := mustRun(t, recs, Options{Decider: fixed(decider.StatusAnswered, quelor.Admit)})
		assertReport(t, rep, Report{
			Total: 3, Scored: 1, Skipped: 2, Agreement: 1,
			FailedOutcomes: 1,
			Rows:           []Row{row("d", quelor.Admit, quelor.Admit, "ok")},
		})
	})
	t.Run("empty-class-failed", func(t *testing.T) {
		recs := []trace.Record{
			acq("d", "u", quelor.Admit, nil, nil),
			settle("d", &trace.Outcome{Failed: true, ExitCode: 2}),
		}
		rep := mustRun(t, recs, Options{Decider: fixed(decider.StatusAnswered, quelor.Admit)})
		assertReport(t, rep, Report{
			Total: 2, Scored: 1, Skipped: 1, Agreement: 1,
			AdmittedThenFailed: 1, AdmittedThenFailedRate: 1,
			FailedOutcomes: 1,
			Rows:           []Row{row("d", quelor.Admit, quelor.Admit, "failed")},
		})
	})
}

func TestDeterministic(t *testing.T) {
	recs := []trace.Record{
		acq("b", "u1", quelor.Admit, map[string]int64{"cpu": 3}, []quelor.Reading{reading("cpu", true, false, 1)}),
		settle("b", failedOutcome("unknown")),
		acq("a", "u2", quelor.Defer, nil, nil),
		{Op: "renew", DecisionRef: "nope"},
		settle("a", okOutcome()),
	}
	opt := Options{Decider: byUnit(map[string]quelor.VerdictKind{
		"u1": quelor.Admit,
		"u2": quelor.Admit,
	})}
	a := mustRun(t, recs, opt)
	b := mustRun(t, recs, opt)
	if !reflect.DeepEqual(a, b) || a.String() != b.String() {
		t.Fatalf("not deterministic\n%s\n%s", a.String(), b.String())
	}
	if a.Rows[0].DecisionRef != "b" || a.Rows[0].Candidate != quelor.Defer {
		t.Fatalf("first row %+v", a.Rows[0])
	}
	if a.Rows[1].DecisionRef != "a" || a.Rows[1].Candidate != quelor.Admit {
		t.Fatalf("second row %+v", a.Rows[1])
	}
}

func TestDecideRequestShape(t *testing.T) {
	var got decider.Request
	d := decider.Func(func(_ context.Context, req decider.Request) (decider.Response, error) {
		got = req
		return decider.Response{Status: decider.StatusAnswered, Verdict: quelor.Verdict{Kind: quelor.Admit}}, nil
	})
	recs := []trace.Record{acq("d", "unit-7", quelor.Admit, map[string]int64{"cpu": 1}, []quelor.Reading{reading("cpu", true, false, 3)})}
	recs[0].TraceID = "tr-shape"
	rep := mustRun(t, recs, Options{Decider: d})
	if rep.Rows[0].Candidate != quelor.Admit {
		t.Fatalf("candidate %s", rep.Rows[0].Candidate)
	}
	if got.DeciderAPIVersion != decider.APIVersion || got.Op != decider.OpAcquire {
		t.Fatalf("api %q op %q", got.DeciderAPIVersion, got.Op)
	}
	if got.Meta.TraceID != "tr-shape" || got.Meta.Now != "" {
		t.Fatalf("meta %+v", got.Meta)
	}
	if got.Envelope.UnitID != "unit-7" || got.Envelope.Demands["cpu"] != 1 {
		t.Fatalf("envelope %+v", got.Envelope)
	}
	if len(got.Readings) != 1 || got.Readings[0].Resource != "cpu" || !got.Readings[0].Usable() {
		t.Fatalf("readings %+v", got.Readings)
	}
	if got.Lease != nil || len(got.SpendDelta) != 0 || got.PolicyProfile != "" || got.ExpectRulesetHash != "" {
		t.Fatalf("unexpected request fields %+v", got)
	}
}

func TestEmpty(t *testing.T) {
	rep, err := Run(context.Background(), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertReport(t, rep, Report{})
	if rep.String() == "" || !strings.Contains(rep.String(), "total=0 scored=0 skipped=0\n") {
		t.Fatalf("summary %q", rep.String())
	}
}

func TestReportString(t *testing.T) {
	rep := mustRun(t, []trace.Record{acq("d1", "u", quelor.Admit, nil, nil)}, Options{Decider: fixed(decider.StatusAnswered, quelor.Admit)})
	const want = "total=1 scored=1 skipped=0\n" +
		"agreement=1 false_red=0\n" +
		"admitted_then_failed=0 rate=0\n" +
		"deferred_but_would_have_finished=0 rate=0\n" +
		"failed_outcomes=0\n" +
		"ref=d1 original=ADMIT candidate=ADMIT agree=true outcome=\n"
	if rep.String() != want {
		t.Fatalf("string:\n%q\nwant:\n%q", rep.String(), want)
	}
}

func TestReportJSON(t *testing.T) {
	rep := Report{
		Total:     1,
		Scored:    1,
		Agreement: 1,
		Rows:      []Row{{DecisionRef: "d", Original: quelor.Admit, Candidate: quelor.Defer, Outcome: "ok"}},
	}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"total", "scored", "agreement", "false_red",
		"admitted_then_failed", "deferred_but_would_have_finished",
		"admitted_then_failed_rate", "deferred_but_would_have_finished_rate",
		"failed_outcomes", "skipped", "rows",
	} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing %s in %s", k, b)
		}
	}
	rows, ok := m["rows"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("rows %#v", m["rows"])
	}
	row0, ok := rows[0].(map[string]any)
	if !ok {
		t.Fatalf("row %T", rows[0])
	}
	for _, k := range []string{"decision_ref", "original", "candidate", "agree", "outcome"} {
		if _, ok := row0[k]; !ok {
			t.Fatalf("missing row key %s in %s", k, b)
		}
	}
	if row0["decision_ref"] != "d" || row0["original"] != "ADMIT" || row0["candidate"] != "DEFER" || row0["outcome"] != "ok" || row0["agree"] != false {
		t.Fatalf("row values %v", row0)
	}
}

func TestRunErrors(t *testing.T) {
	recs := []trace.Record{acq("d", "u", quelor.Admit, nil, nil)}
	_, err := Run(context.Background(), recs, Options{})
	if err == nil || !strings.Contains(err.Error(), "decider") {
		t.Fatalf("nil decider: %v", err)
	}
	boom := errors.New("boom")
	_, err = Run(context.Background(), recs, Options{Decider: decider.Func(func(context.Context, decider.Request) (decider.Response, error) {
		return decider.Response{}, boom
	})})
	if !errors.Is(err, boom) {
		t.Fatalf("decide error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Run(ctx, recs, Options{Decider: fixed(decider.StatusAnswered, quelor.Admit)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
	_, err = Run(ctx, nil, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled empty: %v", err)
	}
}

func mustRun(t *testing.T, recs []trace.Record, opt Options) Report {
	t.Helper()
	rep, err := Run(context.Background(), recs, opt)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Total != len(recs) || rep.Scored+rep.Skipped != rep.Total || len(rep.Rows) != rep.Scored {
		t.Fatalf("invariants: total=%d scored=%d skipped=%d rows=%d records=%d",
			rep.Total, rep.Scored, rep.Skipped, len(rep.Rows), len(recs))
	}
	agrees := 0
	for _, row := range rep.Rows {
		if row.Agree != (row.Original == row.Candidate) {
			t.Fatalf("agree flag %#v", row)
		}
		if row.Agree {
			agrees++
		}
	}
	wantAgr := 0.0
	if rep.Scored > 0 {
		wantAgr = float64(agrees) / float64(rep.Scored)
	}
	if rep.Agreement != wantAgr {
		t.Fatalf("agreement %v want %v", rep.Agreement, wantAgr)
	}
	return rep
}

func assertReport(t *testing.T, got, want Report) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("report mismatch\ngot  %s\n%#v\nwant %#v", got.String(), got, want)
	}
}

func acq(ref, unit string, kind quelor.VerdictKind, demands map[string]int64, readings []quelor.Reading) trace.Record {
	return trace.Record{
		Op:          "acquire",
		DecisionRef: ref,
		TraceID:     "tr-" + ref,
		Envelope:    &quelor.Envelope{UnitID: unit, TenantID: "ten", HolderID: "h", Demands: demands},
		Verdict:     &quelor.Verdict{Kind: kind, Reason: "recorded"},
		Readings:    readings,
	}
}

func settle(ref string, o *trace.Outcome) trace.Record {
	return trace.Record{Op: "settle", DecisionRef: ref, Outcome: o}
}

func reading(resource string, known, stale bool, available int64) quelor.Reading {
	return quelor.Reading{Resource: resource, Known: known, Stale: stale, Available: available, Capacity: available}
}

func failedOutcome(class string) *trace.Outcome {
	return &trace.Outcome{Failed: true, Class: quelor.OutcomeClass(class), ExitCode: 1}
}

func okOutcome() *trace.Outcome {
	return &trace.Outcome{}
}

func row(ref string, original, candidate quelor.VerdictKind, outcome string) Row {
	return Row{
		DecisionRef: ref,
		Original:    original,
		Candidate:   candidate,
		Agree:       original == candidate,
		Outcome:     outcome,
	}
}

func fixed(status decider.Status, kind quelor.VerdictKind) decider.Decider {
	return decider.Func(func(context.Context, decider.Request) (decider.Response, error) {
		return decider.Response{
			Status:  status,
			Verdict: quelor.Verdict{Kind: kind},
		}, nil
	})
}

func byUnit(kinds map[string]quelor.VerdictKind) decider.Decider {
	return decider.Func(func(_ context.Context, req decider.Request) (decider.Response, error) {
		kind, ok := kinds[req.Envelope.UnitID]
		if !ok {
			return decider.Response{}, errors.New("unexpected unit " + req.Envelope.UnitID)
		}
		return decider.Response{Status: decider.StatusAnswered, Verdict: quelor.Verdict{Kind: kind}}, nil
	})
}
