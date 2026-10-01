import {
  applyInputValidation,
  type EndpointRequestContext,
  type HttpCacheOptions,
  type HttpMiddleware,
  type HttpRequestContext,
  type InferSchema,
  type SchemaDefinition,
  type SecurityGuard,
  SecurityMiddleware,
  type SecurityOptions,
} from '@putnami/application';
import type { ResolvedMap, TagSelector, Token } from '@putnami/runtime';
import { wrapWithMiddlewareRaw } from './route-middleware.utils';
import { isFilterOptions, isTagSelector, resolveInjection, useContainer } from '@putnami/runtime/inject';

// ---------------------------------------------------------------------------
// LoaderCacheOptions — extends HTTP cache with server-side caching
// ---------------------------------------------------------------------------

/**
 * Cache options for loaders.
 * Extends HTTP cache headers with server-side caching support.
 */
export interface LoaderCacheOptions extends HttpCacheOptions {
  /**
   * Server-side cache TTL in milliseconds.
   * When set, loader results are cached server-side using the layered cache.
   * The cache key includes the content hash, so cache is auto-invalidated on deploy.
   */
  ttl?: number;

  /**
   * Custom cache key generator.
   * When not provided, defaults to: `loader:{route}:{params}:{contentHash}`
   * @example
   * key: (ctx) => `user:${ctx.params.id}`
   */
  key?: (ctx: HttpRequestContext) => string;
}

// ---------------------------------------------------------------------------
// LoaderDefinition — the object produced by loader()
// ---------------------------------------------------------------------------

const LOADER_MARKER = 'putnami:loader' as const;

/**
 * The DI roots a loader resolves via `.inject()`, surfaced for build-time
 * static-safety analysis. A `.static()`
 * route whose loader injects a request/session-scoped provider — directly or
 * transitively — is a determinism violation provable from the DI graph.
 */
export interface LoaderInjectMeta {
  /** Concrete provider tokens injected (class / named / symbol). */
  readonly tokens: readonly Token[];
  /**
   * True when the loader injects via a tag/filter selector, which resolves to a
   * runtime-determined set of providers. Such roots can't be fully enumerated
   * statically, so a `.static()` route that uses them is a hybrid: not proven
   * dynamic, but backed by the runtime guard.
   */
  readonly dynamic: boolean;
}

export interface LoaderDefinition {
  readonly __loader: typeof LOADER_MARKER;
  readonly handler: (ctx: HttpRequestContext) => unknown;
  readonly schemas?: {
    readonly params?: SchemaDefinition;
    readonly query?: SchemaDefinition;
  };
  readonly cache?: LoaderCacheOptions;
  readonly middleware?: readonly HttpMiddleware[];
  /**
   * When true the loader runs at **build time** and its result is baked into
   * the statically rendered page. Set via `loader().static()`. The page that
   * owns the loader determines the final render mode; a static loader on an
   * SSR page still runs per request.
   */
  readonly static?: boolean;
  /** DI roots resolved via `.inject()`, for static-safety analysis. */
  readonly inject?: LoaderInjectMeta;
}

/**
 * Summarize an `.inject()` token map into the serialisable {@link LoaderInjectMeta}.
 * Concrete tokens become analysis roots; tag/filter selectors flip `dynamic`
 * because they resolve a runtime-determined set of providers.
 */
function buildInjectMeta(injectMap: Record<string, Token | TagSelector>): LoaderInjectMeta {
  const tokens: Token[] = [];
  let dynamic = false;
  for (const value of Object.values(injectMap)) {
    if (isTagSelector(value) || isFilterOptions(value)) {
      dynamic = true;
    } else {
      tokens.push(value as Token);
    }
  }
  return { tokens, dynamic };
}

export function isLoaderDefinition(value: unknown): value is LoaderDefinition {
  return typeof value === 'object' && value !== null && (value as LoaderDefinition).__loader === LOADER_MARKER;
}

