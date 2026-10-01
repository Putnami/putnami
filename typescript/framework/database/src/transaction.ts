import { tryContext, useContext, useLogger } from '@putnami/runtime';
import { throwIfAborted } from './abort';
import { classifyRollbackCause } from './errors';
import { database } from './factory';
import { recordTransaction } from './observability';
import { resolveDatasourceName } from './primary-datasource';
import type { ReservedSqlClient, SqlClient } from './sql-client';

const TX_KEY = Symbol.for('__putnami_tx__');

type ReservedConnection = {
  conn: ReservedSqlClient;
  hasWrite: boolean;
};

type TransactionState = {
  active: boolean;
  connections: Map<string, ReservedConnection>;
  opening: Map<string, Promise<ReservedConnection>>;
  /**
   * The tx-telemetry duration origin, from `performance.now()` — not
   * `Date.now()`: the boundary record carries `durationUs` as well as
   * `durationMs`, and millisecond granularity would report every sub-millisecond
   * transaction as 0µs.
   */
  startedAt: number;
  timeoutId?: Timer;
  timedOut?: boolean;
  /**
   * Set the instant a finalizer (commit/rollback/timeout) takes ownership of
   * this transaction. Guards against a timeout racing an in-flight commit and
   * both trying to finalize the same connections.
   */
  finalizing?: boolean;
  /**
   * The in-flight rollback started by a timeout, if any. A later commit awaits
   * it so the caller doesn't return before the rollback (and connection
   * release) has actually completed.
   */
  timeoutRollback?: Promise<void>;
};

function getTxState(): TransactionState | undefined {
  const context = tryContext<Record<symbol, unknown>>();
  return context?.[TX_KEY] as TransactionState | undefined;
}

/** Map a connection-map key to the datasource label used in tx telemetry. */
function txDatasourceLabel(key: string): string {
  return key === '__default__' ? 'default' : key;
}

/**
 * The connection source the transaction machinery reserves from. Defaults to the
 * real {@link database} pool factory and is never mutated in production, where it
 * stays bound to {@link database}. It exists only so deterministic tests can
 * exercise the commit/rollback boundary — including the multi-datasource,
 * non-atomic path — without a live Postgres (see {@link __setTransactionConnectionFactory}).
 */
let connectionFactory: (dbName: string | undefined) => Promise<SqlClient> = database;

/**
 * @internal Test seam: substitute the connection source used by
 * {@link useTxConnection}. Pass `undefined` to restore the real {@link database}
 * factory. Not part of the public API — it is intentionally excluded from the
 * package barrel so it never leaks into application code.
 */
export function __setTransactionConnectionFactory(
  factory: ((dbName: string | undefined) => Promise<SqlClient>) | undefined,
): void {
  connectionFactory = factory ?? database;
}

/**
 * Whether the current context has an active (transactional-mode) transaction —
 * i.e. `withTransaction()`/`runInTransaction()` opened one and it has not yet
 * been committed or rolled back. The request-scoped `UnitOfWork` reads this to
 * stay the single committer: it delegates to {@link commit}/{@link rollback}
 * only while a transaction is still active, so a finalize after the ambient
 * state was already resolved (a timeout rollback, or an explicit commit inside
 * the handler) is a safe no-op rather than a double-finalize.
 */
export function isTransactionActive(): boolean {
  return getTxState()?.active === true;
}

export interface TransactionOptions {
  /** Timeout in milliseconds. When exceeded, the transaction is automatically rolled back. 0 = no timeout (default). */
  timeoutMs?: number;
}

/**
 * Flag the current context for transactional mode.
 * No connection or transaction is opened — that happens lazily on the first write.
 */
export function withTransaction(options?: TransactionOptions): void {
  const context = useContext<Record<symbol, unknown>>();
  const existing = context[TX_KEY] as TransactionState | undefined;
  if (existing?.active) {
    return;
  }
  const state: TransactionState = {
    active: true,
    connections: new Map(),
    opening: new Map(),
    startedAt: performance.now(),
  };

  context[TX_KEY] = state;

  // Outside runInContext(), useContext() returns a fresh object each call.
  if (getTxState() !== state) {
    delete context[TX_KEY];
    throw new Error('withTransaction() requires an active context. Wrap execution in runInContext().');
  }

  const timeoutMs = options?.timeoutMs ?? 0;
  if (timeoutMs > 0) {
    state.timeoutId = setTimeout(() => {
      // Bail if a commit/rollback is already finalizing — flipping `timedOut`
      // here would race the in-flight finalizer and double-release connections.
      if (!state.active || state.finalizing) return;
      state.timedOut = true;
      state.finalizing = true;
      const logger = useLogger('sql');
      logger.warn(`Transaction timed out after ${timeoutMs}ms — rolling back`);
      // The callback runs detached, so a rejected rollback would otherwise
      // surface as an unhandled rejection. Log and swallow it instead, and
      // record the promise so a racing commit can await it before returning.
      state.timeoutRollback = rollback().catch((error) => {
        logger.error('Transaction timeout rollback failed', { error });
      });
    }, timeoutMs);
  }
}

