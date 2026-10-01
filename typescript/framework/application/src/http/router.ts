import type { RouteOptions } from './route.type';

/** Result of a route lookup, containing the matched handler, extracted parameters, and content-negotiation metadata. */
export interface HandlerResult<T> {
  route: string;
  handler: T;
  accept: Set<string>;
  statusCode: number;
  params?: Record<string, string>;
  score: number;
  headMeta?: RouteOptions['headMeta'];
  csrfExempt?: boolean;
  minimal?: boolean;
  securityHeaders?: false;
  firstPartyErrors?: boolean;
}

type Handler<T> = {
  handler: T;
  accept: Set<string>;
  statusCode: number;
  headMeta?: RouteOptions['headMeta'];
  csrfExempt?: boolean;
  minimal?: boolean;
  securityHeaders?: false;
  firstPartyErrors?: boolean;
};

type Node<T> = {
  route?: string;
  children?: Record<number, Node<T>>;
  handlers?: Handler<T>[];
  paramName?: string;
  paramNode?: Node<T>;
  wildcard?: Handler<T>[];
};

/**
 * Precomputed entry in the static-route fast-path index. Holds the handlers
 * registered at an exact, parameter-free path plus the slash count used to
 * reproduce the trie's match score without walking the tree.
 */
type StaticEntry<T> = {
  route: string;
  handlers: Handler<T>[];
  slashes: number;
};

type NodeMatch<T> = {
  route: string;
  handler: Handler<T>;
  params?: Record<string, string>;
  score: number;
};

type Context<T> = {
  path: string;
  n: Node<T>;
  i: number;
  matches: NodeMatch<T>[];
  slashesCounter?: number;
  params?: Record<string, string> | undefined;
  requestAccept: string[];
};

const CODE_SLASH = 47; // '/'.charCodeAt(0);
const CODE_STAR = 42; // '*'.charCodeAt(0);
const CODE_OPEN_BRACKER = 91; // '['.charCodeAt(0);
const CODE_CLOSE_BRACKET = 93; // ']'.charCodeAt(0);
const CODE_OPEN_PARENT = 40; // '('.charCodeAt(0);
const CODE_CLOSE_PARENT = 41; // ')'.charCodeAt(0);
const CODE_QUESTION = 63; // '?'.charCodeAt(0);

/** Radix-tree based URL router that supports path parameters, wildcards, and content-type negotiation. */
export class Router<T = undefined> {
  readonly tree: Node<T> = {};

  /**
   * O(1) index of exact, parameter-free routes (no `[param]`, `*`, optional
   * `(…)` segment, or trailing slash). Populated alongside the trie in
   * {@link add}. Consulted by {@link find} only when {@link dynamicCount} is 0 —
   * i.e. when this router holds no dynamic routes, so an exact-path lookup is
   * provably the complete match set and the trie walk + scoring can be skipped.
   */
  private readonly staticRoutes = new Map<string, StaticEntry<T>>();

  /**
   * Count of registered routes that are NOT eligible for the static fast path
   * (parameterised, wildcard, optional-segment, or trailing-slash routes). While
   * this is non-zero a request path could match both a static and a dynamic
   * route, so {@link find} must walk the trie to preserve cross-route fallthrough.
   */
  private dynamicCount = 0;

  /** Prints the routing tree structure using the provided logger for debugging purposes. */
  print(looger: { info: (...s: (unknown | unknown)[]) => void }) {
    const printVisitor = (n: Node<T>, path = '') => {
      if (!n) {
        return;
      }
      if (n.wildcard) {
        looger?.info(path, n?.route, n.wildcard);
      } else if (n.handlers?.length) {
        looger?.info(path, n?.route, n.paramName);
      }
      if (n.children) {
        for (const [k, next] of Object.entries(n.children)) {
          // Object.entries returns string keys; charCode trie stores numeric keys as strings
          printVisitor(next, path + String.fromCharCode(k as unknown as number));
        }
      }
    };

    printVisitor(this.tree);
  }

