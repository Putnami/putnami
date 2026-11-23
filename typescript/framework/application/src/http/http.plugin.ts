import './bun-serve-http2';
import type { ServerWebSocket } from 'bun';
import { type Server, serve } from 'bun';
import { type ContainerContext, HttpException, runInContext, useConfig, useLogger } from '@putnami/runtime';
import { READY_LOG_KEY, type ReadyEndpoint, readyMarker } from '@putnami/runtime/jobs';
import type { WsDataType } from '../api/ws-context.type';
import { WebSocketDispatcher } from '../api/ws-dispatcher';
import { Application, type GenerateResult, type Module, type Plugin } from '../application';
import { PutnamiConfig } from '../application/putnami.config';
import { commitRequestSession, loadRequestSession } from '../session/request-session';
import { SessionConfig } from '../session/session.config';
import { CompressionMiddleware } from './compression.middleware';
import { CsrfMiddleware, isCsrfMiddleware } from './csrf.middleware';
import { isDevHtmlRequest, renderDevErrorPage } from './dev-error-page';
import {
  attachRequestScope,
  buildHttpContext,
  closeRequestScope,
  type RequestLifetime,
  DEFAULT_MAX_BODY_SIZE_BYTES,
} from './http-context.builder';
import type { HttpRequestContext, HttpRequestContextInternal, Server as HttpServer } from './http-context.type';
import { HttpDispatcher, isRequestStop, requestEnded } from './http-dispatcher';
import type { HttpMethod } from './http-method.type';
import { HttpMethodsMiddleware, resolveHttpMethodsOptions } from './http-methods.middleware';
import type { HttpMiddleware } from './http-middleware.type';
import { HttpResponse } from './http-response';
import { retainRawHttpStreamScope } from './raw-http-stream';
import { OriginGuardMiddleware } from './origin-guard.middleware';
import { LinkedAbortController, type RequestDeadline, RequestDeadlines } from './request-deadlines';
import { type NativeEndpoint, nativeEndpoints, patternsMayOverlap } from './native-endpoints';
import { RouteController } from './route.controller';
import { requestAcceptOf } from './request-accept';
import { acceptScore, type HandlerResult } from './router';
import { type MiddlewareChain, RouteChains } from './route-scope';
import type { NativeResponder, RequestRewrite, RouteHandler, RouteOptions } from './route.type';
import { SecurityHeadersMiddleware } from './security-headers.middleware';
import type { ServerOptions } from './server.config';
import { normalizeGeneratedHttpRoutePath, type GeneratedHttpRoute } from '../http-routes/generation';

type RouteEntry = { method: HttpMethod; path: string; handler: RouteHandler; options?: RouteOptions };
type NativeRouteEntry = { method?: HttpMethod; path: string; responder: NativeResponder };

/** A Bun native route handler: it gets the request, and the server for the routes the pipeline serves. */
type BunNativeHandler = (req: Request, server: Server<WsDataType>) => Response | Promise<Response>;

/** A Bun native route value: a static response, a bare handler, or a per-method map of either. */
type BunNativeRoute = Response | BunNativeHandler | Record<string, Response | BunNativeHandler>;

/**
 * The address a bound server accepts traffic on, or `undefined` when it is not
 * addressable.
 *
 * The port comes from the BOUND server (`server.port`), not from the requested
 * one, so `PORT=0` still announces an address a client can reach. A port that
 * is absent (a unix-socket listener) or outside `[1,65535]` is not addressable:
 * the listening record then carries no readiness marker, and the application's
 * own ready record announces completed startup.
 */
function boundHttpEndpoint(port: number | undefined): ReadyEndpoint | undefined {
  if (port === undefined || !Number.isInteger(port) || port < 1 || port > 65_535) {
    return undefined;
  }
  // "localhost" matches the address the log message prints and the one a
  // developer can actually open; Bun binds every interface.
  return { scheme: 'http', host: 'localhost', port };
}

/**
 * The attributes of the listening log record. An addressable server adds a
 * server claim under the reserved key {@link READY_LOG_KEY}.
 *
 * A served app's stdout is a LOG stream — the `@putnami/typescript` extension
 * re-emits each line as a log event — so this is how the framework reaches the
 * runtime event stream it does not own: the extension's forwarder recognizes
 * the key and emits the typed `ready` event
 * (`protocols/runtime/ready_marker.go`). The human message is untouched.
 */
export function listeningRecord(port: number | undefined, durationMs: number): Record<string, unknown> {
  const endpoint = boundHttpEndpoint(port);
  if (!endpoint) {
    return { durationMs };
  }
  return { durationMs, [READY_LOG_KEY]: readyMarker({ target: 'server', endpoints: [endpoint], durationMs }) };
}

/**
 * Maximum time to let in-flight requests drain on shutdown before force-closing
 * remaining connections. Kept below the orchestrator-facing graceful-shutdown
 * budget (see `SHUTDOWN_TIMEOUT_MS`, 10s) so the process-level force-exit timer
 * never fires before the drain completes.
 */
const DRAIN_TIMEOUT_MS = 8000;

/** HTTP methods representable by v1 for a Bun native route without a method map. */
const METHODLESS_NATIVE_HTTP_METHODS = ['DELETE', 'GET', 'HEAD', 'OPTIONS', 'PATCH', 'POST', 'PUT'] as const;