/**
 * Resolve a database connection, transaction-aware.
 * - Outside transactional mode: returns the pool connection (same as `database()`).
 * - In transactional mode + read + no tx opened yet: returns pool connection (zero cost reads).
 * - In transactional mode + write: lazily reserves a connection and BEGINs a transaction.
 * - In transactional mode + read/write + tx already opened for this db: returns the tx connection.
 */
export async function useTxConnection(dbName: string | undefined, mode: 'read' | 'write'): Promise<SqlClient> {
  throwIfAborted();

  const tx = getTxState();
  const resolvedDbName = resolveDatasourceName(dbName);
  const key = resolvedDbName ?? '__default__';

  // Check timeout before active — rollback sets active=false, but we still want to reject operations
  if (tx?.timedOut) {
    throw new Error('Transaction has timed out and was rolled back');
  }

  // No transactional mode → normal pool connection
  if (!tx?.active) {
    return connectionFactory(resolvedDbName);
  }

  // Already have a tx connection for this db → reuse it
  const existing = tx.connections.get(key);
  if (existing) {
    if (mode === 'write') {
      existing.hasWrite = true;
    }
    return existing.conn;
  }

  // Read in transactional mode, but no tx opened yet → use pool (zero cost)
  if (mode === 'read') {
    return connectionFactory(resolvedDbName);
  }

  // Write in transactional mode → reserve connection and BEGIN once per db.
  // Concurrent writers await the same opening promise.
  let opening = tx.opening.get(key);
  if (!opening) {
    opening = (async () => {
      const pool = await connectionFactory(resolvedDbName);
      const reserved = await pool.reserve();
      try {
        await reserved`BEGIN`;
        const opened = { conn: reserved, hasWrite: true };
        tx.connections.set(key, opened);
        return opened;
      } catch (error) {
        reserved.release();
        throw error;
      } finally {
        tx.opening.delete(key);
      }
    })();

    tx.opening.set(key, opening);
  }

  const opened = await opening;
  opened.hasWrite = true;
  return opened.conn;
}

/**
 * Commit all lazily-opened transactions.
 * @throws {Error} if no active transaction exists.
 */
export async function commit(): Promise<void> {
  const tx = getTxState();

  // A timeout that already started rolling back owns finalization (and has set
  // active=false); committing now would double-finalize the same connections.
  // Check this *before* the active guard so the caller gets the precise reason.
  // Wait for the in-flight rollback to settle (releasing connections) first.
  if (tx?.timedOut || tx?.finalizing) {
    if (tx?.timeoutRollback) {
      await tx.timeoutRollback;
    }
    throw new Error('Transaction has timed out and was rolled back');
  }

  if (!tx?.active) {
    throw new Error(
      'commit() called without an active transaction. Call withTransaction() or use runInTransaction() first.',
    );
  }

  tx.finalizing = true;

  if (tx.timeoutId) {
    clearTimeout(tx.timeoutId);
    tx.timeoutId = undefined;
  }

  if (tx.opening.size > 0) {
    const openingResults = await Promise.allSettled([...tx.opening.values()]);
    const openingErrors = openingResults
      .filter((result): result is PromiseRejectedResult => result.status === 'rejected')
      .map((result) =>
        result.reason instanceof Error ? result.reason : new Error(`Transaction opening failed: ${result.reason}`),
      );

    if (openingErrors.length > 0) {
      // Classify BEFORE rolling back so the boundary telemetry records the real
      // opening cause rather than the generic explicit-rollback sentinel.
      await rollback(openingErrors[0]);
      if (openingErrors.length === 1) {
        throw openingErrors[0];
      }
      throw new AggregateError(openingErrors, 'Transaction opening failed on multiple databases');
    }
  }

  const entries = [...tx.connections.entries()];
  const duration = performance.now() - tx.startedAt;
  tx.connections.clear();
  tx.active = false;

  const errors: Error[] = [];
  for (const [name, { conn }] of entries) {
    try {
      await conn`COMMIT`;
      // Per-datasource boundary telemetry: this datasource committed.
      recordTransaction({ datasource: txDatasourceLabel(name), outcome: 'committed', duration });
    } catch (e) {
      const err = e instanceof Error ? e : new Error(`Commit failed for db '${name}': ${e}`);
      errors.push(err);
      // A best-effort partial commit (NOT 2PC): this datasource's COMMIT failed.
      // Record it rolled-back with the classified, secret-free cause.
      recordTransaction({
        datasource: txDatasourceLabel(name),
        outcome: 'rolled-back',
        duration,
        cause: classifyRollbackCause(err),
      });
    } finally {
      conn.release();
    }
  }

  if (errors.length === 1) {
    throw errors[0];
  }
  if (errors.length > 1) {
    throw new AggregateError(errors, 'Transaction commit failed on multiple databases');
  }
}

