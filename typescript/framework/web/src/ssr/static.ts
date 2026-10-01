import type { HttpRequestContext } from '@putnami/application';

// ---------------------------------------------------------------------------
// Render modes (the "WHEN" axis)
// ---------------------------------------------------------------------------

/**
 * How a route is rendered. Declared explicitly per route and **never inferred**
 * (core principle: deterministic and reviewable).
 *
 * - `ssr` — rendered per request (the default).
 * - `ssg` — rendered once at build time and emitted as static HTML.
 * - `isr` — like `ssg`, but the static output is revalidated on a TTL and/or
 *   when one of its cache tags is revalidated.
 */
export type RenderMode = 'ssr' | 'ssg' | 'isr';

/**
 * What hydrates on the client.
 *
 * - `full` — the whole React tree hydrates (classic SSR pages).
 * - `islands` — only `*.island.tsx` boundaries hydrate; the rest is static HTML.
 * - `none` — zero client JavaScript (a static page with no islands).
 */
export type HydrationMode = 'full' | 'islands' | 'none';

/** Concrete params for a single pre-rendered instance of a dynamic route. */
export type StaticPathParams = Record<string, string>;

/** Incremental Static Regeneration controls. */
export interface StaticRevalidate {
  /** Re-render a stale page after this many **seconds** (ISR by TTL). */
  seconds?: number;
  /** Re-render when any of these cache tags is revalidated (ISR by tag). */
  tags?: readonly string[];
}

/**
 * Options accepted by `page().static(opts)` / `loader().static(opts)`.
 */
export interface StaticOptions {
  /**
   * Incremental Static Regeneration. A bare number is shorthand for
   * `{ seconds }`. Omit for pure SSG (built once, served until the next build).
   *
   * @example page().static({ revalidate: 60 })                 // every 60s
   * @example page().static({ revalidate: { tags: ['posts'] } }) // on tag evict
   */
  revalidate?: number | StaticRevalidate;

  /**
   * For dynamic routes (`[param]` segments), enumerate the concrete params to
   * pre-render at build. **Must be a pure function** — it runs at build time
   * with no request data.
   *
   * @example
   * page().static({ paths: async () => (await listPosts()).map((p) => ({ slug: p.slug })) })
   */
  paths?: () => StaticPathParams[] | Promise<StaticPathParams[]>;
}

/** Normalised, internal representation of a route's static configuration. */
export interface StaticConfig {
  readonly mode: 'ssg' | 'isr';
  readonly revalidate?: StaticRevalidate;
  readonly paths?: () => StaticPathParams[] | Promise<StaticPathParams[]>;
}

/**
 * The serialisable slice of {@link StaticConfig} carried in the generated SSR
 * module. The `paths()` function is intentionally omitted — it cannot be
 * serialised and is only needed at build time, where the page module is loaded
 * directly.
 */
export interface StaticRouteMeta {
  readonly mode: 'ssg' | 'isr';
  readonly revalidate?: StaticRevalidate;
}

/** Widen a {@link StaticRouteMeta} (no `paths`) into a runtime {@link StaticConfig}. */
export function toStaticConfig(meta: StaticRouteMeta): StaticConfig {
  return { mode: meta.mode, ...(meta.revalidate ? { revalidate: meta.revalidate } : {}) };
}

/** Narrow a {@link StaticConfig} into the serialisable {@link StaticRouteMeta}. */
export function toStaticMeta(config: StaticConfig): StaticRouteMeta {
  return { mode: config.mode, ...(config.revalidate ? { revalidate: config.revalidate } : {}) };
}

function normalizeRevalidate(revalidate: StaticOptions['revalidate']): StaticRevalidate | undefined {
  if (revalidate === undefined) return undefined;
  if (typeof revalidate === 'number') {
    return revalidate > 0 ? { seconds: revalidate } : undefined;
  }
  const out: { seconds?: number; tags?: readonly string[] } = {};
  if (typeof revalidate.seconds === 'number' && revalidate.seconds > 0) {
    out.seconds = revalidate.seconds;
  }
  if (revalidate.tags && revalidate.tags.length > 0) {
    out.tags = [...revalidate.tags];
  }
  return out.seconds !== undefined || out.tags !== undefined ? out : undefined;
}

