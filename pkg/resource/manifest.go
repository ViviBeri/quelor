// Package resource loads scarce-resource manifests and serves freshness-aware
// readings. Unknown or stale readings are never reported as available capacity.
package resource

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/coherera/quelor/pkg/quelor"
	"gopkg.in/yaml.v3"
)

// CapacityModel selects how an engine accounts for a resource.
type CapacityModel string

const (
	ModelGCRA         CapacityModel = "gcra"
	ModelLease        CapacityModel = "lease"
	ModelHardBudget   CapacityModel = "hard_budget"
	ModelWindowBucket CapacityModel = "window_bucket"
	ModelCircuit      CapacityModel = "circuit"
)

// UnknownPolicy is what an engine should do when a reading is unknown.
// hold_cursor is fail-open: it admits so work can continue from the last
// known cursor. Spend-class resources (hard_budget) must never use it.
type UnknownPolicy string

const (
	UnknownDeferShort UnknownPolicy = "defer_short"
	UnknownAssumeBusy UnknownPolicy = "assume_busy"
	UnknownHoldCursor UnknownPolicy = "hold_cursor"
	UnknownDeny       UnknownPolicy = "deny"
)

// ErrInvalidManifest is returned for a manifest that fails validation.
var ErrInvalidManifest = errors.New("invalid resource manifest")

// ErrFailOpenSpend is returned when hard_budget uses unknown_policy hold_cursor.
// A spend budget must not admit on an unknown reading.
var ErrFailOpenSpend = errors.New("hard_budget cannot use fail-open unknown_policy hold_cursor")

// Duration is a time.Duration that YAML- and JSON-encodes as a Go duration string.
type Duration time.Duration

// String returns the Go duration string, such as "1h0m0s" or "30s".
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML parses a duration string. Null and empty scalars are zero.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value == nil {
		return errors.New("duration: nil yaml node")
	}
	if value.Tag == "!!null" || (value.Kind == yaml.ScalarNode && (value.Value == "" || value.Value == "null" || value.Value == "~")) {
		*d = 0
		return nil
	}
	if value.Kind != yaml.ScalarNode {
		return errors.New("duration: expected a duration string")
	}
	parsed, err := parseDurationString(value.Value)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// MarshalYAML encodes the duration with time.Duration.String.
func (d Duration) MarshalYAML() (interface{}, error) {
	return time.Duration(d).String(), nil
}

// UnmarshalJSON parses a JSON string duration. Null becomes zero.
func (d *Duration) UnmarshalJSON(b []byte) error {
	if d == nil {
		return errors.New("duration: nil receiver")
	}
	if string(b) == "null" {
		*d = 0
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration: expected string: %w", err)
	}
	parsed, err := parseDurationString(s)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// MarshalJSON encodes the duration as a JSON string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func parseDurationString(s string) (Duration, error) {
	if s == "" {
		return 0, nil
	}
	p, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("duration %q: %w", s, err)
	}
	return Duration(p), nil
}

// Manifest is the declared shape of one scarce resource.
type Manifest struct {
	ID               string        `json:"id" yaml:"id"`
	CapacityModel    CapacityModel `json:"capacity_model" yaml:"capacity_model"`
	Capacity         int64         `json:"capacity" yaml:"capacity"`
	Window           Duration      `json:"window" yaml:"window"`
	FreshnessTTL     Duration      `json:"freshness_ttl" yaml:"freshness_ttl"`
	UnknownPolicy    UnknownPolicy `json:"unknown_policy" yaml:"unknown_policy"`
	MinCapacity      int64         `json:"min_capacity" yaml:"min_capacity"`
	FailureThreshold int           `json:"failure_threshold" yaml:"failure_threshold"`
	OpenFor          Duration      `json:"open_for" yaml:"open_for"`
}

// Verdict maps an unknown-reading policy onto an admission verdict.
// defer_short and assume_busy defer. hold_cursor admits so a service can
// continue from its last known cursor. deny refuses.
func (p UnknownPolicy) Verdict() quelor.VerdictKind {
	switch p {
	case UnknownHoldCursor:
		return quelor.Admit
	case UnknownDeny:
		return quelor.Deny
	case UnknownDeferShort, UnknownAssumeBusy:
		return quelor.Defer
	default:
		return quelor.Defer
	}
}

// WithDefaults fills zero-valued policy, AIMD floor, circuit threshold, and open_for.
// Zero means "unset": min_capacity 1, failure_threshold 3, open_for 30s,
// unknown_policy defer_short. The same defaults apply for every capacity model.
func (m *Manifest) WithDefaults() {
	if m.UnknownPolicy == "" {
		m.UnknownPolicy = UnknownDeferShort
	}
	if m.MinCapacity == 0 {
		m.MinCapacity = 1
	}
	if m.FailureThreshold == 0 {
		m.FailureThreshold = 3
	}
	if m.OpenFor == 0 {
		m.OpenFor = Duration(30 * time.Second)
	}
}

