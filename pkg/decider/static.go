package decider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/coherera/quelor/pkg/quelor"
)

// Unavailable builds a response the engine will not enforce.
// reason is stored on Verdict.Reason.
func Unavailable(id, reason string) Response {
	var r Response
	r.DeciderAPIVersion = APIVersion
	r.Status = StatusUnavailable
	r.DeciderID = id
	r.Verdict.Reason = reason
	return r
}

// Static returns a Decider that always answers v with StatusAnswered and
// ModeEnforce. DecisionRef is the first 16 hex chars of
// SHA-256(op + unit_id + verdict kind), with the three strings concatenated
// and no separator.
func Static(v quelor.Verdict, id string) Decider {
	return Func(func(_ context.Context, req Request) (Response, error) {
		var resp Response
		resp.DeciderAPIVersion = APIVersion
		resp.Status = StatusAnswered
		resp.Mode = ModeEnforce
		resp.Verdict = v
		resp.DecisionRef = decisionRef(req.Op, req.Envelope.UnitID, v.Kind)
		resp.DeciderID = id
		resp.TraceID = req.Meta.TraceID
		return resp, nil
	})
}

func decisionRef(op Op, unitID string, kind quelor.VerdictKind) string {
	sum := sha256.Sum256([]byte(string(op) + unitID + string(kind)))
	return hex.EncodeToString(sum[:])[:16]
}
