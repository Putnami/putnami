import type { HttpRequestContext, HttpRequestContextInternal } from './http-context.type';
import type { HttpMiddleware } from './http-middleware.type';
import type { HttpResponse } from './http-response';
import { type MiddlewareForm, middlewareFormOf } from './middleware-form';
import type { RouteHandler } from './route.type';

/** What a compiled route's middlewares answer: at once, or through a promise. */
export type CompiledAnswer = HttpResponse | undefined | Promise<HttpResponse | undefined>;

/** Where the handler of a compiled route hands its answer, or the error past its own answer. */
export interface RouteContinuation {
  answered(response: HttpResponse | undefined): CompiledAnswer;
  failed(error: unknown): CompiledAnswer;
}

/** Runs a compiled route's handler and hands its answer on; see `HttpDispatcher.runRoute`. */
export interface RouteDispatcher {
  runRoute(
    ctx: HttpRequestContextInternal,
    handler: RouteHandler<HttpRequestContext>,
    continuation: RouteContinuation,
  ): CompiledAnswer;
}

/**
 * The forms of `middlewares`, in order, when every one has a form; otherwise
 * the reason the route keeps the middleware chain.
 */
export function compiledForms(
  middlewares: readonly HttpMiddleware[],
): { readonly forms: readonly MiddlewareForm<unknown>[] } | { readonly reason: string } {
  const forms: MiddlewareForm<unknown>[] = [];
  for (let i = 0; i < middlewares.length; i++) {
    const form = middlewareFormOf(middlewares[i]);
    if (form === undefined) {
      const name = middlewares[i].name;
      return {
        reason: `middleware ${i + 1} of ${middlewares.length} (${name || 'anonymous'}) has no synchronous form`,
      };
    }
    forms.push(form);
  }
  return { forms };
}

/** The state of a form that let the request pass: it runs no other hook. */
const PASSED: unique symbol = Symbol('passed');

const NO_STATES: unknown[] = [];

/**
 * One request on a compiled route: it runs `forms`, the first outermost,
 * around the route's handler, as the middleware chain built from the same
 * forms would. Each form's `passes`, `answer` and `enter` run outermost
 * first, then the handler, then each `leave` innermost first, in one
 * continuation. An error goes to the `fail` of the forms outside the one that
 * threw it, then becomes the request's answer through `answerError`, as an
 * error that escapes the chain does. The answer comes back at once when no
 * hook and no handler answered through a promise.
 */
export class CompiledRequest<E> implements RouteContinuation {
  private readonly states: unknown[];

  constructor(
    private readonly ctx: HttpRequestContextInternal,
    private readonly forms: readonly MiddlewareForm<unknown>[],
    /** Passed back to `answerError` as it is: what the caller needs to answer an error. */
    private readonly served: E,
    private readonly answerError: (
      ctx: HttpRequestContextInternal,
      served: E,
      error: unknown,
    ) => HttpResponse | Promise<HttpResponse>,
  ) {
    this.states = forms.length === 0 ? NO_STATES : new Array(forms.length);
  }

  /** Runs the forms, and `handler` inside them through `dispatcher`, which hands its answer to this request. */
  run(dispatcher: RouteDispatcher, handler: RouteHandler<HttpRequestContext>): CompiledAnswer {
    const { ctx, forms, states } = this;
    let depth = 0;
    try {
      for (; depth < forms.length; depth++) {
        const form = forms[depth];
        if (form.passes?.(ctx)) {
          states[depth] = PASSED;
          continue;
        }
        if (form.answer !== undefined) {
          const early = form.answer(ctx);
          if (early !== undefined) {
            if (!(early instanceof Promise)) return this.unwind(depth, early);
            const at = depth;
            return early.then(
              (response) => this.unwind(at, response),
              (error: unknown) => this.fail(at, error),
            );
          }
        }
        states[depth] = form.enter === undefined ? undefined : form.enter(ctx);
      }
    } catch (error) {
      return this.fail(depth, error);
    }
    return dispatcher.runRoute(ctx, handler, this);
  }

  answered(response: HttpResponse | undefined): CompiledAnswer {
    return this.unwind(this.forms.length, response);
  }

  failed(error: unknown): CompiledAnswer {
    return this.fail(this.forms.length, error);
  }

  /**
   * The answer of the first `depth` forms once the part inside them answered
   * `response`: each leaves, innermost first. One that throws hands the error
   * to the forms outside it.
   */
  private unwind(depth: number, response: HttpResponse | undefined): CompiledAnswer {
    const { ctx, forms, states } = this;
    let answer = response;
    let i = depth - 1;
    try {
      for (; i >= 0; i--) {
        const state = states[i];
        const leave = forms[i].leave;
        if (state !== PASSED && leave !== undefined) answer = leave(ctx, answer, state);
      }
    } catch (error) {
      return this.fail(i, error);
    }
    return answer;
  }

  /**
   * The error that escaped the part inside the first `depth` forms: each of
   * them sees it, innermost first, then it becomes the request's answer.
   */
  private fail(depth: number, error: unknown): HttpResponse | Promise<HttpResponse> {
    const { ctx, forms, states } = this;
    let current = error;
    for (let i = depth - 1; i >= 0; i--) {
      const state = states[i];
      const onFail = forms[i].fail;
      if (state === PASSED || onFail === undefined) continue;
      try {
        onFail(ctx, current, state);
      } catch (thrown) {
        current = thrown;
      }
    }
    return this.answerError(ctx, this.served, current);
  }
}
