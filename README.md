# Quelor

**Workload admission and flow control for CI and agents. Stop starting work that cannot finish.**

[![License](https://img.shields.io/github/license/coherera/quelor)](LICENSE)
[![CI](https://github.com/coherera/quelor/actions/workflows/ci.yml/badge.svg)](https://github.com/coherera/quelor/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/coherera/quelor)](https://goreportcard.com/report/github.com/coherera/quelor)
[![Go Reference](https://pkg.go.dev/badge/github.com/coherera/quelor.svg)](https://pkg.go.dev/github.com/coherera/quelor)
[![Release](https://img.shields.io/github/v/release/coherera/quelor?include_prereleases)](https://github.com/coherera/quelor/releases)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/coherera/quelor/badge)](https://securityscorecards.dev/viewer/?uri=github.com/coherera/quelor)
[![Stars](https://img.shields.io/github/stars/coherera/quelor?style=social)](https://github.com/coherera/quelor/stargazers)
[![Conformance: candidate](https://img.shields.io/badge/conformance-candidate-lightgrey.svg)](docs/conformance.md)

Read the [manifesto](https://quelor.dev/manifesto/).

## The problem

CI pipelines and agent sessions start work against budgets they never check: API rate windows, runner slots, model credit windows, dollar caps, and dependencies that are already down. The result is a wall of red that is not defects, paged humans who cannot fix a 429, and reruns that spend the same empty bucket twice.

Quelor admits work against machine resources: API budgets, executor slots, model credit windows, dollar budgets, dependency health. Resource classes are extensible by profile.

## Quickstart (GitHub Action)

```yaml
- name: Admit before start
  id: gate
  uses: coherera/quelor@v0
  with:
    resource: github_api
    need: '300'        # calls the next step expects to make
    optional: 'true'   # defer (skip) instead of going red when short

- name: API-heavy step
  if: steps.gate.outputs.verdict == 'admit'
  run: ./scripts/chatty-step.sh
```

Add it as the first step of a chatty workflow. If the shared token budget is spent or GitHub cannot be read, the step defers with a reason instead of failing, and your checks stay honest. Outputs: `verdict` (`admit`, `defer`, `degraded`) and `remaining`. A full example is in [examples/workflows/chatty-ai-step.yml](examples/workflows/chatty-ai-step.yml).

The same check runs anywhere, with no daemon:

```
$ GITHUB_TOKEN=$(gh auth token) quelor admit --resource github_api --need 50
admit github_api need=50 remaining=4950 reset=2026-10-31T15:00:00Z
```

Exit codes: `0` admit, `42` defer (optional mode), `43` degraded (not enough budget in required mode, or the reading is unknown: unknown is never treated as available).

## What it is

- A **lease API**: `AcquireLease`, `RenewLease`, `SettleLease`. Admit, renew, settle, classify. See [docs/architecture.md](docs/architecture.md).
- `quelord`: a single deterministic daemon. Floors and caps, coalescing, AIMD on observed 429/503, progressive renewal for long sessions, scarcest-resource tickets with aging, per-holder reservation quotas, authenticated holders.
- A **five-class outcome classifier**: `resource-exhausted`, `dependency-unavailable`, `flaky`, `real-defect`, `unknown`. Only `real-defect` goes red. `quelor classify` ships a default ruleset for GitHub Actions failures.
- A **pluggable Decider** interface ([docs/decider-interface.md](docs/decider-interface.md)) with a rules-first reference Decider (YAML + CEL, [docs/policy-cel.md](docs/policy-cel.md)), content-addressed rulesets, and log-only shadow slots.
- **Replay** (`quelor replay --trace t.jsonl --decider ref.yaml`) and a **conformance suite** ([docs/conformance.md](docs/conformance.md)), so you can prove your own Decider or engine.
- A **decision trace** as JSONL with a published schema ([schema/trace.v1.json](schema/trace.v1.json)) and optional OpenTelemetry spans.
- Benchmarks with published methodology ([docs/benchmarks.md](docs/benchmarks.md)): `go run ./cmd/quelor-bench`.

## What it is not

- Not a durable execution engine. Temporal, Hatchet, Inngest and friends run already-admitted work.
- Not a CI runner or a merge queue.
- Not a per-token LLM gateway. Gateways enforce per call; Quelor admits per unit and renews per quantum.
- Not a queue you must hand your payloads and credentials to. The default mode keeps both with you.
- Not a model on your outage path. The hot path is deterministic. Always.

## Run the daemon

```
go install github.com/coherera/quelor/cmd/quelord@latest
quelord -config quelord.yaml
```

Endpoints: `POST /v1/lease/acquire`, `/v1/lease/renew`, `/v1/lease/settle`, `/v1/observe`, `GET /healthz`, `GET /metrics`. Resource classes are declared as YAML manifests ([docs/resource-classes.md](docs/resource-classes.md)).

## Status

`0.x`. Envelope and verdict schemas freeze at `1.0` after a production census. Four independent version lines apply: lease API, envelope schema, trace schema, Decider interface. Pricing for hosted cells is unset; the open engine is complete on its own.

## Where this leads

Hosted multi-tenant cells on Octopus, and the COHERERA platform. Nothing in this repository depends on either.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Contributions are accepted under the Developer Certificate of Origin (`git commit -s`). Security reports: [SECURITY.md](SECURITY.md). Governance: [GOVERNANCE.md](GOVERNANCE.md). Trademarks: [TRADEMARKS.md](TRADEMARKS.md). Starter work: [docs/good-first-issues.md](docs/good-first-issues.md).

---

Quelor is built and maintained by [ARAMAI](https://aramai.net). Apache-2.0.
