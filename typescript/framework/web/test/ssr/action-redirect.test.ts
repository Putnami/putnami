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
  ACTION_REQUEST_HEADER,
} from '../../src/shared/client-redirect';
import { registerRouteAction } from '../../src/ssr/route-registration.utils';

const createContext = (enhanced: boolean) =>
  ({
    req: new Request('http://localhost/tasks/7', { method: 'POST' }),
    headers: new Headers(enhanced ? { [ACTION_REQUEST_HEADER]: '1' } : {}),
    params: {},
    route: '/tasks/[id]',
    queryParams: () => ({}),
    body: async () => undefined,
    statusCode: 0,
  }) as unknown as HttpRequestContext;

/** Registers one eager action and returns the handler its HTTP route got. */
function registerAction(
  handler: (ctx: HttpRequestContext) => unknown,
  middleware?: HttpMiddleware[],
  isLazy = false,
): RouteHandler {
  let registered: RouteHandler | undefined;
  const httpPlugin = {
    post: (_route: string, routeHandler: RouteHandler) => {
      registered = routeHandler;
    },
  } as unknown as HttpPlugin;
  const actionModule = { default: { handler } };

  registerRouteAction({
    httpPlugin,
    cache: { loadModule: async () => actionModule } as never,
    route: '/tasks/[id]',
    httpRoute: '/tasks/:id',
    node: {} as RouteObject,
    action: (isLazy ? async () => actionModule : actionModule) as never,
    isLazy,
    middleware,
  });

  if (!registered) throw new Error('registerRouteAction registered no POST route');
  return registered;
}

describe('action route — redirects for the client action', () => {
  it('answers a thrown redirect with a 204 that carries its location and status', async () => {
    const route = registerAction(() => redirect('/tasks', 303));

    const result = (await route(createContext(true))) as HttpResponse;

    expect(result).toBeInstanceOf(HttpResponse);
    expect(result.status).toBe(204);
    expect(result.getHeader(CLIENT_REDIRECT_HEADER)).toBe('/tasks');
    expect(result.getHeader(CLIENT_REDIRECT_STATUS_HEADER)).toBe('303');
    expect(result.getHeader('Location')).toBeUndefined();
  });

  it('answers a returned redirect the same way and keeps its other headers', async () => {
    const route = registerAction(
      () =>
        new HttpResponse(undefined, {
          status: 302,
          headers: [
            ['Location', '/tasks'],
            ['Set-Cookie', 'session=abc'],
          ],
        }),
    );

    const result = (await route(createContext(true))) as HttpResponse;

    expect(result.status).toBe(204);
    expect(result.getHeader(CLIENT_REDIRECT_HEADER)).toBe('/tasks');
    expect(result.getHeader(CLIENT_REDIRECT_STATUS_HEADER)).toBe('302');
    expect(result.getHeader('Set-Cookie')).toBe('session=abc');
  });

  it('answers a redirect from a lazy action the same way', async () => {
    const route = registerAction(() => redirect('/tasks'), undefined, true);

    const result = (await route(createContext(true))) as HttpResponse;

    expect(result.status).toBe(204);
    expect(result.getHeader(CLIENT_REDIRECT_HEADER)).toBe('/tasks');
  });

  it('answers a redirect from the route middleware the same way', async () => {
    const login: HttpMiddleware = async () => HttpResponse.redirect('/login');
    const route = registerAction(() => ({ ok: true }), [login]);

    const result = (await route(createContext(true))) as HttpResponse;

    expect(result.status).toBe(204);
    expect(result.getHeader(CLIENT_REDIRECT_HEADER)).toBe('/login');
  });

  it('leaves a response that is no redirect unchanged', async () => {
    const response = HttpResponse.json({ errors: {} }, { status: 422 });
    const route = registerAction(() => response);

    expect(await route(createContext(true))).toBe(response);
  });

  it('rethrows a failure that is no redirect', async () => {
    const route = registerAction(() => {
      throw new Error('boom');
    });

    await expect(route(createContext(true))).rejects.toThrow('boom');
  });

  it('keeps the real redirect for a plain HTML form POST', async () => {
    const route = registerAction(() => redirect('/tasks'));

    const thrown = await Promise.resolve(route(createContext(false))).catch((error: unknown) => error);

    expect(thrown).toBeInstanceOf(HttpResponse);
    expect((thrown as HttpResponse).status).toBe(302);
    expect((thrown as HttpResponse).getHeader('Location')).toBe('/tasks');
  });
});
