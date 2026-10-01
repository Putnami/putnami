import type { HttpRequestContext } from './http-context.type';
import type { HttpResponse } from './http-response';

/**
 * Route handler can return:
 * - HttpResponse: Used as-is
 * - string: Wrapped as text response
 * - object/array: Wrapped as JSON response
 * - undefined: No response (try next handler)
 */
export type RouteHandlerResult = HttpResponse | string | object | undefined;

export type RouteHandler<C extends HttpRequestContext = HttpRequestContext> = (
  context: C,
) => Promise<RouteHandlerResult> | RouteHandlerResult;

/**
 * A responder for a native, pipeline-bypassing route (registered via
 * `HttpPlugin.native()`).
 *
 * Either a fixed `Response` / `HttpResponse` (served by Bun's native router
 * without invoking JS per request — the bare-metal path, ideal for
 * health/version/static), or a bare `(req) => Response` handler.
 *
 * Native routes run **none** of the framework pipeline: no request context, no
 * middleware, no secure-by-default headers, no DI scope, no content negotiation.
 * Use them only for trusted, self-contained hot paths.
 */
export type NativeResponder =
  | Response
  | HttpResponse
  | ((req: Request) => Response | HttpResponse | Promise<Response | HttpResponse>);

export type RouteOptions = {
  accept?: string[];
  statusCode?: number;
  /** Pre-computed HEAD metadata callback. When provided, HEAD requests skip handler execution. */
  headMeta?: (ctx: HttpRequestContext) => HttpResponse | undefined;
  /** When `true`, CSRF token validation is skipped for this route. Set automatically for `api()` routes. */
  csrfExempt?: boolean;
  /**
   * When `true`, the always-on security middleware (origin guard, security
   * headers) is skipped for this route even when it is enabled app-wide — the
   * per-route counterpart of `http({ secure: false })`. Use only for trusted,
   * unauthenticated hot paths.
   */
  minimal?: boolean;
  /**
   * When `false`, only the secure response headers middleware is skipped for
   * this route. Other secure defaults such as the origin guard still run.
   */
  securityHeaders?: false;
  /**
   * When `true`, an unhandled `HttpException` on this route serializes as the
   * first-party client-contract error envelope (`{ code, error, message, details? }`,
   * with `code` the stable wire code from `x-putnami-client`) instead of the
   * default `{ statusCode, message, error }` body. Set automatically for routes
   * registered under `api({ client })`.
   */
  firstPartyErrors?: boolean;
};