/** Default max inbound WebSocket message size (1 MiB) — bounds client-controlled memory. */
const DEFAULT_WS_MAX_PAYLOAD_BYTES = 1_048_576;
/** Default max buffered outbound WebSocket bytes (16 MiB) before the connection is closed. */
const DEFAULT_WS_BACKPRESSURE_BYTES = 16_777_216;

/** Loads an asynchronous store's session before the inner middlewares and writes it back after them. */
const requestSessionMiddleware: HttpMiddleware = async (context, next) => {
  await loadRequestSession();
  let response: HttpResponse | undefined;
  try {
    response = await next();
  } catch (chainError) {
    // A response the chain throws (a redirect) is an answer, not a failure:
    // like a returned one, it needs its session written.
    if (chainError instanceof HttpResponse && !requestEnded(context)) {
      await commitRequestSession();
      throw chainError;
    }
    // The commit still runs, but the chain's own error is the one this
    // request reports.
    await commitRequestSession().catch((commitError: unknown) => {
      if (!isRequestStop(commitError, context)) useLogger().error('Session commit failed', commitError);
    });
    throw chainError;
  }
  await commitRequestSession();
  return response;
};

/**
 * Whether the configured session store may keep its sessions behind
 * asynchronous reads, so each request needs {@link requestSessionMiddleware}.
 * The cookie store, the default, never does: an app on it runs no session
 * middleware. A session config that cannot be read keeps the middleware, which
 * reports the problem on the request as before.
 */
function sessionStoreMayBeAsync(): boolean {
  try {
    return useConfig(SessionConfig).store !== 'cookie';
  } catch {
    return true;
  }
}

/**
 * Resolves an explicit socket idle timeout (seconds) for `serve()`, derived
 * from the request timeout with a small buffer so the application-level 408
 * fires first. This bounds slow-loris / stalled sockets instead of relying on
 * the runtime's implicit default. Clamped to Bun's supported range [1, 255].
 */
export function resolveIdleTimeoutSeconds(requestTimeoutMs: number): number {
  const baseMs = requestTimeoutMs > 0 ? requestTimeoutMs : 30_000;
  return Math.min(255, Math.max(1, Math.ceil(baseMs / 1000) + 5));
}

export class HttpPlugin implements Plugin {
  /**
   * Aborted when this run of the server begins draining, before its in-flight
   * requests are waited on, so a negotiated SSE stream ends with no terminal
   * at once instead of at the forced close, and a WebSocket conversation,
   * hijacked from the server and out of `server.stop()`'s reach, sends its
   * shutdown terminal before any force-close. A stopped plugin that starts again
   * arms a new one: the aborted signal of the previous run must not end the
   * streams of the next one as they open. The Go twin is
   * `ServerPlugin.drainSignal()`.
   */
  private _drain = new AbortController();
  private router = new RouteController<HttpRequestContext>();
  private server: Server<WsDataType> | undefined;
  private options: ServerOptions;
  private middlewares: HttpMiddleware[] = [];
  private outermostMiddlewares: HttpMiddleware[] = [];
  private _routeEntries: RouteEntry[] = [];
  private _nativeRoutes: NativeRouteEntry[] = [];
  private _rewrites: RequestRewrite[] = [];

  constructor(options: ServerOptions = {}) {
    this.options = options;
  }

  use(middleware: HttpMiddleware): this {
    this.middlewares.push(middleware);
    return this;
  }

  prepend(middleware: HttpMiddleware): this {
    this.middlewares.unshift(middleware);
    return this;
  }

  /**
   * Register a middleware in front of every other one, the server's own
   * compression, security headers and origin guard included, so it sees every
   * request and every response, a refusal included. Telemetry uses it to count
   * the requests an inner middleware turns away. A later call runs inside an
   * earlier one.
   */
  useOutermost(middleware: HttpMiddleware): this {
    if (!this.outermostMiddlewares.includes(middleware)) {
      this.outermostMiddlewares.push(middleware);
    }
    return this;
  }

  /**
   * Register a request rewrite. Before route matching, the first rewrite, in
   * registration order, that returns a path a route serves replaces the
   * request's path; the query stays. Route matching, middleware and the
   * handler see the rewritten URL in `ctx.url` and `ctx.path()`; `ctx.req.url`
   * keeps the client's. Native routes are not rewritten.
   *
   * @example Serve a host's pages under a prefix
   * ```ts
   * http().rewrite((req, path) => (new URL(req.url).hostname === 'fr.example.com' ? `/fr${path}` : undefined));
   * ```
   */
  rewrite(rewrite: RequestRewrite): this {
    this._rewrites.push(rewrite);
    return this;
  }

  route(method: HttpMethod, path: string, handler: RouteHandler, options?: RouteOptions): this {
    this._routeEntries.push({ method, path, handler, options });
    this.router.route(method, path, handler, options);
    return this;
  }

  get(path: string, handler: RouteHandler, options?: RouteOptions): this {
    return this.route('GET', path, handler, options);
  }

  post(path: string, handler: RouteHandler, options?: RouteOptions): this {
    return this.route('POST', path, handler, options);
  }

  put(path: string, handler: RouteHandler, options?: RouteOptions): this {
    return this.route('PUT', path, handler, options);
  }

  delete(path: string, handler: RouteHandler, options?: RouteOptions): this {
    return this.route('DELETE', path, handler, options);
  }

  patch(path: string, handler: RouteHandler, options?: RouteOptions): this {
    return this.route('PATCH', path, handler, options);
  }

