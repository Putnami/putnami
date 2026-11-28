import { BadRequestException, type ContainerContext, type DetachedScope, type ScopeContext } from '@putnami/runtime';
import { SCOPE_CONTAINER_KEY } from '@putnami/runtime/inject';
import { parseQueryString } from '@putnami/utils';
import { HttpAbortException } from './http.exception';
import type { HttpRequestContext, Server } from './http-context.type';
import type { HttpMethod } from './http-method.type';
import type { HttpResponse } from './http-response';
import type { RouteHandler } from './route.type';
import type { HandlerResult } from './router';
import { UrlScanner } from './url.scanner';

/** The lifetime of a served request that has a deadline: its signal and its end. */
/**
 * The key a request context holds its {@link RequestAbortHooks} under.
 * `@putnami/database` reads the same key (`Symbol.for`, so two copies of the
 * framework agree) to cancel a query without building the request's signal.
 */
export const REQUEST_ABORT_HOOKS = Symbol.for('putnami.request.abortHooks');

/**
 * What a request's signal tells, without the signal: whether the request
 * ended, and a listener called once when it ends. A request served with a
 * deadline ends on its timeout or when its client leaves, as its signal does.
 */
export interface RequestAbortHooks {
  aborted(): boolean;
  onAbort(listener: () => void): void;
  offAbort(listener: () => void): void;
}

export interface RequestLifetime extends RequestAbortHooks {
  /** Builds the request's signal; called once, on its first read. */
  signal(): AbortSignal;
  /** Ends the request now: its signal aborts with a `TimeoutError`. */
  stop(): void;
}

type BuildContextArgs = {
  req: Request;
  /** The URL the request is routed and handled at, when a rewrite replaced the client's. */
  url?: string;
  server?: Server;
  /**
   * The request's signal, or a function that builds it the first time the
   * context's `signal` is read. Without one, the context reads `req.signal`.
   */
  signal?: AbortSignal | (() => AbortSignal);
  /**
   * The lifetime of a request with a deadline: it builds the signal on its
   * first read and gives the context its `__endRequest`.
   */
  lifetime?: RequestLifetime;
  maxBodySizeBytes?: number;
};

/**
 * The per-request HTTP context, implemented as a class so every request shares
 * one hidden class (monomorphic field access). The request accessors
 * (`queryParams`, `body`, `path`, `query`, `host`, `domain`, `secured`) are
 * prototype getters that build a bound closure on first read and keep it in a
 * pre-declared slot: a request pays only for the accessors it reads, and an
 * assignment (the validation wrapper) replaces one in place. Mutable plumbing
 * slots are declared and pre-initialised so later assignment by the
 * dispatcher/middleware never triggers a hidden-class transition.
 */
class HttpContext {
  readonly req: Request;
  readonly headers: Headers;
  readonly method: HttpMethod;
  readonly url: string;
  readonly server?: Server;

  route?: string;
  params?: Record<string, string>;
  statusCode?: number;
  user?: Record<string, unknown>;

  __requestError?: unknown;
  __requestErrorLogged?: boolean;
  __callbacks?: ((r: HttpResponse) => HttpResponse)[];
  __matchedHandlers?: HandlerResult<RouteHandler<HttpRequestContext>>[];
  /** The logger's route path and start time on a compiled route; see `LoggerMiddleware`. */
  __accessRoute?: string;
  __accessStart?: number;

  // The accessors (`path`, `query`, `body`...) are bound closures, so they keep
  // working when extracted, destructured (the logger middleware) or copied into
  // a sub-context (WebSocket / SSE / stream / gRPC), all of which call them
  // without the original `this`. Each is built on its first read, from a
  // prototype getter, and held in its slot below: a request pays only for the
  // accessors it reads. Assigning one (the validation wrapper) replaces it.
  private _queryParams?: () => Record<string, string>;
  private _body?: <T>() => Promise<T | undefined>;
  private _secured?: () => boolean;
  private _host?: () => string;
  private _domain?: () => string;
  private _path?: () => string;
  private _query?: () => string;
  private readonly _maxBodySizeBytes?: number;

  private _signal?: AbortSignal;
  private _signalSource?: () => AbortSignal;
  private readonly _lifetime?: RequestLifetime;
  /** The request's abort hooks, an own property so a context copy keeps them. */
  [REQUEST_ABORT_HOOKS]: RequestAbortHooks | undefined;
  private _endRequest?: () => void;
  /** The Authorization header once read, `null` before its first read. */
  private _authorization?: string | null;
  /** The request's log context once built; see {@link logContext}. */
  private _logContext?: Record<string, unknown>;
  /** The route path of the deferred `http` log context, set by {@link deferHttpLogContext}. */
  private _logRoute?: string;
  /** The method of the deferred `http` log context, as the request arrived. */
  private _logMethod?: string;
  private readonly _scanner: UrlScanner;
  private _cachedQuery?: Record<string, string>;
  private _scopeContainerContext?: ContainerContext;
  private _scope?: DetachedScope;

