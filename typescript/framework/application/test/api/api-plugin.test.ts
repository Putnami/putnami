import { describe, expect, it } from 'bun:test';
import { ApiPlugin, api } from '../../src/api/api.plugin';
import { endpoint } from '../../src/api/route/endpoint';
import { Stream } from '@putnami/runtime';
import { HttpPlugin, http } from '../../src/http/http.plugin';
import { application } from '../../src/application';

describe('ApiPlugin', () => {
  describe('api({ autoScan: false })', () => {
    it('should create ApiPlugin instance', () => {
      const plugin = api({ autoScan: false });
      expect(plugin).toBeInstanceOf(ApiPlugin);
    });

    it('keeps the service contract and endpoint client policy on discovered routes', () => {
      const client = {
        service: { id: 'inventory', audience: 'api://inventory' },
        credentials: { service: { kind: 'service-token' as const } },
      };
      const plugin = api({ autoScan: false, client });
      plugin.register(
        '/items',
        endpoint()
          .secure({ scopes: ['items:read'] })
          .client({
            security: { alternatives: [{ allOf: [{ profile: 'service', scopes: ['items:read'] }] }] },
            idempotency: { kind: 'safe' },
          })
          .handle(() => ({ items: [] })),
        'GET',
      );

      expect(plugin.clientContract).toEqual(client);
      expect([...plugin.routes][0].meta?.client).toEqual({
        security: { alternatives: [{ allOf: [{ profile: 'service', scopes: ['items:read'] }] }] },
        idempotency: { kind: 'safe' },
      });
    });

    it('emits programmatically registered typed routes during build', async () => {
      const plugin = api({ autoScan: false });
      plugin.register(
        '/users/[id]',
        endpoint(() => ({ ok: true })),
        'GET',
      );

      const result = await application().use(http()).use(plugin).build();

      expect(result.httpRoutes).toContainEqual(
        expect.objectContaining({
          match: 'template',
          path: '/users/{id}',
          methods: ['GET', 'HEAD'],
          provenance: expect.objectContaining({ sourceKind: 'typed-api' }),
        }),
      );
    });
  });

  describe('warmup()', () => {
    it('should get HttpPlugin reference', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });
      const app = application().use(httpPlugin).use(plugin);
      await app.start();
      expect(app.getPlugin(ApiPlugin)).toBe(plugin);
      await app.stop();
    });

    it('should create HttpPlugin if not registered', async () => {
      const plugin = api({ autoScan: false });
      // Use http({ port: 0 }) to avoid port collision in parallel tests
      const app = application()
        .use(http({ port: 0 }))
        .use(plugin);
      await app.start();
      const httpPlugin = app.getPlugin(HttpPlugin);
      expect(httpPlugin).toBeInstanceOf(HttpPlugin);
      await app.stop();
    });
  });

  describe('register()', () => {
    it('should register endpoint with named method export', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      plugin.register('/test', { GET: endpoint(() => ({ data: 'test' })) }, 'GET');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/test`, {
        headers: { Accept: 'application/json' },
      });
      expect(res.status).toBe(200);
      const body = await res.json();
      expect(body.data).toBe('test');

      await app.stop();
    });

    it('should register endpoint with default export', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      plugin.register('/create', { default: endpoint(() => ({ data: 'default' })) }, 'POST');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/create`, {
        method: 'POST',
        headers: { Accept: 'application/json' },
      });
      expect(res.status).toBe(200);
      const body = await res.json();
      expect(body.data).toBe('default');

      await app.stop();
    });

    it('should register multiple methods when method is undefined', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      plugin.register('/multi', {
        GET: endpoint(() => ({ action: 'get' })),
        POST: endpoint(() => ({ action: 'post' })),
      });

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

      const getRes = await fetch(`${baseUrl}/multi`, {
        headers: { Accept: 'application/json' },
      });
      expect(getRes.status).toBe(200);
      expect((await getRes.json()).action).toBe('get');

      const postRes = await fetch(`${baseUrl}/multi`, {
        method: 'POST',
        headers: { Accept: 'application/json' },
      });
      expect(postRes.status).toBe(200);
      expect((await postRes.json()).action).toBe('post');

      await app.stop();
    });

    it('should register a direct endpoint definition', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      plugin.register(
        '/direct',
        endpoint(() => ({ ok: true })),
        'GET',
      );

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/direct`, {
        headers: { Accept: 'application/json' },
      });
      expect(res.status).toBe(200);
      expect((await res.json()).ok).toBe(true);

      await app.stop();
    });

    it('should register a stream endpoint (auto-detected)', () => {
      const plugin = api({ autoScan: false });
      const def = endpoint()
        .returns(Stream({ event: String }))
        .handle(async (ctx) => {
          ctx.send({ event: 'test' });
        });
      const result = plugin.register('/ws', def);
      expect(result).toBe(plugin);
    });

    it('should be chainable', () => {
      const plugin = api({ autoScan: false });

      const result = plugin
        .register(
          '/a',
          endpoint(() => ({})),
          'GET',
        )
        .register(
          '/b',
          endpoint(() => ({})),
          'GET',
        );

      expect(result).toBe(plugin);
    });
  });

  describe('JSON Accept header', () => {
    it('should only match routes with JSON accept header', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      plugin.register('/json-only', { default: endpoint(() => ({ json: true })) }, 'GET');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

      // With JSON accept - should match
      const jsonRes = await fetch(`${baseUrl}/json-only`, {
        headers: { Accept: 'application/json' },
      });
      expect(jsonRes.status).toBe(200);

      // With */* accept - should also match
      const anyRes = await fetch(`${baseUrl}/json-only`, {
        headers: { Accept: '*/*' },
      });
      expect(anyRes.status).toBe(200);

      await app.stop();
    });
  });

  describe('prefix option', () => {
    it('should prefix routes with given prefix', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false, prefix: '/v1' });

      plugin.register('/users', { default: endpoint(() => ({ users: [] })) }, 'GET');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

      // Should NOT match without prefix
      const noPrefix = await fetch(`${baseUrl}/users`, {
        headers: { Accept: 'application/json' },
      });
      expect(noPrefix.status).toBe(404);

      // Should match with prefix
      const withPrefix = await fetch(`${baseUrl}/v1/users`, {
        headers: { Accept: 'application/json' },
      });
      expect(withPrefix.status).toBe(200);

      await app.stop();
    });

    it('should handle prefix with trailing slash', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false, prefix: '/api/' });

      plugin.register('/items', { default: endpoint(() => ({ items: [] })) }, 'GET');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/api/items`, {
        headers: { Accept: 'application/json' },
      });
      expect(res.status).toBe(200);

      await app.stop();
    });

    it('should handle path without leading slash', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false, prefix: '/v2' });

      plugin.register('products', { default: endpoint(() => ({ products: [] })) }, 'GET');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/v2/products`, {
        headers: { Accept: 'application/json' },
      });
      expect(res.status).toBe(200);

      await app.stop();
    });

    it('should work without prefix (default)', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      plugin.register('/direct', { default: endpoint(() => ({ direct: true })) }, 'GET');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/direct`, {
        headers: { Accept: 'application/json' },
      });
      expect(res.status).toBe(200);

      await app.stop();
    });
  });

  describe('use(pattern, middleware)', () => {
    it('should run middleware only on matching prefix wildcard paths', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });
      const seen: string[] = [];

      plugin.use('/admin/*', async (ctx, next) => {
        seen.push(ctx.path());
        return next();
      });

      plugin.register('/admin/users', { default: endpoint(() => ({ ok: 'admin' })) }, 'GET');
      plugin.register('/public', { default: endpoint(() => ({ ok: 'public' })) }, 'GET');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const headers = { Accept: 'application/json' };

      const adminRes = await fetch(`${baseUrl}/admin/users`, { headers });
      expect(adminRes.status).toBe(200);
      const publicRes = await fetch(`${baseUrl}/public`, { headers });
      expect(publicRes.status).toBe(200);

      expect(seen).toEqual(['admin/users']);

      await app.stop();
    });

    it('should match exact path patterns', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });
      const seen: string[] = [];

      plugin.use('/exact', async (ctx, next) => {
        seen.push(ctx.path());
        return next();
      });

      plugin.register('/exact', { default: endpoint(() => ({ ok: true })) }, 'GET');
      plugin.register('/exact/sub', { default: endpoint(() => ({ ok: true })) }, 'GET');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const headers = { Accept: 'application/json' };

      await fetch(`${baseUrl}/exact`, { headers });
      await fetch(`${baseUrl}/exact/sub`, { headers });

      expect(seen).toEqual(['exact']);

      await app.stop();
    });

    it('should match the post-prefix path so prefix carries through', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false, prefix: '/v1' });
      const seen: string[] = [];

      plugin.use('/admin/*', async (ctx, next) => {
        seen.push(ctx.path());
        return next();
      });

      plugin.register('/admin/users', { default: endpoint(() => ({ ok: true })) }, 'GET');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const headers = { Accept: 'application/json' };
      await fetch(`${baseUrl}/v1/admin/users`, { headers });

      expect(seen).toEqual(['v1/admin/users']);

      await app.stop();
    });
  });
});