  /**
   * Register a **native** route that bypasses the framework pipeline entirely.
   *
   * Native routes are served by Bun's own router — no request context, no
   * middleware, no secure-by-default headers, no DI scope, no content
   * negotiation. A fixed `Response`/`HttpResponse` is served at bare-metal speed
   * (Bun answers it without invoking JS per request); a function gets the raw
   * `Request`. Use only for trusted, self-contained hot paths (health, version,
   * static payloads). Path params use the usual `[name]` syntax.
   *
   * @example Static health/version (bare-metal)
   * ```ts
   * http().native('/healthz', HttpResponse.json({ status: 'ok' }));
   * ```
   *
   * @example Bare handler for a single method
   * ```ts
   * http().native('GET', '/version', () => HttpResponse.json({ version }));
   * ```
   */
  native(path: string, responder: NativeResponder): this;
  native(method: HttpMethod, path: string, responder: NativeResponder): this;
  native(
    methodOrPath: HttpMethod | string,
    pathOrResponder: string | NativeResponder,
    responder?: NativeResponder,
  ): this {
    if (responder === undefined) {
      this._nativeRoutes.push({ path: methodOrPath, responder: pathOrResponder as NativeResponder });
    } else {
      this._nativeRoutes.push({ method: methodOrPath as HttpMethod, path: pathOrResponder as string, responder });
    }
    return this;
  }

  merge(other: HttpPlugin): this {
    this.router = this.router.merge(other.router);
    this.mergeMiddlewares(other.middlewares);
    for (const middleware of other.outermostMiddlewares) this.useOutermost(middleware);
    this._routeEntries.push(...other._routeEntries);
    this._nativeRoutes.push(...other._nativeRoutes);
    this._rewrites.push(...other._rewrites);
    return this;
  }

  /**
   * Merge another HttpPlugin's routes with a prefix applied to all paths.
   * Used by plugins (e.g. ReactPlugin) to apply module-level paths at warmup time.
   */
  mergeWithPrefix(other: HttpPlugin, prefix: string, prefixFn: (path: string, prefix: string) => string): this {
    for (const { method, path, handler, options } of other._routeEntries) {
      this.route(method, prefixFn(path, prefix), handler, options);
    }
    this.mergeMiddlewares(other.middlewares);
    for (const middleware of other.outermostMiddlewares) this.useOutermost(middleware);
    for (const { method, path, responder } of other._nativeRoutes) {
      this._nativeRoutes.push({ method, path: prefixFn(path, prefix), responder });
    }
    this._rewrites.push(...other._rewrites);
    return this;
  }

  /** The URL the first rewrite whose path a route serves gives the request, or `undefined`. */
  private rewrittenUrl(req: Request): string | undefined {
    const url = new URL(req.url);
    for (const rewrite of this._rewrites) {
      const path = rewrite(req, url.pathname);
      if (path === undefined || path === url.pathname || this.router.findMethods(path).length === 0) continue;
      url.pathname = path;
      return url.href;
    }
    return undefined;
  }

  /**
   * Merge middleware while keeping a single CSRF authority for the resulting
   * request pipeline. Server-level `csrf` options are resolved at startup and
   * take precedence over middleware contributed by merged plugins (notably the
   * secure-by-default React plugin).
   */
  private mergeMiddlewares(incoming: readonly HttpMiddleware[]): void {
    let hasCsrfMiddleware = Boolean(this.options.csrf) || this.middlewares.some(isCsrfMiddleware);
    for (const middleware of incoming) {
      if (isCsrfMiddleware(middleware)) {
        if (hasCsrfMiddleware) continue;
        hasCsrfMiddleware = true;
      }
      this.middlewares.push(middleware);
    }
  }

  /**
   * Look up a route handler for the given method and path.
   * Returns the handler, matched params, and route pattern — or undefined if no match.
   *
   * Used by GrpcPlugin for direct handler invocation (bypassing server.fetch()).
   */
  resolveHandler(
    method: HttpMethod,
    path: string,
    accept: string[] = ['*/*'],
  ): { handler: RouteHandler; params: Record<string, string>; route: string } | undefined {
    const mockContext = {
      method,
      path: () => path,
      headers: new Headers({ Accept: accept.join(', ') }),
    } as HttpRequestContext;

    const handlers = this.router.find(mockContext);
    if (handlers.length === 0) return undefined;

    const first = handlers[0];
    return { handler: first.handler, params: first.params ?? {}, route: first.route };
  }

