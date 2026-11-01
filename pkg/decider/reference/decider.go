package reference

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cel.dev/cel-go/common/types"

	"github.com/coherera/quelor/pkg/decider"
	"github.com/coherera/quelor/pkg/quelor"
)

const deciderID = "reference"

// Decider is the in-process reference implementation of decider.Decider.
type Decider struct {
	rs *Ruleset
}

var refProvenance = decider.Provenance{
	Step:          "rules",
	Tier:          "reference",
	EvidenceClass: "deterministic",
}

var _ decider.Decider = (*Decider)(nil)

// New returns a Decider for a parsed ruleset. A nil ruleset admits everything.
func New(rs *Ruleset) *Decider {
	if rs == nil {
		rs = &Ruleset{Mode: decider.ModeEnforce}
	}
	return &Decider{rs: rs}
}

// Default returns a Decider with no rules in enforce mode.
func Default() *Decider {
	return New(nil)
}

// Decide implements decider.Decider.
func (d *Decider) Decide(ctx context.Context, req decider.Request) (decider.Response, error) {
	if d == nil || d.rs == nil {
		return evaluate(ctx, &Ruleset{Mode: decider.ModeEnforce}, decider.ModeEnforce, req)
	}
	mode := d.rs.Mode
	if mode == "" {
		mode = decider.ModeEnforce
	}
	return evaluate(ctx, d.rs, mode, req)
}

func evaluate(ctx context.Context, rs *Ruleset, mode decider.Mode, req decider.Request) (decider.Response, error) {
	start := time.Now()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return decider.Response{}, err
	}
	if rs == nil {
		resp := baseResponse(req)
		resp.Status = decider.StatusUnavailable
		resp.Abstains = []string{"no-active-ruleset"}
		resp.LatencyMS = elapsedMS(start)
		return resp, nil
	}
	if mode == "" {
		mode = decider.ModeEnforce
	}
	hash := rs.Hash()
	ref := decisionRef(hash, req.Meta.TraceID, req.Envelope.UnitID, string(req.Op))
	if req.ExpectRulesetHash != "" && req.ExpectRulesetHash != hash {
		resp := baseResponse(req)
		resp.Status = decider.StatusAbstained
		resp.Mode = mode
		resp.DecisionRef = ref
		resp.RulesetID = rs.ID
		resp.RulesetHash = hash
		resp.Abstains = []string{"ruleset-moved"}
		resp.LatencyMS = elapsedMS(start)
		return resp, nil
	}

	vars := activation(req)
	var abstains []string
	var matched *Rule
	for i := range rs.Rules {
		rule := &rs.Rules[i]
		if !rule.allows(req.Op) {
			continue
		}
		ok, err := rule.eval(ctx, vars)
		if err != nil {
			if ctx.Err() != nil {
				return decider.Response{}, ctx.Err()
			}
			abstains = append(abstains, rule.ID+":eval-error")
			continue
		}
		if ok {
			matched = rule
			break
		}
	}

	verdict := quelor.Verdict{Kind: quelor.Admit}
	var ruleIDs []string
	if matched != nil {
		verdict.Kind = matched.Verdict
		verdict.Reason = matched.Reason
		ruleIDs = []string{matched.ID}
		if matched.Verdict == quelor.Defer && matched.WakeAfter > 0 {
			wake := evalNow(req.Meta).Add(matched.WakeAfter)
			verdict.WakeAt = &wake
		}
	}

	resp := baseResponse(req)
	resp.Status = decider.StatusAnswered
	resp.Mode = mode
	resp.Verdict = verdict
	resp.DecisionRef = ref
	resp.RulesetID = rs.ID
	resp.RulesetHash = hash
	resp.RuleIDs = ruleIDs
	resp.Abstains = abstains
	resp.LatencyMS = elapsedMS(start)
	return resp, nil
}

func baseResponse(req decider.Request) decider.Response {
	return decider.Response{
		DeciderAPIVersion: decider.APIVersion,
		Readings:          cloneReadings(req.Readings),
		Provenance:        refProvenance,
		TraceID:           req.Meta.TraceID,
		DeciderID:         deciderID,
	}
}

func (r *Rule) eval(ctx context.Context, vars map[string]any) (bool, error) {
	if r == nil || r.prog == nil {
		return false, errors.New("reference: rule is not compiled")
	}
	out, _, err := r.prog.ContextEval(ctx, vars)
	if err != nil {
		return false, err
	}
	if out == nil || types.IsError(out) || types.IsUnknown(out) {
		return false, fmt.Errorf("reference: cel result %v", out)
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("reference: non-bool cel result %v", out.Type())
	}
	return b, nil
}

func activation(req decider.Request) map[string]any {
	return map[string]any{
		"op":            string(req.Op),
		"unit_id":       req.Envelope.UnitID,
		"tenant_id":     req.Envelope.TenantID,
		"holder_id":     req.Envelope.HolderID,
		"affinity_key":  req.Envelope.AffinityKey,
		"coalesce_key":  req.Envelope.CoalesceKey,
		"priority":      string(req.Envelope.Priority),
		"priority_rank": int64(req.Envelope.Priority.Rank()),
		"generation":    req.Envelope.Generation,
		"demands":       nonNilIntMap(req.Envelope.Demands),
		"spend_delta":   nonNilIntMap(req.SpendDelta),
		"readings":      readingValues(req.Readings),
	}
}

func nonNilIntMap(in map[string]int64) map[string]int64 {
	if in == nil {
		return map[string]int64{}
	}
	return in
}

func readingValues(in []quelor.Reading) map[string]any {
	out := make(map[string]any, len(in))
	for _, rd := range in {
		out[rd.Resource] = map[string]any{
			"available": rd.Available,
			"capacity":  rd.Capacity,
			"known":     rd.Known,
			"stale":     rd.Stale,
			"usable":    rd.Usable(),
			"age_ms":    rd.AgeMS,
		}
	}
	return out
}

func evalNow(meta decider.CallMeta) time.Time {
	if meta.Now != "" {
		if t, err := time.Parse(time.RFC3339Nano, meta.Now); err == nil {
			return t
		}
	}
	return time.Now()
}

func decisionRef(rulesetHash, traceID, unitID, op string) string {
	full := hexSHA256([]byte(rulesetHash + traceID + unitID + op))
	return full[:24]
}

func cloneReadings(in []quelor.Reading) []quelor.Reading {
	if len(in) == 0 {
		return nil
	}
	out := make([]quelor.Reading, len(in))
	copy(out, in)
	return out
}

func elapsedMS(start time.Time) float64 {
	return float64(time.Since(start)) / float64(time.Millisecond)
}
