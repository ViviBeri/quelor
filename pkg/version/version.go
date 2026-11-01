// Package version carries build metadata injected by goreleaser.
package version

import "fmt"

var (
	Version = "0.1.0-dev"
	Commit  = "none"
)

// String returns the one-line version.
func String() string { return fmt.Sprintf("quelor %s (%s)", Version, Commit) }