  async start(app: Module): Promise<void> {
    const logger = useLogger('putnami');
    const startedAt = Date.now();

    const config = useConfig(PutnamiConfig, { confInit: this.options });
    // `port: 0` is passed straight to Bun's serve(), which binds an ephemeral
    // port atomically and reports it via server.port. Pre-resolving the port
    // here would open a TOCTOU window where a concurrent process binds it first.
    const port = this.options.port ?? process.env['PORT'] ?? config.port;
    const maxBodySizeBytes =
      typeof this.options.maxBodySizeBytes === 'number' ? this.options.maxBodySizeBytes : DEFAULT_MAX_BODY_SIZE_BYTES;
    const requestTimeoutMs = typeof this.options.requestTimeoutMs === 'number' ? this.options.requestTimeoutMs : 30_000;

    const httpDispatcher = new HttpDispatcher(this.router);
    // One timer for the deadlines of every request this run serves.
    const deadlines = requestTimeoutMs > 0 ? new RequestDeadlines(requestTimeoutMs) : undefined;
    if (this._drain.signal.aborted) this._drain = new AbortController();
    const drain = this._drain.signal;
    if (this.options.onError) {
      httpDispatcher.errorHandler = this.options.onError;
    }
    const wsDispatcher = new WebSocketDispatcher(this.router);

    // Capture ContainerContext for per-request DI scoping.
    // Uses getActiveContext() to avoid triggering the lazy context getter
    // when no DI registrations exist (which would create an unstarted context).
    const containerContext: ContainerContext | undefined =
      app instanceof Application ? app.getActiveContext() : undefined;

    // The full chain, and for each matched route the chain of only the
    // middlewares that act on it, composed on the route's first request.
    const routeChains = new RouteChains(this.buildMiddlewares(maxBodySizeBytes));

    // Secure-by-default opt-out must be explicit and loud.
    if (this.options.secure === false) {
      logger.warn(
        '⚠️  security middleware disabled (secure: false) — origin guard, security headers, and auto HEAD/OPTIONS are off',
      );
    }

    /**
     * Serves one request through the framework pipeline. `matched` is the
     * match Bun's native router already made; without it, the JS router
     * matches the request.
     */
    const serveRequest = (
      req: Request,
      server: Server<WsDataType>,
      matched?: HandlerResult<RouteHandler<HttpRequestContext>>[],
    ): Promise<Response> => {
      // A request with a deadline has a lifetime: its signal aborts when the
      // client disconnects (Bun's req.signal) and when the request outlives
      // requestTimeoutMs, so its queries, client calls and render stop
      // instead of running on after the 408. The signal is built the first
      // time something reads it: a request that never reads it pays for no
      // controller.
      const lifetime = deadlines !== undefined ? new ServedRequest(req) : undefined;
      const context = buildHttpContext<HttpRequestContextInternal>({
        req,
        url: this._rewrites.length > 0 ? this.rewrittenUrl(req) : undefined,
        // Bun.serve returns a Server type that is structurally compatible with HttpServer
        server: server as unknown as HttpServer,
        lifetime,
        maxBodySizeBytes,
      });
      context.__drain = drain;
      if (lifetime !== undefined) {
        // A request that reached its deadline before the sweeper reached it
        // ends now (__endRequest): its signal aborts, as it would at the timeout.
        context.deadlineAt = Date.now() + requestTimeoutMs;
      }

      // An error the chain throws becomes the request's answer; it runs in the
      // request context, so the logger has its traceId / logContext.
      const answerChainError = (e: unknown): HttpResponse | Promise<HttpResponse> => {
        // A request that ended (its timeout fired or its client left)
        // answers 408. Its stopped work unwinds quietly; any other
        // error is logged as it would be during the request.
        if (lifetime?.timedOut || requestEnded(context)) {
          const loggedLive = e instanceof HttpResponse || e instanceof HttpException;
          if (!loggedLive && !isRequestStop(e, context) && !context.__requestErrorLogged) {
            useLogger().error('Unhandled error', e);
          }
          return HttpResponse.json({ error: 'Request Timeout', statusCode: 408 }, { status: 408 });
        }
        if (e instanceof HttpResponse) {
          return e;
        }
        if (isDevHtmlRequest(context)) {
          return renderDevErrorPage(e, context);
        }
        if (e instanceof HttpException) {
          return HttpResponse.json(e.getResponse(), { status: e.getStatus() });
        }
        if (!context.__requestErrorLogged) {
          useLogger().error('Unhandled error', e);
        }
        return new HttpResponse('Internal Server Error', { status: 500 });
      };

      // The native response, once the request scope is closed: at once when
      // the request never opened one, as most do.
      const finishRequest = (res: HttpResponse | undefined): Response | Promise<Response> => {
        let response: Response;
        try {
          if (!res) {
            response = this.unmatchedResponse(context);
          } else {
            response = res.get();
            if (context.__rawHttpStreamResponse && response.body && !lifetime?.timedOut) {
              // The stream owns the scope from here, and closes it.
              return retainRawHttpStreamScope(response, context, () => closeRequestScope(context));
            }
            if (lifetime?.timedOut && response.body) {
              const answered = response;
              return response.body
                .cancel()
                .catch(() => {})
                .then(() => closeRequestScope(context))
                .then(() => answered);
            }
          }
        } catch (error) {
          return failRequest(error);
        }
        const closed = closeRequestScope(context);
        return closed === undefined ? response : closed.then(() => response);
      };

      // The request failed outside the chain's own answer: its scope closes, then the error goes on.
      const failRequest = (error: unknown): Promise<never> => {
        const closed = closeRequestScope(context);
        return closed === undefined ? Promise.reject(error) : closed.then(() => Promise.reject(error));
      };

      // No async frame of its own: the chain's promise carries the request, and
      // its answer becomes the response in the continuations below.
      const executeRequest = (): Promise<Response> => {
        let middlewareChain: MiddlewareChain;
        let dispatch: () => Promise<HttpResponse | undefined>;
        try {
          // Match route BEFORE running middlewares so ctx.route/params are available
          if (matched === undefined) httpDispatcher.matchRoute(context);
          else httpDispatcher.useMatch(context, matched);
          middlewareChain = routeChains.select(context);
          dispatch = () => httpDispatcher.handle(context);
          // Attach the DI context so a per-request scope is created lazily — only
          // if a handler or middleware actually resolves a dependency. Requests
          // that never touch DI pay nothing for scoping. useContainer() /
          // resolve() / .inject() materialise the scope on first access.
          if (containerContext) {
            attachRequestScope(context, containerContext);
          }
        } catch (error) {
          return Promise.reject(error);
        }

        let answered: Promise<HttpResponse | undefined>;
        try {
          answered = runInContext(context, () => {
            let chained: Promise<HttpResponse | undefined>;
            try {
              // A middleware that returns a plain value still answers, as `await` let it.
              chained = Promise.resolve(middlewareChain(context, dispatch));
            } catch (error) {
              chained = Promise.reject(error);
            }
            return chained.then(undefined, answerChainError);
          }) as Promise<HttpResponse | undefined>;
        } catch (error) {
          return failRequest(error);
        }
        return answered.then(finishRequest, failRequest);
      };

      if (lifetime === undefined || deadlines === undefined) {
        return executeRequest();
      }
      return lifetime.answer(deadlines, context, executeRequest);
    };

    // Native routes are served by Bun's own router, ahead of (and bypassing)
    // the JS pipeline below. Anything not matched here falls through to fetch().
    // Endpoints Bun can match alone join them, through the pipeline.
    const nativeRoutes = this.buildNativeRoutes(serveRequest);

    this.server = serve<WsDataType>({
      port,
      hostname: '0.0.0.0',
      // Native routes bypass the framework pipeline entirely (no context,
      // middleware, security, DI, or negotiation). The cast bridges our dynamic
      // builder to Bun's path-literal-generic `routes` type.
      ...(nativeRoutes ? { routes: nativeRoutes as never } : {}),
      // Explicit idle timeout bounds stalled/slow-loris sockets (Bun's default
      // is implicit). Derived from requestTimeoutMs so the 408 path fires first.
      idleTimeout: resolveIdleTimeoutSeconds(requestTimeoutMs),
      // HTTP/2 in cleartext for a client that opens with the HTTP/2 preface
      // (prior knowledge), beside HTTP/1.1 and WebSocket upgrades on the same
      // port. Its request body is read as it arrives, which is what carries a
      // Connect client or bidirectional stream.
      http2: this.options.http2 !== false,
      fetch: (req, server) => serveRequest(req, server),
      websocket: {
        // Bound client-controlled memory: reject oversized inbound frames and
        // close connections that build up unbounded outbound back-pressure (DoS).
        maxPayloadLength: this.options.webSocket?.maxPayloadLength ?? DEFAULT_WS_MAX_PAYLOAD_BYTES,
        backpressureLimit: this.options.webSocket?.backpressureLimit ?? DEFAULT_WS_BACKPRESSURE_BYTES,
        closeOnBackpressureLimit: true,
        // Bun's websocket handler types use a narrower WebSocket than ServerWebSocket<WsDataType>;
        // the runtime objects are identical — cast bridges the bun.serve / dispatcher type gap.
        message: async (ws, message) => wsDispatcher.message(ws as unknown as ServerWebSocket<WsDataType>, message),
        open: async (ws) => wsDispatcher.open(ws as unknown as ServerWebSocket<WsDataType>),
        close: async (ws, code, reason) =>
          wsDispatcher.close(ws as unknown as ServerWebSocket<WsDataType>, code, reason),
      },
    });

    logger.info(
      `⚡️ listening http://localhost:${this.server.port}`,
      listeningRecord(this.server.port, Date.now() - startedAt),
    );

    app.onStop(async () => {
      await this.drainServer();
    });
  }

