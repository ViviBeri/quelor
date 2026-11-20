# Gate model work on credit allowances

This example uses Quelor's engine-local resource accounting to reserve model
credit allowances before an AI step runs. It does not call a model provider,
read provider billing data, or charge a payment method. The application remains
responsible for estimating and reporting consumption in integer cents.

## Configure and start quelord

From the repository root, start the daemon:

```sh
go run ./cmd/quelord -config examples/quelord-model-credits.yaml
```

The config loads two resources from
`examples/manifests/model-credits.yaml`:

- `model_credits_monthly`: a 20,000-cent `window_bucket` that resets every
  30 days.
- `model_spend_lifetime`: a 50,000-cent `hard_budget` that does not refill.
  Its `unknown_policy: deny` prevents admission when its reading is unknown.

Both resources are engine-local when loaded by `quelord`. The daemon does not
attach external resource readers. The values are example limits, not live
provider balances.

## Acquire, renew, and settle

The examples below reserve 1,000 cents from each resource for one unit of work.
The daemon's default local configuration has no authentication token configured,
so these local requests omit the Authorization header. If you configure a
shared or holder token, send it as a bearer token on every request.

### 1. Acquire a lease

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

### 2. Renew the lease

Renew the same lease and reserve the next 500-cent quantum from both budgets:

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

### 3. Settle actual consumption

After the work completes, report the actual amount consumed for each resource.
This example reports 800 cents consumed from the original 1,000-cent
reservation; unused capacity is refunded according to each resource model.

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

Settlement is idempotent. A repeated settlement returns the stored result with
`duplicate: true` and does not release capacity a second time.

## Accounting notes

- All amounts are integer cents; convert provider usage and pricing into cents
  in the calling application.
- `window_bucket` refunds unused reservation only if settlement occurs in the
  same 30-day window in which the lease was acquired.
- `hard_budget` refunds unused reservation without refilling the declared
  lifetime capacity.
- The sample's 30-day window starts when the daemon initializes the resource;
  this is not a calendar-month billing boundary.
- Do not put provider API keys, prompts, or customer data in this example.
