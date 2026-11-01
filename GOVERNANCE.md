# Governance

## Maintainers

Quelor is built and maintained by ARAMAI (https://aramai.net). Maintainers are
listed in `.github/CODEOWNERS`.

## How decisions are made

* Day-to-day changes are decided by maintainer review on pull requests.
* Changes to a published contract (lease API, envelope schema, trace schema,
  Decider interface) follow semver for that line and need an issue-first
  discussion. See `docs/architecture.md` for the four independent version lines.
* An OSS steward agent assists with issue and pull-request triage (labelling,
  reproducing, first-pass review, CI follow-up). **Humans approve releases and
  merge anything that changes a contract invariant.** The agent never tags a
  release.

## Becoming a maintainer

Sustained, high-quality contributions plus an invitation from an existing
maintainer. We want maintainers from outside ARAMAI.

## Conflicts

Raise them privately at security@aramai.net (also the Code of Conduct contact).
