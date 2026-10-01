import type { HttpMiddleware, HttpPlugin, RouteHandler, RouteOptions } from '@putnami/application';
import type { ReactNode } from 'react';
import type { LoaderFunction, RouteObject } from 'react-router';
import { generatePageId, type RouteTreeHelper } from '../shared/route-tree.utils';
import type { ActionDefinition } from './action';
import RootHtml from './default.html';
import { actionHandler, loaderHandler, recordActionFailure, recordActionOutcome } from './handlers';
import type { LoaderDefinition } from './loader';
import type {
  ActionModule,
  LazyActionModule,
  LazyLoaderModule,
  LazyPageModule,
  LoaderModule,
  PageModule,
} from './module.types';
import type { PageCacheOptions } from './page';
import { pageRenderer } from './page.renderer';
import type { LazyPageData, MiddlewareResolver, ReactModuleCache } from './react-module-cache';
import type { ReactPluginConfig } from './react-plugin';
import type { PutnamiReactConfigExtras } from './react-ssr.config';
import { inModuleOrDefault } from './react-ssr.utils';
import { evictPatterns, wrapPageWithCache } from './route-cache.utils';
import { registerJsonEndpoint, registerLazyJsonEndpoint } from './route-endpoint.utils';
import { applyMiddlewareSource, type MiddlewareSource, wrapWithMiddleware } from './route-middleware.utils';
import type { StaticRouteMeta } from './static';

type ReactRuntimeConfig = ReactPluginConfig & PutnamiReactConfigExtras;

/**
 * Options accepted by `ReactApplication.reactPage`. Shared with collaborators
 * (e.g. not-found registration) that register catch-all page routes.
 */
export interface ReactPageOptions {
  page: ReactNode | PageModule | LazyPageModule;
  loader?: LoaderModule | LazyLoaderModule;
  action?: ActionModule | LazyActionModule;
  statusCode?: number;
  notFoundMiddleware?: HttpMiddleware[];
  /**
   * Static render metadata, emitted by the generator when the page used
   * `.static()`. When present the HTML route is served from the
   * pre-rendered static output (SSG/ISR) instead of rendered per request.
   */
  static?: StaticRouteMeta;
}

/**
 * Find or create a page route node within the nearest layout for `route`.
 */
export function ensurePageNode(routeHelper: RouteTreeHelper<RouteObject>, route: string): RouteObject {
  const { layout, remainingPath } = routeHelper.findNearestLayout(route);
  layout.children ||= [];

  const pageId = generatePageId(layout.id || 'root', remainingPath);
  const isIndex = remainingPath === '';

  const existingNode = layout.children.find((c: RouteObject) => c.path === remainingPath && !c.id?.endsWith('-layout'));

  if (existingNode) {
    existingNode.id = pageId;
    if (isIndex) existingNode.index = true;
    return existingNode;
  }

  const pageNode: RouteObject = { path: remainingPath, id: pageId, index: isIndex };
  layout.children.push(pageNode);
  return pageNode;
}

/**
 * Shared rendering dependencies for a ReactApplication's page renderers: the
 * runtime config plus live accessors for the route tree, basename, and
 * hydration script (read lazily so they reflect later mutations).
 */
export interface PageRenderContext {
  config: ReactRuntimeConfig;
  routes: () => RouteObject[];
  basename: () => string | undefined;
  hydrateScript: () => string;
}

/**
 * Build the SSR page renderer for a route, wrapped with server-side caching
 * when the page sets a TTL. Shared by eager pages and the lazy first-request
 * renderer.
 */
export function createPageRenderer(
  ctx: PageRenderContext,
  route: string,
  cacheOpts: PageCacheOptions | undefined,
): RouteHandler {
  const baseRenderer = pageRenderer(
    ctx.routes,
    () => ctx.config.page || RootHtml,
    ctx.hydrateScript,
    ctx.config.ssrTimeout,
    ctx.basename,
    ctx.config.clientFetchTimeout,
  );
  return wrapPageWithCache(baseRenderer, route, cacheOpts);
}

