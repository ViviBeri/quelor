// Package decision contains types shared by decision requests and traces.
// It has no dependency on either consumer package.
package decision

// Op identifies an engine operation. Individual APIs expose only the
// operation constants relevant to their contract.
type Op string

// Mode describes how a decider verdict is applied.
type Mode string

const (
	ModeLogOnly Mode = "log-only"
	ModeAdvise  Mode = "advise"
	ModeEnforce Mode = "enforce"
)

// Provenance is decider-defined metadata and remains opaque to the engine.
type Provenance struct {
	Step          string `json:"step,omitempty"`
	Tier          string `json:"tier,omitempty"`
	EvidenceClass string `json:"evidence_class,omitempty"`
}
