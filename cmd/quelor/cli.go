package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/coherera/quelor/pkg/admit"
	"github.com/coherera/quelor/pkg/classify"
	"github.com/coherera/quelor/pkg/decider/reference"
	"github.com/coherera/quelor/pkg/quelor"
	"github.com/coherera/quelor/pkg/replay"
	"github.com/coherera/quelor/pkg/trace"
	"github.com/coherera/quelor/pkg/version"
)

const exitOK = 0
const exitIO = 1
const exitUsage = 2

const usageText = `usage: quelor <command> [flags]

commands:
  admit --resource <github_api|github_search|github_graphql|github_code_search> --need <n> [--optional] [--json] [--api-url <url>]
  classify [--exit-code <n>] [--http-status <n>] [--timed-out] [--log-file <path>|-] [--rules <path>]
  replay --trace <file.jsonl> --decider <file.yaml> [--json]
  version
`

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	switch args[0] {
	case "admit":
		return runAdmit(args[1:], stdout, stderr, getenv)
	case "classify":
		return runClassify(args[1:], stdin, stdout, stderr)
	case "replay":
		return runReplay(args[1:], stdout, stderr)
	case "version":
		return runVersion(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stderr, usageText)
		return exitUsage
	default:
		fmt.Fprintf(stderr, "quelor: unknown command %q\n", args[0])
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
}

func runAdmit(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("quelor admit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	resource := fs.String("resource", "", "GitHub rate-limit resource (github_api, github_search, github_graphql, github_code_search)")
	need := fs.Int64("need", 0, "quantity required; must be > 0")
	optional := fs.Bool("optional", false, "defer instead of degrade when remaining is short")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	apiURL := fs.String("api-url", "", "GitHub API base URL (default $GITHUB_API_URL or https://api.github.com)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "admit: unexpected argument %q\n", fs.Arg(0))
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	if strings.TrimSpace(*resource) == "" || *need <= 0 {
		fmt.Fprintln(stderr, "admit: --resource is required and --need must be > 0")
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	base := strings.TrimSpace(*apiURL)
	if base == "" {
		base = strings.TrimSpace(getenv("GITHUB_API_URL"))
	}
	client := &admit.Client{
		BaseURL: base,
		Token:   strings.TrimSpace(getenv("GITHUB_TOKEN")),
	}
	reading, err := client.Read(context.Background(), strings.TrimSpace(*resource))
	result := admit.Decide(reading, err, *need, *optional)
	if werr := result.WriteGitHubOutput(getenv("GITHUB_OUTPUT")); werr != nil {
		fmt.Fprintf(stderr, "admit: write GITHUB_OUTPUT: %v\n", werr)
		return exitIO
	}
	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			fmt.Fprintf(stderr, "admit: %v\n", err)
			return exitIO
		}
	} else if _, err := fmt.Fprintln(stdout, result.Line()); err != nil {
		fmt.Fprintf(stderr, "admit: %v\n", err)
		return exitIO
	}
	return result.Exit
}

func runClassify(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("quelor classify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	exitCode := fs.Int("exit-code", 0, "process exit code of the unit")
	httpStatus := fs.Int("http-status", 0, "HTTP status observed by the unit")
	timedOut := fs.Bool("timed-out", false, "unit timed out")
	logFile := fs.String("log-file", "", "log file path, or - for stdin")
	rulesPath := fs.String("rules", "", "ruleset file; default ruleset when empty")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "classify: unexpected argument %q\n", fs.Arg(0))
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	logText, err := readClassifyLog(*logFile, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "classify: %v\n", err)
		return exitUsage
	}
	var rs *classify.Ruleset
	if *rulesPath != "" {
		rs, err = classify.Load(*rulesPath)
		if err != nil {
			fmt.Fprintf(stderr, "classify: %v\n", err)
			return exitUsage
		}
	} else {
		rs = classify.Default()
	}
	if rs == nil {
		fmt.Fprintln(stderr, "classify: no ruleset")
		return exitUsage
	}
	got := rs.Classify(quelor.ExecutionOutcome{
		ExitCode:   *exitCode,
		HTTPStatus: *httpStatus,
		Log:        logText,
		TimedOut:   *timedOut,
	})
	if !got.Failed {
		fmt.Fprintln(stdout, "success")
		return exitOK
	}
	fmt.Fprintf(stdout, "%s rule=%s\n", got.Class, got.RuleID)
	return exitOK
}

func readClassifyLog(path string, stdin io.Reader) (string, error) {
	if path != "" && path != "-" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	if path == "" && !readerProvided(stdin) {
		return "", nil
	}
	if stdin == nil {
		return "", nil
	}
	b, err := io.ReadAll(stdin)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func readerProvided(stdin io.Reader) bool {
	if stdin == nil {
		return false
	}
	f, ok := stdin.(*os.File)
	if !ok {
		return true
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice == 0
}

func runReplay(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("quelor replay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tracePath := fs.String("trace", "", "trace jsonl")
	deciderPath := fs.String("decider", "", "reference decider yaml")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "replay: unexpected argument %q\n", fs.Arg(0))
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	if *tracePath == "" || *deciderPath == "" {
		fmt.Fprintln(stderr, "replay: --trace and --decider are required")
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	tf, err := os.Open(*tracePath)
	if err != nil {
		fmt.Fprintf(stderr, "replay: %v\n", err)
		return exitIO
	}
	defer tf.Close()
	recs, err := trace.ReadAll(tf)
	if err != nil {
		fmt.Fprintf(stderr, "replay: %v\n", err)
		return exitIO
	}
	raw, err := os.ReadFile(*deciderPath)
	if err != nil {
		fmt.Fprintf(stderr, "replay: %v\n", err)
		return exitIO
	}
	rs, err := reference.Parse(raw)
	if err != nil {
		fmt.Fprintf(stderr, "replay: %v\n", err)
		return exitIO
	}
	dec := reference.New(rs)
	if dec == nil {
		fmt.Fprintln(stderr, "replay: nil decider")
		return exitIO
	}
	report, err := replay.Run(context.Background(), recs, replay.Options{
		Decider:    dec,
		Classifier: classify.Default(),
	})
	if err != nil {
		fmt.Fprintf(stderr, "replay: %v\n", err)
		return exitIO
	}
	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			fmt.Fprintf(stderr, "replay: %v\n", err)
			return exitIO
		}
		return exitOK
	}
	text := report.String()
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if _, err := io.WriteString(stdout, text); err != nil {
		fmt.Fprintf(stderr, "replay: %v\n", err)
		return exitIO
	}
	return exitOK
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "version: unexpected argument %q\n", args[0])
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	fmt.Fprintln(stdout, version.String())
	return exitOK
}
