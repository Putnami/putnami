import type { ServerWebSocket } from 'bun';
import { type Server, serve } from 'bun';
import { type ContainerContext, HttpException, runInContext, useConfig, useLogger } from '@putnami/runtime';
import { READY_LOG_KEY, type ReadyData, readyMarker } from '@putnami/runtime/jobs';
import type { WsDataType } from '../api/ws-context.type';
import { WebSocketDispatcher } from '../api/ws-dispatcher';
import { Application, type GenerateResult, type Module, type Plugin } from '../application';
import { PutnamiConfig } from '../application/putnami.config';
import { CompressionMiddleware } from './compression.middleware';
import { CsrfMiddleware, isCsrfMiddleware } from './csrf.middleware';
import { isDevHtmlRequest, renderDevErrorPage } from './dev-error-page';
import {
  attachRequestScope,
  buildHttpContext,
  closeRequestScope,
  DEFAULT_MAX_BODY_SIZE_BYTES,
} from './http-context.builder';
import type { HttpRequestContext, HttpRequestContextInternal, Server as HttpServer } from './http-context.type';
import { HttpDispatcher } from './http-dispatcher';
import type { HttpMethod } from './http-method.type';
import { HttpMethodsMiddleware, resolveHttpMethodsOptions } from './http-methods.middleware';
import type { HttpMiddleware } from './http-middleware.type';
import { HttpResponse } from './http-response';
import { retainRawHttpStreamScope } from './raw-http-stream';
import { OriginGuardMiddleware } from './origin-guard.middleware';
import { RouteController } from './route.controller';
import type { NativeResponder, RouteHandler, RouteOptions } from './route.type';
import { SecurityHeadersMiddleware } from './security-headers.middleware';
import type { ServerOptions } from './server.config';
import { normalizeGeneratedHttpRoutePath, type GeneratedHttpRoute } from '../http-routes/generation';

type RouteEntry = { method: HttpMethod; path: string; handler: RouteHandler; options?: RouteOptions };
type NativeRouteEntry = { method?: HttpMethod; path: string; responder: NativeResponder };

/** A Bun native route value: a static response, a bare handler, or a per-method map of either. */
type BunNativeRoute =
  | Response
  | ((req: Request) => Response | Promise<Response>)
  | Record<string, Response | ((req: Request) => Response | Promise<Response>)>;

/** Module-level sentinel for the request-timeout race (identity-compared; avoids a per-request Symbol). */
const TIMEOUT_SENTINEL = Symbol('request-timeout');

/**
 * Builds the machine-readable readiness payload attached to the listening log
 * record under the reserved key {@link READY_LOG_KEY}.
 *
 * A served app's stdout is a LOG stream — the `@putnami/typescript` extension
 * re-emits each line as a log event — so this is how the framework reaches the
 * runtime event stream it does not own: the extension's forwarder recognizes
 * the key and emits the typed `ready` event
 * (`protocols/runtime/ready_marker.go`). The human message is untouched, so the
 * marker is the ONLY readiness path since B6b deleted the log probe.
 *
 * The port comes from the BOUND server (`server.port`), not from the requested
 * one, so `PORT=0` still announces an address a client can reach. A port that
 * is absent (a unix-socket listener) or outside `[1,65535]` degrades to a
 * workload claim — startup did finish — rather than a server claim nobody can
 * act on, which the protocol rejects outright.
 */