  constructor(args: BuildContextArgs) {
    const req = args.req;
    this.req = req;
    this._lifetime = args.lifetime;
    this[REQUEST_ABORT_HOOKS] = args.lifetime;
    this._endRequest = undefined;
    if (typeof args.signal === 'function') {
      this._signal = undefined;
      this._signalSource = args.signal;
    } else if (args.signal !== undefined || args.lifetime === undefined) {
      this._signal = args.signal ?? req.signal;
      this._signalSource = undefined;
    } else {
      this._signal = undefined;
      this._signalSource = undefined;
    }
    // Each read of a native request field builds its value again: read each once.
    const headers = req.headers;
    this.headers = headers;
    this.method = req.method as HttpMethod;
    this.url = args.url ?? req.url;
    this.server = args.server;
    this._scanner = new UrlScanner(this.url, headers);
    this._authorization = null;

    // Pre-initialise the mutable plumbing so assigning these later keeps the shape stable.
    this.route = undefined;
    this.params = undefined;
    this.statusCode = undefined;
    this.user = undefined;
    this._cachedQuery = undefined;
    this._logContext = undefined;
    this._logRoute = undefined;
    this._logMethod = undefined;
    this._scopeContainerContext = undefined;
    this._scope = undefined;
    this.__requestError = undefined;
    this.__requestErrorLogged = false;
    this.__callbacks = undefined;
    this.__matchedHandlers = undefined;
    this.__accessRoute = undefined;
    this.__accessStart = undefined;

    this._maxBodySizeBytes = args.maxBodySizeBytes;
    this._queryParams = undefined;
    this._body = undefined;
    this._secured = undefined;
    this._host = undefined;
    this._domain = undefined;
    this._path = undefined;
    this._query = undefined;
  }

  get queryParams(): () => Record<string, string> {
    if (this._queryParams === undefined) {
      // Built on the request's own context, so a context derived from it
      // reads the request's accessor, and a later replacement of it.
      const owner = requestOf(this);
      owner._queryParams = () => {
        if (owner._cachedQuery === undefined) {
          owner._cachedQuery = parseQueryString(owner._scanner.query);
        }
        return owner._cachedQuery;
      };
    }
    return this._queryParams as () => Record<string, string>;
  }

  set queryParams(queryParams: () => Record<string, string>) {
    this._queryParams = queryParams;
  }

  get body(): <T>() => Promise<T | undefined> {
    if (this._body === undefined) {
      const owner = requestOf(this);
      owner._body = parseBody(owner.req, owner._maxBodySizeBytes);
    }
    return this._body as <T>() => Promise<T | undefined>;
  }

  set body(body: <T>() => Promise<T | undefined>) {
    this._body = body;
  }

  get secured(): () => boolean {
    if (this._secured === undefined) {
      const owner = requestOf(this);
      owner._secured = () => owner._scanner.secured;
    }
    return this._secured as () => boolean;
  }

  set secured(secured: () => boolean) {
    this._secured = secured;
  }

  get host(): () => string {
    if (this._host === undefined) {
      const owner = requestOf(this);
      owner._host = () => owner._scanner.host;
    }
    return this._host as () => string;
  }

  set host(host: () => string) {
    this._host = host;
  }

  get domain(): () => string {
    if (this._domain === undefined) {
      const owner = requestOf(this);
      owner._domain = () => owner._scanner.domain;
    }
    return this._domain as () => string;
  }

  set domain(domain: () => string) {
    this._domain = domain;
  }

  get path(): () => string {
    if (this._path === undefined) {
      const owner = requestOf(this);
      owner._path = () => owner._scanner.path;
    }
    return this._path as () => string;
  }

  set path(path: () => string) {
    this._path = path;
  }

  get query(): () => string {
    if (this._query === undefined) {
      const owner = requestOf(this);
      owner._query = () => owner._scanner.query;
    }
    return this._query as () => string;
  }

  set query(query: () => string) {
    this._query = query;
  }

  /** The request's signal, built on first read when the server gave a builder. */
  get signal(): AbortSignal {
    if (this._signal === undefined) {
      if (this._signalSource) {
        this._signal = this._signalSource();
      } else {
        this._signal = this._lifetime ? this._lifetime.signal() : this.req.signal;
      }
      this._signalSource = undefined;
    }
    return this._signal;
  }

  /**
   * Ends the request now: its signal aborts, as it would at the timeout. Only
   * a request with a deadline has one; the function is built on first read.
   */
  get __endRequest(): (() => void) | undefined {
    if (this._endRequest === undefined && this._lifetime !== undefined) {
      const lifetime = this._lifetime;
      this._endRequest = () => lifetime.stop();
    }
    return this._endRequest;
  }

