import type { HttpMiddleware } from '@putnami/application';
import { cleanupTransaction } from './transaction';

/**
 * HTTP middleware that acts as a safety net for transactions.
 * Should be added early in the middleware chain (so it wraps all handlers).
 *
 * - On normal completion or error, rolls back any uncommitted transaction.
 * - When the request is aborted (client disconnect / timeout), triggers an
 *   early rollback so reserved connections are released back to the pool
 *   without waiting for in-flight work to finish naturally.
 *
 * Usage:
 * ```ts
 * http().use(TransactionMiddleware())
 * ```
 */
export const TransactionMiddleware = (): HttpMiddleware => async (context, next) => {
  const signal = context.signal ?? context.req?.signal;

  let abortCleanupPromise: Promise<boolean> | undefined;
  let removeAbortListener: (() => void) | undefined;

  if (signal && !signal.aborted) {
    const onAbort = () => {
      abortCleanupPromise = cleanupTransaction();
    };
    signal.addEventListener('abort', onAbort, { once: true });
    removeAbortListener = () => signal.removeEventListener('abort', onAbort);
  }

  try {
    return await next();
  } finally {
    removeAbortListener?.();
    // Await the abort-triggered cleanup if it started, then run a final pass
    // (idempotent — no-op if the abort handler already cleaned up).
    if (abortCleanupPromise) {
      await abortCleanupPromise;
    }
    await cleanupTransaction();
  }
};
