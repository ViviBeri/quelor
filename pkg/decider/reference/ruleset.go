// Package reference implements the open rules-as-data Decider.
// A ruleset is YAML plus CEL. Expressions are compiled once at parse time.
// Mode is a pointer-side setting and is not part of the content hash.
package reference

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"cel.dev/cel-go/cel"
	"gopkg.in/yaml.v3"

	"github.com/coherera/quelor/pkg/decider"
	"github.com/coherera/quelor/pkg/quelor"
)

// Ruleset is an immutable compiled policy document.
type Ruleset struct {
	ID    string
	Mode  decider.Mode
	Rules []Rule
}

// Rule is one compiled admission rule. Ops empty matches acquire and renew.
type Rule struct {
	ID        string
	When      string
	Verdict   quelor.VerdictKind
	Reason    string
	WakeAfter time.Duration
	Ops       []decider.Op

	prog cel.Program
}

type yamlRuleset struct {
	ID    string     `yaml:"id"`
	Mode  string     `yaml:"mode"`
	Rules []yamlRule `yaml:"rules"`
}

type yamlRule struct {
	ID        string   `yaml:"id"`
	When      string   `yaml:"when"`
	Verdict   string   `yaml:"verdict"`
	Reason    string   `yaml:"reason"`
	WakeAfter string   `yaml:"wake_after"`
	Ops       []string `yaml:"ops"`
}

type canonDoc struct {
	ID    string      `json:"id"`
	Rules []canonRule `json:"rules"`
}

type canonRule struct {
	ID        string   `json:"id"`
	When      string   `json:"when"`
	Verdict   string   `json:"verdict"`
	Reason    string   `json:"reason"`
	WakeAfter string   `json:"wake_after"`
	Ops       []string `json:"ops"`
}

var (
	celEnvOnce sync.Once
	celEnv     *cel.Env
	celEnvErr  error
)

// celEnvironment declares readings as map(string, dyn). CEL's checker rejects
// field selection on map types; rules use readings[id].available. Each value
// is still a map of available, capacity, known, stale, usable, and age_ms.
func celEnvironment() (*cel.Env, error) {
	celEnvOnce.Do(func() {
		celEnv, celEnvErr = cel.NewEnv(
			cel.Variable("op", cel.StringType),
			cel.Variable("unit_id", cel.StringType),
			cel.Variable("tenant_id", cel.StringType),
			cel.Variable("holder_id", cel.StringType),
			cel.Variable("affinity_key", cel.StringType),
			cel.Variable("coalesce_key", cel.StringType),
			cel.Variable("priority", cel.StringType),
			cel.Variable("priority_rank", cel.IntType),
			cel.Variable("generation", cel.IntType),
			cel.Variable("demands", cel.MapType(cel.StringType, cel.IntType)),
			cel.Variable("spend_delta", cel.MapType(cel.StringType, cel.IntType)),
			cel.Variable("readings", cel.MapType(cel.StringType, cel.DynType)),
		)
	})
	return celEnv, celEnvErr
}

// Parse decodes a ruleset YAML document and compiles every rule.
func Parse(data []byte) (*Ruleset, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var doc yamlRuleset
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("reference: yaml: %w", err)
	}
	mode, err := parseMode(doc.Mode)
	if err != nil {
		return nil, err
	}
	env, err := celEnvironment()
	if err != nil {
		return nil, fmt.Errorf("reference: cel env: %w", err)
	}
	seen := make(map[string]struct{}, len(doc.Rules))
	rules := make([]Rule, 0, len(doc.Rules))
	for _, raw := range doc.Rules {
		rule, err := buildRule(env, raw, seen)
		if err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	return &Ruleset{ID: doc.ID, Mode: mode, Rules: rules}, nil
}

func parseMode(s string) (decider.Mode, error) {
	switch decider.Mode(s) {
	case "":
		return decider.ModeEnforce, nil
	case decider.ModeLogOnly, decider.ModeAdvise, decider.ModeEnforce:
		return decider.Mode(s), nil
	default:
		return "", fmt.Errorf("reference: invalid mode %q", s)
	}
}

