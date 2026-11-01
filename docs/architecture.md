# Architecture

Quelor admits a unit of work against machine resources before the work starts,
then settles what it actually used. This page maps the packages to the lease
lifecycle. Every statement names the file it comes from.

## Lease lifecycle

The contract is `quelor.LeaseAPI` (`pkg/quelor/types.go`): three calls.

| Call | What it does |
| --- | --- |
| `AcquireLease` | Takes an `Envelope` (unit id, tenant, holder, `Demands` of resource id to quantity, priority, optional coalesce key and generation, optional deadline). Returns a `Verdict` (`ADMIT`, `DEFER`, `SHED`, `DENY`) and, on `ADMIT`, a `Lease`. A refusal is a typed verdict, not an error (`pkg/engine/acquire.go`). |
| `RenewLease` | Extends an active lease and may reserve a further quantum (`Delta`). A refusal sets `Hibernate` so the holder checkpoints; the lease and its expiry are left untouched (`pkg/engine/settle.go`). |
| `SettleLease` | Releases the lease. Reports `Consumed` against `Declared` (the difference is `Delta`), optionally an `ExecutionOutcome` to classify, and optionally `Observations` of upstream status codes. A second settle of the same lease returns the stored result with `duplicate: true` (`pkg/engine/settle.go`). |

`SettleResult` carries `class` (one of five outcome classes) and `rule_id` (why).
`rule_id` is a classifier rule id, `success`, or `superseded` for a run that a newer
generation replaced. See [Resource classes](resource-classes.md) and the classifier
section below.

## Packages

| Package | Role |
| --- | --- |
| `pkg/quelor` | Open contract types: envelope, lease, verdict, outcome classes, request and result shapes. |
| `pkg/engine` | The deterministic engine. Implements `LeaseAPI`. No network or model calls on this path (package doc, `pkg/engine/engine.go`). |
| `pkg/resource` | Resource manifests (YAML), the registry, and freshness-aware readings. |
| `pkg/decider` | The pluggable Decider interface, an HTTP client, shadow fan-out, static and kill-switch helpers. See [Decider interface](decider-interface.md). |
| `pkg/decider/reference` | The rules-first reference Decider: YAML rules with CEL conditions. See [Policy and CEL](policy-cel.md). |
| `pkg/classify` | Ordered first-match failure classifier with an embedded GitHub Actions ruleset. |
| `pkg/trace` | JSONL decision trace writer and reader; schema in `schema/trace.v1.json`. |
| `pkg/replay` | Offline scoring of a candidate Decider against a recorded trace. |
| `pkg/server` | HTTP handler and `quelord` YAML config. |
| `pkg/admit` | Daemon-free check of a live GitHub `/rate_limit` reading (`quelor admit`). |
| `conformance`, `bench` | Public conformance suite and scenario benchmarks. See [Conformance](conformance.md) and [Benchmarks](benchmarks.md). |

Binaries: `cmd/quelor` (admit, classify, replay, version), `cmd/quelord` (daemon),
`cmd/quelor-bench` (benchmark report).

## What happens on acquire

In order, from `AcquireLease` (`pkg/engine/acquire.go`):

1. Authenticate the holder. A configured shared or per-holder token is compared in
   constant time; failure is `DENY unauthenticated-holder` (HTTP 401).
2. Validate the envelope: non-empty `unit_id`, positive demands, every demanded resource
   registered (`DENY invalid-envelope:unit_id`, `invalid-demand:<id>`, `unknown-resource:<id>`).
   A passed `deadline` is `SHED deadline-passed`.
3. If the unit already holds a lease, return it (replay, never a double reservation).
4. Read every demanded resource. Readings carry `known`, `stale`, `age_ms`; only
   known and fresh readings count as capacity.
5. Consult the Decider with the engine lock released.
6. Apply the Decider verdict if it is enforced and not `ADMIT`. If the Decider is
   unreachable, fail static: a hard budget is never admitted on a guess, and an
   unknown-policy demand yields only to an operator directive. The verdict is stamped
   `bound-unreachable`.
7. Apply each resource's unknown-reading policy (`defer_short`, `assume_busy`,
   `hold_cursor`, `deny`).
8. Coalescing: for a shared `coalesce_key`, `DROP_NEW` sheds the newcomer, an older
   `generation` is shed as stale, otherwise the older lease is marked superseded.
9. Holder quota (`max_leases`, `max_units`): `DENY holder-quota-exceeded`.
10. Tickets: contention on a scarce resource is serialized by priority rank, and rank
    improves by one step per `aging_step` waited, so low-priority work is not starved.
11. Check capacity on every resource and reserve all of them atomically, or defer.

`DEFER` carries a `wake_at` hint and a `wake_token`; the engine emits a wake event when
capacity returns.

## Capacity models and feedback

Five capacity models (`lease`, `gcra`, `hard_budget`, `window_bucket`, `circuit`) are
documented in [Resource classes](resource-classes.md). Observed upstream statuses
(`POST /v1/observe`, or `Observations` on settle) drive AIMD: HTTP 429 or 503 halves the
effective capacity down to `min_capacity`; a 2xx or 3xx raises it by 5 percent of
capacity per observation, up to the declared capacity (`pkg/engine/state.go`). A
`Retry-After` blocks the resource until it elapses. Circuit resources use a failure
counter instead (see the resource-classes page).

## Timers

`Engine.Advance(now)` applies due timers: resource windows, lease expiry, ticket expiry
and wake events. `Engine.Run(ctx)` drives it from the real clock in `quelord`
(`pkg/engine/advance.go`). Tests and `bench` inject a fake clock through
`engine.Config.Clock`.

## Classifier

`classify.Ruleset.Classify` maps an `ExecutionOutcome` (exit code, HTTP status, log,
timed-out flag) to one of five classes: `resource-exhausted`, `dependency-unavailable`,
`flaky`, `real-defect`, `unknown`. Exit 0 with HTTP below 400 and no timeout is success
before any rule runs. Otherwise the first matching rule wins; with none, the ruleset
default applies. `RuleID` is stable per ruleset id. `RuleSuccess`, `RuleSuperseded` and
the empty `RuleDefault` are exported constants. The embedded ruleset is
`pkg/classify/default_gha.yaml`.

Known gap (tracked in code as `TODO(v0.2)`): a job the platform refused before step 1 that
exits 0 reads as success, so `gha-budget-wall` is unreachable for an exit-0 refusal.

## Trace

Every acquire, renew, settle, expiry and observe writes one JSONL record (`pkg/trace`),
carrying the envelope hash, ruleset id and hash, rule ids, stamps, and for settles the
outcome. The file is opened append-only with mode 0600. `quelor replay` reads it back.

## Versioning

Four independent version lines: lease API, envelope schema, trace schema (`trace.v1`),
and Decider interface (`APIVersion = "1"`). The binary version is in `pkg/version`.
