import type { HttpMiddleware } from '@putnami/application';
import type { ScopeContext } from '@putnami/runtime';
import { useContainer } from '@putnami/runtime/inject';
import { cleanupTransaction } from './transaction';
import { TransactionMiddleware } from './transaction.middleware';
import { UnitOfWork } from './unit-of-work';

/**
 * Resolve the request-scoped {@link UnitOfWork} from the active DI scope, or
 * `undefined` when there is no scope on this request or no UnitOfWork provider is
 * registered (i.e. the `sql()` plugin was not opted into `unitOfWork: true`).
 * Resolving it materializes exactly one UnitOfWork per request — cached in the
 * scope container — shared by the middleware boundary and any handler that
 * injects it (e.g. to call {@link UnitOfWork.setRollbackOnly}).
 */
function resolveUnitOfWork(): UnitOfWork | undefined {
  let container: ScopeContext;
  try {
    container = useContainer();
  } catch {
    // No active DI scope (no registrations / DI not attached) → no UnitOfWork.
    return undefined;
  }
  return container.has(UnitOfWork) ? container.get(UnitOfWork) : undefined;
}

/**
 * HTTP middleware that owns the request-scoped {@link UnitOfWork} transaction
 * boundary. Add it early in the chain (so it wraps every handler) alongside
 * `sql({ unitOfWork: true })`:
 *
 * ```ts
 * http().use(UnitOfWorkMiddleware());
 * ```
 *
 * Per request it resolves the UnitOfWork from the DI scope and:
 *
 *   - {@link UnitOfWork.begin | opens} it before the handler runs (zero-cost
 *     until the first repository write);
 *   - {@link UnitOfWork.commit | commits} on handler success (a rollback-only
 *     unit rolls back instead);
 *   - {@link UnitOfWork.rollback | rolls back} on a thrown error or on
 *     AbortSignal (client disconnect / request timeout), releasing reserved
 *     connections early; and
 *   - always runs the existing {@link cleanupTransaction} safety net in the
 *     `finally` — a no-op once the unit has finalized.
 *
 * A partial multi-datasource commit surfaces as a thrown error (the documented
 * non-atomic boundary), never as a successful response.
 *
 * When no UnitOfWork provider is registered (the plugin was not opted in) it
 * degrades to {@link TransactionMiddleware}'s plain safety-net behaviour, so it
 * is a strict superset of TransactionMiddleware.
 */
export const UnitOfWorkMiddleware = (): HttpMiddleware => async (context, next) => {
  const uow = resolveUnitOfWork();
  if (!uow) {
    // No opt-in on this request: behave exactly like the safety-net middleware.
    return TransactionMiddleware()(context, next);
  }

  uow.begin();

  const signal = context.signal ?? context.req?.signal;
  let abortRollback: Promise<void> | undefined;
  let removeAbortListener: (() => void) | undefined;

  if (signal?.aborted) {
    // Already aborted before we started — roll back rather than commit.
    abortRollback = uow.rollback();
  } else if (signal) {
    const onAbort = () => {
      abortRollback = uow.rollback();
    };
    signal.addEventListener('abort', onAbort, { once: true });
    removeAbortListener = () => signal.removeEventListener('abort', onAbort);
  }

  try {
    const result = await next();
    // Commit on success. A rollback-only unit, or one already finalized by an
    // abort mid-request, rolls back / no-ops here — never a double-commit.
    await uow.commit();
    return result;
  } catch (error) {
    await uow.rollback();
    throw error;
  } finally {
    removeAbortListener?.();
    // Await the abort-triggered rollback if it started, then run the final
    // safety net (idempotent — a no-op once the unit has finalized).
    if (abortRollback) {
      await abortRollback;
    }
    await cleanupTransaction();
  }
};