/**
 * Resolve lazy page data (definition + middleware + status) with LRU caching.
 * `resolveMiddleware` composes the route's ancestor-layout chain (loading lazy
 * layouts at request time, see {@link RouteMiddlewareResolver}) with the page's
 * own middleware, so a lazy `.secure()` layout is enforced on its descendants.
 */
export function resolveLazyPageData(
  cache: ReactModuleCache,
  resolveMiddleware: MiddlewareResolver,
  route: string,
  lazyPage: LazyPageModule,
  notFoundMiddleware?: HttpMiddleware[],
): Promise<LazyPageData> {
  return cache.resolveLazyPageData(route, lazyPage, resolveMiddleware, notFoundMiddleware);
}

/**
 * Create a renderer that loads a lazy page module on first request, caching the
 * resolved renderer, middleware, and status code for subsequent requests.
 */
export function createLazyPageRenderer(
  ctx: PageRenderContext,
  cache: ReactModuleCache,
  resolveMiddleware: MiddlewareResolver,
  route: string,
  lazyPage: LazyPageModule,
  notFoundMiddleware?: HttpMiddleware[],
): RouteHandler {
  let cachedRenderer: RouteHandler | undefined;
  let cachedMiddleware: HttpMiddleware[] | undefined;
  let cachedStatusCode: number | undefined;

  return async (reqCtx) => {
    if (!cachedRenderer) {
      const { pageDef, middleware, statusCode } = await resolveLazyPageData(
        cache,
        resolveMiddleware,
        route,
        lazyPage,
        notFoundMiddleware,
      );
      cachedMiddleware = middleware;
      cachedStatusCode = statusCode;
      cachedRenderer = createPageRenderer(ctx, route, pageDef?.cache);
    }

    if (cachedStatusCode !== undefined) {
      reqCtx.statusCode = cachedStatusCode;
    }

    const handler = cachedMiddleware ? wrapWithMiddleware(cachedRenderer, cachedMiddleware) : cachedRenderer;
    return handler(reqCtx);
  };
}

/** Parameters for {@link registerRouteLoader}. */
interface RouteLoaderRegistration {
  httpPlugin: HttpPlugin;
  cache: ReactModuleCache;
  /** Original route, used to key the lazy module cache. */
  route: string;
  /** HTTP route (bracket form) the JSON endpoint is registered under. */
  httpRoute: string;
  /** Route node the React-Router loader is attached to. */
  node: RouteObject;
  loader: LoaderModule | LazyLoaderModule;
  isLazy: boolean;
  /** Module-cache key prefix (`loader` for pages, `layout-loader` for layouts). */
  cacheKeyPrefix: string;
  /** JSON endpoint suffix (`.json` for pages, `-layout.json` for layouts). */
  suffix: string;
  /**
   * Layout + page middleware for the endpoint. A thunk (rather than an array) is
   * used when an ancestor layout is lazy and its `.secure()` can only be
   * resolved at request time (see {@link RouteMiddlewareResolver}).
   */
  middleware: MiddlewareSource;
  routeOptions?: RouteOptions;
}

/**
 * Register the loader JSON endpoint for a route (lazy or eager) and attach the
 * React-Router loader to its route node. Shared by pages and layouts.
 */