/**
 * Resolve user-facing `StaticOptions` into the internal `StaticConfig`.
 * The presence of any `revalidate` directive switches the mode from `ssg`
 * to `isr`.
 */
export function normalizeStatic(options: StaticOptions = {}): StaticConfig {
  const revalidate = normalizeRevalidate(options.revalidate);
  return {
    mode: revalidate ? 'isr' : 'ssg',
    ...(revalidate ? { revalidate } : {}),
    ...(options.paths ? { paths: options.paths } : {}),
  };
}

// ---------------------------------------------------------------------------
// Determinism contract — "static means static"
// ---------------------------------------------------------------------------

/**
 * Thrown when a `.static()` route is proven to depend on request-scoped state.
 *
 * There are two halves of determinism rule #1, both surfaced as this error:
 *
 * 1. **Runtime tripwire** — a `.static()` loader/page that *touches*
 *    request-scoped data during the pre-render pass (`new StaticRenderViolation(route, accessed)`).
 * 2. **Static proof** — a `.static()` route whose DI dependency graph reaches a
 *    request/session-scoped provider, proven at build time before rendering and
 *    independent of which code paths run (`StaticRenderViolation.scope(...)`).
 *
 * Either way the build fails loudly instead of silently downgrading the route
 * to SSR.
 */
export class StaticRenderViolation extends Error {
  /** The offending route pattern. */
  readonly route: string;
  /** The request-scoped member or provider that was accessed/reached. */
  readonly accessed: string;
  /**
   * For DI-graph (`scope`) violations: the resolution path from the route's
   * injected root to the offending scoped provider, as readable token names.
   */
  readonly resolutionPath?: readonly string[];

  constructor(route: string, accessed: string, options?: { message?: string; resolutionPath?: readonly string[] }) {
    super(
      options?.message ??
        `Static route "${route}" accessed request-scoped data \`ctx.${accessed}\` during pre-render. ` +
          `A .static() route must not depend on the request. Either remove the request access, ` +
          `or drop .static() so the route renders as SSR.`,
    );
    this.route = route;
    this.accessed = accessed;
    if (options?.resolutionPath) {
      this.resolutionPath = options.resolutionPath;
    }
    this.name = 'StaticRenderViolation';
  }

  /**
   * Build a violation for a `.static()` route that resolves a request/session-
   * scoped DI provider, proven from the dependency graph at build time.
   *
   * @param route - The route pattern.
   * @param provider - Readable name of the offending scoped provider.
   * @param resolutionPath - root → … → provider, as readable token names.
   */
  static scope(route: string, provider: string, resolutionPath: readonly string[]): StaticRenderViolation {
    const path = resolutionPath.length > 0 ? resolutionPath.join(' → ') : provider;
    const message =
      `Static route "${route}" resolves request/session-scoped provider \`${provider}\` through its DI graph ` +
      `(${path}). A .static() route must not depend on request-scoped state. Either make the provider a singleton, ` +
      `stop injecting it into the static route, or drop .static() so the route renders as SSR.`;
    return new StaticRenderViolation(route, provider, { message, resolutionPath });
  }
}

export function isStaticRenderViolation(value: unknown): value is StaticRenderViolation {
  return value instanceof StaticRenderViolation;
}

/**
 * Request-context members that are inherently request-scoped. Reading any of
 * them inside a `.static()` loader is a determinism violation.
 */
const REQUEST_SCOPED_KEYS = new Set<string>([
  'req',
  'request',
  'headers',
  'header',
  'cookies',
  'cookie',
  'getCookie',
  'user',
  'auth',
  'query',
  'queryParams',
  'searchParams',
  'ip',
  'clientIp',
  'method',
  'url',
  'originalUrl',
  'host',
  'session',
  'body',
  'formData',
]);