  /** The answer to a request no handler answered: 404, or 405 with `Allow` when another method serves the path. */
  private unmatchedResponse(context: HttpRequestContextInternal): Response {
    if ((context.__matchedHandlers?.length || 0) > 0) {
      return createJsonErrorResponse(404, 'Not Found');
    }
    const requestMethod = context.req.method.toUpperCase();
    if (requestMethod === 'HEAD' || requestMethod === 'OPTIONS' || requestMethod === 'TRACE') {
      return createJsonErrorResponse(404, 'Not Found');
    }
    const methods = this.router.findMethods(context.path());
    if (methods.length > 0) {
      return createJsonErrorResponse(405, 'Method Not Allowed', { Allow: formatAllowHeader(methods) });
    }
    return createJsonErrorResponse(404, 'Not Found');
  }

  /**
   * Gracefully drains the HTTP server during shutdown:
   * 1. Stop accepting new connections and let in-flight requests finish
   *    (`server.stop(false)`), bounded by {@link DRAIN_TIMEOUT_MS}.
   * 2. Force-close any stragglers still active past the deadline
   *    (`server.stop(true)`).
   *
   * Owns server teardown in one place and is idempotent, so it is safe to call
   * from both the `onStop` hook and {@link stop}. Skipping this step aborts
   * in-flight requests and surfaces as user-visible 502s.
   */
  private async drainServer(): Promise<void> {
    const server = this.server;
    if (!server) return;
    this.server = undefined;
    // Told first, so every negotiated stream and WebSocket conversation this
    // run serves ends now — the in-flight wait below would otherwise hold each
    // one to the deadline.
    this._drain.abort();

    let timer: ReturnType<typeof setTimeout> | undefined;
    const deadline = new Promise<void>((resolve) => {
      timer = setTimeout(resolve, DRAIN_TIMEOUT_MS);
      (timer as unknown as { unref?: () => void }).unref?.();
    });

    try {
      await Promise.race([Promise.resolve(server.stop(false)), deadline]);
    } finally {
      if (timer) clearTimeout(timer);
      // Force-close anything still active past the drain deadline.
      await server.stop(true);
    }
  }