// Validate checks the manifest. Zero-valued defaults are applied on a copy,
// so Validate does not mutate the receiver. hard_budget with hold_cursor is
// rejected: a spend-class resource must never fail open on an unknown reading.
func (m Manifest) Validate() error {
	m.WithDefaults()
	if m.ID == "" {
		return fmt.Errorf("%w: id required", ErrInvalidManifest)
	}
	switch m.CapacityModel {
	case ModelGCRA, ModelLease, ModelHardBudget, ModelWindowBucket, ModelCircuit:
	default:
		return fmt.Errorf("%w: capacity_model %q", ErrInvalidManifest, m.CapacityModel)
	}
	if m.CapacityModel == ModelCircuit {
		if m.Capacity < 0 {
			return fmt.Errorf("%w: capacity must be >= 0", ErrInvalidManifest)
		}
	} else if m.Capacity <= 0 {
		return fmt.Errorf("%w: capacity must be > 0", ErrInvalidManifest)
	}
	needsWindow := m.CapacityModel == ModelGCRA || m.CapacityModel == ModelWindowBucket
	if needsWindow && m.Window <= 0 {
		return fmt.Errorf("%w: window must be > 0", ErrInvalidManifest)
	}
	if m.Window < 0 {
		return fmt.Errorf("%w: window must be >= 0", ErrInvalidManifest)
	}
	if m.FreshnessTTL < 0 {
		return fmt.Errorf("%w: freshness_ttl must be >= 0", ErrInvalidManifest)
	}
	switch m.UnknownPolicy {
	case UnknownDeferShort, UnknownAssumeBusy, UnknownHoldCursor, UnknownDeny:
	default:
		return fmt.Errorf("%w: unknown_policy %q", ErrInvalidManifest, m.UnknownPolicy)
	}
	if m.CapacityModel == ModelHardBudget && m.UnknownPolicy == UnknownHoldCursor {
		return fmt.Errorf("%w: %w", ErrInvalidManifest, ErrFailOpenSpend)
	}
	if m.MinCapacity < 1 {
		return fmt.Errorf("%w: min_capacity must be >= 1", ErrInvalidManifest)
	}
	if m.FailureThreshold < 1 {
		return fmt.Errorf("%w: failure_threshold must be >= 1", ErrInvalidManifest)
	}
	if m.OpenFor < 0 {
		return fmt.Errorf("%w: open_for must be >= 0", ErrInvalidManifest)
	}
	return nil
}

// ParseManifest parses one YAML manifest and applies defaults.
// A multi-document stream or a resources list that yields any other count is an error.
func ParseManifest(b []byte) (Manifest, error) {
	ms, err := ParseManifests(b)
	if err != nil {
		return Manifest{}, err
	}
	if len(ms) != 1 {
		return Manifest{}, fmt.Errorf("%w: expected 1 manifest, got %d", ErrInvalidManifest, len(ms))
	}
	return ms[0], nil
}

// ParseManifests parses a YAML stream. Each document is either one manifest or
// a top-level "resources" list. Documents are separated by "---". Unknown fields
// are rejected. Defaults are applied and every manifest is validated.
func ParseManifests(b []byte) ([]Manifest, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	out := make([]Manifest, 0)
	docN := 0
	for {
		var raw yaml.Node
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", docN+1, err)
		}
		docN++
		n := unwrapDoc(&raw)
		if isBlank(n) {
			continue
		}
		if n.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("document %d: manifest must be a mapping", docN)
		}
		var batch []Manifest
		if mappingKey(n, "resources") {
			var wrap struct {
				Resources []Manifest `yaml:"resources"`
			}
			if err := decodeKnown(n, &wrap); err != nil {
				return nil, fmt.Errorf("document %d: %w", docN, err)
			}
			batch = wrap.Resources
		} else {
			var m Manifest
			if err := decodeKnown(n, &m); err != nil {
				return nil, fmt.Errorf("document %d: %w", docN, err)
			}
			batch = []Manifest{m}
		}
		for i := range batch {
			batch[i].WithDefaults()
			if err := batch[i].Validate(); err != nil {
				return nil, fmt.Errorf("document %d: %w", docN, err)
			}
			out = append(out, batch[i])
		}
	}
}

// LoadManifestFile reads a YAML file and parses it with ParseManifests.
func LoadManifestFile(path string) ([]Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load manifests %s: %w", path, err)
	}
	ms, err := ParseManifests(b)
	if err != nil {
		return nil, fmt.Errorf("load manifests %s: %w", path, err)
	}
	return ms, nil
}

func decodeKnown(n *yaml.Node, out any) error {
	b, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	return dec.Decode(out)
}

func unwrapDoc(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = n.Content[0]
	}
	return n
}

func isBlank(n *yaml.Node) bool {
	n = unwrapDoc(n)
	if n == nil || n.Kind == 0 {
		return true
	}
	if n.Tag == "!!null" {
		return true
	}
	if n.Kind == yaml.ScalarNode && (n.Value == "" || n.Value == "null" || n.Value == "~") {
		return true
	}
	return false
}

func mappingKey(n *yaml.Node, key string) bool {
	if n == nil || n.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return true
		}
	}
	return false
}
