import type { HttpRequestContextInternal } from './http-context.type';
import type { HttpMiddleware } from './http-middleware.type';
import type { HttpResponse } from './http-response';
import { type MiddlewareForm, middlewareFormOf } from './middleware-form';

/** What a compiled route's middlewares answer: at once, or through a promise. */
export type CompiledAnswer = HttpResponse | undefined | Promise<HttpResponse | undefined>;

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
      return { reason: `middleware ${i + 1} of ${middlewares.length} (${name || 'anonymous'}) has no synchronous form` };
    }
    forms.push(form);
  }
  return { forms };
}

/** The state of a form that let the request pass: it runs no other hook. */
const PASSED: unique symbol = Symbol('passed');

/**
 * Runs `forms`, the first outermost, around `dispatch`, as the middleware
 * chain built from the same forms would: each form's `passes`, `answer` and
 * `enter` outermost first, then the handler, then each `leave` innermost
 * first, in one continuation. An error goes to the `fail` of the forms
 * outside the one that threw it, then becomes the request's answer through
 * `answerError`, as an error that escapes the chain does. The answer comes
 * back at once when no hook and no handler answered through a promise.
 */
export function runCompiledRoute(
  ctx: HttpRequestContextInternal,
  forms: readonly MiddlewareForm<unknown>[],
  dispatch: (
    answered: (response: HttpResponse | undefined) => CompiledAnswer,
    failed: (error: unknown) => CompiledAnswer,
  ) => CompiledAnswer,
  answerError: (error: unknown) => HttpResponse | Promise<HttpResponse>,
): CompiledAnswer {
  const count = forms.length;
  const states: unknown[] = count === 0 ? EMPTY : new Array(count);

  // The error that escaped the part inside the first `depth` forms: each of
  // them sees it, innermost first, then it becomes the request's answer.
  const fail = (depth: number, error: unknown): HttpResponse | Promise<HttpResponse> => {
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
    return answerError(current);
  };

  // The answer of the first `depth` forms once the part inside them answered
  // `response`: each leaves, innermost first. One that throws hands the error
  // to the forms outside it.
  const unwind = (depth: number, response: HttpResponse | undefined): CompiledAnswer => {
    let answer = response;
    let i = depth - 1;
    try {
      for (; i >= 0; i--) {
        const state = states[i];
        const leave = forms[i].leave;
        if (state !== PASSED && leave !== undefined) answer = leave(ctx, answer, state);
      }
    } catch (error) {
      return fail(i, error);
    }
    return answer;
  };

  let depth = 0;
  try {
    for (; depth < count; depth++) {
      const form = forms[depth];
      if (form.passes?.(ctx)) {
        states[depth] = PASSED;
        continue;
      }
      if (form.answer !== undefined) {
        const early = form.answer(ctx);
        if (early !== undefined) {
          const at = depth;
          return early instanceof Promise
            ? early.then(
                (response) => unwind(at, response),
                (error: unknown) => fail(at, error),
              )
            : unwind(at, early);
        }
      }
      states[depth] = form.enter === undefined ? undefined : form.enter(ctx);
    }
  } catch (error) {
    return fail(depth, error);
  }
  return dispatch(
    (response) => unwind(count, response),
    (error) => fail(count, error),
  );
}

const EMPTY: unknown[] = [];