  set __endRequest(endRequest: (() => void) | undefined) {
    this._endRequest = endRequest;
  }

  /**
   * The raw Authorization header, captured for downstream service-to-service
   * forwarding. Read from the request on first use: most requests never
   * forward it. Header names are case-insensitive: one lookup finds either spelling.
   */
  get __authorizationHeader(): string | undefined {
    if (this._authorization === null) {
      this._authorization = this.headers.get('Authorization') ?? undefined;
    }
    return this._authorization;
  }

  set __authorizationHeader(authorization: string | undefined) {
    this._authorization = authorization;
  }

  set signal(signal: AbortSignal) {
    this._signal = signal;
    this._signalSource = undefined;
  }

  /**
   * The request-scoped log context every logger of the request writes and
   * reads. When the logger middleware deferred its `http` field, the first
   * read builds `{ http: { method, routePath } }`: a request that writes no
   * record and sets no field never builds it.
   *
   * A context derived from this one with `Object.create` (a client call's
   * telemetry scope) reads and builds the request's own log context, as it
   * read the field the logger middleware used to set on the request.
   */
  get logContext(): Record<string, unknown> | undefined {
    const owner = ownerOf(this);
    if (owner._logContext === undefined && owner._logRoute !== undefined) {
      owner._logContext = { http: { method: owner._logMethod, routePath: owner._logRoute } };
      owner._logRoute = undefined;
      owner._logMethod = undefined;
    }
    return owner._logContext;
  }

  set logContext(logContext: Record<string, unknown> | undefined) {
    this._logContext = logContext;
    this._logRoute = undefined;
    this._logMethod = undefined;
  }

  /**
   * Defers the `http` log field, with the method and route path the request
   * has now; false when the log context already exists.
   */
  deferHttpLogContext(method: string, routePath: string): boolean {
    const owner = ownerOf(this);
    if (owner._logContext !== undefined) return false;
    owner._logRoute = routePath;
    owner._logMethod = method;
    return true;
  }

  /**
   * The path the router matches: the URL path less one leading slash, with a
   * leading slash again. A path that starts with `//` keeps one slash less,
   * as `path()` gives it; any other path is the URL path itself.
   */
  __pathname(): string {
    const pathname = this._scanner.pathname;
    return pathname.charCodeAt(1) === 47 && pathname.charCodeAt(0) === 47 ? pathname.slice(1) : pathname;
  }

  throw(status: number, message?: string): never {
    throw new HttpAbortException(status, message);
  }

  /** Records the DI context so a request scope can be materialised on demand. */
  attachScope(containerContext: ContainerContext): void {
    this._scopeContainerContext = containerContext;
  }

  /**
   * Returns the per-request DI scope, creating it on first use. Requests whose
   * handler and middleware never resolve a dependency never pay for a scope.
   * Returns `undefined` when no DI context is attached (no registrations).
   */
  resolveScope(): ScopeContext | undefined {
    if (this._scope === undefined) {
      if (this._scopeContainerContext === undefined) {
        return undefined;
      }
      this._scope = this._scopeContainerContext.createScopeSync();
    }
    return this._scope.scope;
  }

  /** Disposes the request scope if (and only if) it was materialised. */
  closeScope(): Promise<void> | void {
    if (this._scope !== undefined) {
      return this._scope.close();
    }
  }
}

// The scope is exposed under the runtime's shared symbol via a prototype getter
// (not an own property), so reading it lazily materialises the scope without
// disturbing the instance's hidden class. `SCOPE_CONTAINER_KEY` is a runtime
// `symbol` (not `unique symbol`), so it can't be a computed class member name —
// hence the defineProperty here.
Object.defineProperty(HttpContext.prototype, SCOPE_CONTAINER_KEY, {
  configurable: true,
  get(this: HttpContext): ScopeContext | undefined {
    return this.resolveScope();
  },
});

export function buildHttpContext<C extends HttpRequestContext>(context: BuildContextArgs): C {
  return new HttpContext(context) as unknown as C;
}

/**
 * Defers the `{ method, routePath }` http log field of a request until its log
 * context is first read: when a record is written or a field is set. Returns
 * false when the context cannot defer it (not a pipeline request context, or
 * its log context already exists): the caller then sets the field itself.
 */
export function deferHttpLogContext(ctx: HttpRequestContext, method: string, routePath: string): boolean {
  return ctx instanceof HttpContext && ctx.deferHttpLogContext(method, routePath);
}

/**
 * The request context that holds the log state a context reads: itself, or,
 * for a context derived with `Object.create`, the first prototype that owns
 * it. Every context owns it from its constructor; a derived one owns it once
 * it set its own log context.
 */
