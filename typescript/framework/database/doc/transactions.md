# Transactions

Use transactions to group multiple database operations into atomic units. If any operation fails, all changes are rolled back.

## Table of contents

- [Quick start](#quick-start)
- [runInTransaction](#runintransaction)
- [How it works](#how-it-works)
- [Multi-database support](#multi-database-support)
- [Transaction timeout](#transaction-timeout)
- [Request abort handling](#request-abort-handling)
- [Safety net middleware](#safety-net-middleware)
- [Request-scoped UnitOfWork](#request-scoped-unitofwork)
- [Consume-once and rotation (typed Outcome)](#consume-once-and-rotation-typed-outcome)
- [Manual primitives](#manual-primitives)
- [Best practices](#best-practices)

## Quick start

```ts
import { runInTransaction } from '@putnami/database';

async function transferFunds(fromId: string, toId: string, amount: number) {
  await runInTransaction(async () => {
    const from = await accountRepo.findOne({ id: fromId });
    const to = await accountRepo.findOne({ id: toId });

    await accountRepo.save({ ...from, balance: from.balance - amount });
    await accountRepo.save({ ...to, balance: to.balance + amount });
    await ledgerRepo.save({ fromId, toId, amount, date: new Date() });
  });
}
```

No transaction parameter is passed to repositories. No connection management. Commit on success, rollback on error.

## runInTransaction

`runInTransaction(fn, options?)` is the recommended API for all transactional code.

```ts
import { runInTransaction } from '@putnami/database';

// Returns the callback result
const order = await runInTransaction(async () => {
  const o = await orderRepo.findOne({ id: orderId });
  o.status = 'shipped';
  await orderRepo.save(o);
  await auditRepo.save({ action: 'ship', orderId });
  return o;
});

// With a timeout — auto-rollback if the callback takes too long
const order = await runInTransaction(async () => {
  // ...
}, { timeoutMs: 5000 });
```

**Behavior:**

- Flags the current async context for transactional mode.
- On success: commits all lazily-opened transactions and returns the result.
- On error: rolls back all lazily-opened transactions and rethrows.
- Read-only calls inside have zero overhead (no transaction is opened).

**Options:**

| Option | Type | Default | Description |
|---|---|---|---|
| `timeoutMs` | `number` | `0` | Auto-rollback after this many milliseconds. `0` = no timeout. |

## How it works

Transactions are **lazy** and **write-aware**. This means:

1. `runInTransaction(fn)` marks the context — it does **not** open a database connection or `BEGIN`.
2. Read operations (`find`, `findOne`, `exists`, `get`) use the normal connection pool. Zero cost.
3. The first **write** operation (`save`, `delete`, `saveMany`, `deleteMany`) reserves a connection from the pool and issues `BEGIN`. All subsequent operations on that database use this connection.
4. On success, `COMMIT` is sent. On error, `ROLLBACK` is sent and the connection is released.

```text
runInTransaction(fn)
  → flags context

repo.find(...)              // read → pool connection (zero cost)
repo.save(...)              // write → reserve connection, BEGIN
repo.find(...)              // read → reuses tx connection (read-your-writes)
repo.save(...)              // write → reuses tx connection

fn returns
  → COMMIT, release connection
```

This design means:

- **Read-only transactions have zero overhead.** No connection is reserved, no `BEGIN`/`COMMIT` is issued.
- **Read-your-writes consistency.** After the first write, all subsequent reads on the same database go through the transaction connection.
- **Repositories are unaware of transactions.** No API changes, no `tx` parameter passing.

## Multi-database support

When repositories target different databases, each gets its own independent transaction. No distributed transaction complexity.

```ts
// OrderRepo uses db: 'orders', AuditRepo uses db: 'audit'
await runInTransaction(async () => {
  await orderRepo.save(order);    // BEGIN on 'orders' db
  await auditRepo.save(log);      // BEGIN on 'audit' db
});
// COMMIT on 'orders', then COMMIT on 'audit'
```

Each database commits independently. There is no two-phase commit — this is a deliberate trade-off for simplicity.

## Transaction timeout

Set a timeout to auto-rollback transactions that take too long. This prevents runaway operations from holding reserved connections indefinitely.

```ts
// Via runInTransaction
await runInTransaction(async () => {
  await orderRepo.save(order);
  await paymentService.charge(order); // slow external call
}, { timeoutMs: 5000 });

// Via withTransaction
withTransaction({ timeoutMs: 5000 });
await orderRepo.save(order);
await commit();
```

When the timeout fires:

1. The transaction is rolled back and all reserved connections are released.
2. Any subsequent query in the same context throws: `"Transaction has timed out and was rolled back"`.
3. The timeout is cleared on `commit()` or `rollback()`, so fast transactions pay no cost.

## Request abort handling

When the HTTP request carries an `AbortSignal` (e.g. client disconnect or server timeout), the SQL layer reacts automatically:

- **In-flight queries are cancelled.** The framework sends a PostgreSQL protocol-level cancel request so the server stops executing the query.
- **New queries are rejected.** Any call to `useTxConnection()` or repository methods after abort throws `QueryAbortError`.
- **Open transactions are rolled back.** The `TransactionMiddleware` triggers `cleanupTransaction()` on abort, releasing reserved connections back to the pool.

This means aggressive HTTP timeouts won't pile up abandoned queries or exhaust your connection pool.

### Using abort utilities directly

For custom queries outside the Repository, you can use the abort helpers:

```ts
import { abortableQuery, throwIfAborted, useTxConnection } from '@putnami/database';

async function customQuery() {
  throwIfAborted(); // fast-fail if already cancelled

  const sql = await useTxConnection(undefined, 'read'); // abort-checked
  const rows = await abortableQuery(sql`SELECT * FROM large_table`); // cancellable
  return rows;
}
```

### QueryAbortError

Thrown when a query is aborted. Carries PostgreSQL error code `57014` ("canceling statement due to user request").

```ts
import { QueryAbortError } from '@putnami/database';

try {
  await repo.find({ status: 'active' });
} catch (error) {
  if (error instanceof QueryAbortError) {
    // Client disconnected — safe to ignore
    return;
  }
  throw error;
}
```

## Safety net middleware

Add `TransactionMiddleware` to catch uncommitted transactions at the end of a request. This protects against bugs where `withTransaction()` is called manually but `commit()`/`rollback()` is forgotten.

```ts
import { http } from '@putnami/application';
import { TransactionMiddleware } from '@putnami/database';

export const app = () =>
  application()
    .use(http().use(TransactionMiddleware()));
```

The middleware provides two layers of protection:

1. **On request end** — rolls back any uncommitted transaction and logs a warning.
2. **On request abort** — listens for the request's `AbortSignal` and triggers early rollback so reserved connections are released without waiting for the handler to finish.

## Request-scoped UnitOfWork

For a per-request "transaction owns the request" boundary — commit on handler success, roll back on error or abort — opt into the DI request-scoped `UnitOfWork`. Enable the provider on the `sql()` plugin and add `UnitOfWorkMiddleware` to the HTTP chain:

```ts
import { http } from '@putnami/application';
import { sql, UnitOfWorkMiddleware } from '@putnami/database';

export const app = () =>
  application()
    .use(sql({ unitOfWork: true }))
    .use(http().use(UnitOfWorkMiddleware()));
```

With both in place, every request that writes runs inside one transaction per participating datasource:

- The middleware opens the unit before the handler (zero-cost until the first write).
- On handler success it **commits**; on a thrown error or on the request's `AbortSignal` it **rolls back**, releasing reserved connections.
- A `new Repository(...)` and an injected/`provideRepository(...)` repository both join the same unit through the ambient transaction — no wiring per repository.

The `UnitOfWork` is the single committer: it opens the ambient transaction itself (so any nested `runInTransaction` joins rather than commits), delegates to the same `commit()`/`rollback()` primitives, and latches after the first finalize so the boundary never double-commits.

### Two-repository service example

Two repositories writing in one request run inside the same unit, so the writes commit or roll back together — no wiring per repository. This handler rotates a tenant's signing key and re-binds it atomically: it revokes the predecessor and installs the successor (`rotateRow`), then binds the successor (`save`), all in the request's unit.

```ts
import { Outcome, Repository } from '@putnami/database';
import { SigningKeys } from './tables/signing-keys';
import { KeyBindings } from './tables/key-bindings';

// POST /signing-keys/rotate — runs inside the request-scoped UnitOfWork.
async function rotateSigningKey(predecessorId: string, successorId: string, tenant: string) {
  const keys = new Repository(SigningKeys);
  const bindings = new Repository(KeyBindings);

  const outcome = await keys.rotateRow({
    keyColumn: 'id',
    predecessorKey: predecessorId,
    stateColumn: 'state',
    expected: 'active',
    revoked: 'revoked',
    successorColumns: ['id', 'tenant', 'state'],
    successorValues: [successorId, tenant, 'active'],
  });
  if (outcome !== Outcome.Applied) {
    // Predecessor not revoked (already rotated / absent) → nothing installed.
    return { outcome };
  }
  // Second repository, SAME unit: bind the successor. If this throws (e.g. the
  // tenant is already bound), the middleware rolls the rotation back with it —
  // the successor key never appears unbound, and the predecessor stays active.
  await bindings.save({ keyId: successorId, tenant });
  return { outcome };
}
```

`rotateRow` performs two writes (revoke + insert) and is atomic **only** inside a transaction; joining the request's unit is what makes the whole revoke → install → bind sequence all-or-nothing. A live proof of this lives in `typescript/samples/06-database` (`test/unit-of-work.test.ts`) and its Go twin `go/samples/unit-of-work-proof`.

### Nesting (join-outer, no savepoints)

A `runInTransaction` inside a handler that is already under an open `UnitOfWork` **joins** the unit rather than opening or committing its own — join-outer with no savepoints, the same semantics as nesting `runInTransaction` (see [Best practices](#best-practices)). An inner rollback rolls back the whole unit; the outer boundary remains the single committer.

### Timeout, cancel, and panic

- **Timeout** — bound the whole request transaction with `sql({ unitOfWork: true, unitOfWorkTimeoutMs: 5000 })` so a wedged handler cannot pin a pooled connection. See [Transaction timeout](#transaction-timeout).
- **Cancel / abort** — on the request's `AbortSignal` (client disconnect or server timeout) the middleware **rolls the unit back** and releases reserved connections without waiting for the handler. See [Request abort handling](#request-abort-handling).
- **Panic / thrown error** — a thrown error rolls the unit back and rethrows; the `finally` safety net (`cleanupTransaction`) then runs as a no-op once the unit has finalized.

### setRollbackOnly()

Inject the `UnitOfWork` to force a rollback from a handler that still returns a success response:

```ts
import { resolve } from '@putnami/runtime/inject';
import { UnitOfWork } from '@putnami/database';

async function handler() {
  // ...business logic detects a soft failure...
  resolve(UnitOfWork).setRollbackOnly(); // boundary rolls back despite success
  return { status: 'skipped' };
}
```

### Multi-datasource

A unit spanning one datasource is atomic. Spanning several it is **best-effort, not two-phase commit**: each datasource commits in turn, and a mid-commit failure is surfaced as an error (a partial commit is never reported as success). Design cross-datasource writes accordingly — see [Multi-database support](#multi-database-support).

### provideRepository

`provideRepository(tableDef)` registers a repository as a DI provider without a subclass; resolve it with `repositoryToken(tableDef)`. The `new Repository(tableDef)` path keeps working unchanged and still joins the ambient transaction / active `UnitOfWork`.

```ts
import { provideRepository, repositoryToken } from '@putnami/database';

app.register(provideRepository(UserTable));
// ...later, in a handler:
const users = resolve(repositoryToken(UserTable));
```

## Consume-once and rotation (typed Outcome)

The optimistic-concurrency helpers — `consumeOnce`, `compareAndSet`, and `rotateRow` — return a typed, **closed** `Outcome` instead of a bare row count, so callers switch exhaustively on a stable result taxonomy. The values are mirrored byte-for-byte from the Go adapter and the `protocols/transaction` contract, so both runtimes report the identical code for the same scenario.

| `Outcome` | Retryable | Meaning |
|---|---|---|
| `applied` | no | The unit of work committed (the transition ran). |
| `already-consumed-conflict` | no | The target was already consumed — an idempotency key, a once-only token, or a unique-violation on insert — so it was rejected as a conflict rather than re-applied. |
| `not-found` | no | A required target row did not exist, so nothing was applied. |
| `retryable-serialization-failure` | **yes** | The transaction aborted with a serialization/deadlock failure the caller may safely retry. |

`outcomeRetryable(o)` is true only for `retryable-serialization-failure`; every other outcome is terminal.

### consumeOnce — claim a row exactly once

```ts
import { Outcome, Repository } from '@putnami/database';

// Redeem a device code exactly once. The still-unclaimed guard ("consumed = false")
// plus Postgres row locking make this safe even under racing redemptions and
// without an enclosing transaction — exactly one caller observes `applied`.
const outcome = await codes.consumeOnce(
  'code', code,               // key column = value
  'consumed = false',         // still-unclaimed guard
  'consumed = true, consumed_by = $1', userId, // the claim transition + its args
);
switch (outcome) {
  case Outcome.Applied: /* first redemption */ break;
  case Outcome.AlreadyConsumedConflict: /* already redeemed */ break;
  case Outcome.NotFound: /* unknown code */ break;
}
```

`compareAndSet(keyColumn, key, column, expected, next)` is the value-equality sibling: it transitions `column` from `expected` to `next`, reporting `already-consumed-conflict` when the row exists but has already moved. A nullish `expected` does **not** match SQL `NULL` — use `consumeOnce` with an explicit `IS NULL` guard for that.

### rotateRow — an atomic predecessor→successor rotation

`rotateRow` compare-and-sets the predecessor's state, and only when that applies inserts the successor. It performs two writes and is atomic **only inside a transaction**, so wrap it in `runInTransaction` / a `UnitOfWork` — outside one, the revoke would commit even if the successor insert fails. See the [two-repository service example](#two-repository-service-example) above.

### Multi-datasource is not atomic

A consume-once claim or a rotation spanning **several** datasources is best-effort, **not two-phase commit** (see [Multi-database support](#multi-database-support) and [Multi-datasource](#multi-datasource)). Keep such a unit within a single datasource when the outcome must be exact — the `already-consumed` vs `not-found` disambiguation is only snapshot-consistent within one transaction.

## Manual primitives

For advanced control flow where the callback pattern doesn't fit, use the low-level primitives directly.

```ts
import { withTransaction, commit, rollback } from '@putnami/database';

async function conditionalSave(items: Item[]) {
  withTransaction();

  for (const item of items) {
    await itemRepo.save(item);

    if (await isDuplicate(item)) {
      await rollback();
      return { status: 'duplicate' };
    }
  }

  await commit();
  return { status: 'ok' };
}
```

### withTransaction(options?)

Flags the current async context for transactional mode. Does not open any connection or transaction. Idempotent — calling it twice is a no-op. Accepts an optional `{ timeoutMs }` for auto-rollback.

### commit()

Commits all lazily-opened transactions and releases their connections. No-op if no writes happened.

### rollback()

Rolls back all lazily-opened transactions and releases their connections. No-op if no writes happened.

### Safety net

`TransactionMiddleware` and `UnitOfWorkMiddleware` end every request with an internal safety net that rolls back any uncommitted transaction and releases its connection. The safety net is not exported: install the middleware instead of calling it.

## Best practices

1. **Use `runInTransaction` by default.** It handles commit, rollback, and cleanup automatically. Use manual primitives only when you need conditional commit/rollback logic.

2. **Keep transactions short.** Each open transaction holds a reserved connection from the pool. Long-running transactions reduce available connections for other requests.

3. **Don't nest `runInTransaction` calls.** Nested calls join the outer transaction — there are no savepoints. If the inner call throws, the entire transaction rolls back.

4. **Use `TransactionMiddleware` as a safety net.** Even if your code always commits/rolls back correctly, the middleware catches edge cases (uncaught exceptions, forgotten commits).

5. **Reads outside transactions are fine.** If your service method only reads data, you don't need a transaction. The framework uses the connection pool directly with zero overhead.

6. **Multi-database transactions are not atomic across databases.** If `COMMIT` fails on one database after succeeding on another, the committed database cannot be rolled back. Design your data model to minimize cross-database writes when atomicity matters.
