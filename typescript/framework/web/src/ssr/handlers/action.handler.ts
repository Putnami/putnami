import { type HttpRequestContext, HttpResponse } from '@putnami/application';
import { useContext, HttpException } from '@putnami/runtime';
import { ACTION_OUTCOME_CONTEXT_KEY, type ActionOutcome, contextSlots } from '../../shared/context-slots';

/**
 * Publishes how the action settled on the request context. The handler decides
 * nothing about who reads it: a plugin registered around the request picks the
 * slot up after `next()` resolves.
 */
const record = (ctx: HttpRequestContext, outcome: ActionOutcome) => {
  contextSlots(ctx)[ACTION_OUTCOME_CONTEXT_KEY] = { route: ctx.route, outcome };
};

/**
 * Classifies a settled action by the status it answers with.
 *
 * 400 and 422 mean the submitted input was wrong; any other failing status
 * means the action itself failed. A redirect is a success — it is how a form
 * normally ends — so only 4xx and 5xx are anything but `ok`.
 */
const outcomeOf = (status: number): ActionOutcome => {
  if (status === 400 || status === 422) return 'validation_error';
  return status >= 400 ? 'error' : 'ok';
};

/**
 * Publishes the outcome of an action that returned.
 *
 * Exported because the seam has two call sites: {@link actionHandler} wraps the
 * React Router route node, while a plain HTML form POST reaches the HTTP route
 * the router never sees. A reader of the slot — the analytics page-view
 * middleware, for one — would otherwise observe every enhanced submission and
 * no no-JavaScript one.
 *
 * The outcome is read off the response the action produced, not off the mere
 * fact that it returned. Returning `json({ errors }, { status: 400 })` is the
 * documented way to fail a form validation (see `doc/forms-and-actions.md`),
 * so treating every return as a success would publish every rejected
 * submission as a successful one.
 *
 * @param ctx - The request context the slot lives on.
 * @param result - Whatever the action handler returned.
 */
export function recordActionOutcome(ctx: HttpRequestContext, result: unknown): void {
  if (result instanceof HttpResponse) {
    record(ctx, outcomeOf(result.status ?? 200));
    return;
  }
  if (result instanceof Response) {
    record(ctx, outcomeOf(result.status));
    return;
  }
  record(ctx, 'ok');
}

/**
 * Publishes the outcome of an action that threw.
 *
 * @param ctx - The request context the slot lives on.
 * @param error - The thrown value.
 */
export function recordActionFailure(ctx: HttpRequestContext, error: unknown): void {
  // 400/422 is the caller's input being wrong, not the action failing.
  record(ctx, outcomeOf(error instanceof HttpException ? error.getStatus() : 500));
}

export const actionHandler = (handler: (ctx: HttpRequestContext) => unknown | HttpResponse) => async () => {
  const ctx = useContext<HttpRequestContext>();
  try {
    const result = await handler(ctx);
    recordActionOutcome(ctx, result);

    if (result instanceof HttpResponse || result instanceof Response) {
      return result;
    }
    if (!result) {
      return new HttpResponse(undefined, { status: 204 });
    }
    if (Array.isArray(result) || typeof result === 'object') {
      return HttpResponse.json(result);
    }
    return undefined;
  } catch (e) {
    recordActionFailure(ctx, e);
    if (e instanceof HttpException) {
      return HttpResponse.json(e.getResponse(), { status: e.getStatus() });
    }
    throw e;
  }
};
