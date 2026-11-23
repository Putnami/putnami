import type { HttpMethod } from './http-method.type';
import type { HandlerResult } from './router';
import type { RouteHandler, RouteOptions } from './route.type';

/**
 * The key a route handler carries when it must stay on the fetch path.
 * `Symbol.for` keeps the mark readable across two copies of the framework.
 */
const SERVED_BY_FETCH = Symbol.for('putnami.http.servedByFetch');

/**
 * Keeps a route on the server's `fetch` path, matched by the framework's
 * router instead of Bun's routes table. The framework marks its own pages,
 * stream and WebSocket routes and gRPC/Connect calls with it. Any other route
 * of a method Bun serves by name may be matched by Bun; the request then runs
 * the same pipeline. Returns the handler.
 *
 * @example
 * ```ts
 * http().get('/legacy/[id]', servedByFetch((ctx) => ({ id: ctx.params?.['id'] })));
 * ```
 */
export function servedByFetch<H extends RouteHandler>(handler: H): H {
  (handler as H & { [SERVED_BY_FETCH]?: true })[SERVED_BY_FETCH] = true;
  return handler;
}

function isServedByFetch(handler: RouteHandler): boolean {
  return (handler as RouteHandler & { [SERVED_BY_FETCH]?: true })[SERVED_BY_FETCH] === true;
}

/** The HTTP methods Bun's routes table serves by name; HEAD and OPTIONS stay on fetch. */
const NATIVE_METHODS: ReadonlySet<string> = new Set(['GET', 'POST', 'PUT', 'PATCH', 'DELETE']);

/** A registered route, as the HTTP plugin records it. */
export interface RegisteredRoute {
  readonly method: HttpMethod;
  readonly path: string;
  readonly handler: RouteHandler;
  readonly options?: RouteOptions;
}

/** A route Bun's native router matches in place of the JS router. */
export interface NativeEndpoint {
  readonly method: HttpMethod;
  /** The route pattern, as the JS router records it (`/users/[id]`). */
  readonly route: string;
  /** The same pattern in Bun's syntax (`/users/:id`). */
  readonly bunPath: string;
  readonly hasParams: boolean;
  /** The match the JS router would give, less its params and score. */
  readonly match: Omit<HandlerResult<RouteHandler>, 'params' | 'score'>;
  /** Slash count of the route: the JS router's score is `slashes * 10 * accept score`. */
  readonly slashes: number;
  /**
   * The route's segments after its leading slash: a static segment, or the
   * name of a param. Bun matches the raw request target, while `req.url` is
   * normalized: the path these segments and Bun's params give must be the
   * URL's own path, or the JS router answers the request.
   */
  readonly parts: readonly { readonly param: boolean; readonly value: string }[];
}

/** The path a match of `endpoint` with `params` stands for, as the URL would spell it. */
export function matchedPath(endpoint: NativeEndpoint, params: Record<string, string> | undefined): string | undefined {
  let path = '';
  for (const part of endpoint.parts) {
    if (!part.param) {
      path += `/${part.value}`;
      continue;
    }
    const value = params?.[part.value];
    if (value === undefined) return undefined;
    path += `/${value}`;
  }
  return path === '' ? '/' : path;
}

/** A static segment both routers read the same way, or a whole-segment `[param]`. */
const STATIC_SEGMENT = /^[A-Za-z0-9._~!$&'+,;=@-]+$/;
const PARAM_SEGMENT = /^\[([A-Za-z_$][\w$]*)\]$/;

/** The segments of a pattern, `null` for a param, or `undefined` when the pattern uses any other syntax. */
function segmentsOf(route: string): (string | null)[] | undefined {
  if (route === '/') return [];
  if (route.length < 2 || route[0] !== '/') return undefined;
  const segments: (string | null)[] = [];
  for (const segment of route.slice(1).split('/')) {
    if (STATIC_SEGMENT.test(segment)) segments.push(segment);
    else if (PARAM_SEGMENT.test(segment)) segments.push(null);
    else return undefined;
  }
  return segments;
}

/** Whether a path can match both patterns. */
function overlaps(a: readonly (string | null)[], b: readonly (string | null)[]): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    if (a[i] !== null && b[i] !== null && a[i] !== b[i]) return false;
  }
  return true;
}

/**
 * Whether two route patterns, in Putnami (`[id]`) or Bun (`:id`, `*`)
 * syntax, may match a common path. Conservative: any syntax beyond static
 * segments and whole-segment params counts as matching anything.
 */
