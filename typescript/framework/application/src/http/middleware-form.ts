import type { HttpRequestContext } from './http-context.type';
import type { HttpMiddleware } from './http-middleware.type';
import type { HttpResponse } from './http-response';

/**
 * The synchronous form of a middleware: what it does around the inner chain,
 * as hooks with no `next` and no promise of their own. A request runs them in
 * this order:
 *
 * 1. `passes`: `true` leaves the request untouched; no other hook runs.
 * 2. `answer`: an answer (or a promise of one) ends the request there, as a
 *    middleware that returns without calling `next()`; the other hooks do not
 *    run, and the middlewares outside this one see that answer.
 * 3. `enter`: before the inner chain; what it returns is the request's state
 *    for `leave` and `fail`.
 * 4. `leave`: the inner chain answered; what it returns is the middleware's
 *    answer.
 * 5. `fail`: the inner chain failed; the error then goes on to the
 *    middlewares outside this one. An error `fail` throws replaces it.
 *
 * An error thrown by `answer`, `enter` or `leave` goes to the middlewares
 * outside this one, not to this one's `fail`, as an error thrown before
 * `next()` or in the continuation of its result would.
 *
 * {@link middlewareFromForm} builds the `(ctx, next)` middleware of a form,
 * so the middleware and its form cannot differ.
 */
export interface MiddlewareForm<S = undefined> {
  readonly passes?: (ctx: HttpRequestContext) => boolean;
  readonly answer?: (
    ctx: HttpRequestContext,
  ) => HttpResponse | Promise<HttpResponse | undefined> | undefined;
  readonly enter?: (ctx: HttpRequestContext) => S;
  readonly leave?: (ctx: HttpRequestContext, response: HttpResponse | undefined, state: S) => HttpResponse | undefined;
  readonly fail?: (ctx: HttpRequestContext, error: unknown, state: S) => void;
}

/**
 * The key a middleware carries its form under. `Symbol.for` keeps it readable
 * when generated routes and the root HTTP plugin resolve the framework from
 * two locations.
 */
const MIDDLEWARE_FORM = Symbol.for('putnami.http.middleware.form');

type FormedMiddleware = HttpMiddleware & { [MIDDLEWARE_FORM]?: MiddlewareForm<unknown> };

/** The form `middleware` was built from, or `undefined` when it has none. */
export function middlewareFormOf(middleware: HttpMiddleware): MiddlewareForm<unknown> | undefined {
  return (middleware as FormedMiddleware)[MIDDLEWARE_FORM];
}

/** Builds the middleware that runs `form` around `next()`, and records the form on it. */
export function middlewareFromForm<S>(form: MiddlewareForm<S>): HttpMiddleware {
  const { passes, answer, enter, leave, fail } = form;
  // No async frame of its own: the hooks run in the continuation of the inner
  // chain's promise.
  const middleware: HttpMiddleware = (ctx, next) => {
    if (passes !== undefined && passes(ctx)) return untouched(next);
    let state: S;
    try {
      if (answer !== undefined) {
        const early = answer(ctx);
        if (early !== undefined) return early instanceof Promise ? early : Promise.resolve(early);
      }
      state = (enter === undefined ? undefined : enter(ctx)) as S;
    } catch (error) {
      return Promise.reject(error);
    }
    let pending: Promise<HttpResponse | undefined>;
    try {
      pending = next();
    } catch (error) {
      if (fail !== undefined) {
        try {
          fail(ctx, error, state);
        } catch (thrown) {
          return Promise.reject(thrown);
        }
      }
      return Promise.reject(error);
    }
    // A middleware that broke its contract and returned a plain value still works.
    const inner = pending instanceof Promise ? pending : Promise.resolve(pending);
    if (leave === undefined && fail === undefined) return inner;
    return inner.then(
      leave === undefined ? undefined : (response) => leave(ctx, response, state),
      fail === undefined
        ? undefined
        : (error: unknown) => {
            fail(ctx, error, state);
            throw error;
          },
    );
  };
  (middleware as FormedMiddleware)[MIDDLEWARE_FORM] = form as MiddlewareForm<unknown>;
  return middleware;
}

/** The inner chain's answer, as a promise: a synchronous throw rejects, a plain value resolves. */
function untouched(next: () => Promise<HttpResponse | undefined>): Promise<HttpResponse | undefined> {
  try {
    const pending = next();
    return pending instanceof Promise ? pending : Promise.resolve(pending);
  } catch (error) {
    return Promise.reject(error);
  }
}
