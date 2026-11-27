import type { HttpMiddleware } from '@putnami/application';
import type { LazyLayoutModule } from './module.types';
import type { PageDefinition } from './page';
import type { ReactModuleCache } from './react-module-cache';
import { normalizeRoute } from './route-cache.utils';
import { composeMiddleware } from './route-middleware.utils';
import { resolveLayoutOrDefinition } from './route-module.utils';

/**
 * Owns layout + page middleware resolution for a {@link ReactApplication}.
 *
 * Layouts register their middleware here: an eager layout its `.secure()` and
 * other middleware at registration, a lazy layout its module loader, whose
 * middleware is known once the module loads. Every route handler resolves its
 * ancestor chain through {@link resolveLayoutMiddleware} or
 * {@link resolvePageMiddleware} at its first request, after every layout
 * registered, so a layout secures its descendants whatever the order in which
 * they register, and a handler never runs before its middleware is resolved.
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
   * middleware, or with a not-found page's own middleware in its place.
   */
  async resolvePageMiddleware(
    route: string,
    pageDef: PageDefinition | undefined,
    notFoundMiddleware: HttpMiddleware[] | undefined,
  ): Promise<HttpMiddleware[] | undefined> {
    const layoutMw = await this.resolveLayoutMiddleware(route);
    const pageMw =
      notFoundMiddleware ??
      (pageDef?.middleware && pageDef.middleware.length > 0 ? [...pageDef.middleware] : undefined);
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