function buildReadyMarker(port: number | undefined, durationMs: number): ReadyData {
  if (port === undefined || !Number.isInteger(port) || port < 1 || port > 65_535) {
    return readyMarker({ target: 'workload', durationMs });
  }
  return readyMarker({
    target: 'server',
    // "localhost" matches the address the log message prints and the one a
    // developer can actually open; Bun binds every interface.
    endpoints: [{ scheme: 'http', host: 'localhost', port }],
    durationMs,
  });
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
  private _routeEntries: RouteEntry[] = [];
  private _nativeRoutes: NativeRouteEntry[] = [];

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
    this._routeEntries.push(...other._routeEntries);
    this._nativeRoutes.push(...other._nativeRoutes);
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
    for (const { method, path, responder } of other._nativeRoutes) {
      this._nativeRoutes.push({ method, path: prefixFn(path, prefix), responder });
    }
    return this;
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

    const middlewareChain = this.buildMiddlewareChain(maxBodySizeBytes);

    // Secure-by-default opt-out must be explicit and loud.
    if (this.options.secure === false) {
      logger.warn(
        '⚠️  security middleware disabled (secure: false) — origin guard, security headers, and auto HEAD/OPTIONS are off',
      );
    }

    // Native routes are served by Bun's own router, ahead of (and bypassing)
    // the JS pipeline below. Anything not matched here falls through to fetch().
    const nativeRoutes = this.buildNativeRoutes();

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
      fetch: async (req, server) => {
        let requestTimedOut = false;
        // The request signal is Bun's native req.signal, which aborts on client
        // disconnect. We deliberately do NOT wire an extra AbortController for the
        // hard request timeout: on timeout we return 408 and let the in-flight
        // handler unwind on its own (client-disconnect cancellation still applies).
        // This avoids a per-request AbortController + abort listener on the hot path.
        const requestSignal = req.signal;

        const context = buildHttpContext<HttpRequestContextInternal>({
          req,
          // Bun.serve returns a Server type that is structurally compatible with HttpServer
          server: server as unknown as HttpServer,
          signal: requestSignal,
          maxBodySizeBytes,
        });
        context.__drain = drain;

        const executeRequest = async (): Promise<Response> => {
          let scopeTransferred = false;
          // Match route BEFORE running middlewares so ctx.route/params are available
          httpDispatcher.matchRoute(context);

          const dispatch = async () => httpDispatcher.handle(context);

          // Attach the DI context so a per-request scope is created lazily — only
          // if a handler or middleware actually resolves a dependency. Requests
          // that never touch DI pay nothing for scoping. useContainer() /
          // resolve() / .inject() materialise the scope on first access.
          if (containerContext) {
            attachRequestScope(context, containerContext);
          }

          try {
            // Error handling is inside runInContext so the logger
            // always has access to traceId / logContext.
            const res = await runInContext(context, async () => {
              try {
                return await middlewareChain(context, dispatch);
              } catch (e) {
                if (requestTimedOut) {
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
              }
            });
            if (res) {
              const response = res.get();
              if (context.__rawHttpStreamResponse && response.body && !requestTimedOut) {
                const streamed = retainRawHttpStreamScope(response, context, () => closeRequestScope(context));
                scopeTransferred = true;
                return streamed;
              }
              if (requestTimedOut) await response.body?.cancel().catch(() => {});
              return response;
            }
            if ((context.__matchedHandlers?.length || 0) > 0) {
              return createJsonErrorResponse(404, 'Not Found');
            }
            const requestMethod = context.req.method.toUpperCase();
            if (requestMethod === 'HEAD' || requestMethod === 'OPTIONS' || requestMethod === 'TRACE') {
              return createJsonErrorResponse(404, 'Not Found');
            }

            const methods = this.router.findMethods(context.path());
            if (methods.length > 0) {
              const allow = formatAllowHeader(methods);
              return createJsonErrorResponse(405, 'Method Not Allowed', { Allow: allow });
            }
            return createJsonErrorResponse(404, 'Not Found');
          } finally {
            if (!scopeTransferred) await closeRequestScope(context);
          }
        };

        if (requestTimeoutMs <= 0) {
          return executeRequest();
        }

        let timeoutHandle: ReturnType<typeof setTimeout> | undefined;
        const timeoutPromise = new Promise<typeof TIMEOUT_SENTINEL>((resolve) => {
          timeoutHandle = setTimeout(() => {
            requestTimedOut = true;
            resolve(TIMEOUT_SENTINEL);
          }, requestTimeoutMs);
        });

        const result = await Promise.race([executeRequest(), timeoutPromise]);
        if (timeoutHandle) {
          clearTimeout(timeoutHandle);
        }
        if (result === TIMEOUT_SENTINEL) {
          return createJsonErrorResponse(408, 'Request Timeout');
        }
        return result;
      },
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

    const durationMs = Date.now() - startedAt;
    logger.info(`⚡️ listening http://localhost:${this.server.port}`, {
      durationMs,
      [READY_LOG_KEY]: buildReadyMarker(this.server.port, durationMs),
    });

    app.onStop(async () => {
      await this.drainServer();
    });
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

  private buildMiddlewareChain(maxBodySizeBytes?: number) {
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

    // Pre-compose middleware chain once at startup.
    type MiddlewareChainFn = (
      ctx: HttpRequestContext,
      dispatch: () => Promise<HttpResponse | undefined>,
    ) => Promise<HttpResponse | undefined>;

    // Empty-chain bypass: with no middleware the chain is just the dispatch call,
    // so skip the wrapper layer entirely (relevant when the security defaults are
    // turned off, e.g. an unsecured fast lane).
    if (middlewares.length === 0) {
      return (_ctx: HttpRequestContext, dispatch: () => Promise<HttpResponse | undefined>) => dispatch();
    }

    // Compose from the inside out. The innermost middleware receives `dispatch`
    // itself as `next`, so we avoid allocating a redundant terminal `() =>
    // dispatch()` closure on every request.
    let chain: MiddlewareChainFn = (ctx, dispatch) => middlewares[middlewares.length - 1](ctx, dispatch);
    for (let i = middlewares.length - 2; i >= 0; i--) {
      const mw = middlewares[i];
      const next = chain;
      chain = (ctx, dispatch) => mw(ctx, () => next(ctx, dispatch));
    }
    return chain;
  }

  /**
   * Assemble the Bun `routes` map from the registered native routes, or
   * `undefined` when there are none. Static responders are passed through as
   * raw `Response`s (Bun's bare-metal static path); functions are wrapped to
   * coerce `HttpResponse` results; per-method routes become a method map.
   */
  private buildNativeRoutes(): Record<string, BunNativeRoute> | undefined {
    if (this._nativeRoutes.length === 0) {
      return undefined;
    }
    const routes: Record<string, BunNativeRoute> = {};
    for (const { method, path, responder } of this._nativeRoutes) {
      const bunPath = toBunPath(path);
      const value = toBunNativeValue(responder);
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
    return routes;
  }

  async stop(): Promise<void> {
    await this.drainServer();
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