/**
 * Extract the return type of a loader handler function.
 *
 * Since `LoaderDefinition` erases generic type information, this utility
 * works with the handler function directly (before it's wrapped by `loader()`).
 *
 * @example
 * ```typescript
 * // loader.ts
 * const myHandler = async (ctx: EndpointRequestContext) => {
 *   return { user: await getUser(ctx.params.id) };
 * };
 * export default loader(myHandler);
 * export type LoaderData = InferLoaderData<typeof myHandler>;
 *
 * // page.tsx
 * import type { LoaderData } from './loader';
 * const data = useLoaderData<LoaderData>(); // Type-safe!
 * ```
 */
export type InferLoaderData<THandler extends (...args: never[]) => unknown> = Awaited<ReturnType<THandler>>;

// ---------------------------------------------------------------------------
// LoaderBuilder — fluent API built by loader()
// ---------------------------------------------------------------------------

export class LoaderBuilder<
  TParams = undefined,
  TQuery = undefined,
  TInject extends Record<string, Token | TagSelector> | undefined = undefined,
> {
  private _params?: SchemaDefinition;
  private _query?: SchemaDefinition;
  private _cache?: LoaderCacheOptions;
  private _inject?: Record<string, Token | TagSelector>;
  private _middleware: HttpMiddleware[] = [];
  private _static = false;

  /**
   * Set cache options for this loader.
   *
   * - HTTP cache headers (Cache-Control, ETag) when `maxAge`, `etag`, etc. are set
   * - Server-side caching when `ttl` is set
   *
   * @example
   * // Server-side cache only
   * loader().cache({ ttl: 60000 }).handle(...)
   *
   * @example
   * // Combined with HTTP cache
   * loader().cache({ ttl: 60000, maxAge: 30, etag: true }).handle(...)
   *
   * @example
   * // Custom cache key
   * loader().cache({ ttl: 60000, key: (ctx) => `user:${ctx.params.id}` }).handle(...)
   */
  cache(options: LoaderCacheOptions): LoaderBuilder<TParams, TQuery, TInject> {
    this._cache = options;
    return this as unknown as LoaderBuilder<TParams, TQuery, TInject>;
  }

  /**
   * Run this loader at **build time** and bake its result into the statically
   * rendered HTML. Pair with `page().static()`. The handler runs with a
   * request-free context — touching request data is a hard build error.
   *
   * @example
   * loader().static().handle(async () => ({ posts: await listPosts() }))
   */
  static(): LoaderBuilder<TParams, TQuery, TInject> {
    this._static = true;
    return this as unknown as LoaderBuilder<TParams, TQuery, TInject>;
  }

  /** Declare and validate path parameters */
  params<S extends SchemaDefinition>(schema: S): LoaderBuilder<InferSchema<S>, TQuery, TInject> {
    this._params = schema;
    return this as unknown as LoaderBuilder<InferSchema<S>, TQuery, TInject>;
  }

  /** Declare and validate query string parameters */
  query<S extends SchemaDefinition>(schema: S): LoaderBuilder<TParams, InferSchema<S>, TInject> {
    this._query = schema;
    return this as unknown as LoaderBuilder<TParams, InferSchema<S>, TInject>;
  }

  /**
   * Declare dependencies to resolve from the current DI scope. See the
   * `loader()` entry-point doc for a worked example.
   */
  inject<M extends Record<string, Token | TagSelector>>(tokens: M): LoaderBuilder<TParams, TQuery, M> {
    this._inject = tokens;
    return this as unknown as LoaderBuilder<TParams, TQuery, M>;
  }

  /**
   * Require authentication and enforce access rules for this loader.
   *
   * @example Require any authenticated user
   * ```ts
   * loader().secure().handle(...)
   * ```
   *
   * @example Custom guard function
   * ```ts
   * loader().secure((user, ctx) => user.orgId === ctx.params?.orgId).handle(...)
   * ```
   */
  secure(optionsOrGuard?: SecurityOptions | SecurityGuard): LoaderBuilder<TParams, TQuery, TInject> {
    this._middleware.push(SecurityMiddleware(optionsOrGuard ?? {}));
    return this as unknown as LoaderBuilder<TParams, TQuery, TInject>;
  }

  /** Provide the handler function — finalises the loader definition */
  handle(
    handler: TInject extends Record<string, Token | TagSelector>
      ? (deps: ResolvedMap<TInject>, ctx: EndpointRequestContext<TParams, TQuery, undefined>) => unknown
      : (ctx: EndpointRequestContext<TParams, TQuery, undefined>) => unknown,
  ): LoaderDefinition {
    const schemas = this._params || this._query ? { params: this._params, query: this._query } : undefined;
    const needsWrapping = !!schemas;

    let finalHandler: (ctx: HttpRequestContext) => unknown;

    if (this._inject) {
      const tokens = this._inject;
      const injectedHandler = handler as unknown as (deps: unknown, ctx: unknown) => unknown;
      const baseHandler: (ctx: HttpRequestContext) => unknown = (ctx) => {
        const scope = useContainer();
        const deps = resolveInjection(tokens, scope);
        return injectedHandler(deps, ctx);
      };
      finalHandler = needsWrapping ? wrapLoaderWithValidation(baseHandler, schemas) : baseHandler;
    } else {
      const baseHandler = handler as unknown as (ctx: HttpRequestContext) => unknown;
      finalHandler = needsWrapping ? wrapLoaderWithValidation(baseHandler, schemas) : baseHandler;
    }

    if (this._middleware.length > 0) {
      finalHandler = wrapWithMiddlewareRaw(finalHandler, this._middleware);
    }

    const hasMiddleware = this._middleware.length > 0;
    return {
      __loader: LOADER_MARKER,
      handler: finalHandler,
      schemas,
      cache: this._cache,
      middleware: hasMiddleware ? [...this._middleware] : undefined,
      ...(this._static ? { static: true } : {}),
      ...(this._inject ? { inject: buildInjectMeta(this._inject) } : {}),
    };
  }
}