  /** Creates a new router by merging this router's routes with another router's routes. */
  merge(other: Router<T>) {
    const next = new Router<T>();

    const visit = (target: Node<T>, toVisit: Node<T>) => {
      target.route ||= toVisit.route;
      target.wildcard ||= toVisit.wildcard;
      target.paramName ||= toVisit.paramName;
      target.paramNode ||= toVisit.paramNode;
      if (toVisit.handlers) {
        target.handlers ||= [];
        for (const h of toVisit.handlers) {
          target.handlers.push(h);
        }
      }
      if (toVisit.children) {
        for (const [cCode, cNode] of Object.entries(toVisit.children)) {
          target.children ||= {};
          // Object.entries yields string keys; trie children are indexed by numeric char codes
          target.children[cCode as unknown as number] ||= {};
          visit(target.children[cCode as unknown as number], cNode);
        }
      }
    };
    visit(next.tree, this.tree);
    visit(next.tree, other.tree);

    // Rebuild the static fast-path index from both sources. Handler objects are
    // shared with the merged trie (same references), and dynamicCount carries
    // over so a merge that pulls in any dynamic route disables the fast path.
    for (const src of [this, other]) {
      next.dynamicCount += src.dynamicCount;
      for (const [path, entry] of src.staticRoutes) {
        let merged = next.staticRoutes.get(path);
        if (!merged) {
          merged = { route: entry.route, handlers: [], slashes: entry.slashes };
          next.staticRoutes.set(path, merged);
        }
        for (const h of entry.handlers) {
          merged.handlers.push(h);
        }
      }
    }

    return next;
  }

  /** Registers a handler for the given route pattern, supporting path parameters (`[name]`) and wildcards (`*`). */
  add(_route: string, handler: T, option: RouteOptions = {}): Router<T> {
    let route = _route;
    if (typeof route !== 'string') {
      throw new TypeError('Route path must be a string');
    }

    if (route === '') {
      route = '/';
    } else if (route[0] !== '/') {
      route = `/${route}`;
    }
    route = route.trim();

    let n = this.tree;
    const nextChild = (l: number) => {
      if (!n?.children?.[l]) {
        n.children ||= {};
        n.children[l] ||= {};
      }

      return n.children?.[l];
    };

    let prevChar = -1;
    // Whether this route qualifies for the static fast-path index. Cleared the
    // moment a dynamic construct (wildcard, optional segment, or `[param]`) is
    // seen, and re-checked for a trailing slash after the walk.
    let mapEligible = true;

    for (let i = 0; i < route.length; i++) {
      const c = route.charCodeAt(i);

      if (c === CODE_STAR) {
        mapEligible = false;
        n.wildcard ||= [];
        n.wildcard.push({
          handler,
          statusCode: option.statusCode || 200,
          accept: new Set(option.accept || []),
          headMeta: option.headMeta,
          csrfExempt: option.csrfExempt,
          minimal: option.minimal,
          securityHeaders: option.securityHeaders,
          firstPartyErrors: option.firstPartyErrors,
        });
        break;
      }
      if (c === CODE_OPEN_PARENT) {
        mapEligible = false;
        let j = i + 1;
        do {
          j++;
        } while (route.charCodeAt(j) !== CODE_CLOSE_PARENT && j < route.length);

        if (route.charCodeAt(j) !== CODE_CLOSE_PARENT) {
          throw new Error(`Route parsing failed: no closing ')' detected in route "${route}"`);
        }
        i = j;
        continue;
      }
      if (c === CODE_OPEN_BRACKER) {
        mapEligible = false;
        const paramBuf = [];
        let j = i + 1;
        do {
          const p = route.charCodeAt(j);
          paramBuf.push(p);
          j++;
        } while (route.charCodeAt(j) !== CODE_CLOSE_BRACKET && j < route.length);

        if (route.charCodeAt(j) !== CODE_CLOSE_BRACKET) {
          throw new Error(`Route parsing failed: no closing ']' detected in route "${route}"`);
        }
        n.paramName = String.fromCharCode(...paramBuf);
        i = j;

        // Continue processing any characters after the parameter
        if (i + 1 < route.length) {
          const nextChar = route.charCodeAt(i + 1);
          if (nextChar === CODE_SLASH) {
            // Slash after param - use normal children navigation
            n = nextChild(CODE_SLASH);
            i += 1;
            prevChar = CODE_SLASH;
          } else {
            // Non-slash suffix (e.g., .json) - use paramNode
            n.paramNode ||= {};
            n = n.paramNode;
            n = nextChild(nextChar);
            i += 1;
            prevChar = nextChar;
          }
        }
        continue;
      }

      if (prevChar === CODE_SLASH && c === CODE_SLASH) {
        continue;
      }

      n = nextChild(c);
      prevChar = c;
    }
    n.route = route;
    const entryHandler: Handler<T> = {
      handler,
      accept: new Set(option?.accept || []),
      statusCode: option?.statusCode || 200,
      headMeta: option?.headMeta,
      csrfExempt: option?.csrfExempt,
      minimal: option?.minimal,
      securityHeaders: option?.securityHeaders,
      firstPartyErrors: option?.firstPartyErrors,
    };
    n.handlers ||= [];
    n.handlers.push(entryHandler);

    // A trailing slash makes the route unreachable through the trie (request
    // paths are normalised with the trailing slash stripped), so exclude it
    // from the fast path to keep the index faithful to trie semantics.
    if (route.length > 1 && route.charCodeAt(route.length - 1) === CODE_SLASH) {
      mapEligible = false;
    }

    if (mapEligible) {
      this.indexStatic(route, entryHandler);
    } else {
      this.dynamicCount++;
    }

    return this;
  }