export function registerRouteLoader(reg: RouteLoaderRegistration): void {
  const {
    httpPlugin,
    cache,
    route,
    httpRoute,
    node,
    loader,
    isLazy,
    cacheKeyPrefix,
    suffix,
    middleware,
    routeOptions,
  } = reg;

  if (isLazy) {
    const lazyLoader = loader as LazyLoaderModule;
    const lazyLoaderHandler: RouteHandler = async (ctx) => {
      const loaderModule = await cache.loadModule(`${cacheKeyPrefix}:${route}`, lazyLoader);
      const loaderDef = inModuleOrDefault<LoaderDefinition, LoaderModule>(loaderModule, 'loader');
      if (loaderDef) {
        return loaderDef.handler(ctx);
      }
      return undefined;
    };
    node.loader = loaderHandler(lazyLoaderHandler) as LoaderFunction;
    registerLazyJsonEndpoint(httpPlugin, cache, httpRoute, lazyLoader, suffix, middleware, routeOptions);
    return;
  }

  const loaderDef = inModuleOrDefault<LoaderDefinition, LoaderModule>(loader as LoaderModule, 'loader');
  if (loaderDef) {
    const loaderFn = loaderDef.handler;
    node.loader = loaderHandler(loaderFn) as LoaderFunction;
    registerJsonEndpoint(httpPlugin, httpRoute, loaderFn, suffix, middleware, loaderDef.cache, routeOptions);
  }
}

/** Parameters for {@link registerRouteAction}. */
interface RouteActionRegistration {
  httpPlugin: HttpPlugin;
  cache: ReactModuleCache;
  route: string;
  httpRoute: string;
  node: RouteObject;
  action: ActionModule | LazyActionModule;
  isLazy: boolean;
  /**
   * Layout + page middleware for the action. A thunk (rather than an array) is
   * used when an ancestor layout is lazy and its `.secure()` can only be
   * resolved at request time (see {@link RouteMiddlewareResolver}).
   */
  middleware: MiddlewareSource;
  routeOptions?: RouteOptions;
}

/**
 * Register the POST action endpoint for a page (lazy or eager) and attach the
 * React-Router action to its route node. Evicts the action's cache patterns
 * after a successful run.
 */
export function registerRouteAction(reg: RouteActionRegistration): void {
  const { httpPlugin, cache, route, httpRoute, node, action, isLazy, middleware, routeOptions } = reg;
  const postOptions: RouteOptions = { ...routeOptions, accept: ['application/json', '*/*'] };

  if (isLazy) {
    const lazyAction = action as LazyActionModule;
    const lazyActionHandler: RouteHandler = async (ctx) => {
      const actionModule = await cache.loadModule(`action:${route}`, lazyAction);
      const actionDef = inModuleOrDefault<ActionDefinition, ActionModule>(actionModule, 'action');
      if (actionDef) {
        const result = await actionDef.handler(ctx);
        // Evict cache patterns after successful action
        await evictPatterns(actionDef.evict);
        return result;
      }
      return undefined;
    };
    node.action = actionHandler(lazyActionHandler);
    httpPlugin.post(httpRoute, applyMiddlewareSource(publishing(lazyActionHandler), middleware), postOptions);
    return;
  }

  const actionDef = inModuleOrDefault<ActionDefinition, ActionModule>(action as ActionModule, 'action');
  if (actionDef) {
    const actionFn: RouteHandler = async (ctx) => {
      const result = await actionDef.handler(ctx);
      // Evict cache patterns after successful action
      await evictPatterns(actionDef.evict);
      return result;
    };
    node.action = actionHandler(actionFn);
    httpPlugin.post(httpRoute, applyMiddlewareSource(publishing(actionFn), middleware), postOptions);
  }
}

/**
 * Publishes the action outcome on the request context, and changes nothing
 * else about the response.
 *
 * The React Router route node is wrapped in `actionHandler`, which already
 * writes the slot — but that node only runs in the browser. A form POST, with
 * or without JavaScript, reaches this HTTP route instead, so without this
 * wrapper the slot is never written server-side and a reader of it observes no
 * submission at all.
 *
 * @param handler - The action route handler.
 * @returns The same handler, publishing the outcome on the way out.
 */
function publishing(handler: RouteHandler): RouteHandler {
  return async (ctx) => {
    try {
      const result = await handler(ctx);
      recordActionOutcome(ctx, result);
      return result;
    } catch (error) {
      recordActionFailure(ctx, error);
      throw error;
    }
  };
}