/**
 * Rollback all lazily-opened transactions.
 *
 * @param cause The thrown value that triggered the rollback, if any. It is
 *   classified to a secret-free code for the `sql.tx.rollback.<cause>` metric
 *   and log attribute (see {@link classifyRollbackCause}); the raw value is
 *   NEVER emitted. A timeout rollback records `timeout`; an explicit,
 *   cause-less rollback records `explicit-rollback`.
 * @throws {Error} if no active transaction exists.
 */
export async function rollback(cause?: unknown): Promise<void> {
  const tx = getTxState();
  if (!tx?.active) {
    throw new Error(
      'rollback() called without an active transaction. Call withTransaction() or use runInTransaction() first.',
    );
  }

  // Claim finalization so a pending timeout bails instead of rolling back twice.
  tx.finalizing = true;

  if (tx.timeoutId) {
    clearTimeout(tx.timeoutId);
    tx.timeoutId = undefined;
  }

  if (tx.opening.size > 0) {
    await Promise.allSettled([...tx.opening.values()]);
  }

  // Resolve the secret-free cause once for every enrolled datasource. A known
  // thrown cause classifies to its code/SQLSTATE/class; otherwise a timed-out
  // transaction records `timeout` and a plain explicit rollback records
  // `explicit-rollback`.
  const causeCode = classifyRollbackCause(cause) || (tx.timedOut ? 'timeout' : 'explicit-rollback');

  const entries = [...tx.connections.entries()];
  const duration = performance.now() - tx.startedAt;
  tx.connections.clear();
  tx.active = false;

  for (const [name, { conn }] of entries) {
    try {
      await conn`ROLLBACK`;
    } catch {
      // Swallow rollback errors — connection may already be broken
    } finally {
      conn.release();
    }
    recordTransaction({ datasource: txDatasourceLabel(name), outcome: 'rolled-back', duration, cause: causeCode });
  }
}

/**
 * Run a function within a transactional context.
 * This is the recommended API for most use cases:
 * - Automatically calls `withTransaction()` before the function.
 * - On success: commits all lazily-opened transactions.
 * - On error: rolls back all lazily-opened transactions and rethrows.
 *
 * ```ts
 * const order = await runInTransaction(async () => {
 *   const o = await orderRepo.findOne({ id: orderId });
 *   o.status = 'shipped';
 *   await orderRepo.save(o);
 *   await auditRepo.save({ action: 'ship', orderId });
 *   return o;
 * });
 * ```
 */
export async function runInTransaction<T>(fn: () => T | Promise<T>, options?: TransactionOptions): Promise<T> {
  const existing = getTxState();
  const isOuter = !existing?.active;

  if (isOuter) {
    withTransaction(options);
  }

  try {
    const result = await fn();
    if (isOuter) {
      await commit();
    }
    return result;
  } catch (e) {
    if (isOuter) {
      const tx = getTxState();
      if (tx?.timeoutRollback) {
        await tx.timeoutRollback;
      } else if (tx?.active) {
        // Thread the caught error so the boundary telemetry records its
        // classified, secret-free cause rather than the generic sentinel.
        await rollback(e);
      }
    }
    throw e;
  }
}

/**
 * Safety net: rollback any uncommitted transactions.
 * Intended to be called at the end of a request lifecycle.
 * Returns true if a rollback was performed.
 */
export async function cleanupTransaction(): Promise<boolean> {
  const tx = getTxState();
  if (!tx?.active || tx.connections.size === 0) {
    if (tx) {
      tx.active = false;
    }
    return false;
  }

  const logger = useLogger('sql');
  logger.warn('Uncommitted transaction detected — rolling back');
  await rollback();
  return true;
}
