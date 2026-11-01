# Policy: YAML rules with CEL

The reference Decider (`pkg/decider/reference`) is rules-first and deterministic. A
ruleset is a YAML document; each rule's `when` is a [CEL](https://cel.dev) expression
compiled once at parse time. The first rule whose `when` is true decides; if none
match, the verdict is `ADMIT`.

## Ruleset

```yaml
id: ci-policy-v1
mode: enforce            # enforce (default), advise, or log-only
rules:
  - id: protect-release
    when: priority_rank >= 3 && readings["github_api"].available < 500
    verdict: DEFER
    wake_after: 5m
    reason: keep headroom for release work
  - id: no-maintenance-in-flight
    when: priority == "maintenance" && demands["runner_slots"] > 2
    verdict: SHED
    ops: [acquire]
    reason: maintenance may use at most two slots
```

| Field | Meaning |
| --- | --- |
| `id` | Ruleset id. Recorded in the trace. |
| `mode` | `enforce`, `advise`, or `log-only`. Empty means `enforce`. Mode is not part of the content hash. |
| `rules[].id` | Required, unique within the ruleset. |
| `rules[].when` | CEL expression that must type-check to `bool`. |
| `rules[].verdict` | `ADMIT`, `DEFER`, `SHED`, or `DENY`. |
| `rules[].reason` | Copied onto the verdict. |
| `rules[].wake_after` | Duration; valid only with `DEFER`. Sets `wake_at` from the engine clock. |
| `rules[].ops` | `acquire`, `renew`, or both. Empty matches both. |

Unknown YAML fields are rejected. Errors are reported with the rule id.

## Variables

Declared in `celEnvironment` (`pkg/decider/reference/ruleset.go`):

| Variable | Type |
| --- | --- |
| `op` | string: `acquire` or `renew` |
| `unit_id`, `tenant_id`, `holder_id`, `affinity_key`, `coalesce_key` | string |
| `priority` | string |
| `priority_rank` | int: 0 operator-directive, 1 release-critical, 2 change, 3 scheduled, 4 maintenance |
| `generation` | int |
| `demands`, `spend_delta` | map of string to int (never nil) |
| `readings` | map of resource id to a map with `available`, `capacity`, `known`, `stale`, `usable`, `age_ms` |

Index readings as `readings["id"].available`. Use `readings["id"].usable` rather than
`known` alone when you care about freshness. Referencing a resource that has no reading
raises an evaluation error for that rule.

## Evaluation

- Rules are tried in order. An evaluation error (a missing key, a cost over the
  100000 limit) does not stop evaluation: the rule is skipped and `<rule-id>:eval-error`
  is added to the response's `abstains`.
- A rule that does not allow the current `op` is skipped.
- The response carries `ruleset_id`, `ruleset_hash` and the matching `rule_ids`, and the
  engine writes them into the trace.

## Content hash

`Ruleset.Hash()` is the SHA-256 of canonical JSON of `{id, rules}`, with `mode` excluded.
YAML formatting and comments do not change it. If a request carries
`expect_ruleset_hash` that differs, the Decider abstains with `ruleset-moved` rather
than answer under a policy the caller did not expect. This makes a trace
reproducible: the hash names exactly which policy decided.

## Running it

- Daemon: `decider: {mode: reference, ruleset: policy.yaml}` in `quelord.yaml`.
- Offline: `quelor replay --trace t.jsonl --decider policy.yaml` scores the ruleset
  against a recorded trace. Replay rebuilds each acquire from the trace's envelope and
  readings, and an `ADMIT` is turned into `DEFER` (`engine-floor`) when a reading for a
  demanded resource is unusable or below the demand, so a candidate cannot look better
  than the engine floor allows (`pkg/replay/replay.go`).
- Shadow: run a second Decider with `shadows:` and compare, without enforcing. See
  [Decider interface](decider-interface.md).

## Limits

The reference Decider evaluates a request in isolation: no state across calls, no
network, no model. State such as per-holder usage is kept by the engine
(quotas, tickets), not by rules.