  /** The server's middlewares, outermost first. */
  private buildMiddlewares(maxBodySizeBytes?: number): HttpMiddleware[] {
    const middlewares: HttpMiddleware[] = [];
    // Keep one explicit CSRF middleware at most. When server-level CSRF is
    // configured, it is materialised below with the authoritative options, so
    // discard contributed instances rather than silently losing those options.
    let hasCsrfMiddleware = Boolean(this.options.csrf);
    for (const middleware of this.middlewares) {
      if (isCsrfMiddleware(middleware)) {
        if (hasCsrfMiddleware) continue;
        hasCsrfMiddleware = true;
      }
      middlewares.push(middleware);
    }

    // Secure-by-default master switch. `secure: false` flips the always-on
    // security middleware (origin guard, security headers, auto HEAD/OPTIONS) off
    // by default; an explicit per-option setting still takes precedence.
    const secureByDefault = this.options.secure !== false;

    // Auto-enable origin guard (CSRF protection via Origin / Sec-Fetch-Site)
    const originGuardEnabled =
      this.options.originGuard === undefined ? secureByDefault : this.options.originGuard !== false;
    if (originGuardEnabled) {
      const guardOpts = typeof this.options.originGuard === 'object' ? this.options.originGuard : {};
      middlewares.unshift(OriginGuardMiddleware(guardOpts));
    }

    // Auto-enable secure-by-default response headers (nosniff, X-Frame-Options,
    // CSP, etc.). Placed in front of the origin guard so the headers are applied
    // even to rejection responses; sits inside compression so they are set
    // before the body is encoded.
    const securityHeadersEnabled =
      this.options.securityHeaders === undefined ? secureByDefault : this.options.securityHeaders !== false;
    if (securityHeadersEnabled) {
      const securityOpts = typeof this.options.securityHeaders === 'object' ? this.options.securityHeaders : {};
      middlewares.unshift(SecurityHeadersMiddleware(securityOpts));
    }

    // Auto-enable compression when opted in (outermost middleware so it compresses the final response)
    if (this.options.compression) {
      const compressionOpts = typeof this.options.compression === 'object' ? this.options.compression : {};
      middlewares.unshift(CompressionMiddleware(compressionOpts));
    }

    // An asynchronous session store (database) is read before every other
    // middleware and written after the response is built: the session API
    // that handlers and middlewares call is synchronous. It sits inside the
    // outermost slot, so the request's span covers the read and the write.
    if (sessionStoreMayBeAsync()) middlewares.unshift(requestSessionMiddleware);

    // Outermost slot (telemetry): in front of compression, so a request any
    // other middleware refuses is still observed.
    middlewares.unshift(...this.outermostMiddlewares);

    // Auto-enable CSRF token protection (double-submit cookie) when opted in
    if (this.options.csrf) {
      const csrfOpts = typeof this.options.csrf === 'object' ? this.options.csrf : {};
      // Thread the server's body-size limit into the CSRF form-token extraction
      // so it enforces the same bound as ctx.body(); an explicit CsrfOptions
      // value still wins.
      middlewares.push(CsrfMiddleware({ maxBodySizeBytes, ...csrfOpts }));
    }

    // Auto-handle HEAD / OPTIONS / TRACE (part of the secure defaults — skipped
    // under `secure: false` unless `httpMethods` is explicitly configured).
    const httpMethodsConfig = this.resolveHttpMethodsConfig();
    if (httpMethodsConfig.head || httpMethodsConfig.options || httpMethodsConfig.trace) {
      middlewares.push(HttpMethodsMiddleware(this.router, httpMethodsConfig));
    }

    return middlewares;
  }

  /**
   * Assemble the Bun `routes` map, or `undefined` when it would be empty.
   *
   * Registered native routes come first: static responders are passed
   * through as raw `Response`s (Bun's bare-metal static path), functions are
   * wrapped to coerce `HttpResponse` results, per-method routes become a
   * method map.
   *
   * Then each endpoint Bun can match alone ({@link nativeEndpoints}) joins
   * the map under its method, unless a native route holds its path: Bun
   * matches it and hands the match to `serveRequest`, which runs the whole
   * pipeline without the JS router. A request that could be answered
   * differently goes back to `serveRequest` unmatched: a HEAD Bun serves
   * from the GET route, a path Bun reads another way (an encoded param, a
   * leading `//`), an Accept the endpoint refuses. A server with rewrites
   * maps no endpoint: a rewrite may route the request elsewhere.
   */
  private buildNativeRoutes(
    serveRequest: (
      req: Request,
      server: Server<WsDataType>,
      matched?: HandlerResult<RouteHandler<HttpRequestContext>>[],
    ) => Promise<Response>,
  ): Record<string, BunNativeRoute> | undefined {
    const routes: Record<string, BunNativeRoute> = {};
    let empty = true;
    for (const { method, path, responder } of this._nativeRoutes) {
      const bunPath = toBunPath(path);
      const value = toBunNativeValue(responder);
      empty = false;
      if (method) {
        let byMethod = routes[bunPath] as Record<string, typeof value> | undefined;
        if (byMethod === undefined) {
          byMethod = {};
          routes[bunPath] = byMethod;
        }
        byMethod[method] = value;
      } else {
        routes[bunPath] = value;
      }
    }

    // A native route runs ahead of the pipeline: an endpoint whose pattern any
    // native route may share a path with stays on the fetch path.
    const nativePaths = this._nativeRoutes.map(({ path }) => path);
    const endpoints = this._rewrites.length === 0 ? nativeEndpoints(this._routeEntries) : [];
    for (const endpoint of endpoints) {
      if (nativePaths.some((path) => patternsMayOverlap(endpoint.route, path))) continue;
      let byMethod = routes[endpoint.bunPath] as Record<string, BunNativeHandler> | undefined;
      if (byMethod === undefined) {
        byMethod = {};
        routes[endpoint.bunPath] = byMethod;
      }
      byMethod[endpoint.method] = nativeEndpointHandler(endpoint, serveRequest);
      empty = false;
    }
    return empty ? undefined : routes;
  }