export function patternsMayOverlap(a: string, b: string): boolean {
  const left = looseSegmentsOf(routerRoute(a));
  const right = looseSegmentsOf(routerRoute(b));
  if (left === undefined || right === undefined) return true;
  const n = Math.max(left.length, right.length);
  for (let i = 0; i < n; i++) {
    const l = left[i];
    const r = right[i];
    // A rest segment matches whatever follows it.
    if (l === REST || r === REST) return true;
    if (l === undefined || r === undefined) return false;
    if (l !== null && r !== null && l !== r) return false;
  }
  return true;
}

const REST = Symbol('rest');

/**
 * Segments of a pattern: a static string, `null` for a param (`[id]` or
 * `:id`), {@link REST} for a final `*` or `[...rest]`; `undefined` for any
 * other syntax.
 */
function looseSegmentsOf(route: string): (string | null | typeof REST)[] | undefined {
  if (route === '/') return [];
  const parts = route.slice(1).split('/');
  const segments: (string | null | typeof REST)[] = [];
  for (let i = 0; i < parts.length; i++) {
    const part = parts[i];
    const last = i === parts.length - 1;
    if (STATIC_SEGMENT.test(part)) segments.push(part);
    else if (PARAM_SEGMENT.test(part) || /^:[A-Za-z_$][\w$]*$/.test(part)) segments.push(null);
    else if (last && (part === '*' || /^\[\.\.\.[A-Za-z_$][\w$]*\]$/.test(part))) segments.push(REST);
    else return undefined;
  }
  return segments;
}

/** The pattern as the JS router records it: a leading slash, trimmed. */
function routerRoute(path: string): string {
  const route = path === '' ? '/' : path[0] === '/' ? path : `/${path}`;
  return route.trim();
}

/**
 * The routes Bun's native router can match with the same answer as the JS
 * router: a route of a method Bun serves by name ({@link NATIVE_METHODS}),
 * not marked {@link servedByFetch}, alone at its pattern for its method, whose pattern uses only static and whole-segment param
 * segments and overlaps no other pattern of its method. A method with any
 * pattern of another syntax (a wildcard, an optional or suffixed param) maps
 * none of its routes.
 */
export function nativeEndpoints(routes: readonly RegisteredRoute[]): NativeEndpoint[] {
  const byMethod = new Map<HttpMethod, RegisteredRoute[]>();
  for (const route of routes) {
    let list = byMethod.get(route.method);
    if (list === undefined) {
      list = [];
      byMethod.set(route.method, list);
    }
    list.push(route);
  }

  const endpoints: NativeEndpoint[] = [];
  for (const [method, list] of byMethod) {
    if (!NATIVE_METHODS.has(method)) continue;
    const patterns = new Map<string, { segments: (string | null)[]; registered: RegisteredRoute[] }>();
    let simple = true;
    for (const registered of list) {
      const route = routerRoute(registered.path);
      const segments = segmentsOf(route);
      if (segments === undefined) {
        simple = false;
        break;
      }
      const pattern = patterns.get(route);
      if (pattern === undefined) patterns.set(route, { segments, registered: [registered] });
      else pattern.registered.push(registered);
    }
    if (!simple) continue;

    for (const [route, { segments, registered }] of patterns) {
      if (registered.length !== 1 || isServedByFetch(registered[0].handler)) continue;
      let alone = true;
      for (const [other, pattern] of patterns) {
        if (other !== route && overlaps(segments, pattern.segments)) {
          alone = false;
          break;
        }
      }
      if (!alone) continue;
      const { handler, options } = registered[0];
      endpoints.push({
        method,
        route,
        bunPath: route.replace(/\[([^\]]+)\]/g, ':$1'),
        hasParams: segments.includes(null),
        slashes: segments.length === 0 ? 1 : segments.length,
        parts:
          route === '/'
            ? []
            : route
                .slice(1)
                .split('/')
                .map((segment) => {
                  const param = PARAM_SEGMENT.exec(segment);
                  return param ? { param: true, value: param[1] } : { param: false, value: segment };
                }),
        match: {
          route,
          handler,
          accept: new Set(options?.accept ?? []),
          statusCode: options?.statusCode || 200,
          headMeta: options?.headMeta,
          csrfExempt: options?.csrfExempt,
          minimal: options?.minimal,
          securityHeaders: options?.securityHeaders,
          firstPartyErrors: options?.firstPartyErrors,
        },
      });
    }
  }
  return endpoints;
}
