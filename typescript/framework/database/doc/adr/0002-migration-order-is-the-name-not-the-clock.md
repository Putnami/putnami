# ADR 0002 — Migration order is the name, not the clock

- **Status**: accepted
- **Scope**: `@putnami/database` (`typescript/framework/database`)

## Context

Forward and backward order must mirror each other, or a rollback drops a table
before its dependant. `executed_at` cannot give the backward order: a batch ties
within one millisecond, and fleet clock skew records a sequence that never
happened. `localeCompare` cannot give the forward order: it is ICU- and
locale-dependent (`billing-api/…` vs `billing/…`, case order) while the Go runner,
which shares the `${datasource}:${name}` state store, compares with `<`.

Applying a migration is two writes, the DDL and the bookkeeping row; a crash
between them leaves an unrecorded change that the next run re-applies. The pool's
`statement_timeout` protects request queries and kills index rebuilds.

## Decision

1. **Order is the name, compared byte-wise** (`a < b ? -1 : a > b ? 1 : 0`),
   matching the Go runner and the bundle protocol. Rollback, rollback-to, and
   reset reverse that order and never read `executed_at`.
2. **Apply is atomic.** Body and success row commit in one transaction on a
   reserved connection. On failure the body rolls back, the failure row is written
   best-effort outside it, one terminal record is logged, and the run aborts with
   a `MigrationError` naming the migration. The row never masks the apply error.
3. **An applied migration is immutable.** Before skipping an applied migration,
   the runner re-hashes its body; a mismatch fails the run with both hashes.
   Rollback SQL is verified against its stored hash, and the state row is claimed
   with a conditional delete before the down body runs, so two runners cannot
   roll back one migration twice.
4. **One writer per datasource, across languages.** Every operation holds a
   PostgreSQL advisory lock on `hashtext('putnami.migration:<datasource>')`, the
   same literal and hash the Go runner uses.
5. **Migration bodies are not request queries.** Up and down bodies run with
   `statement_timeout = 0` and the datasource's declared schema as `search_path`,
   both transaction-local (`is_local`), so neither leaks onto the pool.

## Rejected alternatives

- **Order by `executed_at` or `localeCompare`.** Both produce orders that differ
  from reality or from the Go runner.
- **DDL and bookkeeping in two transactions.** Leaves an applied, unrecorded
  window that the next run re-applies.
- **Skip applied migrations by name.** Editing an applied migration becomes a
  silent no-op and the schema drifts from the repository.
- **A process-level lock.** Only the database is shared by every instance and
  both languages.
- **Keep the pool's `statement_timeout`.** It aborts backfills and index builds
  mid-migration.

## Consequences

- A name is a contract: renaming an applied migration makes a new migration.
- Every forward run re-hashes every applied migration, linear in their count,
  paid at startup.
- The session-scoped lock is released in a `finally`; a release failure is logged
  as its own signal, not folded into the migration error.
- A rollback whose row has no `down_sql` falls back to the registered definition;
  a migration with no down SQL at all fails loudly.
