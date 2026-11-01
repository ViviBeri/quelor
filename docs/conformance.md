# Conformance

The `conformance` package is a Go test suite you run against your own engine or Decider.
It is how the project checks that the reference engine keeps the invariants other
implementations must also keep. Status: **candidate**. The suite has a revision
(`conformance.Version`, currently `"1"`) and its case names are published ids, but it has
not been through an external implementation yet.

Both suites take `*testing.T`, so run them from a `_test.go` file.

## Engine suite

Implement `conformance.Factory` and call `RunEngine`:

```go
func TestMyEngine(t *testing.T) {
    conformance.RunEngine(t, func(t *testing.T, spec conformance.Spec) quelor.LeaseAPI {
        return buildMyEngine(t, spec) // a fresh engine per call
    })
}
```

`Spec` tells your factory what to install: `Resources` (id, capacity, and flags for a
spend budget, an unknown or stale reading, and the unknown policy), an optional
`SharedToken`, a `HolderMaxLeases` quota, `DeciderUnavailable`, and a `Now` clock your
engine must use. `conformance/engine_test.go` shows the reference wiring.

The 14 engine cases (`conformance/suite.go`):

| Case | Invariant |
| --- | --- |
| `verdicts/admit`, `defer`, `shed`, `deny` | Each verdict kind is produced under the condition that defines it. |
| `unknown-is-not-available/defer_short`, `/deny` | An unknown or stale reading is never counted as capacity; the policy decides. |
| `fail-static/decider-unreachable` | With the Decider down, the engine still answers. |
| `fail-static/never-fail-open-on-spend` | A spend budget is never admitted on a guess. |
| `idempotent-settle` | A repeated settle returns the stored result with `duplicate` set. |
| `settle-declared-vs-consumed` | Settle reports declared, consumed and their difference. |
| `meta-admission/holder-quota` | A holder past its lease quota is denied. |
| `meta-admission/authenticated-holder` | A wrong or missing token is refused. |
| `atomic-multi-resource` | A multi-resource demand reserves all of it or none. |
| `renew-refusal-hibernate` | A refused renewal tells the holder to checkpoint. |

## Decider suite

Implement `conformance.DeciderFactory` and call `RunDecider`:

```go
conformance.RunDecider(t, func(t *testing.T) decider.Decider { return myDecider },
    conformance.DeciderOptions{RulesetAware: true})
```

Cases (`conformance/decider.go`): `answers-with-valid-status`,
`decision-ref-present-and-idempotent`, `decider-id-set`, `api-version-echoed`,
`does-not-mutate-request`, `provenance-opaque`, `json-roundtrip`, and `not-slower-than`
(250 ms budget). Set `RulesetAware: true` to add `ruleset-moved`: a request whose
`expect_ruleset_hash` does not match must be abstained with `ruleset-moved`.

## Report

Both calls return a `Report` (`suite`, `version`, `passed`, `failed`, and one entry per
case). If `QUELOR_CONFORMANCE_JSON` names a path, the report is written there as JSON when
the test ends:

```
QUELOR_CONFORMANCE_JSON=report.json go test ./... -run TestMyEngine
```

## Does the suite catch a broken engine?

`conformance/engine_test.go` runs the suite against a deliberately fail-open engine
(`TestEngineConformance_PROVES_RED`) and requires named cases to fail. Do the same for
yours: a green run only means something if you have seen the suite go red against a
wrong implementation.

## What it does not cover

The suite is a floor, not a certification. It does not test coalescing, tickets and
aging, AIMD, circuit breakers, renewal quanta, or the HTTP surface. Those are tested in
`pkg/engine` and `pkg/server`.
