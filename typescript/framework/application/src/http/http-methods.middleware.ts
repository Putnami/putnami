import type { HttpRequestContext, HttpRequestContextInternal } from './http-context.type';
import type { HttpMethod } from './http-method.type';
import type { HttpMiddleware } from './http-middleware.type';
import { HttpResponse } from './http-response';
import type { RouteController } from './route.controller';

export interface HttpMethodsOptions {
  /** Enable automatic HEAD response derived from GET handlers. Default: `true` */
  head?: boolean;
  /** Enable automatic OPTIONS response with `Allow` header. Default: `true` */
  options?: boolean;
  /** Enable TRACE method echo. Default: `false` (disabled for security). */
  trace?: boolean;
}

const WS_METHODS = new Set(['MESSAGE', 'OPEN', 'CLOSE']);

/** Headers excluded from TRACE echoes to prevent information leaks. */
const TRACE_EXCLUDED_HEADERS = new Set(['cookie', 'authorization', 'proxy-authorization', 'set-cookie']);

export function resolveHttpMethodsOptions(input?: HttpMethodsOptions | false): {
  head: boolean;
  options: boolean;
  trace: boolean;
} {
  if (input === false) return { head: false, options: false, trace: false };
  return {
    head: input?.head !== false,
    options: input?.options !== false,
    trace: input?.trace === true,
  };
}

/**
 * Compute the `Allow` header value for a given path.
 */
function computeAllowHeader(methods: string[], config: { head: boolean; options: boolean; trace: boolean }): string {
  const set = new Set(methods.filter((m) => !WS_METHODS.has(m)));
  if (config.head) set.add('HEAD');
  if (config.options) set.add('OPTIONS');
  if (config.trace) set.add('TRACE');
  return [...set].sort().join(', ');
}

/**
 * Build a TRACE response that echoes the request message.
 * Sensitive headers (Cookie, Authorization, Proxy-Authorization) are excluded.
 */
export function buildTraceResponse(req: Request, ctx: HttpRequestContext): HttpResponse {
  const query = ctx.query();
  const requestTarget = `${ctx.path()}${query ? `?${query}` : ''}`;
  const lines: string[] = [`TRACE ${requestTarget} HTTP/1.1`];

  req.headers.forEach((value, name) => {
    if (!TRACE_EXCLUDED_HEADERS.has(name.toLowerCase())) {
      lines.push(`${name}: ${value}`);
    }
  });

  const body = lines.join('\r\n');
  return new HttpResponse(body, {
    status: 200,
    headers: { 'Content-Type': 'message/http' },
  });
}

/**
 * Build a middleware that auto-handles HEAD, OPTIONS, and TRACE.
 *
 * - **HEAD**: If no explicit HEAD route is registered, delegates to the GET
 *   handler and strips the response body while preserving headers.
 * - **OPTIONS**: If no explicit OPTIONS route is registered, responds with
 *   `204 No Content` and an `Allow` header listing available methods.
 * - **TRACE**: Disabled by default. When enabled, echoes the request back
 *   excluding sensitive headers to prevent XST attacks.
 *
 * The middleware is designed to run *before* user middlewares so it can
 * modify route matching context. User-registered HEAD / OPTIONS / TRACE
 * routes take precedence over auto-generated responses.
 */
export function HttpMethodsMiddleware(
  router: RouteController,
  config: { head: boolean; options: boolean; trace: boolean },
): HttpMiddleware {
  return async (ctx, next) => {
    const method = ctx.req.method;

    // HEAD — delegate to GET handler if no explicit HEAD route was matched
    if (config.head && method === 'HEAD' && !ctx.route) {
      // Temporarily switch method to GET for route lookup
      const saved = ctx.method;
      (ctx as { method: HttpMethod }).method = 'GET';

      // Re-match route using GET
      const handlers = router.find(ctx);
      if (handlers.length > 0) {
        ctx.route = handlers[0].route;
        ctx.params = handlers[0].params;
        ctx.statusCode = handlers[0].statusCode;
        // Update cached handlers so handle() uses the GET handlers
        // RouteController<CONTEXT>.find() returns handlers typed to CONTEXT; cast aligns with base HttpRequestContext
        (ctx as HttpRequestContextInternal).__matchedHandlers = handlers as unknown as ReturnType<
          RouteController<HttpRequestContext>['find']
        >;

        // Optimised path: use pre-computed HEAD metadata when available
        // (e.g. static files provide Content-Type, Content-Length, ETag, etc.)
        if (handlers[0].headMeta) {
          (ctx as { method: HttpMethod }).method = saved;
          const metaResponse = handlers[0].headMeta(ctx);
          if (metaResponse) return metaResponse;
          // headMeta returned undefined — fall through to full handler
          (ctx as { method: HttpMethod }).method = 'GET';
        }

        const res = await next();
        (ctx as { method: HttpMethod }).method = saved;

        if (res) {
          // Build the native response to capture Content-Length, then strip body
          const native = res.get();
          const headers: [string, string][] = [];
          native.headers.forEach((value, name) => {
            headers.push([name, value]);
          });
          return new HttpResponse(undefined, {
            status: native.status,
            statusText: native.statusText,
            headers,
          });
        }
        return res;
      }
      (ctx as { method: HttpMethod }).method = saved;
    }

    // OPTIONS — respond with Allow header if no explicit route was matched
    if (config.options && method === 'OPTIONS' && !ctx.route) {
      const methods = router.findMethods(ctx.path());
      if (methods.length > 0) {
        const allow = computeAllowHeader(methods, config);
        return new HttpResponse(undefined, { status: 204 }).setHeader('Allow', allow);
      }
    }

    // TRACE — echo the request (disabled by default)
    if (config.trace && method === 'TRACE') {
      return buildTraceResponse(ctx.req, ctx);
    }

    return next();
  };
}
