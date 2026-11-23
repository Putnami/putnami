import { afterEach, describe, expect, it } from 'bun:test';
import { api } from '../../src/api/api.plugin';
import { endpoint } from '../../src/api/route/endpoint';
import { application } from '../../src/application';
import type { HttpRequestContext } from '../../src/http/http-context.type';
import { http, type HttpPlugin } from '../../src/http/http.plugin';
import { HttpResponse } from '../../src/http/http-response';
import {
  nativeEndpoints,
  patternsMayOverlap,
  type RegisteredRoute,
  servedNatively,
} from '../../src/http/native-endpoints';
import type { RouteController } from '../../src/http/route.controller';

const endpointRoute = (method: RegisteredRoute['method'], path: string): RegisteredRoute => ({
  method,
  path,
  handler: servedNatively(() => ({ ok: true })),
  options: { accept: ['application/json'] },
});
const plainRoute = (method: RegisteredRoute['method'], path: string): RegisteredRoute => ({
  method,
  path,
  handler: () => ({ ok: true }),
});
const mapped = (routes: RegisteredRoute[]) => nativeEndpoints(routes).map((e) => `${e.method} ${e.bunPath}`);

describe('nativeEndpoints', () => {
  it('maps a marked endpoint alone at its pattern, in Bun syntax', () => {
    expect(mapped([endpointRoute('GET', '/users/[id]'), endpointRoute('POST', 'users')])).toEqual([
      'GET /users/:id',
      'POST /users',
    ]);
  });

  it('leaves out a route that is not a marked endpoint', () => {
    expect(mapped([plainRoute('GET', '/health'), endpointRoute('GET', '/items')])).toEqual(['GET /items']);
  });

  it('leaves out overlapping patterns of one method, and only of that method', () => {
    const routes = [
      endpointRoute('GET', '/users/[id]'),
      endpointRoute('GET', '/users/me'),
      endpointRoute('POST', '/users/me'),
      endpointRoute('GET', '/users/[id]/tasks'),
    ];
    expect(mapped(routes)).toEqual(['GET /users/:id/tasks', 'POST /users/me']);
  });

  it('leaves out a pattern with two handlers', () => {
    expect(mapped([endpointRoute('GET', '/items'), endpointRoute('GET', '/items')])).toEqual([]);
  });

  it('maps no route of a method with a wildcard, an optional or a suffixed pattern', () => {
    for (const other of ['/[...rest]', '/files/*', '/report.[format]', '/docs/(lang)', '/trailing/']) {
      expect(
        mapped([endpointRoute('GET', '/items'), plainRoute('GET', other), endpointRoute('PUT', '/items')]),
      ).toEqual(['PUT /items']);
    }
  });
});

describe('patternsMayOverlap', () => {
  it('tells whether two patterns may share a path, in either syntax', () => {
    expect(patternsMayOverlap('/files/latest', '/files/*')).toBe(true);
    expect(patternsMayOverlap('/users/[id]', '/users/:userId')).toBe(true);
    expect(patternsMayOverlap('/users/[id]', '/assets/*')).toBe(false);
    expect(patternsMayOverlap('/users/[id]', '/users')).toBe(false);
    expect(patternsMayOverlap('/users/[id]', '/[...rest]')).toBe(true);
    expect(patternsMayOverlap('/users/[id]', '/report.[format]')).toBe(true);
  });
});

describe('endpoints served through Bun routes', () => {
  let stop: (() => Promise<void>) | undefined;
  afterEach(async () => {
    await stop?.();
    stop = undefined;
  });

  /** Starts an api app and counts the requests the JS router matches. */
  async function start(configure?: (plugin: HttpPlugin) => void) {
    const httpPlugin = http({ port: 0 });
    configure?.(httpPlugin);
    const plugin = api({ autoScan: false });
    plugin.register(
      '/users/[id]',
      endpoint().handle((ctx: HttpRequestContext) => ({ id: ctx.params?.['id'], route: ctx.route })),
      'GET',
    );
    plugin.register(
      '/users',
      endpoint().handle(() => HttpResponse.json({ created: true }, { status: 201 })),
      'POST',
    );
    const app = application().use(httpPlugin).use(plugin);
    await app.start();
    stop = () => app.stop();
    const router = (httpPlugin as unknown as { router: RouteController }).router;
    const find = router.find.bind(router);
    let routed = 0;
    router.find = ((ctx: HttpRequestContext) => {
      routed++;
      return find(ctx);
    }) as RouteController['find'];
    return { base: `http://localhost:${httpPlugin.getServer()?.port}`, routed: () => routed };
  }

  it('matches an endpoint in Bun and runs the whole pipeline on it', async () => {
    const { base, routed } = await start();
    const res = await fetch(`${base}/users/42`);
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ id: '42', route: '/users/[id]' });
    expect(res.headers.get('X-Content-Type-Options')).toBe('nosniff');
    const created = await fetch(`${base}/users`, { method: 'POST', body: '{}' });
    expect(created.status).toBe(201);
    await created.arrayBuffer();
    expect(routed()).toBe(0);
  });

  it('hands a request the JS router could answer differently back to it, with the same answer', async () => {
    const { base, routed } = await start();
    const encoded = await fetch(`${base}/users/a%20b`);
    expect(await encoded.json()).toEqual({ id: 'a%20b', route: '/users/[id]' });
    const head = await fetch(`${base}/users/42`, { method: 'HEAD' });
    expect(head.status).toBe(200);
    expect(await head.text()).toBe('');
    const slash = await fetch(`${base}/users/42/`);
    expect(await slash.json()).toEqual({ id: '42', route: '/users/[id]' });
    // HEAD matches twice: once in fetch, once more as GET in the HTTP methods middleware.
    expect(routed()).toBe(4);
  });

  it('keeps 405, OPTIONS and 404 on the fetch path', async () => {
    const { base } = await start();
    const notAllowed = await fetch(`${base}/users/42`, { method: 'DELETE' });
    expect(notAllowed.status).toBe(405);
    expect(notAllowed.headers.get('Allow')).toContain('GET');
    await notAllowed.arrayBuffer();
    const options = await fetch(`${base}/users`, { method: 'OPTIONS' });
    expect(options.status).toBe(204);
    expect(options.headers.get('Allow')).toContain('POST');
    const missing = await fetch(`${base}/nowhere`);
    expect(missing.status).toBe(404);
    await missing.arrayBuffer();
  });

  it('maps no endpoint on a server with rewrites', async () => {
    const { base, routed } = await start((plugin) => plugin.rewrite(() => undefined));
    const res = await fetch(`${base}/users/7`);
    expect(await res.json()).toEqual({ id: '7', route: '/users/[id]' });
    expect(routed()).toBe(1);
  });

  it('leaves a path a native route holds to that route', async () => {
    const { base, routed } = await start((plugin) => plugin.native('/users/[id]', () => new Response('native')));
    const res = await fetch(`${base}/users/7`);
    expect(await res.text()).toBe('native');
    expect(routed()).toBe(0);
  });

  it('leaves an endpoint a native route may shadow to that route', async () => {
    const { base, routed } = await start((plugin) => plugin.native('/users/:userId', () => new Response('native')));
    expect(await (await fetch(`${base}/users/7`)).text()).toBe('native');
    expect(routed()).toBe(0);
  });
});
