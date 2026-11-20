import { tryContext } from '@putnami/runtime';
import type { SqlQuery, SqlResult } from './sql-client';

/**
 * Error thrown when a SQL query is aborted due to a cancelled request.
 * Maps to PostgreSQL error code 57014 ("canceling statement due to user request").
 */
export class QueryAbortError extends Error {
  readonly code = '57014';

  constructor(message = 'Query aborted') {
    super(message);
    this.name = 'QueryAbortError';
  }
}

/**
 * Read the AbortSignal from the current async context.
 * Returns `undefined` outside a context or when no signal was set.
 */
export function useAbortSignal(): AbortSignal | undefined {
  const context = tryContext<{ signal?: AbortSignal }>();
  return context?.signal;
}

/**
 * Throw a `QueryAbortError` if the context signal is already aborted.
 * Safe to call at the start of any operation that should not begin after cancellation.
 */
export function throwIfAborted(): void {
  const signal = useAbortSignal();
  if (signal?.aborted) {
    throw new QueryAbortError();
  }
}

/**
 * Wrap a postgres.js PendingQuery so it is cancelled when the context signal fires.
 *
 * - If the signal is already aborted, throws without starting the query: a
 *   postgres.js query runs only once something awaits it, so nothing is sent.
 * - Otherwise, on abort it calls `pending.cancel()`, which drops a query still
 *   waiting for a connection or sends a PostgreSQL protocol-level cancel
 *   request for a running one, and rejects at once with a `QueryAbortError`:
 *   the caller unwinds even when the connection is dead and the driver never
 *   settles the query.
 * - The listener is cleaned up once the query settles.
 *
 * Pass-through when there is no signal in the context.
 */
export async function abortableQuery<T extends readonly any[]>(pending: SqlQuery<T>): Promise<SqlResult<T>> {
  const signal = useAbortSignal();
  if (!signal) {
    return pending;
  }

  if (signal.aborted) {
    // Never await nor cancel it: cancel() on a query nobody awaits rejects a
    // promise without a reader, an unhandled rejection.
    throw new QueryAbortError();
  }

  let onAbort: (() => void) | undefined;
  const aborted = new Promise<never>((_, reject) => {
    onAbort = () => {
      // Rejects before the cancel can settle the query, so the caller always
      // sees the abort. The query settles later, if ever, with no reader.
      reject(new QueryAbortError());
      Promise.resolve(pending).catch(() => {});
      pending.cancel();
    };
    signal.addEventListener('abort', onAbort, { once: true });
  });

  try {
    return await Promise.race([pending, aborted]);
  } finally {
    if (onAbort) signal.removeEventListener('abort', onAbort);
  }
}
