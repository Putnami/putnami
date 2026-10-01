import { describe, expect, it } from 'bun:test';
import { http } from '../../../src/http/http.plugin';
import { api } from '../../../src/api/api.plugin';
import { EndpointBuilder, isEndpointDefinition, endpoint } from '../../../src/api/route/endpoint';
import { Email, Optional, Uuid } from '../../../src/api/route';
import { application } from '../../../src/application';

describe('endpoint()', () => {
  describe('simple mode', () => {
    it('should create an EndpointDefinition from a handler function', () => {
      const def = endpoint(() => ({ hello: 'world' }));
      expect(isEndpointDefinition(def)).toBe(true);
      expect(typeof def.handler).toBe('function');
    });

    it('should not have schemas in simple mode', () => {
      const def = endpoint(() => ({ ok: true }));
      expect(def.schemas).toBeUndefined();
    });
  });

  describe('builder mode', () => {
    it('should return an EndpointBuilder when called with no arguments', () => {
      const builder = endpoint();
      expect(builder).toBeInstanceOf(EndpointBuilder);
    });

    it('should create an EndpointDefinition with params schema', () => {
      const def = endpoint()
        .params({ id: String })
        .handle(() => ({ id: '1' }));
      expect(isEndpointDefinition(def)).toBe(true);
      expect(def.schemas?.params).toBeDefined();
    });

    it('should create an EndpointDefinition with query schema', () => {
      const def = endpoint()
        .query({ page: Number, search: Optional(String) })
        .handle(() => ({ results: [] }));
      expect(def.schemas?.query).toBeDefined();
    });

    it('should create an EndpointDefinition with body schema', () => {
      const def = endpoint()
        .body({ name: String, email: Email })
        .handle(() => ({ ok: true }));
      expect(def.schemas?.body).toBeDefined();
    });

    it('should create an EndpointDefinition with headers schema', () => {
      const def = endpoint()
        .headers({ 'x-request-id': Uuid })
        .handle(() => ({ ok: true }));
      expect(def.schemas?.headers).toBeDefined();
    });

    it('should create an EndpointDefinition with returns schema', () => {
      const def = endpoint()
        .returns({ id: String, name: String })
        .handle(() => ({ id: '1', name: 'Test' }));
      expect(def.schemas?.returns).toBeDefined();
    });

    it('should set csrfExempt on the definition via .csrfExempt()', () => {
      const exempt = endpoint()
        .csrfExempt()
        .handle(() => ({ ok: true }));
      expect(exempt.csrfExempt).toBe(true);

      // Explicit `.csrfExempt(false)` forces enforcement: the flag is emitted as
      // `false` (not omitted) so it overrides — rather than inherits — the
      // api-level default.
      const enforced = endpoint()
        .csrfExempt(false)
        .handle(() => ({ ok: true }));
      expect(enforced.csrfExempt).toBe(false);

      // Default (opt-in): a plain endpoint carries no csrfExempt flag.
      const plain = endpoint(() => ({ ok: true }));
      expect(plain.csrfExempt).toBeUndefined();

      // Existing .params()/.query()/.body() endpoints keep compiling unchanged.
      const additive = endpoint()
        .params({ id: Uuid })
        .query({ page: Optional(Number) })
        .body({ name: String })
        .handle(() => ({ ok: true }));
      expect(additive.csrfExempt).toBeUndefined();
      expect(additive.schemas?.headers).toBeUndefined();
    });

    it('should support full chaining', () => {
      const def = endpoint()
        .params({ id: Uuid })
        .query({ include: Optional(String) })
        .body({ name: String })
        .returns({ id: String, name: String })
        .handle(() => ({ id: '1', name: 'Test' }));

      expect(isEndpointDefinition(def)).toBe(true);
      expect(def.schemas?.params).toBeDefined();
      expect(def.schemas?.query).toBeDefined();
      expect(def.schemas?.body).toBeDefined();
      expect(def.schemas?.returns).toBeDefined();
    });
  });

  describe('isEndpointDefinition', () => {
    it('should detect EndpointDefinition objects', () => {
      const def = endpoint(() => ({}));
      expect(isEndpointDefinition(def)).toBe(true);
    });

    it('should reject non-EndpointDefinition objects', () => {
      expect(isEndpointDefinition({})).toBe(false);
      expect(isEndpointDefinition(null)).toBe(false);
      expect(isEndpointDefinition(() => {})).toBe(false);
      expect(isEndpointDefinition({ __endpoint: 'wrong' })).toBe(false);
    });
  });
});