  /** Adds a handler to the static fast-path index, sharing the trie's handler object. */
  private indexStatic(route: string, entryHandler: Handler<T>): void {
    let entry = this.staticRoutes.get(route);
    if (!entry) {
      entry = { route, handlers: [], slashes: countSlashes(route) };
      this.staticRoutes.set(route, entry);
    }
    entry.handlers.push(entryHandler);
  }

  /** Finds all handlers matching the given path, sorted by score (best match first). */
  find(_path: string, accept: string[] = []): HandlerResult<T>[] {
    const path = normalizePath(_path);

    // Static fast path: with no dynamic routes registered, an exact-path lookup
    // is provably the complete match set (no `[param]`/`*` route can also match),
    // so we skip the trie traversal, per-node scoring, and the final sort.
    if (this.dynamicCount === 0) {
      const entry = this.staticRoutes.get(path);
      return entry ? matchStaticEntry(entry, accept) : [];
    }

    const matches: NodeMatch<T>[] = [];

    visitNode({ path, n: this.tree, i: 0, matches, requestAccept: accept });

    if (matches.length < 1) {
      return [];
    }

    // Fast path: skip sort for the common single-match case
    if (matches.length === 1) {
      const m = matches[0];
      return [
        {
          handler: m.handler.handler,
          route: m.route,
          score: m.score,
          accept: m.handler.accept,
          statusCode: m.handler.statusCode,
          params: m.params,
          headMeta: m.handler.headMeta,
          csrfExempt: m.handler.csrfExempt,
          minimal: m.handler.minimal,
          securityHeaders: m.handler.securityHeaders,
          firstPartyErrors: m.handler.firstPartyErrors,
        },
      ];
    }

    matches.sort((a, b) => b.score - a.score);

    return matches.map((m) => ({
      handler: m.handler.handler,
      route: m.route,
      score: m.score,
      accept: m.handler.accept,
      statusCode: m.handler.statusCode,
      params: m.params,
      headMeta: m.handler.headMeta,
      csrfExempt: m.handler.csrfExempt,
      minimal: m.handler.minimal,
      securityHeaders: m.handler.securityHeaders,
      firstPartyErrors: m.handler.firstPartyErrors,
    }));
  }
}

/** Counts `/` characters in a route — the slash count the trie reaches at the match node. */
function countSlashes(route: string): number {
  let count = 0;
  for (let i = 0; i < route.length; i++) {
    if (route.charCodeAt(i) === CODE_SLASH) {
      count++;
    }
  }
  return count;
}

