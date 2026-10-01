import { type HttpMiddleware, type HttpRequestContext, HttpResponse, type RouteHandler } from '@putnami/application';
import type { PageDefinition } from './page';
import { normalizeRoute } from './route-cache.utils';

export function composeMiddleware(
  layout: HttpMiddleware[] | undefined,
  page: HttpMiddleware[] | undefined,
): HttpMiddleware[] | undefined {
  if (!layout && !page) return undefined;
  if (!layout) return page;
  if (!page) return layout;
  return [...layout, ...page];
}

/**
 * A middleware chain that is either known synchronously (an array) or can only
 * be resolved at request time (a thunk). The thunk form exists for lazy layouts:
 * a `.secure()` declared on a lazily-imported layout cannot be read until its
 * module loads, which happens on first request rather than at registration.
 * `undefined` means "no middleware".
 */
export type MiddlewareSource = HttpMiddleware[] | (() => Promise<HttpMiddleware[] | undefined>) | undefined;

/**
 * Apply a {@link MiddlewareSource} to a handler. An array wraps immediately; a
 * thunk defers to first request (see {@link wrapWithDeferredMiddleware});
 * `undefined` leaves the handler unwrapped.
 */
export function applyMiddlewareSource(handler: RouteHandler, source: MiddlewareSource): RouteHandler {
  if (!source) return handler;
  if (Array.isArray(source)) return wrapWithMiddleware(handler, source);
  return wrapWithDeferredMiddleware(handler, source);
}

/**
 * Wrap a handler with middleware that is only knowable at request time, e.g. a
 * lazy layout's `.secure()` whose module must load before its security chain is
 * known. The chain is resolved on the first request and memoized for the rest.
 *
 * SECURITY: resolution failures are NOT cached — the memo is dropped so the next
 * request retries instead of permanently shipping the handler unguarded. While a
 * resolution is in flight the awaiting request blocks on it, so the handler can
 * never run before its (possibly auth-bearing) middleware has been resolved.
 */
function wrapWithDeferredMiddleware(
  handler: RouteHandler,
  resolveMiddleware: () => Promise<HttpMiddleware[] | undefined>,
): RouteHandler {
  let pending: Promise<HttpMiddleware[] | undefined> | undefined;
  return async (ctx: HttpRequestContext) => {
    if (!pending) {
      pending = resolveMiddleware().catch((error) => {
        pending = undefined; // fail closed but allow a later request to retry
        throw error;
      });
    }
    const middleware = await pending;
    const finalHandler = middleware ? wrapWithMiddleware(handler, middleware) : handler;
    return finalHandler(ctx);
  };
}

export function wrapWithMiddleware(handler: RouteHandler, middleware: HttpMiddleware[]): RouteHandler {
  return async (ctx: HttpRequestContext) => {
    const dispatch = async (): Promise<HttpResponse | undefined> => {
      const result = await handler(ctx);
      if (result === undefined) return undefined;
      if (result instanceof HttpResponse) return result;
      if (typeof result === 'string') {
        return new HttpResponse(result, {
          status: ctx.statusCode || 200,
          headers: { 'Content-Type': 'text/plain' },
        });
      }
      return HttpResponse.json(result, { status: ctx.statusCode || 200 });
    };

    const chain = middleware.reduceRight<() => Promise<HttpResponse | undefined>>(
      (next, mw) => async () => mw(ctx, next),
      dispatch,
    );

    return chain();
  };
}

/**
 * Wraps a handler so middleware runs around it while the handler's raw
 * result is preserved instead of being converted to an HttpResponse.
 * A response returned by middleware (e.g. 401/403) wins; otherwise the
 * handler's result is returned untouched. Loaders and actions use this
 * variant because their results are data, not HTTP responses.
 */
export function wrapWithMiddlewareRaw<T>(
  handler: (ctx: HttpRequestContext) => T | Promise<T>,
  middleware: HttpMiddleware[],
): (ctx: HttpRequestContext) => Promise<T | HttpResponse> {
  return async (ctx: HttpRequestContext) => {
    let handlerResult!: T;
    const dispatch = async (): Promise<HttpResponse | undefined> => {
      handlerResult = await handler(ctx);
      return undefined;
    };

    const chain = middleware.reduceRight<() => Promise<HttpResponse | undefined>>(
      (next, mw) => async () => mw(ctx, next),
      dispatch,
    );

    const middlewareResult = await chain();
    if (middlewareResult) {
      return middlewareResult;
    }
    return handlerResult;
  };
}

/**
 * Collect middleware from all ancestor layouts for a route, ordered from the
 * root layout down to the route's nearest layout.
 */
export function collectLayoutMiddleware(
  layoutMiddleware: Map<string, HttpMiddleware[]>,
  route: string,
): HttpMiddleware[] | undefined {
  const normalized = normalizeRoute(route);
  const segments = normalized.split('/').filter(Boolean);
  const collected: HttpMiddleware[] = [];

  const rootMw = layoutMiddleware.get('/');
  if (rootMw) collected.push(...rootMw);

  let path = '';
  for (const seg of segments) {
    path += `/${seg}`;
    const mw = layoutMiddleware.get(path);
    if (mw) collected.push(...mw);
  }

  return collected.length > 0 ? collected : undefined;
}

/**
 * Compose page middleware from layout and page config sources. Loader cache
 * middleware is NOT included here — it is applied only to the JSON endpoint
 * inside registerJsonEndpoint().
 */
export function resolvePageMiddleware(
  layoutMiddleware: Map<string, HttpMiddleware[]>,
  route: string,
  pageDef: PageDefinition | undefined,
  notFoundMiddleware: HttpMiddleware[] | undefined,
): HttpMiddleware[] | undefined {
  if (notFoundMiddleware) {
    return notFoundMiddleware;
  }
  const layoutMw = collectLayoutMiddleware(layoutMiddleware, route);
  const pageMw = pageDef?.middleware && pageDef.middleware.length > 0 ? [...pageDef.middleware] : undefined;
  return composeMiddleware(layoutMw, pageMw);
}
