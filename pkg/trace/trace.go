// Package trace is the JSONL decision trace for Quelor admission decisions.
// Records follow schema/trace.v1.json (Draft 2020-12,
// https://quelor.dev/schema/trace.v1.json).
package trace

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/coherera/quelor/pkg/quelor"
)

// SchemaVersion is the schema_version value written on every trace record.
const SchemaVersion = "trace.v1"

// maxLineSize is the largest JSONL line ReadAll will buffer.
// 4MiB of payload plus a margin so the trailing newline still fits.
const maxLineSize = 4*1024*1024 + 1024

// Op is the engine operation a trace record describes.
type Op string

const (
	OpAcquire Op = "acquire"
	OpRenew   Op = "renew"
	OpSettle  Op = "settle"
	OpExpire  Op = "expire"
	OpObserve Op = "observe"
)

// DeciderStatus is the decider answer state recorded on a trace line.
// "none" means no decider ran.
type DeciderStatus string

const (
	DeciderStatusAnswered    DeciderStatus = "answered"
	DeciderStatusAbstained   DeciderStatus = "abstained"
	DeciderStatusUnavailable DeciderStatus = "unavailable"
	DeciderStatusKilled      DeciderStatus = "killed"
	DeciderStatusNone        DeciderStatus = "none"
)

// Mode is how a decider verdict was applied. Empty means the field was unset.
type Mode string

const (
	ModeLogOnly Mode = "log-only"
	ModeAdvise  Mode = "advise"
	ModeEnforce Mode = "enforce"
)

// Provenance is decider-defined and opaque to the engine.
type Provenance struct {
	Step          string `json:"step,omitempty"`
	Tier          string `json:"tier,omitempty"`
	EvidenceClass string `json:"evidence_class,omitempty"`
}

// Outcome is the optional settle-time classification summary.
type Outcome struct {
	Class      quelor.OutcomeClass `json:"class,omitempty"`
	ExitCode   int                 `json:"exit_code,omitempty"`
	HTTPStatus int                 `json:"http_status,omitempty"`
	LogExcerpt string              `json:"log_excerpt,omitempty"`
	Failed     bool                `json:"failed,omitempty"`
}

// Record is one JSONL decision-trace line.
type Record struct {
	SchemaVersion   string           `json:"schema_version"`
	TS              string           `json:"ts"`
	Op              Op               `json:"op"`
	DecisionRef     string           `json:"decision_ref"`
	TraceID         string           `json:"trace_id,omitempty"`
	EnvelopeHash    string           `json:"envelope_hash,omitempty"`
	Envelope        *quelor.Envelope `json:"envelope,omitempty"`
	HolderID        string           `json:"holder_id,omitempty"`
	TenantID        string           `json:"tenant_id,omitempty"`
	LeaseID         string           `json:"lease_id,omitempty"`
	Cell            string           `json:"cell,omitempty"`
	Verdict         *quelor.Verdict  `json:"verdict,omitempty"`
	Readings        []quelor.Reading `json:"readings,omitempty"`
	RulesetID       string           `json:"ruleset_id,omitempty"`
	RulesetHash     string           `json:"ruleset_hash,omitempty"`
	DeciderID       string           `json:"decider_id,omitempty"`
	RuleIDs         []string         `json:"rule_ids,omitempty"`
	Provenance      Provenance       `json:"provenance,omitempty"`
	DeciderStatus   DeciderStatus    `json:"decider_status,omitempty"`
	Mode            Mode             `json:"mode,omitempty"`
	Stamps          []string         `json:"stamps,omitempty"`
	DirectiveSigner string           `json:"directive_signer,omitempty"`
	Declared        map[string]int64 `json:"declared,omitempty"`
	Consumed        map[string]int64 `json:"consumed,omitempty"`
	Outcome         *Outcome         `json:"outcome,omitempty"`
	LatencyMS       float64          `json:"latency_ms,omitempty"`
}

// RequiredFields is the JSON names Validate requires.
// schema/trace.v1.json "required" must stay identical to this set.
func RequiredFields() []string {
	return []string{"schema_version", "ts", "op", "decision_ref"}
}

// ValidationError is returned by Validate and lists every problem found.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	if e == nil {
		return "invalid trace record"
	}
	return "invalid trace record: " + strings.Join(e.Problems, "; ")
}