/**
 * Build a frozen, request-free context for build-time pre-rendering.
 *
 * Safe, build-time members (route `params`, a no-op `logger`, the shared
 * `documentMeta`, etc.) pass through; any access to a request-scoped member
 * throws {@link StaticRenderViolation}, turning a determinism breach into a
 * hard build error.
 */
export function createStaticRenderContext(route: string, params: StaticPathParams): HttpRequestContext {
  const noopLogger = {
    info: () => {},
    warn: () => {},
    error: () => {},
    debug: () => {},
    trace: () => {},
    fatal: () => {},
    child: () => noopLogger,
  };

  const base: Record<string, unknown> = {
    params,
    logger: noopLogger,
    statusCode: 200,
    // Marks the context so framework helpers can detect static pre-render.
    __static: true,
    documentMeta: {},
  };

  return new Proxy(base, {
    get(target, prop, receiver) {
      if (typeof prop === 'string' && REQUEST_SCOPED_KEYS.has(prop) && !(prop in target)) {
        throw new StaticRenderViolation(route, prop);
      }
      return Reflect.get(target, prop, receiver);
    },
  }) as unknown as HttpRequestContext;
}

/**
 * Map a route pathname to its emitted static HTML file path (relative to the
 * static output root). `/` becomes `index.html`; `/blog/hello` becomes
 * `blog/hello.html`.
 */
export function staticHtmlPath(pathname: string): string {
  const trimmed = pathname.replace(/^\/+/, '').replace(/\/+$/, '');
  if (trimmed === '') return 'index.html';
  return `${trimmed}.html`;
}

function assertSafeSplat(routePath: string, splat: string): void {
  const unsafe = (reason: string) => {
    throw new Error(`Unsafe splat param "*" for static path "${routePath}": ${reason}`);
  };

  if (splat.includes('\0')) {
    unsafe('contains a null byte');
  }
  if (splat.startsWith('/') || splat.startsWith('\\')) {
    unsafe('must be relative');
  }
  if (splat.includes('\\')) {
    unsafe('must use "/" path separators');
  }
  if (splat === '') {
    return;
  }

  for (const segment of splat.split('/')) {
    if (segment === '') {
      unsafe('contains an empty segment');
    }
    if (segment === '.' || segment === '..') {
      unsafe('contains a dot segment');
    }
  }
}

/**
 * Substitute concrete params into a React-Router style path. Handles both named
 * params and the catch-all splat (`*`, whose param key is `*`):
 *
 * - `/tasks/:id` + `{ id: '7' }` -> `/tasks/7`
 * - `/docs/*` + `{ '*': 'guide/intro' }` -> `/docs/guide/intro`
 *
 * Named params are percent-encoded (a single URL segment). The splat is
 * substituted **verbatim**: it carries a whole `/`-separated sub-path that is
 * used unchanged as the URL, the emitted file path, and the loader lookup key,
 * so the three can never drift (e.g. a `tooling-&-workspace` slug stays literal).
 */
export function fillRoutePath(routePath: string, params: StaticPathParams): string {
  const withParams = routePath.replace(/:([a-zA-Z_]\w*)/g, (_match, name: string) => {
    const value = params[name];
    if (value === undefined) {
      throw new Error(`Missing param "${name}" for static path "${routePath}"`);
    }
    return encodeURIComponent(value);
  });
  if (!withParams.includes('*')) {
    return withParams;
  }
  const splat = params['*'];
  if (splat === undefined) {
    throw new Error(`Missing splat param "*" for static path "${routePath}"`);
  }
  assertSafeSplat(routePath, splat);
  // Use a replacer function so `$` in the splat is not treated as a replacement token.
  return withParams.replace(/\*/g, () => splat);
}

/** Does a React-Router style path contain dynamic (`:param`) or splat (`*`) segments? */
export function isDynamicRoute(routePath: string): boolean {
  return /:[a-zA-Z_]\w*/.test(routePath) || routePath.includes('*');
}
