import { describe, expect, it } from 'bun:test';
import {
  type HttpMiddleware,
  type HttpPlugin,
  type HttpRequestContext,
  HttpResponse,
  type RouteHandler,
  redirect,
} from '@putnami/application';
import type { RouteObject } from 'react-router';
import {
  CLIENT_REDIRECT_HEADER,
  CLIENT_REDIRECT_STATUS_HEADER,
  LOADER_REQUEST_HEADER,
} from '../../src/shared/client-redirect';
import { registerRouteLoader } from '../../src/ssr/route-registration.utils';

const createContext = (fromClientLoader: boolean) =>
  ({
    req: new Request('http://localhost/tasks/7.json'),
    headers: new Headers(fromClientLoader ? { [LOADER_REQUEST_HEADER]: '1' } : {}),
    params: {},
    route: '/tasks/[id]',
    queryParams: () => ({}),
    body: async () => undefined,
    statusCode: 0,
  }) as unknown as HttpRequestContext;

/** Registers one loader and returns the handler its JSON endpoint got. */
function registerLoader(
  handler: (ctx: HttpRequestContext) => unknown,
  opts: { middleware?: HttpMiddleware[]; isLazy?: boolean; ttl?: number } = {},
): RouteHandler {
  let registered: RouteHandler | undefined;
  const httpPlugin = {
    get: (_route: string, routeHandler: RouteHandler) => {
      registered = routeHandler;
    },
  } as unknown as HttpPlugin;
  const loaderModule = { default: { handler, cache: opts.ttl ? { ttl: opts.ttl } : undefined } };

  registerRouteLoader({
    httpPlugin,
    cache: { loadModule: async () => loaderModule } as never,
    route: '/tasks/[id]',
    httpRoute: '/tasks/:id',
    node: {} as RouteObject,
    loader: (opts.isLazy ? async () => loaderModule : loaderModule) as never,
    isLazy: opts.isLazy ?? false,
    cacheKeyPrefix: 'loader',
    suffix: '.json',
    middleware: opts.middleware,
  });

  if (!registered) throw new Error('registerRouteLoader registered no GET route');
  return registered;
}

describe('loader JSON endpoint — redirects for the client loader', () => {
  it('answers a thrown redirect with a 204 that carries its location and status', async () => {
    const route = registerLoader(() => redirect('/login', 307));

    const result = (await route(createContext(true))) as HttpResponse;

    expect(result).toBeInstanceOf(HttpResponse);
    expect(result.status).toBe(204);
    expect(result.getHeader(CLIENT_REDIRECT_HEADER)).toBe('/login');
    expect(result.getHeader(CLIENT_REDIRECT_STATUS_HEADER)).toBe('307');
    expect(result.getHeader('Location')).toBeUndefined();
  });

  it('answers a redirect from a lazy loader the same way', async () => {
    const route = registerLoader(() => redirect('/login'), { isLazy: true });

    const result = (await route(createContext(true))) as HttpResponse;

    expect(result.status).toBe(204);
    expect(result.getHeader(CLIENT_REDIRECT_HEADER)).toBe('/login');
  });

  it('answers a redirect from a cached loader the same way, and never caches it', async () => {
    let signedIn = false;
    const route = registerLoader(() => (signedIn ? { id: 7 } : redirect('/login')), { ttl: 60 });

    const first = (await route(createContext(true))) as HttpResponse;
    signedIn = true;
    const second = (await route(createContext(true))) as HttpResponse;

    expect(first.status).toBe(204);
    expect(first.getHeader(CLIENT_REDIRECT_HEADER)).toBe('/login');
    expect(second.status).toBe(200);
    expect(second.rawData()).toEqual({ id: 7 });
  });

  it('answers a redirect from the route middleware the same way and keeps its other headers', async () => {
    const login: HttpMiddleware = async () =>
      new HttpResponse(undefined, {
        status: 302,
        headers: [
          ['Location', '/login'],
          ['Set-Cookie', 'return=/tasks/7'],
        ],
      });
    const route = registerLoader(() => ({ id: 7 }), { middleware: [login] });

    const result = (await route(createContext(true))) as HttpResponse;

    expect(result.status).toBe(204);
    expect(result.getHeader(CLIENT_REDIRECT_HEADER)).toBe('/login');
    expect(result.getHeader('Set-Cookie')).toBe('return=/tasks/7');
  });

  it('leaves the loader data unchanged', async () => {
    const route = registerLoader(() => ({ id: 7 }));

    const result = (await route(createContext(true))) as HttpResponse;

    expect(result.status).toBe(200);
    expect(result.rawData()).toEqual({ id: 7 });
  });

  it('rethrows a failure that is no redirect', async () => {
    const route = registerLoader(() => {
      throw new Error('boom');
    });

    await expect(route(createContext(true))).rejects.toThrow('boom');
  });

  it('keeps the real redirect for a request that is not the client loader', async () => {
    const route = registerLoader(() => redirect('/login'));

    const thrown = await Promise.resolve(route(createContext(false))).catch((error: unknown) => error);

    expect(thrown).toBeInstanceOf(HttpResponse);
    expect((thrown as HttpResponse).status).toBe(302);
    expect((thrown as HttpResponse).getHeader('Location')).toBe('/login');
  });
});
