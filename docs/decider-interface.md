# Decider interface

A Decider answers one question: given an envelope and the current readings, what verdict?
The engine owns capacity accounting and fail-static behavior. A Decider owns policy.
Source: `pkg/decider/types.go`.

```go
type Decider interface {
    Decide(ctx context.Context, req Request) (Response, error)
}
```

Implementations must be safe for concurrent use. `decider.Func` adapts a function.

## Request

Same JSON in process and over HTTP.

| Field | Meaning |
| --- | --- |
| `decider_api_version` | `"1"` (`decider.APIVersion`). |
| `op` | `acquire` or `renew`. |
| `envelope` | The `quelor.Envelope` being decided. |
| `lease` | The active lease, on renew. |
| `spend_delta` | The quantum a renew wants to reserve. |
| `readings` | One `quelor.Reading` per demanded resource: `available`, `capacity`, `known`, `stale`, `age_ms`. |
| `policy_profile` | Optional profile name. |
| `expect_ruleset_hash` | If set and different from the Decider's hash, the Decider abstains with `ruleset-moved`. |
| `call_meta` | `trace_id` and the engine clock as RFC 3339. |

## Response

| Field | Meaning |
| --- | --- |
| `status` | `answered`, `abstained`, `unavailable`, or `killed`. |
| `mode` | `log-only` (recorded, never applied), `advise` (recorded as advisory, never applied), `enforce` (applied). Empty means enforce. |
| `verdict` | `kind` (`ADMIT`, `DEFER`, `SHED`, `DENY`), `reason`, optional `wake_at`. |
| `decision_ref` | Id of this decision; written to the trace. |
| `ruleset_id`, `ruleset_hash`, `rule_ids` | Which policy and which rules fired. |
| `abstains` | Reasons the Decider declined to answer. |
| `provenance` | `step`, `tier`, `evidence_class`. Opaque to the engine, which records it and never interprets it. |
| `decider_id`, `latency_ms`, `trace_id` | Identity and timing. |

`Response.Enforced()` is true only for `status == answered` with an empty or `enforce`
mode. Only an enforced, non-`ADMIT` verdict changes what the engine does.

## What the engine does with it

- An enforced `DEFER`, `SHED` or `DENY` is returned to the caller.
- An enforced `ADMIT` is not final: the engine still checks readings, unknown policy,
  coalescing, quota, tickets and capacity (see [Architecture](architecture.md)). A
  Decider cannot admit work past a capacity or spend floor.
- A non-enforced response (`abstained`, or mode `log-only` or `advise`) is recorded and
  the engine proceeds on its own checks.
- If the Decider returns an error, times out (`decider_timeout`, 100 ms unless
  configured), or returns status `unavailable` or `killed`, the engine fails static: spend demands are refused and
  unknown-policy demands follow their manifest policy. The verdict carries the stamp
  `bound-unreachable`. A Decider is never a harder dependency than the work it protects.

## Implementations in this repository

| Implementation | Where | Notes |
| --- | --- | --- |
| Reference Decider | `pkg/decider/reference` | Rules-first YAML plus CEL. See [Policy and CEL](policy-cel.md). With no rules it admits everything. |
| `decider.HTTPClient` | `pkg/decider/http.go` | POSTs the request JSON to a URL, header `X-Quelor-Decider-API: 1`. Default timeout 250 ms. Non-2xx, transport error, timeout, undecodable or oversized (over 1 MiB) body, or a mismatched `decider_api_version` yields `StatusUnavailable` and an error wrapping `ErrUnavailable`. |
| `decider.Static(verdict, id)` | `pkg/decider/static.go` | Always answers one verdict. |
| `decider.Slots` | `pkg/decider/shadow.go` | Returns the primary's response unchanged and fans the request out to shadow Deciders. Shadows are strictly log-only: their responses go to an `OnShadow` callback, never into the result. Default shadow timeout 50 ms; a shadow error or panic cannot change the primary result. |

## Kill switch

`QUELOR_DECIDER=static` forces the reference Decider regardless of configuration
(`decider.Select`, `decider.ForceStatic`).

## Configuring `quelord`

```yaml
decider:
  mode: http            # "reference" (default) or "http"
  url: http://127.0.0.1:9000/decide
  timeout: 200ms
shadows:
  - url: http://127.0.0.1:9001/decide
    timeout: 50ms
```

`mode: reference` takes an optional `ruleset:` path. `http` mode requires `url`.
Each shadow requires `url` (`pkg/server/config.go`).

## Writing your own

Implement the interface, then run the Decider conformance suite against it:
[Conformance](conformance.md). Use `quelor replay` to score it against a recorded
trace before it takes traffic.
