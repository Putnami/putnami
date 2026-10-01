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
 * - If the signal is already aborted, cancels the query immediately and throws.
 * - Otherwise, listens for abort and calls `pending.cancel()` which sends a
 *   PostgreSQL protocol-level cancel request (the server stops executing the query).
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
    pending.cancel();
    throw new QueryAbortError();
  }

  const onAbort = () => pending.cancel();
  signal.addEventListener('abort', onAbort, { once: true });

  try {
    return await pending;
  } finally {
    signal.removeEventListener('abort', onAbort);
  }
}
