import type { HttpRequestContext } from './http-context.type';
import type { HttpMethod } from './http-method.type';
import type { RouteHandler, RouteOptions } from './route.type';
import { type HandlerResult, Router } from './router';

/** Manages per-method route trees and dispatches lookups to the appropriate router. */
export class RouteController<CONTEXT extends HttpRequestContext = HttpRequestContext> {
  protected routes?: Record<string, Router<RouteHandler<CONTEXT>>>;

  /** Creates a new RouteController by merging this controller's routes with another's. */
  merge<C extends HttpRequestContext = CONTEXT>(router: RouteController<C>): RouteController<CONTEXT> {
    const newRouteController = new RouteController<CONTEXT>();
    const visit = <C2 extends HttpRequestContext>(routes?: Record<string, Router<RouteHandler<C2>>>) => {
      if (routes) {
        for (const [k, router] of Object.entries(routes)) {
          newRouteController.routes ||= {};
          const routerForContext = router as unknown as Router<RouteHandler<CONTEXT>>;
          if (newRouteController.routes[k]) {
            newRouteController.routes[k] = newRouteController.routes[k].merge(routerForContext);
          } else {
            newRouteController.routes[k] = routerForContext;
          }
        }
      }
    };

    visit(this.routes);
    visit(router.routes);

    return newRouteController;
  }

  /** Registers a route handler for the given HTTP method and path. */
  route(method: HttpMethod, path: string, handler: RouteHandler, options?: RouteOptions) {
    this.routes ||= {} as Record<HttpMethod, Router<RouteHandler>>;
    this.routes[method] ||= new Router();
    this.routes[method].add(path, handler, options);

    return this;
  }

  /** Returns the highest-scoring handler matching the request context, or undefined if none match. */
  findFirst<FC extends CONTEXT = CONTEXT>(context: CONTEXT): HandlerResult<RouteHandler<FC>> | undefined {
    const handlers = this.find(context);

    if (handlers.length) {
      // find() returns matches ordered best-first (highest score), so index 0 is the most specific match.
      return handlers[0];
    }

    return undefined;
  }

  /** Returns all handlers matching the request context's method and path, ordered by score. */
  find<FC extends CONTEXT = CONTEXT>(context: CONTEXT): HandlerResult<RouteHandler<FC>>[] {
    const requestAccept = listRequestAccept(context.headers.get('Accept') || undefined);

    return this.routes?.[context.method]?.find(context.path(), requestAccept) || [];
  }

  /**
   * Return all HTTP methods that have at least one handler matching the given path.
   */
  findMethods(path: string): string[] {
    if (!this.routes) return [];
    const methods: string[] = [];
    for (const [method, router] of Object.entries(this.routes)) {
      if (router.find(path, ['*/*']).length > 0) {
        methods.push(method);
      }
    }
    return methods;
  }

  sumup(): string {
    let out = '';
    for (const [method, routes] of Object.entries(this.routes || {})) {
      out += `${method}\n\t`;
      out += JSON.stringify(routes.tree, undefined, 2);
    }

    return out;
  }
}

const listRequestAccept = (acceptHeader?: string): string[] => {
  if (!acceptHeader) return ['*/*'];

  const parts = acceptHeader.split(',');
  const result: string[] = [];
  for (let i = 0; i < parts.length; i++) {
    const semi = parts[i].indexOf(';');
    const mediaType = (semi >= 0 ? parts[i].slice(0, semi) : parts[i]).trim();
    if (mediaType) {
      result.push(mediaType);
    }
  }

  return result.length === 0 ? ['*/*'] : result;
};
