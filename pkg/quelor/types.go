// Package quelor holds the open contract types: envelope, lease, verdict,
// outcome classes and the lease API request/response shapes.
package quelor

import (
	"context"
	"time"
)

// Priority orders work. Lower Rank wins.
type Priority string

const (
	PriorityOperatorDirective Priority = "operator-directive"
	PriorityReleaseCritical   Priority = "release-critical"
	PriorityChange            Priority = "change"
	PriorityScheduled         Priority = "scheduled"
	PriorityMaintenance       Priority = "maintenance"
)

// Rank returns 0 (highest priority) .. 4 (lowest). Unknown priorities rank as change.
func (p Priority) Rank() int {
	switch p {
	case PriorityOperatorDirective:
		return 0
	case PriorityReleaseCritical:
		return 1
	case PriorityChange, "":
		return 2
	case PriorityScheduled:
		return 3
	case PriorityMaintenance:
		return 4
	}
	return 2
}

// CoalescePolicy decides what happens when a second unit shares a coalesce key.
type CoalescePolicy string

const (
	CoalesceSupersede CoalescePolicy = "SUPERSEDE" // default: newer generation replaces older
	CoalesceDropNew   CoalescePolicy = "DROP_NEW"  // keep the existing unit, shed the new one
)

// Envelope describes one admittable unit of work.
type Envelope struct {
	UnitID         string           `json:"unit_id"`
	TenantID       string           `json:"tenant_id"`
	HolderID       string           `json:"holder_id"`
	Demands        map[string]int64 `json:"demands"` // resource id -> quantity
	AffinityKey    string           `json:"affinity_key,omitempty"`
	CoalesceKey    string           `json:"coalesce_key,omitempty"`
	CoalescePolicy CoalescePolicy   `json:"coalesce_policy,omitempty"`
	Generation     int64            `json:"generation,omitempty"`
	Priority       Priority         `json:"priority,omitempty"`
	Deadline       *time.Time       `json:"deadline,omitempty"`
}

// VerdictKind is the admission decision.
type VerdictKind string

const (
	Admit VerdictKind = "ADMIT"
	Defer VerdictKind = "DEFER"
	Shed  VerdictKind = "SHED"
	Deny  VerdictKind = "DENY"
)

// Verdict is a decision plus its reason. Stamps record engine-side markers such
// as "bound-unreachable" or "ruleset-moved".
type Verdict struct {
	Kind      VerdictKind `json:"kind"`
	Reason    string      `json:"reason,omitempty"`
	WakeAt    *time.Time  `json:"wake_at,omitempty"`    // DEFER wake hint
	WakeToken string      `json:"wake_token,omitempty"` // DEFER wake grant
	Hibernate bool        `json:"hibernate,omitempty"`  // renew refusal: checkpoint
	Stamps    []string    `json:"stamps,omitempty"`
}

// Lease is a granted reservation.
type Lease struct {
	ID          string           `json:"id"`
	UnitID      string           `json:"unit_id"`
	TenantID    string           `json:"tenant_id"`
	HolderID    string           `json:"holder_id"`
	Declared    map[string]int64 `json:"declared"`
	GrantedAt   time.Time        `json:"granted_at"`
	ExpiresAt   time.Time        `json:"expires_at"`
	DecisionRef string           `json:"decision_ref"`
	Renewals    int              `json:"renewals"`
}

// OutcomeClass is the five-class failure classes.
type OutcomeClass string

const (
	ResourceExhausted     OutcomeClass = "resource-exhausted"
	DependencyUnavailable OutcomeClass = "dependency-unavailable"
	Flaky                 OutcomeClass = "flaky"
	RealDefect            OutcomeClass = "real-defect"
	Unknown               OutcomeClass = "unknown"
)

// ExecutionOutcome is the structured result of a unit; the classifier reads it.
//
// A run cancelled because a newer generation superseded it is reported by the
// engine as neither a failure nor a class (see SettleLease), so it does not
// count against a retry budget.
//
// TODO(v0.2): a job the platform refused before step 1 can still exit 0 and
// read as success. ExecutionOutcome carries no field that tells the two apart;
// decide the public shape in the v0.2 API review (no field is added in v0.1.0).
type ExecutionOutcome struct {
	ExitCode   int    `json:"exit_code"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Log        string `json:"log,omitempty"`
	TimedOut   bool   `json:"timed_out,omitempty"`
}

// Reading is a resource reading with freshness. Unknown readings are never capacity.
type Reading struct {
	Resource   string    `json:"resource"`
	Available  int64     `json:"available"`
	Capacity   int64     `json:"capacity"`
	Known      bool      `json:"known"`
	Stale      bool      `json:"stale"`
	ObservedAt time.Time `json:"observed_at"`
	AgeMS      int64     `json:"age_ms"`
}

// Usable reports whether the reading may be counted as capacity.
func (r Reading) Usable() bool { return r.Known && !r.Stale }

// Observation is an observed upstream status for a resource (drives AIMD and circuits).
type Observation struct {
	Resource   string        `json:"resource"`
	HTTPStatus int           `json:"http_status"`
	RetryAfter time.Duration `json:"retry_after,omitempty"`
}

type AcquireRequest struct {
	Envelope Envelope `json:"envelope"`
	Token    string   `json:"-"`
}
type AcquireResult struct {
	Verdict     Verdict `json:"verdict"`
	Lease       *Lease  `json:"lease,omitempty"`
	DecisionRef string  `json:"decision_ref"`
}
type RenewRequest struct {
	LeaseID  string           `json:"lease_id"`
	HolderID string           `json:"holder_id"`
	Token    string           `json:"-"`
	Delta    map[string]int64 `json:"delta,omitempty"` // next quantum to reserve
}
type RenewResult struct {
	Verdict     Verdict `json:"verdict"`
	Lease       *Lease  `json:"lease,omitempty"`
	DecisionRef string  `json:"decision_ref"`
}
type SettleRequest struct {
	LeaseID      string            `json:"lease_id"`
	HolderID     string            `json:"holder_id"`
	Token        string            `json:"-"`
	Consumed     map[string]int64  `json:"consumed,omitempty"`
	Outcome      *ExecutionOutcome `json:"outcome,omitempty"`
	Observations []Observation     `json:"observations,omitempty"`
}
type SettleResult struct {
	LeaseID   string           `json:"lease_id"`
	Declared  map[string]int64 `json:"declared"`
	Consumed  map[string]int64 `json:"consumed"`
	Delta     map[string]int64 `json:"delta"` // declared - consumed
	Duplicate bool             `json:"duplicate"`
	Class     OutcomeClass     `json:"class,omitempty"`
}

// LeaseAPI is the engine surface conformance tests drive.
type LeaseAPI interface {
	AcquireLease(ctx context.Context, r AcquireRequest) (AcquireResult, error)
	RenewLease(ctx context.Context, r RenewRequest) (RenewResult, error)
	SettleLease(ctx context.Context, r SettleRequest) (SettleResult, error)
}
