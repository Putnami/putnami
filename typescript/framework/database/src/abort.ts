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
 * The key an HTTP request context holds its abort hooks under, as
 * `@putnami/application` sets it (`REQUEST_ABORT_HOOKS`): whether the
 * request ended, and a listener called when it ends, without building the
 * request's AbortSignal.
 */
const REQUEST_ABORT_HOOKS = Symbol.for('putnami.request.abortHooks');

interface RequestAbortHooks {
  aborted(): boolean;
  onAbort(listener: () => void): void;
  offAbort(listener: () => void): void;
}

type AbortContext = { signal?: AbortSignal; [REQUEST_ABORT_HOOKS]?: RequestAbortHooks };

/**
 * The request's abort hooks, unless the context carries a signal of its own:
 * a context derived with its own `signal` is governed by that signal, as the
 * ambient abort signal always governs cancellation. The HTTP request context
 * reads its signal through a getter, never an own property.
 */
function requestHooksOf(context: AbortContext | undefined): RequestAbortHooks | undefined {
  if (context === undefined || Object.hasOwn(context, 'signal')) return undefined;
  return context[REQUEST_ABORT_HOOKS];
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
  const context = tryContext<AbortContext>();
  const hooks = requestHooksOf(context);
  if (hooks !== undefined ? hooks.aborted() : context?.signal?.aborted) {
    throw new QueryAbortError();
  }
}

/**
 * Wrap a pending query so it is cancelled when the context signal fires.
 *
 * - If the signal is already aborted, rejects without starting the query: a
 *   query runs only once something awaits it, so nothing is sent.
 * - Otherwise, on abort it calls `pending.cancel()`, which drops a query still
 *   waiting for a connection or sends a PostgreSQL protocol-level cancel
 *   request for a running one, and rejects at once with a `QueryAbortError`:
 *   the caller unwinds even when the connection is dead and the driver never
 *   settles the query.
 * - The listener is cleaned up once the query settles.
 *
 * Pass-through when there is no signal in the context.
 *
 * It starts the query in the call and adds no async frame: the promise it
 * returns is settled by the query, or rejected at the abort. It never throws;
 * every failure rejects.
 */
export function abortableQuery<T extends readonly any[]>(pending: SqlQuery<T>): Promise<SqlResult<T>> {
  try {
    const context = tryContext<AbortContext>();
    const hooks = requestHooksOf(context);
    if (hooks !== undefined) {
      // Never await nor cancel a query refused here: cancel() on a query
      // nobody awaits rejects a promise without a reader, an unhandled rejection.
      if (hooks.aborted()) return Promise.reject(new QueryAbortError());
      return hookedQuery(pending, hooks);
    }
    const signal = context?.signal;
    if (!signal) {
      return pending.then() as Promise<SqlResult<T>>;
    }
    if (signal.aborted) return Promise.reject(new QueryAbortError());
    return signalledQuery(pending, signal);
  } catch (error) {
    return Promise.reject(error);
  }
}

/**
 * Runs `pending` under a request's abort hooks: one listener, removed once the
 * query settles, and no AbortSignal.
 */
function hookedQuery<T extends readonly any[]>(pending: SqlQuery<T>, hooks: RequestAbortHooks): Promise<SqlResult<T>> {
  let resolve!: (result: SqlResult<T>) => void;
  let reject!: (reason: unknown) => void;
  const answer = new Promise<SqlResult<T>>((resolveAnswer, rejectAnswer) => {
    resolve = resolveAnswer;
    reject = rejectAnswer;
  });
  const onAbort = (): void => {
    // Rejects before the cancel can settle the query, so the caller always
    // sees the abort. The query settles later, if ever, into the handlers below.
    reject(new QueryAbortError());
    pending.cancel();
  };
  pending.then(
    (result) => {
      hooks.offAbort(onAbort);
      resolve(result);
    },
    (error: unknown) => {
      hooks.offAbort(onAbort);
      reject(error);
    },
  );
  try {
    hooks.onAbort(onAbort);
  } catch (error) {
    // The query runs already: the caller reads the failure through the answer,
    // and the query is stopped; it settles, if ever, into the handlers above.
    reject(error);
    pending.cancel();
  }
  return answer;
}

/** {@link hookedQuery} on the context's own signal. */
function signalledQuery<T extends readonly any[]>(pending: SqlQuery<T>, signal: AbortSignal): Promise<SqlResult<T>> {
  let resolve!: (result: SqlResult<T>) => void;
  let reject!: (reason: unknown) => void;
  const answer = new Promise<SqlResult<T>>((resolveAnswer, rejectAnswer) => {
    resolve = resolveAnswer;
    reject = rejectAnswer;
  });
  const onAbort = (): void => {
    reject(new QueryAbortError());
    pending.cancel();
  };
  pending.then(
    (result) => {
      signal.removeEventListener('abort', onAbort);
      resolve(result);
    },
    (error: unknown) => {
      signal.removeEventListener('abort', onAbort);
      reject(error);
    },
  );
  try {
    signal.addEventListener('abort', onAbort, { once: true });
  } catch (error) {
    reject(error);
    pending.cancel();
  }
  return answer;
}
