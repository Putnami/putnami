import type { HttpMiddleware } from '@putnami/application';
import type { LazyLayoutModule } from './module.types';
import type { PageDefinition } from './page';
import type { ReactModuleCache } from './react-module-cache';
import { normalizeRoute } from './route-cache.utils';
import { collectLayoutMiddleware, composeMiddleware, resolvePageMiddleware } from './route-middleware.utils';
import { resolveLayoutOrDefinition } from './route-module.utils';

/**
 * Owns layout + page middleware resolution for a {@link ReactApplication} and
 * prevents lazy layouts from leaving descendant routes temporarily unguarded.
 *
 * An EAGER layout publishes its middleware synchronously at registration, so a
 * child route registered afterwards reads the layout's `.secure()` from the map
 * immediately. A LAZY layout cannot reveal its middleware without executing its
 * async module loader; until it runs, the child page/loader/action handlers
 * would have NO server-side enforcement (a silent auth bypass: an
 * unauthenticated request would get 200 instead of 401).
 *
 * This resolver records a lazy layout's module loader at registration and
 * exposes an async {@link resolveLayoutMiddleware} that loads it — once, cached
 * through the shared module cache — at REQUEST time, before the child handler
 * runs. `ReactApplication.reactPage` routes any handler whose route has a lazy
 * ancestor through that async path (see {@link hasLazyLayoutAncestor}), so the
 * layout's security is always resolved before the child handler executes.
 *
 * Eager-only routes keep using the synchronous map (the path the SSR generator
 * emits, which always imports layouts eagerly).
 */
export class RouteMiddlewareResolver {
  /** Eager layout middleware, keyed by normalized route. */
  private readonly eager = new Map<string, HttpMiddleware[]>();
  /**
   * Memoized async resolvers for lazy layouts, keyed by normalized route. Each
   * loads its module once (deduplicated via the shared module cache) and
   * extracts the layout's middleware.
   */
  private readonly lazy = new Map<string, () => Promise<HttpMiddleware[] | undefined>>();

  constructor(private readonly cache: ReactModuleCache) {}

  /** Record an eager layout's middleware, known synchronously at registration. */
  registerEagerLayout(route: string, middleware: readonly HttpMiddleware[]): void {
    if (middleware.length > 0) {
      this.eager.set(normalizeRoute(route), [...middleware]);
    }
  }

  /**
   * Record a lazy layout. Its middleware is unknown until its module loads, so a
   * memoized async resolver is stored and awaited at request time by descendant
   * handlers (and by the layout's own loader).
   */
  registerLazyLayout(route: string, lazyLayout: LazyLayoutModule): void {
    let pending: Promise<HttpMiddleware[] | undefined> | undefined;
    this.lazy.set(normalizeRoute(route), () => {
      if (!pending) {
        pending = this.loadLazyLayoutMiddleware(route, lazyLayout).catch((error) => {
          // Fail closed but allow retry: a transient module-load failure must not
          // permanently strip enforcement from descendant routes.
          pending = undefined;
          throw error;
        });
      }
      return pending;
    });
  }

  private async loadLazyLayoutMiddleware(
    route: string,
    lazyLayout: LazyLayoutModule,
  ): Promise<HttpMiddleware[] | undefined> {
    const layoutModule = await this.cache.loadModule(`layout:${route}`, lazyLayout);
    const { layoutDef } = resolveLayoutOrDefinition(layoutModule);
    return layoutDef?.middleware && layoutDef.middleware.length > 0 ? [...layoutDef.middleware] : undefined;
  }

  /**
   * Whether any ancestor layout of `route` — including a layout AT `route`
   * itself — is lazy and therefore requires request-time middleware resolution.
   */
  hasLazyLayoutAncestor(route: string): boolean {
    if (this.lazy.size === 0) return false;
    return this.ancestorPaths(route).some((path) => this.lazy.has(path));
  }

  /**
   * Synchronously collect eager ancestor layout middleware (root → nearest).
   * Lazy layouts are intentionally excluded — they have no synchronously-known
   * middleware. Used at render time and for the eager fast path.
   */
  collectLayoutMiddleware(route: string): HttpMiddleware[] | undefined {
    return collectLayoutMiddleware(this.eager, route);
  }

  /**
   * Synchronously compose a route's page middleware from eager layouts only.
   * Used for the common eager path where no ancestor layout is lazy.
   */
  resolvePageMiddlewareSync(
    route: string,
    pageDef: PageDefinition | undefined,
    notFoundMiddleware: HttpMiddleware[] | undefined,
  ): HttpMiddleware[] | undefined {
    return resolvePageMiddleware(this.eager, route, pageDef, notFoundMiddleware);
  }

  /**
   * Resolve the full ancestor layout middleware chain for `route` at request
   * time, loading any lazy ancestor layouts. Ordered root → nearest layout.
   */
  async resolveLayoutMiddleware(route: string): Promise<HttpMiddleware[] | undefined> {
    const collected: HttpMiddleware[] = [];
    for (const path of this.ancestorPaths(route)) {
      const lazy = this.lazy.get(path);
      if (lazy) {
        const mw = await lazy();
        if (mw) collected.push(...mw);
        continue;
      }
      const eager = this.eager.get(path);
      if (eager) collected.push(...eager);
    }
    return collected.length > 0 ? collected : undefined;
  }

  /**
   * Resolve a route's complete page middleware at request time: the full
   * ancestor layout chain (loading lazy layouts) composed with the page's own
   * middleware. A not-found middleware override replaces the chain, mirroring
   * the synchronous {@link resolvePageMiddleware}.
   */
  async resolvePageMiddleware(
    route: string,
    pageDef: PageDefinition | undefined,
    notFoundMiddleware: HttpMiddleware[] | undefined,
  ): Promise<HttpMiddleware[] | undefined> {
    if (notFoundMiddleware) return notFoundMiddleware;
    const layoutMw = await this.resolveLayoutMiddleware(route);
    const pageMw = pageDef?.middleware && pageDef.middleware.length > 0 ? [...pageDef.middleware] : undefined;
    return composeMiddleware(layoutMw, pageMw);
  }

  /** Ancestor layout paths for `route`, ordered root (`/`) → the route itself. */
  private ancestorPaths(route: string): string[] {
    const normalized = normalizeRoute(route);
    const routeForAncestors =
      normalized.endsWith('*') && !normalized.endsWith('/*') ? `${normalized.slice(0, -1)}/*` : normalized;
    const segments = routeForAncestors.split('/').filter(Boolean);
    const paths = ['/'];
    let path = '';
    for (const seg of segments) {
      path += `/${seg}`;
      paths.push(path);
    }
    return paths;
  }
}