  async stop(): Promise<void> {
    await this.drainServer();
  }

  /**
   * The address this server's listener bound, from `start()` until it stops.
   * A listener that is not addressable reports none. Go twin:
   * `ServerPlugin.ReadyEndpoints`.
   */
  readyEndpoints(): ReadyEndpoint[] {
    const endpoint = boundHttpEndpoint(this.server?.port);
    return endpoint ? [endpoint] : [];
  }

  getServer(): Server<WsDataType> | undefined {
    return this.server;
  }

  /** Whether this server derives HEAD responses from framework GET routes. */
  servesImplicitHead(): boolean {
    return this.resolveHttpMethodsConfig().head;
  }

  async generate(app: Module): Promise<GenerateResult> {
    const logger = useLogger('@putnami/application');
    const debug = app.buildOptions?.debug;
    const log = debug ? (msg: string) => logger.debug(`HttpPlugin.generate: ${msg}`) : undefined;

    const { generateServeFiles } = await import('./http-serve-gen');
    const result = generateServeFiles(log);
    const httpRoutes = this.buildGeneratedHttpRoutes();
    return { ...(result.exports ? { exports: result.exports } : {}), httpRoutes };
  }

  private buildGeneratedHttpRoutes(): GeneratedHttpRoute[] {
    type RouteGroup = Omit<GeneratedHttpRoute, 'methods'> & { methods: Set<string> };
    const grouped = new Map<string, RouteGroup>();
    const add = (path: string, methods: readonly string[]) => {
      const normalized = normalizeGeneratedHttpRoutePath(path);
      const key = `${normalized.match}\0${normalized.path}`;
      let group = grouped.get(key);
      if (!group) {
        group = {
          ...normalized,
          methods: new Set<string>(),
          publicEdge: true,
          provenance: { package: '@putnami/application', sourceKind: 'manual' },
        };
        grouped.set(key, group);
      }
      for (const method of methods) group.methods.add(method);
    };

    const implicitHead = this.servesImplicitHead();
    for (const route of this._routeEntries) {
      add(route.path, route.method === 'GET' && implicitHead ? ['GET', 'HEAD'] : [route.method]);
    }
    for (const route of this._nativeRoutes) {
      add(route.path, route.method ? [route.method] : METHODLESS_NATIVE_HTTP_METHODS);
    }

    return [...grouped.values()].map((route) => ({ ...route, methods: [...route.methods].sort() }));
  }

  private resolveHttpMethodsConfig(): { head: boolean; options: boolean; trace: boolean } {
    const secureByDefault = this.options.secure !== false;
    return this.options.httpMethods === undefined && !secureByDefault
      ? { head: false, options: false, trace: false }
      : resolveHttpMethodsOptions(this.options.httpMethods);
  }
}

export function http(options: ServerOptions = {}): HttpPlugin {
  return new HttpPlugin(options);
}

function formatAllowHeader(methods: string[]): string {
  const allow = new Set(methods);
  if (allow.has('GET')) {
    allow.add('HEAD');
  }
  allow.add('OPTIONS');
  return [...allow].join(', ');
}

/**
 * The lifetime of one request served with a deadline, in one object: the
 * signal it builds on first read, its stop, its entry in the shared deadline
 * queue and the answer that the response or the 408 settles first.
 */
class ServedRequest implements RequestLifetime, RequestDeadline {
  at = 0;
  prev: RequestDeadline | undefined = undefined;
  next: RequestDeadline | undefined = undefined;
  queued = false;
  /** Whether the deadline answered the request with a 408. */
  timedOut = false;
  private stopReason: DOMException | undefined = undefined;
  private controller: LinkedAbortController | undefined = undefined;
  private settle: ((response: Response) => void) | undefined = undefined;
  /** The abort listeners, and whether the request follows its client for them. */
  private listeners: Set<() => void> | undefined = undefined;

  constructor(private readonly req: Request) {}

  /** Whether the request ended: it stopped, or its client left. */
  aborted(): boolean {
    return this.stopReason !== undefined || this.req.signal.aborted;
  }

  /** Calls `listener` once, when the request stops or its client leaves. */
  onAbort(listener: () => void): void {
    let listeners = this.listeners;
    if (listeners === undefined) {
      listeners = new Set();
      this.listeners = listeners;
      // One listener on the client's signal for every query of the request.
      this.req.signal.addEventListener('abort', this, { once: true });
    }
    listeners.add(listener);
  }

  offAbort(listener: () => void): void {
    this.listeners?.delete(listener);
  }

  /** The client left. */
  handleEvent(): void {
    this.abortListeners();
  }

