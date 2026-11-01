# Security Policy

## Reporting a vulnerability

Email **security@aramai.net**. Please do not open a public issue for a
vulnerability. Include the affected version, a description, and steps to
reproduce. We acknowledge reports within 2 business days. For high-severity issues our
target is a fix or mitigation within 30 days; lower severities are handled
within 90 days of triage.

## Supported versions

The latest minor release receives security fixes while Quelor is pre-1.0.

## Scope notes

* `quelord` authenticates holders with a shared or per-holder token. Run it
  behind TLS; treat tokens as secrets and never commit them.
* `GET /healthz` and `GET /metrics` are unauthenticated. `/healthz` reports
  the version string; `/metrics` reports counters and gauges. Bind `quelord`
  to a trusted interface or restrict both paths at your proxy.
* The decision trace may contain envelope contents. Protect trace files (they
  are created with mode 0600).
* Quelor never holds your downstream credentials. The GitHub Action reads
  `GITHUB_TOKEN` only to call GitHub's `/rate_limit` endpoint.
