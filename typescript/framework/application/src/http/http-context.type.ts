import type { Context } from '@putnami/runtime';
import type { Principal } from '../security/security.types';
import type { HttpMethod } from './http-method.type';
import type { HttpResponse } from './http-response';
import type { HandlerResult } from './router';
import type { RouteHandler } from './route.type';

export type Server = {
  upgrade<T = undefined>(
    request: Request,
    options?: {
      headers?: HeadersInit;
      data?: T;
    },
  ): boolean;
  /** Bun-specific: resolve client IP from the underlying socket. */
  requestIP?(req: Request): { address: string } | undefined;
};

/**
 * The request context every handler and middleware receives.
 *
 * This is the public, supported surface. Framework-internal plumbing (cached
 * route matches, captured errors, response callbacks, the forwarded
 * Authorization header) is intentionally NOT part of this type — it lives on
 * {@link HttpRequestContextInternal}, which is not exported from the package.
 */
export type HttpRequestContext = Context & {
  url: string;
  method: HttpMethod;
  headers: Headers;
  req: Request;
  server?: Server;
  route?: string;
  params?: Record<string, string>;
  statusCode?: number;
  queryParams: () => Record<string, string>;
  body: <T>() => Promise<T | undefined>;
  secured: () => boolean;
  host: () => string;
  domain: () => string;
  path: () => string;
  query: () => string;

  /**
   * Optional authenticated principal set by auth middleware: typed well-known
   * claims plus any other claim by key (the {@link Principal} index signature
   * keeps existing `ctx.user['someClaim']` readers working).
   */
  user?: Principal;

  /**
   * Abort request processing and respond with given status.
   * @throws HttpAbortException
   */
  throw: (status: number, message?: string) => never;
};

/**
 * @internal Framework-internal augmentation of {@link HttpRequestContext}.
 *
 * Carries the mutable plumbing the dispatcher, middleware, and cookie layer
 * use to coordinate request handling. Kept out of the public `HttpRequestContext`
 * so user code neither sees nor depends on these fields. Not re-exported from
 * the package barrel.
 */
export type HttpRequestContextInternal = HttpRequestContext & {
  /** Raw HTTP response owns the request scope until EOF or cancellation. */
  __rawHttpStreamResponse?: boolean;
  /** Raw Authorization header, captured for downstream service-to-service forwarding. */
  __authorizationHeader?: string;

  /** Identity resolvers captured during the HTTP upgrade and replayed for a bounded WS init frame. */
  __identityResolvers?: Array<
    (context: HttpRequestContext, next: () => Promise<HttpResponse | undefined>) => Promise<HttpResponse | undefined>
  >;

  /** Error caught during request handling, available for middleware logging. */
  __requestError?: unknown;

  /** Whether the request completion middleware already emitted the terminal error. */
  __requestErrorLogged?: boolean;

  /** Response transform callbacks (e.g. Set-Cookie) applied before the response is sent. */
  __callbacks?: ((r: HttpResponse) => HttpResponse)[];

  /** Cached route match results to avoid double lookup. */
  __matchedHandlers?: HandlerResult<RouteHandler<HttpRequestContext>>[];

  /** Whether the matched route is a first-party `api({ client })` endpoint — see `RouteOptions.firstPartyErrors`. */
  __firstPartyErrors?: boolean;

  /**
   * Aborted when the HTTP plugin serving this request begins draining: a
   * graceful stop, a scale to zero, an instance replacement. A negotiated SSE
   * stream ends with no terminal on it, so its consumer reads an interruption
   * and continues on another instance. A WebSocket conversation upgraded from
   * this request ends with its shutdown terminal. Owned by the serving plugin,
   * never by a route: a scanned route folder is shared by every instance in a
   * process.
   */
  __drain?: AbortSignal;
};