  private abortListeners(): void {
    const listeners = this.listeners;
    if (listeners === undefined || listeners.size === 0) return;
    const called = [...listeners];
    listeners.clear();
    for (const listener of called) {
      try {
        listener();
      } catch (error) {
        useLogger().error('Request abort listener failed', error);
      }
    }
  }

  /**
   * One controller aborts with the first of the two reasons: the request
   * stopped, or its client left. Every context of the request (a derived one
   * reads it again) shares it, so the timeout aborts them all.
   */
  signal(): AbortSignal {
    if (this.controller !== undefined) return this.controller.signal;
    const controller = new LinkedAbortController(this.req.signal);
    this.controller = controller;
    if (this.stopReason !== undefined) {
      controller.abort(this.stopReason);
    } else {
      controller.follow();
    }
    return controller.signal;
  }

  stop(): void {
    if (this.stopReason !== undefined) return;
    this.stopReason = new DOMException('Request Timeout', 'TimeoutError');
    this.controller?.abort(this.stopReason);
    this.abortListeners();
  }

  /** The deadline came first: the request stops and answers 408. */
  expire(): void {
    this.timedOut = true;
    this.stop();
    this.settle?.(createJsonErrorResponse(408, 'Request Timeout'));
  }

  /** One promise answers the request: its response, or the 408 the shared deadline sweeper sends first. */
  answer(
    deadlines: RequestDeadlines,
    context: HttpRequestContextInternal,
    run: () => Promise<Response>,
  ): Promise<Response> {
    return new Promise<Response>((resolve, reject) => {
      this.settle = resolve;
      deadlines.enqueue(this);
      run().then(
        (response) => {
          deadlines.remove(this);
          // The deadline bounded the request until its response. A stream or a
          // background task that runs on after it is no longer held to it. A
          // request that never built its signal does not read req.signal here:
          // requestEnded() reads the signal, which still sees the client leave.
          if (!this.timedOut && this.stopReason === undefined && !this.controller?.signal.aborted) {
            context.deadlineAt = undefined;
          }
          resolve(response);
        },
        (error: unknown) => {
          deadlines.remove(this);
          reject(error);
        },
      );
    });
  }
}

function createJsonErrorResponse(status: number, error: string, headers?: HeadersInit): Response {
  return new Response(JSON.stringify({ error, statusCode: status }), {
    status,
    headers: { 'Content-Type': 'application/json', ...(headers || {}) },
  });
}

const NATIVE_PARAM_RE = /\[([^\]]+)\]/g;

/** Convert a Putnami route path (`/users/[id]`) to Bun's native router syntax (`/users/:id`). */
function toBunPath(path: string): string {
  let p = path.trim();
  if (p === '') {
    p = '/';
  } else if (p[0] !== '/') {
    p = `/${p}`;
  }
  return p.replace(NATIVE_PARAM_RE, ':$1');
}

/** Coerce a native responder into a Bun route value: a raw static `Response`, or a handler wrapper. */
function toBunNativeValue(responder: NativeResponder): Response | ((req: Request) => Response | Promise<Response>) {
  if (responder instanceof Response) {
    return responder;
  }
  if (responder instanceof HttpResponse) {
    // Materialise once; copy() so a responder reused across routes isn't consumed.
    return responder.copy().get();
  }
  return (req: Request) => {
    const result = responder(req);
    return result instanceof Promise ? result.then(toNativeResponse) : toNativeResponse(result);
  };
}

function toNativeResponse(result: Response | HttpResponse): Response {
  return result instanceof HttpResponse ? result.get() : result;
}

const CODE_SLASH = 47;

/**
 * The Bun route handler of an endpoint: it hands Bun's match to the
 * pipeline, or the request alone when the JS router could answer it
 * differently (see `buildNativeRoutes`).
 */
function nativeEndpointHandler(
  endpoint: NativeEndpoint,
  serveRequest: (
    req: Request,
    server: Server<WsDataType>,
    matched?: HandlerResult<RouteHandler<HttpRequestContext>>[],
  ) => Promise<Response>,
): BunNativeHandler {
  const { method, match, slashes, hasParams } = endpoint;
  return (req, server) => {
    // Bun serves HEAD from the GET route: the pipeline derives HEAD itself.
    if (req.method !== method) return serveRequest(req, server);
    const url = req.url;
    const pathStart = url.indexOf('/', url.indexOf('//') + 2);
    // Bun folds a leading `//`; the JS router reads it as written.
    if (url.charCodeAt(pathStart + 1) === CODE_SLASH) return serveRequest(req, server);
    let params: Record<string, string> | undefined;
    if (hasParams) {
      // Bun decodes params; the JS router hands them as written.
      const query = url.indexOf('?', pathStart);
      const percent = url.indexOf('%', pathStart);
      if (percent !== -1 && (query === -1 || percent < query)) return serveRequest(req, server);
      params = (req as Request & { params: Record<string, string> }).params;
    }
    const accepted = acceptScore(match.accept, requestAcceptOf(req.headers.get('Accept')));
    if (accepted === 0) return serveRequest(req, server);
    return serveRequest(req, server, [
      {
        route: match.route,
        handler: match.handler,
        accept: match.accept,
        statusCode: match.statusCode,
        params,
        score: slashes * 10 * accepted,
        headMeta: match.headMeta,
        csrfExempt: match.csrfExempt,
        minimal: match.minimal,
        securityHeaders: match.securityHeaders,
        firstPartyErrors: match.firstPartyErrors,
      },
    ]);
  };
}