/**
 * Content-negotiation score for a static handler, mirroring the trie's
 * `addMatch` logic: 1 on an exact Accept match, 0.5 when the handler is
 * unconstrained or the request accepts anything (`*​/*` or no Accept), 0
 * (filtered out) otherwise.
 */
function scoreAccept<T>(handler: Handler<T>, requestAccept: string[]): number {
  let acceptScore = requestAccept.some((item) => handler.accept.has(item)) ? 1 : 0;
  if (acceptScore === 0 && (handler.accept.size === 0 || requestAccept.length === 0 || requestAccept.includes('*/*'))) {
    acceptScore = 0.5;
  }
  return acceptScore;
}

/**
 * Builds the {@link HandlerResult} list for a static index hit, reproducing the
 * score (`slashes * 10 * acceptScore`) and best-first ordering the trie would
 * have produced for the same exact path.
 */
function matchStaticEntry<T>(entry: StaticEntry<T>, requestAccept: string[]): HandlerResult<T>[] {
  const results: HandlerResult<T>[] = [];
  for (const handler of entry.handlers) {
    const acceptScore = scoreAccept(handler, requestAccept);
    if (acceptScore === 0) {
      continue;
    }
    results.push({
      handler: handler.handler,
      route: entry.route,
      score: entry.slashes * 10 * acceptScore,
      accept: handler.accept,
      statusCode: handler.statusCode,
      params: undefined,
      headMeta: handler.headMeta,
      csrfExempt: handler.csrfExempt,
      minimal: handler.minimal,
      securityHeaders: handler.securityHeaders,
      firstPartyErrors: handler.firstPartyErrors,
    });
  }
  if (results.length > 1) {
    results.sort((a, b) => b.score - a.score);
  }
  return results;
}

const normalizePath = (_path: string): string => {
  let path = _path;
  if (path === '') {
    path = '/';
  } else if (path[0] !== '/') {
    path = `/${path}`;
  }

  while (path.length > 1 && path.endsWith('/')) {
    path = path.substring(0, path.length - 1);
  }

  return path;
};

/**
 * Finds the start position of a suffix pattern (e.g., .json) in the path
 * for a parameter node that has a paramNode with children.
 *
 * @param paramNode - The parameter node that may have a suffix pattern
 * @param path - The path being matched
 * @param startIndex - The starting index in the path
 * @returns The index where the suffix starts, or -1 if not found
 */
function findSuffixStart<T>(paramNode: Node<T>, path: string, startIndex: number): number {
  if (!paramNode.children) {
    return -1;
  }

  for (let j = startIndex; j < path.length; j++) {
    const c = path.charCodeAt(j);
    if (c === CODE_SLASH) {
      break;
    }
    const first = paramNode.children?.[c];
    if (!first) {
      continue;
    }

    // Traverse the suffix pattern to see if it matches
    let node: Node<T> | undefined = first;
    let k = j + 1;
    while (node && k < path.length && path.charCodeAt(k) !== CODE_SLASH) {
      const next: Node<T> | undefined = node.children?.[path.charCodeAt(k)];
      if (!next) {
        node = undefined;
        break;
      }
      node = next;
      k++;
    }

    if (!node) {
      continue;
    }

    // Check if we're at a boundary (end of path or slash)
    const atBoundary = k === path.length || path.charCodeAt(k) === CODE_SLASH;
    if (!atBoundary) {
      continue;
    }

    // Check if this node has handlers or can continue with a slash
    if (node.handlers || node.children?.[CODE_SLASH]) {
      return j;
    }
  }

  return -1;
}

/**
 * Extracts the parameter value from the path, handling both regular parameters
 * and parameters with suffix patterns (e.g., /users/[id].json).
 *
 * @param node - The node containing the parameter
 * @param path - The path being matched
 * @param startIndex - The starting index in the path
 * @returns An object containing the parameter buffer and the new index
 */