describe('endpoint() integration with ApiPlugin', () => {
  describe('simple endpoint handler', () => {
    it('should work as a default export with register()', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      const handler = endpoint(() => ({ message: 'hello from endpoint()' }));
      plugin.register('/hello', { default: handler }, 'GET');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/hello`, {
        headers: { Accept: 'application/json' },
      });
      expect(res.status).toBe(200);
      const data = await res.json();
      expect(data.message).toBe('hello from endpoint()');

      await app.stop();
    });
  });

  describe('endpoint with params validation', () => {
    it('should validate and pass params to handler', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      const handler = endpoint()
        .params({ id: Uuid })
        .handle((ctx) => ({ id: ctx.params.id }));

      plugin.register('/users/[id]', { default: handler }, 'GET');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const validId = '550e8400-e29b-41d4-a716-446655440000';
      const res = await fetch(`${baseUrl}/users/${validId}`, {
        headers: { Accept: 'application/json' },
      });
      expect(res.status).toBe(200);
      const data = await res.json();
      expect(data.id).toBe(validId);

      await app.stop();
    });

    it('should return 400 for invalid params', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      const handler = endpoint()
        .params({ id: Uuid })
        .handle((ctx) => ({ id: ctx.params.id }));

      plugin.register('/users/[id]', { default: handler }, 'GET');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/users/not-a-uuid`, {
        headers: { Accept: 'application/json' },
      });
      expect(res.status).toBe(400);

      await app.stop();
    });
  });

  describe('endpoint with body validation', () => {
    it('should validate body and pass to handler', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      const handler = endpoint()
        .body({ name: String, email: Email })
        .handle(async (ctx) => {
          const body = await ctx.body();
          return { name: body.name, email: body.email };
        });

      plugin.register('/users', { default: handler }, 'POST');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/users`, {
        method: 'POST',
        headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: 'John', email: 'john@example.com' }),
      });
      expect(res.status).toBe(200);
      const data = await res.json();
      expect(data.name).toBe('John');
      expect(data.email).toBe('john@example.com');

      await app.stop();
    });

    it('should return 400 for invalid body', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      const handler = endpoint()
        .body({ name: String, email: Email })
        .handle(async (ctx) => {
          const body = await ctx.body();
          return body;
        });

      plugin.register('/users', { default: handler }, 'POST');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/users`, {
        method: 'POST',
        headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: 'John', email: 'invalid-email' }),
      });
      expect(res.status).toBe(400);

      await app.stop();
    });
  });

  describe('endpoint with headers validation', () => {
    it('should validate headers and expose typed values on ctx.headerParams()', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      const handler = endpoint()
        .headers({ 'x-request-id': Uuid, 'x-count': Optional(Number) })
        .handle((ctx) => {
          const headers = ctx.headerParams();
          // Raw Headers object stays intact and usable.
          expect(typeof ctx.headers.get).toBe('function');
          return { requestId: headers['x-request-id'], count: headers['x-count'] };
        });

      plugin.register('/traced', { default: handler }, 'GET');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const requestId = '550e8400-e29b-41d4-a716-446655440000';
      const res = await fetch(`${baseUrl}/traced`, {
        headers: { Accept: 'application/json', 'x-request-id': requestId, 'x-count': '3' },
      });
      expect(res.status).toBe(200);
      const data = await res.json();
      expect(data.requestId).toBe(requestId);
      // Coerced from the string header value, like params/query.
      expect(data.count).toBe(3);

      await app.stop();
    });

    it('should return 400 for an invalid header', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      const handler = endpoint()
        .headers({ 'x-request-id': Uuid })
        .handle((ctx) => ctx.headerParams());

      plugin.register('/traced', { default: handler }, 'GET');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/traced`, {
        headers: { Accept: 'application/json', 'x-request-id': 'not-a-uuid' },
      });
      expect(res.status).toBe(400);

      await app.stop();
    });
  });

  describe('endpoint per-endpoint CSRF exemption', () => {
    it('should exempt only the endpoint that opts out — a sibling still enforces CSRF', async () => {
      const httpPlugin = http({ port: 0, csrf: true });
      const plugin = api({ autoScan: false, csrf: true });

      // Webhook endpoint opts out of CSRF; sibling does not.
      plugin.register(
        '/webhook',
        {
          default: endpoint()
            .csrfExempt()
            .handle(() => ({ received: true })),
        },
        'POST',
      );
      plugin.register('/secure', { default: endpoint().handle(() => ({ saved: true })) }, 'POST');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

      // Exempt endpoint accepts POST without a CSRF token.
      const webhookRes = await fetch(`${baseUrl}/webhook`, {
        method: 'POST',
        headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
        body: JSON.stringify({ event: 'ping' }),
      });
      expect(webhookRes.status).toBe(200);
      expect((await webhookRes.json()).received).toBe(true);

      // Sibling endpoint still rejects the same token-less POST.
      const secureRes = await fetch(`${baseUrl}/secure`, {
        method: 'POST',
        headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
        body: JSON.stringify({ data: 'x' }),
      });
      expect(secureRes.status).toBe(403);
      expect((await secureRes.json()).error).toBe('CSRF token mismatch');

      await app.stop();
    });

    it('.csrfExempt(false) forces enforcement on a route in an otherwise-exempt api()', async () => {
      // Global CSRF is on, but this api() is CSRF-exempt by default (no csr:true).
      const httpPlugin = http({ port: 0, csrf: true });
      const plugin = api({ autoScan: false });

      // Sibling inherits the api default (exempt); the tightened route forces CSRF.
      plugin.register('/inherit', { default: endpoint().handle(() => ({ ok: true })) }, 'POST');
      plugin.register(
        '/enforced',
        {
          default: endpoint()
            .csrfExempt(false)
            .handle(() => ({ ok: true })),
        },
        'POST',
      );

      const app = application().use(httpPlugin).use(plugin);
      await app.start();
      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const post = (path: string) =>
        fetch(`${baseUrl}${path}`, {
          method: 'POST',
          headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
          body: JSON.stringify({ data: 'x' }),
        });

      // Inheriting sibling stays exempt: token-less POST succeeds.
      expect((await post('/inherit')).status).toBe(200);
      // Tightened route enforces CSRF: the same token-less POST is rejected.
      const enforcedRes = await post('/enforced');
      expect(enforcedRes.status).toBe(403);
      expect((await enforcedRes.json()).error).toBe('CSRF token mismatch');

      await app.stop();
    });
  });

  describe('endpoint with multi-method exports', () => {
    it('should register EndpointDefinitions for multiple methods', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      const handler = {
        GET: endpoint(() => ({ method: 'get' })),
        POST: endpoint()
          .body({ name: String })
          .handle(async (ctx) => {
            const body = await ctx.body();
            return { method: 'post', name: body.name };
          }),
      };

      plugin.register('/items', handler);

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

      const getRes = await fetch(`${baseUrl}/items`, {
        headers: { Accept: 'application/json' },
      });
      expect(getRes.status).toBe(200);
      expect((await getRes.json()).method).toBe('get');

      const postRes = await fetch(`${baseUrl}/items`, {
        method: 'POST',
        headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: 'Test' }),
      });
      expect(postRes.status).toBe(200);
      expect((await postRes.json()).name).toBe('Test');

      await app.stop();
    });
  });

  describe('content negotiation', () => {
    it('should return JSON for Accept: application/json', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      plugin.register('/data', { default: endpoint(() => ({ value: 42 })) }, 'GET');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/data`, {
        headers: { Accept: 'application/json' },
      });
      expect(res.status).toBe(200);
      expect(res.headers.get('Content-Type')).toBe('application/json');
      const data = await res.json();
      expect(data.value).toBe(42);

      await app.stop();
    });

    it('should return plain text for Accept: text/plain', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      plugin.register('/data', { default: endpoint(() => ({ value: 42 })) }, 'GET');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/data`, {
        headers: { Accept: 'text/plain' },
      });
      expect(res.status).toBe(200);
      expect(res.headers.get('Content-Type')).toContain('text/plain');
      const text = await res.text();
      expect(text).toContain('42');

      await app.stop();
    });

    it('should return XML for Accept: application/xml', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      plugin.register('/data', { default: endpoint(() => ({ name: 'test' })) }, 'GET');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/data`, {
        headers: { Accept: 'application/xml' },
      });
      expect(res.status).toBe(200);
      expect(res.headers.get('Content-Type')).toContain('application/xml');
      const text = await res.text();
      expect(text).toContain('<?xml');
      expect(text).toContain('<name>test</name>');

      await app.stop();
    });

    it('should return HTML for Accept: text/html', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      plugin.register('/data', { default: endpoint(() => ({ msg: 'hello' })) }, 'GET');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/data`, {
        headers: { Accept: 'text/html' },
      });
      expect(res.status).toBe(200);
      expect(res.headers.get('Content-Type')).toContain('text/html');
      const text = await res.text();
      expect(text).toContain('<!DOCTYPE html>');

      await app.stop();
    });
  });

  describe('endpoint with nested object schemas', () => {
    it('should validate nested objects in body', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      const handler = endpoint()
        .body({ name: String, address: { street: String, city: String } })
        .handle(async (ctx) => {
          const body = await ctx.body();
          return { name: body.name, city: body.address.city };
        });

      plugin.register('/users', { default: handler }, 'POST');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/users`, {
        method: 'POST',
        headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: 'John',
          address: { street: '123 Main St', city: 'Springfield' },
        }),
      });
      expect(res.status).toBe(200);
      const data = await res.json();
      expect(data.name).toBe('John');
      expect(data.city).toBe('Springfield');

      await app.stop();
    });

    it('should return 400 for invalid nested object', async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });

      const handler = endpoint()
        .body({ address: { street: String, city: String } })
        .handle(async (ctx) => await ctx.body());

      plugin.register('/test', { default: handler }, 'POST');

      const app = application().use(httpPlugin).use(plugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/test`, {
        method: 'POST',
        headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
        body: JSON.stringify({ address: 'not-an-object' }),
      });
      expect(res.status).toBe(400);

      await app.stop();
    });
  });
});
