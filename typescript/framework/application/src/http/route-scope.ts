import type { HttpMiddleware } from './http-middleware.type';
import type { HttpRequestContext, HttpRequestContextInternal } from './http-context.type';
import type { HttpResponse } from './http-response';

/**
 * What a middleware can know, at start, about a route it may run on: the
 * method the route is registered for, its path pattern and the options that
 * change what the built-in middlewares do.
 */
export interface RouteTraits {
  readonly method: string;
  readonly route: string;
  readonly minimal: boolean;
  /** `false` when the route opted out of the secure response headers. */
  readonly securityHeaders: boolean;
  readonly csrfExempt: boolean;
}

/** Whether a middleware acts on a route with these traits. */
export type ActsOnRoute = (route: RouteTraits) => boolean;

/**
 * The key a middleware carries its route predicate under. `Symbol.for` keeps
 * the declaration readable when generated routes and the root HTTP plugin
 * resolve the framework from two locations.
 */
const ACTS_ON_ROUTE = Symbol.for('putnami.http.middleware.actsOnRoute');

type ScopedMiddleware = HttpMiddleware & { [ACTS_ON_ROUTE]?: ActsOnRoute };

/**
 * Declares on `middleware` which routes it acts on, and returns the same
 * function. A request matched to a route the predicate refuses does not run
 * the middleware at all.
 *
 * The predicate must be conservative: it may answer `false` only when the
 * middleware, on every request matched to that route alone, would call
 * `next()` and return its result unchanged. A request with no matched route,
 * or with several candidate routes, still runs every middleware.
 */
export function actsOnRoutes(middleware: HttpMiddleware, actsOn: ActsOnRoute): HttpMiddleware {
  (middleware as ScopedMiddleware)[ACTS_ON_ROUTE] = actsOn;
  return middleware;
}

/** Whether `middleware` acts on `route`. A middleware that declares nothing acts on every route. */
export function middlewareActsOn(middleware: HttpMiddleware, route: RouteTraits): boolean {
  const actsOn = (middleware as ScopedMiddleware)[ACTS_ON_ROUTE];
  return actsOn === undefined || actsOn(route);
}

