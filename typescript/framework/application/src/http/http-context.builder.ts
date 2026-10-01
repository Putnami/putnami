import type { ContainerContext, DetachedScope, ScopeContext } from '@putnami/runtime';
import { SCOPE_CONTAINER_KEY } from '@putnami/runtime/inject';
import { parseQueryString } from '@putnami/utils';
import { HttpAbortException } from './http.exception';
import type { HttpRequestContext, Server } from './http-context.type';
import type { HttpMethod } from './http-method.type';
import type { HttpResponse } from './http-response';
import type { RouteHandler } from './route.type';
import type { HandlerResult } from './router';
import { UrlScanner } from './url.scanner';

type BuildContextArgs = {
  req: Request;
  server?: Server;
  signal?: AbortSignal;
  maxBodySizeBytes?: number;
};

/**
 * The per-request HTTP context, implemented as a class so every request shares
 * one hidden class (monomorphic field access) and the request-derived accessors
 * (`path`, `query`, `host`, `domain`, `secured`, `throw`) live on the prototype
 * instead of being reallocated as closures on each request.
 *
 * `queryParams` and `body` stay per-instance functions: they are replaced in
 * place by the validation wrapper and `body` is extracted-and-called, so a bound
 * function keeps both patterns working while preserving a stable instance shape.
 * Mutable plumbing slots are declared and pre-initialised so later assignment by
 * the dispatcher/middleware never triggers a hidden-class transition.
 */
class HttpContext {
  readonly req: Request;
  readonly signal: AbortSignal;
  readonly headers: Headers;
  readonly method: HttpMethod;
  readonly url: string;
  readonly server?: Server;

  route?: string;
  params?: Record<string, string>;
  statusCode?: number;
  user?: Record<string, unknown>;

  __authorizationHeader?: string;
  __requestError?: unknown;
  __requestErrorLogged?: boolean;
  __callbacks?: ((r: HttpResponse) => HttpResponse)[];
  __matchedHandlers?: HandlerResult<RouteHandler<HttpRequestContext>>[];

  // These accessors are per-instance bound closures (not prototype methods) so
  // they keep working when extracted and copied into sub-contexts (WebSocket /
  // SSE / stream / gRPC) or destructured (e.g. the logger middleware) — all of
  // which call them without the original `this`.
  queryParams: () => Record<string, string>;
  body: <T>() => Promise<T | undefined>;
  secured: () => boolean;
  host: () => string;
  domain: () => string;
  path: () => string;
  query: () => string;

  private readonly _scanner: UrlScanner;
  private _cachedQuery?: Record<string, string>;
  private _scopeContainerContext?: ContainerContext;
  private _scope?: DetachedScope;

  constructor(args: BuildContextArgs) {
    const req = args.req;
    this.req = req;
    this.signal = args.signal ?? req.signal;
    this.headers = req.headers;
    this.method = req.method as HttpMethod;
    this.url = req.url;
    this.server = args.server;
    this._scanner = new UrlScanner(req.url, req.headers);

    // Capture raw Authorization header for downstream service-to-service JWT forwarding.
    this.__authorizationHeader = req.headers.get('Authorization') ?? req.headers.get('authorization') ?? undefined;

    // Pre-initialise the mutable plumbing so assigning these later keeps the shape stable.
    this.route = undefined;
    this.params = undefined;
    this.statusCode = undefined;
    this.user = undefined;
    this._cachedQuery = undefined;
    this._scopeContainerContext = undefined;
    this._scope = undefined;
    this.__requestError = undefined;
    this.__requestErrorLogged = false;
    this.__callbacks = undefined;
    this.__matchedHandlers = undefined;

    this.queryParams = () => {
      if (this._cachedQuery === undefined) {
        this._cachedQuery = parseQueryString(this._scanner.query);
      }
      return this._cachedQuery;
    };
    this.body = parseBody(req, args.maxBodySizeBytes);
    this.secured = () => this._scanner.secured;
    this.host = () => this._scanner.host;
    this.domain = () => this._scanner.domain;
    this.path = () => this._scanner.path;
    this.query = () => this._scanner.query;
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
      return (await req.json()) as T;
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