function ownerOf(ctx: HttpContext): HttpContext {
  let owner: HttpContext = ctx;
  while (!Object.hasOwn(owner, '_logContext')) {
    const proto = Object.getPrototypeOf(owner) as HttpContext | null;
    if (proto === null || proto === HttpContext.prototype) return ctx;
    owner = proto;
  }
  return owner;
}

/**
 * A copy of a request context, for a context that outlives the request (a
 * stream). A spread copies own fields only: this adds the request accessors,
 * the log context and the Authorization header. It does not carry `throw`,
 * `signal`, `__endRequest` or the request's DI scope; a stream sets its own
 * signal.
 */
export function copyRequestContext(ctx: HttpRequestContext): HttpRequestContext {
  return {
    ...ctx,
    queryParams: ctx.queryParams,
    body: ctx.body,
    secured: ctx.secured,
    host: ctx.host,
    domain: ctx.domain,
    path: ctx.path,
    query: ctx.query,
    logContext: ctx.logContext,
    __authorizationHeader: (ctx as HttpRequestContext & { __authorizationHeader?: string }).__authorizationHeader,
  } as HttpRequestContext;
}

/**
 * The request's own context, which holds its URL scanner and its accessors:
 * itself, or the prototype a context derived with `Object.create` reads
 * through, even when the derived one set its own log context.
 */
function requestOf(ctx: HttpContext): HttpContext {
  let owner: HttpContext = ctx;
  while (!Object.hasOwn(owner, '_scanner')) {
    const proto = Object.getPrototypeOf(owner) as HttpContext | null;
    if (proto === null || proto === HttpContext.prototype) return ctx;
    owner = proto;
  }
  return owner;
}

/** Attach a DI context to a request so its scope is created lazily on first resolve. */
export function attachRequestScope(ctx: HttpRequestContext, containerContext: ContainerContext): void {
  (ctx as unknown as HttpContext).attachScope(containerContext);
}

/** Close a request's DI scope if it was materialised; a no-op otherwise. */
export function closeRequestScope(ctx: HttpRequestContext): Promise<void> | void {
  return (ctx as unknown as HttpContext).closeScope();
}

const parseBody =
  (req: Request, maxBodySizeBytes?: number) =>
  async <T>(): Promise<T | undefined> => {
    await ensureBodyWithinLimit(req, maxBodySizeBytes);

    const contentTypeHeader = req.headers.get('Content-Type');
    const contentType = contentTypeHeader?.split(';')[0]?.trim().toLowerCase() ?? '';
    if (contentType === 'application/x-www-form-urlencoded' || contentType === 'multipart/form-data') {
      const form = await req.formData();
      const data = {} as Record<string, string | File>;
      form.forEach((value, key) => {
        data[key] = value;
      });

      return data as T;
    }
    if (contentType === 'application/json') {
      // The text is read first so that only a parse failure, the caller's
      // fault, answers 400; a failed read keeps its own error.
      const text = await req.text();
      if (text.trim() === '') return undefined;
      try {
        return JSON.parse(text) as T;
      } catch {
        throw new BadRequestException('Malformed JSON body');
      }
    }
    return undefined;
  };

/** Default request-body size limit (1 MiB), shared by `parseBody` and the CSRF form-token extraction. */
export const DEFAULT_MAX_BODY_SIZE_BYTES = 1_048_576;

/**
 * Reject a request whose body exceeds `maxBodySizeBytes` with a 413
 * `HttpAbortException` — via the `Content-Length` header when present (no body
 * read at all), otherwise by stream-counting a clone with early rejection.
 * Exported so every body-buffering consumer (e.g. CSRF form-token extraction)
 * enforces the same limit as `parseBody`.
 */
export async function ensureBodyWithinLimit(req: Request, maxBodySizeBytes?: number): Promise<void> {
  if (!maxBodySizeBytes || maxBodySizeBytes <= 0 || !Number.isFinite(maxBodySizeBytes)) {
    return;
  }

  const contentLengthHeader = req.headers.get('Content-Length');
  const contentLength = contentLengthHeader ? Number.parseInt(contentLengthHeader, 10) : Number.NaN;
  if (!Number.isNaN(contentLength)) {
    if (contentLength > maxBodySizeBytes) {
      throw new HttpAbortException(413, `Payload Too Large: body exceeds ${maxBodySizeBytes} bytes`);
    }
    return; // Content-Length is present and within limit
  }

  // No Content-Length header — stream-count bytes with early rejection
  const clonedBody = req.clone().body;
  if (!clonedBody) return;
  let totalBytes = 0;
  await clonedBody.pipeTo(
    new WritableStream({
      write(chunk) {
        totalBytes += chunk.byteLength;
        if (totalBytes > maxBodySizeBytes) {
          throw new HttpAbortException(413, `Payload Too Large: body exceeds ${maxBodySizeBytes} bytes`);
        }
      },
    }),
  );
}
