# Good first issues

Starter work drawn from the pre-release review and from gaps visible in the code today.
Each item names the files to read and a "done when" you can check. Open an issue (or
comment on an existing one) before you start so nobody duplicates it. Read
[CONTRIBUTING.md](../CONTRIBUTING.md) first: every change needs a test, and a new control
needs a test that fails without it.

Sign off every commit (`git commit -s`).

## 1. Tests for `cmd/quelord`

`go test -cover ./cmd/...` reports 0.0% for `cmd/quelord`. `run()` in
`cmd/quelord/main.go` already returns an exit code, so it can be driven directly.

Done when: tests cover `-version`, a missing `-config` (exit 2), an unreadable or invalid
config (exit 1), and a clean start and shutdown on a `127.0.0.1:0` listener (you may need to
make the signal context injectable). Use a temporary directory for the config and trace file.

## 2. Tests for `cmd/quelor-bench`

Also at 0.0% coverage. `main()` in `cmd/quelor-bench/main.go` parses `-units` and `-json`
and prints `bench.RunAll` output.

Done when: the flag handling is split into a function that takes arguments and a writer,
and tests check that `-json` output parses as JSON and that a small `-units` value runs
fast. Keep the CLI's behavior and output format unchanged.

## 3. Timed-out cases in the classifier signature fixtures

`pkg/classify/testdata/gha_signatures.json` has no way to set `TimedOut`, so the
timeout rules are only covered by unit tests in `classify_test.go`.

Done when: the fixture row struct in `pkg/classify/signatures_test.go` accepts an optional
`timed_out`, and there are rows for a bare timeout (`gha-wall-clock`), a timeout with
connection-failure text (`gha-dependency`), and a timeout with a 503 status.

## 4. Review default ruleset ids before 0.2

Rule ids are meant to be stable, but some names are weak: the `gha-` prefix ties an id to
one platform; `gha-rate-limit` and `gha-rate-limit-text` split by mechanism and not by
meaning; `gha-real-defect` is named after its class; `gha-dependency`, `gha-upstream-5xx`
and `gha-timeout` overlap. See `pkg/classify/default_gha.yaml`.

Done when: a short proposal (an issue or a markdown file) lists each id, whether to keep or
rename it, and the migration note for consumers who key on `RuleID`. Do not rename
anything in the same change. `RuleSuccess`, `RuleSuperseded` and `RuleDefault` are already
exported constants in `pkg/classify/classify.go`; the proposal should say whether the
ruleset ids get constants too.

## 5. Make `conformance` and `bench` honest about `*testing.T`

`conformance.Factory` and `RunEngine` take `*testing.T` (`conformance/suite.go`), and
`bench` is an importable root package whose scenarios are meant for a command. That is
exported API surface that imports the `testing` package.

Done when: each package's doc comment states that it is for test and tooling use, and an
issue or a design note compares two options: keep as is with a stability note, or move to
`internal/` or a `testing`-suffixed path with a thin public wrapper. Pick no breaking
change without maintainer sign-off.

## 6. Remove or justify the `server.Options` placeholder

`server.New(be Backend, _ Options)` in `pkg/server/server.go` takes an empty struct that
is ignored. Callers (`cmd/quelord/main.go` and the server tests) pass `server.Options{}`.

Done when: the options are either removed from the signature (with all callers updated),
or one real option is added with a test, for example a request body limit (`maxBody` in
`server.go` is a constant today). Discuss the choice in the issue first, since this is
public API.

## 7. De-duplicate `Mode`, `Op` and `Provenance`

`pkg/decider/types.go` and `pkg/trace/trace.go` define the same `Mode`, `Op` and
`Provenance` shapes separately, and the engine converts between them.

Done when: one package owns each type and the other uses a type alias (or the engine
conversion is documented as intentional), `go build ./...` and the trace schema test still
pass, and the JSON for both is unchanged.

## 8. Sign release artifacts and publish an SBOM

`.goreleaser.yaml` publishes archives and `checksums.txt` but does not sign the checksums
or attach an SBOM. Builds already use `-trimpath` and a commit-timestamp `mod_timestamp`.

Done when: the goreleaser config adds keyless `cosign` signing of the checksum file and an
SBOM for each archive, `release.yml` gets the permissions it needs (`id-token: write`),
and `SECURITY.md` explains how to verify a download. Check the goreleaser v2 docs for
the exact keys.

## 9. Keep holder tokens out of the config file

`quelord.yaml` takes a `shared_token_env` (read from the environment), but
`holder_tokens` is a plaintext map in the YAML file (`pkg/server/config.go`).

Done when: the config can read per-holder tokens from a file or from environment
variables, the plaintext map still works, a test shows the file source wins or loses to
the map in a documented order, and the new field is described in the README's
"Run the daemon" section. Never log a token.
