import { file } from 'bun';
import {
  type HttpRequestContext,
  type HttpPlugin,
  HttpResponse,
  type RouteOptions,
  type RouteHandler,
} from '@putnami/application';
import { joinPath } from '@putnami/utils';
import { LOADER_REQUEST_HEADER } from '../shared/client-redirect';
import { followableRedirects } from './followable-redirect';
import type { LoaderCacheOptions, LoaderDefinition } from './loader';
import type { LazyLoaderModule, LoaderModule } from './module.types';
import type { ReactModuleCache } from './react-module-cache';
import { buildJsonResponseWithCache, executeLoaderWithCache } from './route-cache.utils';
import { applyMiddlewareSource, type MiddlewareSource } from './route-middleware.utils';
import { inModuleOrDefault } from './react-ssr.utils';

/**
 * Wraps a loader JSON handler with its route middleware, and answers a redirect
 * to the client loader in the form it can follow (see {@link followableRedirects}).
 */
function loaderRoute(handler: RouteHandler, middleware: MiddlewareSource): RouteHandler {
  return followableRedirects(applyMiddlewareSource(handler, middleware), LOADER_REQUEST_HEADER);
}

export function registerJsonEndpoint(
  httpPlugin: HttpPlugin,
  route: string,
  loader: (ctx: HttpRequestContext) => unknown,
  suffix: string,
  middleware: MiddlewareSource,
  cacheOpts: LoaderCacheOptions | undefined,
  routeOptions: RouteOptions = {},
): void {
  const handler: RouteHandler = async (ctx) => {
    const data = await executeLoaderWithCache(loader, ctx, route, cacheOpts);
    return buildJsonResponseWithCache(data, ctx, cacheOpts);
  };

  httpPlugin.get(`${route}${suffix}`, loaderRoute(handler, middleware), {
    ...routeOptions,
    accept: ['application/json', '*/*'],
  });
}

export function registerLazyJsonEndpoint(
  httpPlugin: HttpPlugin,
  moduleCache: ReactModuleCache,
  route: string,
  lazyLoader: LazyLoaderModule,
  suffix: string,
  middleware: MiddlewareSource,
  routeOptions: RouteOptions = {},
): void {
  const handler: RouteHandler = async (ctx) => {
    const loaderModule = await moduleCache.loadModule(`loader:${route}${suffix}`, lazyLoader);
    const loaderDef = inModuleOrDefault<LoaderDefinition, LoaderModule>(loaderModule, 'loader');
    if (!loaderDef) {
      return HttpResponse.json(null, { status: 200 });
    }

    const cacheOpts = loaderDef.cache;
    const data = await executeLoaderWithCache(loaderDef.handler, ctx, route, cacheOpts);
    return buildJsonResponseWithCache(data, ctx, cacheOpts);
  };

  httpPlugin.get(`${route}${suffix}`, loaderRoute(handler, middleware), {
    ...routeOptions,
    accept: ['application/json', '*/*'],
  });
}

export function registerScriptEndpoint(
  httpPlugin: HttpPlugin,
  route: string,
  path: string,
  outdir: string,
  routeOptions: RouteOptions = {},
): void {
  const headers: Record<string, string> = {
    'Cache-Control': 'public, max-age=31536000, immutable',
    'Content-Disposition': `filename="${route.substring(1)}"`,
    'Content-Type': route.endsWith('.js') ? 'application/javascript' : 'application/json',
  };
  if (path.endsWith('.gz')) {
    headers['Content-Encoding'] = 'gzip';
  }
  const filePath = joinPath(outdir, path);
  httpPlugin.get(route, () => new HttpResponse(file(filePath), { headers }), routeOptions);
}