// Validate checks required fields and known enums. Empty optional enums are unset.
func Validate(rec Record) error {
	var problems []string
	if rec.SchemaVersion == "" {
		problems = append(problems, "schema_version is required")
	} else if rec.SchemaVersion != SchemaVersion {
		problems = append(problems, "schema_version must be \"trace.v1\"")
	}
	if rec.TS == "" {
		problems = append(problems, "ts is required")
	} else if _, err := time.Parse(time.RFC3339Nano, rec.TS); err != nil {
		problems = append(problems, "ts must be RFC3339Nano")
	}
	if rec.Op == "" {
		problems = append(problems, "op is required")
	} else if !validOp(rec.Op) {
		problems = append(problems, "op is invalid")
	}
	if rec.DecisionRef == "" {
		problems = append(problems, "decision_ref is required")
	}
	if rec.DeciderStatus != "" && !validDeciderStatus(rec.DeciderStatus) {
		problems = append(problems, "decider_status is invalid")
	}
	if rec.Mode != "" && !validMode(rec.Mode) {
		problems = append(problems, "mode is invalid")
	}
	if rec.Verdict != nil && rec.Verdict.Kind != "" && !validVerdictKind(rec.Verdict.Kind) {
		problems = append(problems, "verdict.kind is invalid")
	}
	if rec.Outcome != nil && rec.Outcome.Class != "" && !validOutcomeClass(rec.Outcome.Class) {
		problems = append(problems, "outcome.class is invalid")
	}
	if len(problems) == 0 {
		return nil
	}
	return &ValidationError{Problems: problems}
}

func validOp(op Op) bool {
	switch op {
	case OpAcquire, OpRenew, OpSettle, OpExpire, OpObserve:
		return true
	default:
		return false
	}
}

func validDeciderStatus(s DeciderStatus) bool {
	switch s {
	case DeciderStatusAnswered, DeciderStatusAbstained, DeciderStatusUnavailable, DeciderStatusKilled, DeciderStatusNone:
		return true
	default:
		return false
	}
}

func validMode(m Mode) bool {
	switch m {
	case ModeLogOnly, ModeAdvise, ModeEnforce:
		return true
	default:
		return false
	}
}

func validVerdictKind(k quelor.VerdictKind) bool {
	switch k {
	case quelor.Admit, quelor.Defer, quelor.Shed, quelor.Deny:
		return true
	default:
		return false
	}
}

func validOutcomeClass(c quelor.OutcomeClass) bool {
	switch c {
	case quelor.ResourceExhausted, quelor.DependencyUnavailable, quelor.Flaky, quelor.RealDefect, quelor.Unknown:
		return true
	default:
		return false
	}
}

// HashEnvelope returns the lowercase hex SHA-256 of env encoded with encoding/json.
// encoding/json sorts object keys, so map insertion order does not change the digest.
func HashEnvelope(env quelor.Envelope) string {
	b, err := json.Marshal(env)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ReadAll parses JSONL records from r. Blank and whitespace-only lines are
// skipped. A line longer than 4MiB yields an error that includes the 1-based
// line number. Malformed JSON likewise names the line.
func ReadAll(r io.Reader) ([]Record, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineSize)
	var out []Record
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("trace: line %d: %w", lineNo, err)
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("trace: line %d: %w", lineNo+1, err)
	}
	return out, nil
}

func cloneRecord(rec Record) Record {
	rec.Envelope = cloneEnvelope(rec.Envelope)
	rec.Verdict = cloneVerdict(rec.Verdict)
	rec.Readings = cloneReadings(rec.Readings)
	rec.RuleIDs = cloneStrings(rec.RuleIDs)
	rec.Stamps = cloneStrings(rec.Stamps)
	rec.Declared = cloneIntMap(rec.Declared)
	rec.Consumed = cloneIntMap(rec.Consumed)
	rec.Outcome = cloneOutcome(rec.Outcome)
	return rec
}

func cloneEnvelope(e *quelor.Envelope) *quelor.Envelope {
	if e == nil {
		return nil
	}
	cp := *e
	cp.Demands = cloneIntMap(e.Demands)
	if e.Deadline != nil {
		d := *e.Deadline
		cp.Deadline = &d
	}
	return &cp
}

func cloneVerdict(v *quelor.Verdict) *quelor.Verdict {
	if v == nil {
		return nil
	}
	cp := *v
	cp.Stamps = cloneStrings(v.Stamps)
	if v.WakeAt != nil {
		t := *v.WakeAt
		cp.WakeAt = &t
	}
	return &cp
}

func cloneOutcome(o *Outcome) *Outcome {
	if o == nil {
		return nil
	}
	cp := *o
	return &cp
}

func cloneReadings(in []quelor.Reading) []quelor.Reading {
	if in == nil {
		return nil
	}
	out := make([]quelor.Reading, len(in))
	copy(out, in)
	return out
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneIntMap(in map[string]int64) map[string]int64 {
	if in == nil {
		return nil
	}
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