func buildRule(env *cel.Env, raw yamlRule, seen map[string]struct{}) (Rule, error) {
	if raw.ID == "" {
		return Rule{}, errors.New("reference: rule id is empty")
	}
	if _, ok := seen[raw.ID]; ok {
		return Rule{}, fmt.Errorf("reference: duplicate rule id %q", raw.ID)
	}
	seen[raw.ID] = struct{}{}

	if !validVerdict(raw.Verdict) {
		return Rule{}, fmt.Errorf("reference: rule %q: invalid verdict %q", raw.ID, raw.Verdict)
	}
	kind := quelor.VerdictKind(raw.Verdict)

	var wake time.Duration
	if raw.WakeAfter != "" {
		if kind != quelor.Defer {
			return Rule{}, fmt.Errorf("reference: rule %q: wake_after is only valid for DEFER", raw.ID)
		}
		d, err := time.ParseDuration(raw.WakeAfter)
		if err != nil {
			return Rule{}, fmt.Errorf("reference: rule %q: wake_after: %w", raw.ID, err)
		}
		if d < 0 {
			return Rule{}, fmt.Errorf("reference: rule %q: wake_after must not be negative", raw.ID)
		}
		wake = d
	}

	ops := make([]decider.Op, 0, len(raw.Ops))
	for _, op := range raw.Ops {
		switch decider.Op(op) {
		case decider.OpAcquire, decider.OpRenew:
			ops = append(ops, decider.Op(op))
		default:
			return Rule{}, fmt.Errorf("reference: rule %q: invalid op %q", raw.ID, op)
		}
	}

	prog, err := compileWhen(env, raw.When)
	if err != nil {
		return Rule{}, fmt.Errorf("reference: rule %q: %w", raw.ID, err)
	}
	return Rule{
		ID:        raw.ID,
		When:      raw.When,
		Verdict:   kind,
		Reason:    raw.Reason,
		WakeAfter: wake,
		Ops:       ops,
		prog:      prog,
	}, nil
}

func validVerdict(v string) bool {
	switch quelor.VerdictKind(v) {
	case quelor.Admit, quelor.Defer, quelor.Shed, quelor.Deny:
		return true
	default:
		return false
	}
}

func compileWhen(env *cel.Env, when string) (cel.Program, error) {
	ast, iss := env.Compile(when)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("cel: %w", iss.Err())
	}
	got := "none"
	if ast != nil && ast.OutputType() != nil {
		got = ast.OutputType().String()
	}
	if ast == nil || got != "bool" {
		return nil, fmt.Errorf("cel: when must return bool, got %s", got)
	}
	prg, err := env.Program(ast, cel.EvalOptions(cel.OptOptimize), cel.CostLimit(100000))
	if err != nil {
		return nil, fmt.Errorf("cel: %w", err)
	}
	return prg, nil
}

func (r Rule) allows(op decider.Op) bool {
	if len(r.Ops) == 0 {
		return true
	}
	for _, allowed := range r.Ops {
		if allowed == op {
			return true
		}
	}
	return false
}

// Hash is the hex SHA-256 of the canonical JSON object {id, rules}.
// Mode is excluded. Formatting of the source YAML does not affect the digest.
func (rs *Ruleset) Hash() string {
	if rs == nil {
		return ""
	}
	return hexSHA256(canonicalJSON(rs))
}

func canonicalJSON(rs *Ruleset) []byte {
	doc := canonDoc{
		ID:    rs.ID,
		Rules: make([]canonRule, 0, len(rs.Rules)),
	}
	for _, rule := range rs.Rules {
		ops := make([]string, 0, len(rule.Ops))
		for _, op := range rule.Ops {
			ops = append(ops, string(op))
		}
		wake := ""
		if rule.WakeAfter > 0 {
			wake = rule.WakeAfter.String()
		}
		doc.Rules = append(doc.Rules, canonRule{
			ID:        rule.ID,
			When:      rule.When,
			Verdict:   string(rule.Verdict),
			Reason:    rule.Reason,
			WakeAfter: wake,
			Ops:       ops,
		})
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return []byte("{}")
	}
	b := buf.Bytes()
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	return b
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
