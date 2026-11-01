// Command quelor-bench prints a Quelor admission-control scenario report.
package main

import (
	"flag"
	"fmt"

	"github.com/coherera/quelor/bench"
)

func main() {
	units := flag.Int("units", 20000, "units each of 3 holders submits in the shared-token-bucket scenario")
	asJSON := flag.Bool("json", false, "print the report as JSON")
	flag.Parse()

	rep := bench.RunAll(bench.Options{
		Units:          *units,
		Seed:           1,
		LatencySamples: 20000,
	})
	if *asJSON {
		fmt.Print(rep.JSON())
		return
	}
	fmt.Print(rep.String())
}
