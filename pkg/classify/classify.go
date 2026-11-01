// Package classify maps an execution outcome onto Quelor's five-class
// failure classes using an ordered, first-match ruleset.
package classify

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"regexp"
	"sync"

	"github.com/coherera/quelor/pkg/quelor"
	"gopkg.in/yaml.v3"
)

// Rule is one ordered classifier entry. A rule matches when any specified
// condition matches. Empty exit_codes, empty http_status, an empty log_regex,
// and a nil TimedOut are unspecified and are not conditions.
type Rule struct {
	ID         string              `json:"id" yaml:"id"`
	Class      quelor.OutcomeClass `json:"class" yaml:"class"`
	ExitCodes  []int               `json:"exit_codes,omitempty" yaml:"exit_codes,omitempty"`
	HTTPStatus []int               `json:"http_status,omitempty" yaml:"http_status,omitempty"`
	LogRegex   string              `json:"log_regex,omitempty" yaml:"log_regex,omitempty"`
	TimedOut   *bool               `json:"timed_out,omitempty" yaml:"timed_out,omitempty"`
	Note       string              `json:"note,omitempty" yaml:"note,omitempty"`

	re *regexp.Regexp
}

// Ruleset is an immutable, ordered rule list. Parse compiles every log_regex;
// callers share the pointer and leave the value unchanged. Classify is then
// safe for concurrent use.
type Ruleset struct {
	ID      string              `json:"id" yaml:"id"`
	Rules   []Rule              `json:"rules" yaml:"rules"`
	Default quelor.OutcomeClass `json:"default" yaml:"default"`
}

// Result is the classification of one execution outcome.
// RuleID is "success" for a clean outcome and empty when Default is used.
type Result struct {
	Failed bool
	Class  quelor.OutcomeClass
	RuleID string
}

// Classify applies rs to o. Exit 0 with HTTP status below 400 (0 counts) and
// TimedOut false is success before any rule runs. Otherwise the first matching
// rule wins. With no match, Class is the ruleset default and RuleID is empty.
// TODO(v0.2): exit 0 is treated as success before any rule runs, so a job the
// platform refused before step 1 can read green. Running the rules first would
// break the contract that a green exit ignores log text (see the "success
// ignores panic log" cases) and the default ruleset has no refusal signature,
// so the fix needs a signal ExecutionOutcome does not carry yet; deferred to
// the v0.2 API review rather than adding a public field now.
func (rs *Ruleset) Classify(o quelor.ExecutionOutcome) Result {
	if o.ExitCode == 0 && o.HTTPStatus < 400 && !o.TimedOut {
		return Result{RuleID: "success"}
	}
	for i := range rs.Rules {
		if rs.Rules[i].matches(o) {
			return Result{
				Failed: true,
				Class:  rs.Rules[i].Class,
				RuleID: rs.Rules[i].ID,
			}
		}
	}
	return Result{Failed: true, Class: rs.Default}
}

// matches reports whether any specified condition holds.
func (r *Rule) matches(o quelor.ExecutionOutcome) bool {
	if len(r.ExitCodes) > 0 && intsContain(r.ExitCodes, o.ExitCode) {
		return true
	}
	if len(r.HTTPStatus) > 0 && intsContain(r.HTTPStatus, o.HTTPStatus) {
		return true
	}
	if r.re != nil && r.re.MatchString(o.Log) {
		return true
	}
	if r.TimedOut != nil && *r.TimedOut == o.TimedOut {
		return true
	}
	return false
}

func (r *Rule) hasCondition() bool {
	return len(r.ExitCodes) > 0 || len(r.HTTPStatus) > 0 || r.LogRegex != "" || r.TimedOut != nil
}

func intsContain(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func validClass(c quelor.OutcomeClass) bool {
	switch c {
	case quelor.ResourceExhausted,
		quelor.DependencyUnavailable,
		quelor.Flaky,
		quelor.RealDefect,
		quelor.Unknown:
		return true
	default:
		return false
	}
}

// Parse decodes a YAML ruleset. Unknown fields are rejected. Every class must
// be one of the five outcome classes, every rule needs at least one condition,
// and every log_regex is compiled once.
func Parse(b []byte) (*Ruleset, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, fmt.Errorf("classify: parse yaml: empty document")
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var rs Ruleset
	if err := dec.Decode(&rs); err != nil {
		return nil, fmt.Errorf("classify: parse yaml: %w", err)
	}
	if err := rs.validate(); err != nil {
		return nil, err
	}
	return &rs, nil
}

func (rs *Ruleset) validate() error {
	if !validClass(rs.Default) {
		return fmt.Errorf("classify: default class %q is not a known outcome class", rs.Default)
	}
	for i := range rs.Rules {
		r := &rs.Rules[i]
		if !validClass(r.Class) {
			return fmt.Errorf("classify: rules[%d] id %q: class %q is not a known outcome class", i, r.ID, r.Class)
		}
		if !r.hasCondition() {
			return fmt.Errorf("classify: rules[%d] id %q: no conditions", i, r.ID)
		}
		if r.LogRegex == "" {
			continue
		}
		re, err := regexp.Compile(r.LogRegex)
		if err != nil {
			return fmt.Errorf("classify: rules[%d] id %q: log_regex: %w", i, r.ID, err)
		}
		r.re = re
	}
	return nil
}

// Load reads a YAML ruleset from path.
func Load(path string) (*Ruleset, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("classify: load %s: %w", path, err)
	}
	return Parse(b)
}

//go:embed default_gha.yaml
var defaultGHA []byte

var (
	defaultOnce sync.Once
	defaultRS   *Ruleset
	defaultErr  error
)

// Default returns the shared embedded GitHub Actions ruleset.
// The pointer is stable; callers leave the value unchanged.
func Default() *Ruleset {
	defaultOnce.Do(func() {
		defaultRS, defaultErr = Parse(defaultGHA)
	})
	if defaultErr != nil {
		panic("classify: embedded default ruleset is invalid: " + defaultErr.Error())
	}
	return defaultRS
}