const PLAIN_SEGMENT = /^[A-Za-z0-9._~!$&'+,;=@-]*$/;
const PARAM_SEGMENT = /^\[[A-Za-z_$][\w$]*\]$/;

/**
 * Whether the route pattern `route` may match the request path `path`.
 * Conservative: a pattern with any syntax beyond static segments and
 * whole-segment `[param]`s may match any path.
 */
export function routeMayMatch(route: string, path: string): boolean {
  const routeSegments = route.split('/');
  const pathSegments = path.split('/');
  for (const segment of routeSegments) {
    if (!PLAIN_SEGMENT.test(segment) && !PARAM_SEGMENT.test(segment)) return true;
  }
  if (routeSegments.length !== pathSegments.length) return false;
  for (let i = 0; i < routeSegments.length; i++) {
    if (routeSegments[i] !== pathSegments[i] && !PARAM_SEGMENT.test(routeSegments[i])) return false;
  }
  return true;
}

/** A middleware chain composed once: it runs the middlewares in order, then `dispatch`. */
export type MiddlewareChain = (
  ctx: HttpRequestContext,
  dispatch: () => Promise<HttpResponse | undefined>,
) => Promise<HttpResponse | undefined>;

const DISPATCH_ONLY: MiddlewareChain = (_ctx, dispatch) => dispatch();

/** Composes `middlewares`, the first one outermost, into one chain. */
export function composeMiddlewareChain(middlewares: readonly HttpMiddleware[]): MiddlewareChain {
  if (middlewares.length === 0) return DISPATCH_ONLY;
  // Compose from the inside out. The innermost middleware receives `dispatch`
  // itself as `next`, so no terminal `() => dispatch()` closure is allocated
  // per request.
  const innermost = middlewares[middlewares.length - 1];
  let chain: MiddlewareChain = (ctx, dispatch) => innermost(ctx, dispatch);
  for (let i = middlewares.length - 2; i >= 0; i--) {
    const mw = middlewares[i];
    const next = chain;
    chain = (ctx, dispatch) => mw(ctx, () => next(ctx, dispatch));
  }
  return chain;
}

const MINIMAL = 1;
const NO_SECURITY_HEADERS = 2;
const CSRF_EXEMPT = 4;
const TRAIT_VARIANTS = 8;

/**
 * The middleware chains of one server: the full chain, and for each matched
 * route the chain of only the middlewares that act on it, composed the first
 * time a request reaches that route.
 */
export class RouteChains {
  readonly full: MiddlewareChain;
  /** Chains by method, then route pattern, then option bits. */
  private readonly routes = new Map<string, Map<string, (MiddlewareChain | undefined)[]>>();

  constructor(private readonly middlewares: readonly HttpMiddleware[]) {
    this.full = composeMiddlewareChain(middlewares);
  }

  /**
   * The chain a request runs. A request matched to exactly one route runs
   * that route's chain; any other request, unmatched or with several
   * candidates, runs the full chain.
   */
  select(ctx: HttpRequestContext): MiddlewareChain {
    const matched = (ctx as HttpRequestContextInternal).__matchedHandlers;
    if (matched === undefined || matched.length !== 1) return this.full;
    return this.forRoute(ctx.method, matched[0]);
  }

  /** The chain of a request matched to `handler` alone, for `method`. */
  forRoute(
    method: string,
    handler: { route: string; minimal?: boolean; securityHeaders?: boolean; csrfExempt?: boolean },
  ): MiddlewareChain {
    const bits =
      (handler.minimal === true ? MINIMAL : 0) |
      (handler.securityHeaders === false ? NO_SECURITY_HEADERS : 0) |
      (handler.csrfExempt === true ? CSRF_EXEMPT : 0);
    let byRoute = this.routes.get(method);
    if (byRoute === undefined) {
      byRoute = new Map();
      this.routes.set(method, byRoute);
    }
    let variants = byRoute.get(handler.route);
    if (variants === undefined) {
      variants = new Array(TRAIT_VARIANTS);
      byRoute.set(handler.route, variants);
    }
    let chain = variants[bits];
    if (chain === undefined) {
      chain = composeMiddlewareChain(this.middlewaresFor(traitsOf(method, handler.route, bits)));
      variants[bits] = chain;
    }
    return chain;
  }

  /** The middlewares, in order, that act on a request matched to `handler` alone, for `method`. */
  middlewaresForRoute(
    method: string,
    handler: { route: string; minimal?: boolean; securityHeaders?: boolean; csrfExempt?: boolean },
  ): HttpMiddleware[] {
    const bits =
      (handler.minimal === true ? MINIMAL : 0) |
      (handler.securityHeaders === false ? NO_SECURITY_HEADERS : 0) |
      (handler.csrfExempt === true ? CSRF_EXEMPT : 0);
    return this.middlewaresFor(traitsOf(method, handler.route, bits));
  }

  /** The middlewares, in order, that act on a route with these traits. */
  middlewaresFor(route: RouteTraits): HttpMiddleware[] {
    return this.middlewares.filter((mw) => middlewareActsOn(mw, route));
  }
}

function traitsOf(method: string, route: string, bits: number): RouteTraits {
  return {
    method,
    route,
    minimal: (bits & MINIMAL) !== 0,
    securityHeaders: (bits & NO_SECURITY_HEADERS) === 0,
    csrfExempt: (bits & CSRF_EXEMPT) !== 0,
  };
}

/**
 * The synchronous form of a middleware, for a route compiled at start: what
 * it does before the inner chain and after it, with no `next` and no
 * promise. A middleware that has one may run, on a route whose every
 * middleware has one, without a chain layer of its own; it must then behave
 * exactly as the middleware would.
 */
export interface CompiledMiddleware {
  /** Before the inner chain: an answer that ends the request there, or `undefined` to go on. */
  readonly enter?: (ctx: HttpRequestContext) => HttpResponse | undefined;
  /** After the inner chain answered: the response the middleware returns. */
  readonly leave?: (ctx: HttpRequestContext, response: HttpResponse | undefined) => HttpResponse | undefined;
  /** The inner chain threw: what the middleware does before the error goes on. */
  readonly fail?: (ctx: HttpRequestContext, error: unknown) => void;
}

const COMPILED_FORM = Symbol.for('putnami.http.middleware.compiled');

type CompilableMiddleware = HttpMiddleware & { [COMPILED_FORM]?: CompiledMiddleware };

/** Gives `middleware` its synchronous form, and returns the same function. */
export function withCompiledForm(middleware: HttpMiddleware, form: CompiledMiddleware): HttpMiddleware {
  (middleware as CompilableMiddleware)[COMPILED_FORM] = form;
  return middleware;
}

/** The synchronous form of `middleware`, or `undefined` when it has none. */
export function compiledFormOf(middleware: HttpMiddleware): CompiledMiddleware | undefined {
  return (middleware as CompilableMiddleware)[COMPILED_FORM];
}
