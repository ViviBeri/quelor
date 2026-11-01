// Package conformance is the public Quelor conformance suite. Third parties
// implement Factory or DeciderFactory and call RunEngine or RunDecider.
package conformance

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"
)

// Version is the conformance suite revision written into every report.
const Version = "1"

// Report is the JSON document produced by a suite run.
type Report struct {
	Suite   string       `json:"suite"`
	Version string       `json:"version"`
	Passed  int          `json:"passed"`
	Failed  int          `json:"failed"`
	Cases   []CaseResult `json:"cases"`
}

// CaseResult is one published test id.
type CaseResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
}

// WriteJSON encodes the report as indented JSON, including a trailing newline.
func (r Report) WriteJSON(w io.Writer) error {
	if w == nil {
		return errors.New("conformance: nil writer")
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func summarize(suite string, cases []CaseResult) Report {
	if cases == nil {
		cases = []CaseResult{}
	}
	out := Report{
		Suite:   suite,
		Version: Version,
		Cases:   cases,
	}
	for _, c := range cases {
		if c.Passed {
			out.Passed++
		} else {
			out.Failed++
		}
	}
	return out
}

func publishReport(t *testing.T, rep *Report) {
	if t == nil || rep == nil {
		return
	}
	path := os.Getenv("QUELOR_CONFORMANCE_JSON")
	if path == "" {
		return
	}
	t.Cleanup(func() {
		f, err := os.Create(path)
		if err != nil {
			t.Errorf("QUELOR_CONFORMANCE_JSON: %v", err)
			return
		}
		defer f.Close()
		if err := rep.WriteJSON(f); err != nil {
			t.Errorf("QUELOR_CONFORMANCE_JSON: %v", err)
		}
	})
}
