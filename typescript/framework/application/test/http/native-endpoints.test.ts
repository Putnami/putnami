import { afterEach, describe, expect, it } from 'bun:test';
import { Stream } from '@putnami/runtime';
import { connect } from 'node:net';
import { api } from '../../src/api/api.plugin';
import { endpoint } from '../../src/api/route/endpoint';
import { application } from '../../src/application';
import { grpc } from '../../src/grpc/grpc.plugin';
import { proto } from '../../src/proto';
import type { HttpRequestContext } from '../../src/http/http-context.type';
import { http, type HttpPlugin } from '../../src/http/http.plugin';
import { HttpResponse } from '../../src/http/http-response';
import {
  nativeEndpoints,
  patternsMayOverlap,
  type RegisteredRoute,
  servedByFetch,
} from '../../src/http/native-endpoints';
import type { RouteController } from '../../src/http/route.controller';

const endpointRoute = (method: RegisteredRoute['method'], path: string): RegisteredRoute => ({
  method,
  path,
  handler: () => ({ ok: true }),
  options: { accept: ['application/json'] },
});
const plainRoute = (method: RegisteredRoute['method'], path: string): RegisteredRoute => ({
  method,
  path,
  handler: () => ({ ok: true }),
});
const fetchRoute = (method: RegisteredRoute['method'], path: string): RegisteredRoute => ({
  method,
  path,
  handler: servedByFetch(() => ({ ok: true })),
});
const mapped = (routes: RegisteredRoute[]) => nativeEndpoints(routes).map((e) => `${e.method} ${e.bunPath}`);

describe('nativeEndpoints', () => {
  it('maps a route alone at its pattern, in Bun syntax', () => {
    expect(mapped([endpointRoute('GET', '/users/[id]'), endpointRoute('POST', 'users')])).toEqual([
      'GET /users/:id',
      'POST /users',
    ]);
  });

  it('maps a plain route like an endpoint', () => {
    expect(mapped([plainRoute('GET', '/health'), endpointRoute('GET', '/items')])).toEqual([
      'GET /health',
      'GET /items',
    ]);
  });

  it('leaves out a route kept on the fetch path, and still reads its pattern', () => {
    expect(mapped([fetchRoute('GET', '/health'), endpointRoute('GET', '/items')])).toEqual(['GET /items']);
    expect(mapped([fetchRoute('GET', '/users/[id]'), endpointRoute('GET', '/users/me')])).toEqual([]);
  });

  it('leaves out a method Bun does not serve by name', () => {
    expect(mapped([plainRoute('OPEN', '/ws'), plainRoute('OPTIONS', '/items'), plainRoute('HEAD', '/items')])).toEqual(
      [],
    );
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

describe('routes kept on the fetch path', () => {
  it('keeps an api() stream route and its WebSocket handlers out of Bun routes', async () => {
    const plugin = api({ autoScan: false });
    plugin.register(
      '/feed',
      endpoint()
        .returns(Stream({ id: String }))
        .handle(async (ctx) => {
          ctx.send({ id: 'a' });
        }),
      'GET',
    );
    plugin.register(
      '/items',
      endpoint().handle(() => ({ items: [] })),
      'GET',
    );
    const httpPlugin = http({ port: 0 });
    const app = application().use(httpPlugin).use(plugin);
    await app.start();
    try {
      const entries = (httpPlugin as unknown as { _routeEntries: RegisteredRoute[] })._routeEntries;
      expect(entries.some((e) => e.path.endsWith('/feed') && e.method === 'GET')).toBe(true);
      expect(mapped(entries)).toEqual(['GET /items']);
    } finally {
      await app.stop();
    }
  });
});

describe('gRPC and bidirectional stream routes', () => {
  it('stay on the fetch path, beside an endpoint Bun matches', async () => {
    const httpPlugin = http({ port: 0 });
    const apiPlugin = api({ autoScan: false });
    apiPlugin.register(
      '/items',
      endpoint().handle(() => ({ items: [] })),
      'GET',
    );
    apiPlugin.register(
      '/chat',
      endpoint()
        .body(Stream({ text: String }))
        .returns(Stream({ text: String }))
        .handle(async (ctx) => {
          for await (const message of ctx.messages()) ctx.send(message);
          return { text: 'bye' };
        }),
      'GET',
    );
    const app = application()
      .use(httpPlugin)
      .use(apiPlugin)
      .use(proto({ packageName: 'test.v1', output: false }))
      .use(grpc());
    await app.start();
    try {
      const entries = (httpPlugin as unknown as { _routeEntries: RegisteredRoute[] })._routeEntries;
      const posts = entries.filter((e) => e.method === 'POST').map((e) => e.path);
      expect(posts).toContain('/grpc.health.v1.Health/Check');
      expect(posts.some((path) => path.includes('ServerReflection'))).toBe(true);
      expect(posts.some((path) => path.startsWith('/test.v1.'))).toBe(true);
      expect(entries.some((e) => e.method === 'GET' && e.path.endsWith('/chat'))).toBe(true);
      expect(mapped(entries)).toEqual(['GET /items']);
    } finally {
      await app.stop();
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

  it('matches a plain http() route in Bun, and leaves a route kept on fetch to the JS router', async () => {
    const { base, routed } = await start((plugin) => {
      plugin.get('/hello/[name]', (ctx: HttpRequestContext) => ({ hello: ctx.params?.['name'], path: ctx.path() }));
      plugin.get(
        '/stream',
        servedByFetch(() => ({ fetched: true })),
      );
    });
    const res = await fetch(`${base}/hello/ada`);
    expect(await res.json()).toEqual({ hello: 'ada', path: 'hello/ada' });
    expect(res.headers.get('X-Content-Type-Options')).toBe('nosniff');
    expect(routed()).toBe(0);
    expect(await (await fetch(`${base}/stream`)).json()).toEqual({ fetched: true });
    expect(routed()).toBe(1);
  });

  /** Sends a raw request line, which fetch would normalize, and returns the response text. */
  function raw(base: string, target: string): Promise<string> {
    const { port } = new URL(base);
    return new Promise((resolve, reject) => {
      const socket = connect(Number(port), '127.0.0.1', () => {
        socket.write(`GET ${target} HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n`);
      });
      let text = '';
      socket.on('data', (chunk) => {
        text += chunk.toString();
      });
      socket.on('end', () => resolve(text));
      socket.on('error', reject);
    });
  }

  it('hands a target Bun reads another way than the URL back to the JS router', async () => {
    const { base, routed } = await start((plugin) => {
      plugin.get('/files/[name]', (ctx: HttpRequestContext) => ({ name: ctx.params?.['name'] }));
      plugin.get('/plain', () => ({ plain: true }));
    });
    for (const target of ['/files/..', '/files/%2e%2e', '/files/a/../..', '/files/a\\b']) {
      const before = routed();
      const answer = await raw(base, target);
      expect(answer).not.toContain('"name":".."');
      expect(answer).not.toContain('\\');
      expect(routed()).toBe(before + 1);
    }
    const before = routed();
    expect(await raw(base, '/files/readme')).toContain('"name":"readme"');
    expect(await raw(base, '/x/../plain')).toContain('"plain":true');
    expect(routed()).toBe(before + 1);
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
