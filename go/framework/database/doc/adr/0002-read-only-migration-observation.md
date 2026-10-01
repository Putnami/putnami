# ADR 0002 — Separate migration observation from mutation authority

- **Status**: accepted
- **Scope**: `go.putnami.dev/database` (`go/framework/database`)

## Context

Production services run with a restricted database identity. A dedicated
migration job creates the schema and applies migrations; the service only
inspects migration status and drift before becoming ready, and its identity
may lack `CREATE`.

## Decision

`Migrator.Status` and `Migrator.Verify` use a read-only connection and issue
only the state-store `SELECT`. They neither initialize the state store nor
take the writer advisory lock. PostgreSQL `undefined_table` or
`invalid_schema_name` on that read means nothing is recorded yet: status
returns an empty store and the SQL runner reports every registered definition
as pending. Other read failures stay errors.

Apply and rollback keep state-store initialization, advisory locking and
transactional behavior.

## Consequences

- A runtime identity needs only `SELECT` to inspect migration state.
- A missing store shows as pending work, never as a successful apply.
- Connectivity, authorization and unexpected failures still block readiness.