// ---------------------------------------------------------------------------
// loader() — two-mode entry point
// ---------------------------------------------------------------------------

/**
 * Define a page loader.
 *
 * **Simple mode** — pass a handler directly:
 * ```ts
 * export default loader((ctx) => {
 *   return { users: getUsers() };
 * });
 * ```
 *
 * **Builder mode** — chain validation schemas:
 * ```ts
 * export default loader()
 *   .params({ id: Uuid })
 *   .query({ include: Optional(String) })
 *   .handle(async (ctx) => {
 *     // ctx.params.id — validated as UUID
 *     return { user: await getUser(ctx.params.id) };
 *   });
 * ```
 *
 * **With dependency injection:**
 * ```ts
 * export default loader()
 *   .params({ id: Uuid })
 *   .inject({ userService: UserService })
 *   .handle(async ({ userService }, ctx) => {
 *     return { user: await userService.getUser(ctx.params.id) };
 *   });
 * ```
 *
 * **With security:**
 * ```ts
 * export default loader()
 *   .secure({ roles: ['user'] })
 *   .params({ id: Uuid })
 *   .handle(async (ctx) => {
 *     return { user: await getUser(ctx.params.id) };
 *   });
 * ```
 */
export function loader(handler: (ctx: HttpRequestContext) => unknown): LoaderDefinition;
export function loader(): LoaderBuilder;
export function loader(handler?: (ctx: HttpRequestContext) => unknown): LoaderDefinition | LoaderBuilder {
  if (handler) {
    return { __loader: LOADER_MARKER, handler };
  }
  return new LoaderBuilder();
}

// ---------------------------------------------------------------------------
// Internal
// ---------------------------------------------------------------------------

function wrapLoaderWithValidation(
  handler: (ctx: HttpRequestContext) => unknown,
  schemas: LoaderDefinition['schemas'],
): (ctx: HttpRequestContext) => unknown {
  return (ctx: HttpRequestContext) => {
    applyInputValidation(ctx, schemas);
    return handler(ctx);
  };
}