function extractParameterValue<T>(
  node: Node<T>,
  path: string,
  startIndex: number,
): { value: string; newIndex: number } {
  const hasSuffix = !!node.paramNode;
  let suffixStart = -1;

  if (hasSuffix && node.paramNode?.children) {
    suffixStart = findSuffixStart(node.paramNode, path, startIndex);
  }

  if (suffixStart >= startIndex) {
    return { value: path.slice(startIndex, suffixStart), newIndex: suffixStart };
  }

  // Extract until slash
  let i = startIndex;
  while (i < path.length && path.charCodeAt(i) !== CODE_SLASH) {
    i++;
  }

  return { value: path.slice(startIndex, i), newIndex: i };
}

/**
 * Score a content-negotiated match for `handler` and push it into `ctx.matches`.
 * The accept scoring lives in {@link scoreAccept} (shared with the static fast
 * path `matchStaticEntry`) so the trie walk and the static index cannot diverge.
 */
function pushMatch<T>(
  ctx: Context<T>,
  node: Node<T>,
  coef: number,
  slashesCounter: number,
  handler: Handler<T>,
  currentParams?: Record<string, string>,
): void {
  if (slashesCounter === 0) {
    return;
  }
  const acceptScore = scoreAccept(handler, ctx.requestAccept);
  if (acceptScore === 0) {
    return;
  }
  const match: NodeMatch<T> = {
    route: node.route as string,
    handler,
    score: slashesCounter * coef * acceptScore,
  };
  if (currentParams) {
    match.params = { ...currentParams };
  }
  ctx.matches.push(match);
}

/** Capture the remaining path as the `*` parameter for catch-all routes. */
function visitWildcard<T>(ctx: Context<T>, slashesCounter: number): void {
  const { n, path, i, params } = ctx;
  if (!n.wildcard) {
    return;
  }
  const remainingPath = path.slice(i);
  for (const handler of n.wildcard) {
    let currentParams = params;
    if (remainingPath) {
      currentParams = {
        ...params,
        '*': remainingPath.startsWith('/') ? remainingPath.slice(1) : remainingPath,
      };
    }
    pushMatch(ctx, n, 0.1, slashesCounter, handler, currentParams);
  }
}

/**
 * Descend a parameter node: extract the segment value, try a suffix match
 * through `paramNode`, record a leaf handler, then continue past the next slash.
 */
function visitParamNode<T>(ctx: Context<T>, startIndex: number, slashesCounter: number): void {
  const { n, path, params, matches, requestAccept } = ctx;
  if (!n.paramName) {
    return;
  }

  const paramResult = extractParameterValue(n, path, startIndex);
  const i = paramResult.newIndex;
  const currentParams = { ...params, [n.paramName]: paramResult.value };

  // Check for suffix match through paramNode
  if (n.paramNode && i < path.length && path.charCodeAt(i) !== CODE_SLASH) {
    const suffixNext = n.paramNode.children?.[path.charCodeAt(i)];
    if (suffixNext) {
      visitNode({ path, n: suffixNext, i: i + 1, matches, slashesCounter, params: currentParams, requestAccept });
    }
  }

  if (n.handlers && i === path.length) {
    for (const handler of n.handlers) {
      pushMatch(ctx, n, 0.3, slashesCounter, handler, currentParams);
    }
  }

  const next = n.children?.[CODE_SLASH];
  if (next) {
    visitNode({ path, n: next, i: i + 1, matches, slashesCounter, params: currentParams, requestAccept });
  }
}

function visitNode<T>(context: Context<T>): void {
  const { n, path, matches, requestAccept, params } = context;
  const i = context.i;
  let slashesCounter = context.slashesCounter ?? 0;

  if (n.wildcard) {
    visitWildcard(context, slashesCounter);
  }

  if (!path[i]) {
    if (n.handlers && !n.paramName) {
      for (const handler of n.handlers) {
        pushMatch(context, n, 10, slashesCounter, handler, params);
      }
    }
    return;
  }

  const c = path.charCodeAt(i);
  if (c === CODE_SLASH) {
    slashesCounter++;
  }
  if (c === CODE_QUESTION) {
    return;
  }

  const staticChild = n.children?.[c];
  if (staticChild) {
    visitNode({ path, n: staticChild, i: i + 1, matches, slashesCounter, params, requestAccept });
  }

  if (n.paramName) {
    visitParamNode(context, i, slashesCounter);
  }
}
