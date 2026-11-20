# Gate model work on credit allowances

This example uses Quelor's engine-local resource accounting to reserve model
credit allowances before an AI step runs. It does not call a model provider,
read provider billing data, or charge a payment method. The calling application
must estimate and report consumption in integer cents.

## Start quelord

From the repository root:

```sh
go run ./cmd/quelord -config examples/quelord-model-credits.yaml
```

The config loads two resources from
`examples/manifests/model-credits.yaml`:

- `model_credits_monthly`: a 20,000-cent `window_bucket` that resets every
  720 hours (30 days).
- `model_spend_lifetime`: a 50,000-cent `hard_budget` that never refills.
  Its `unknown_policy: deny` refuses admission when its reading is unknown.

These resources are engine-local. The daemon does not attach external resource
readers, so the example values are allowances maintained by Quelor, not live
provider balances. The 720-hour window starts when the daemon initializes the
resource; it is not a calendar-month billing boundary.

## Acquire, renew, and settle

The examples reserve 1,000 cents from each resource for one unit of work.
The default local configuration has no authentication token configured. If
you configure a shared or holder token, send it as a bearer token on every
lease request.

### Acquire

```sh
ACQUIRE=$(curl --fail-with-body --silent --show-error \
  -X POST http://127.0.0.1:8080/v1/lease/acquire \
  -H 'Content-Type: application/json' \
  -d '{
    "envelope": {
      "unit_id": "model-review-001",
      "tenant_id": "example",
      "holder_id": "local-demo",
      "priority": "change",
      "demands": {
        "model_credits_monthly": 1000,
        "model_spend_lifetime": 1000
      }
    }
  }')
printf '%s\n' "$ACQUIRE" | jq .
LEASE_ID=$(printf '%s' "$ACQUIRE" | jq -r '.lease.id')
```

Proceed only when `.verdict.kind` is `ADMIT` and `.lease.id` is present.
A `DEFER`, `SHED`, or `DENY` verdict is not a grant.

### Renew

Reserve the next 500-cent quantum on the same lease:

```sh
curl --fail-with-body --silent --show-error \
  -X POST http://127.0.0.1:8080/v1/lease/renew \
  -H 'Content-Type: application/json' \
  -d "{
    \"lease_id\": \"$LEASE_ID\",
    \"holder_id\": \"local-demo\",
    \"delta\": {
      \"model_credits_monthly\": 500,
      \"model_spend_lifetime\": 500
    }
  }" | jq .
```

Continue only when the renewal verdict is `ADMIT`. A refused renewal leaves
the existing lease expiry unchanged.

### Settle

After the work completes, report the actual consumption. This example reports
800 cents consumed from the total 1,500-cent reservation (the original 1,000 cents plus the 500-cent renewal):

```sh
curl --fail-with-body --silent --show-error \
  -X POST http://127.0.0.1:8080/v1/lease/settle \
  -H 'Content-Type: application/json' \
  -d "{
    \"lease_id\": \"$LEASE_ID\",
    \"holder_id\": \"local-demo\",
    \"consumed\": {
      \"model_credits_monthly\": 800,
      \"model_spend_lifetime\": 800
    }
  }" | jq .
```

Settlement is idempotent. A repeated settlement returns the stored result
with `duplicate: true` and does not release capacity a second time.

## Accounting notes

- Amounts are integer cents. Convert provider usage and pricing into cents in
  the calling application.
- `window_bucket` refunds unused reservation only if settlement occurs in the
  same window in which the lease was acquired.
- `hard_budget` refunds the unused portion of the reservation. Consumed
  amounts remain charged against the lifetime budget; refunds do not increase
  the resource's declared capacity.
- Do not put provider API keys, prompts, or customer data in this example.
