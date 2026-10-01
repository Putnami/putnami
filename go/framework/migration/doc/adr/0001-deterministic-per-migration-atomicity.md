# ADR 0001 — Order migrations deterministically and commit one record per migration

- **Status**: accepted
- **Scope**: `go.putnami.dev/migration` and its registered runners

## Context

Feature packages contribute migration sources independently, across backend
kinds, so map or registration order must not decide execution. A state row
written apart from its body can disagree with the schema. Rollback is unsafe if
the current source replaces the down body approved when the migration ran.

## Decision

The registry has one runner per kind and iterates kinds lexicographically.
Each runner defines deterministic target ordering. The SQL runner sorts
datasources and names, takes one PostgreSQL advisory lock per datasource, and
runs each body with its success row in one transaction. A batch is not one
transaction: earlier successes stay committed when a later migration fails,
and returned records show that prefix.

Applied hashes make re-runs idempotent and drive drift checks. Rollback runs
stored rows newest first, executing only the down body stored with the hash;
missing or changed down material fails closed. A source without a runner is
allowed only on the automatic metadata lifecycle path; explicit operator
commands fail before any runner runs.

## Rejected alternatives

- **Registration or map order.** Order would vary between processes.
- **One transaction for the whole batch.** Backends share no transaction
  manager, and PostgreSQL datasources are not 2PC.
- **Write the success row after the body commits.** A crash separates schema
  from state.
- **Regenerate rollback SQL from source.** It could run an unreviewed
  destructive inverse.

## Consequences

- After a failure, callers inspect the committed prefix and retry.
- Migration names are ordering keys and stay stable once published.
- A new kind needs a runner with its own ordering and atomicity rule.
