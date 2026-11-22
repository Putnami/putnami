import { afterEach, describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { api } from '../../src/api/api.plugin';
import { endpoint } from '../../src/api/route/endpoint';
import { type Application, application } from '../../src/application';
import { grpc } from '../../src/grpc/grpc.plugin';
import { http } from '../../src/http/http.plugin';
import { proto } from '../../src/proto';
import { HttpResponse } from '../../src/http/http-response';

describe('endpoint answers', () => {
  let app: Application | undefined;

  afterEach(async () => {
    await app?.stop();
    app = undefined;
  });

  async function serve(register: (plugin: ReturnType<typeof api>, httpPlugin: ReturnType<typeof http>) => void) {
    const httpPlugin = http({ port: 0 });
    const plugin = api({ autoScan: false });
    register(plugin, httpPlugin);
    app = application().use(httpPlugin).use(plugin);
    await app.start();
    return `http://localhost:${httpPlugin.getServer()?.port}`;
  }

  specTest(
    'an endpoint that returns nothing answers 204 No Content',
    {
      feature: 'typescript/api-contracts',
      requirement: 'empty-result',
      check: 'an-empty-result-answers-204',
    },
    async () => {
      const baseUrl = await serve((plugin) => {
        plugin.register('/items/[id]', { DELETE: endpoint(() => undefined) }, 'DELETE');
        plugin.register(
          '/accepted',
          {
            default: endpoint((ctx) => {
              ctx.statusCode = 202;
              return undefined;
            }),
          },
          'POST',
        );
      });

      const deleted = await fetch(`${baseUrl}/items/7`, { method: 'DELETE' });
      expect(deleted.status).toBe(204);
      expect(await deleted.text()).toBe('');

      const accepted = await fetch(`${baseUrl}/accepted`, { method: 'POST' });
      expect(accepted.status).toBe(202);
      expect(await accepted.text()).toBe('');
    },
  );

  specTest(
    "an endpoint's middleware still runs on an empty result",
    {
      feature: 'typescript/api-contracts',
      requirement: 'empty-result',
      check: 'an-empty-result-answers-204',
    },
    async () => {
      const baseUrl = await serve((plugin) => {
        plugin.register(
          '/shared',
          {
            default: endpoint()
              .cors()
              .handle(() => undefined),
          },
          'GET',
        );
      });

      const res = await fetch(`${baseUrl}/shared`, { headers: { Origin: 'https://example.com' } });
      expect(res.status).toBe(204);
      expect(res.headers.get('Access-Control-Allow-Origin')).toBe('*');
    },
  );

  specTest(
    'the Connect bridge still answers an empty result with an empty message',
    {
      feature: 'typescript/api-contracts',
      requirement: 'empty-result',
      check: 'an-empty-result-answers-204',
    },
    async () => {
      const httpPlugin = http({ port: 0 });
      const plugin = api({ autoScan: false });
      plugin.register('/pings', { GET: endpoint(() => undefined) }, 'GET');
      app = application()
        .use(httpPlugin)
        .use(plugin)
        .use(proto({ packageName: 'test.v1', output: false }))
        .use(grpc());
      await app.start();
      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

      expect((await fetch(`${baseUrl}/pings`)).status).toBe(204);

      const rpc = await fetch(`${baseUrl}/test.v1.PingsService/ListPings`, {
        method: 'POST',
        headers: { 'content-type': 'application/json', 'connect-protocol-version': '1' },
        body: '{}',
      });
      expect(rpc.status).toBe(200);
      expect(await rpc.json()).toEqual({});
    },
  );

  specTest(
    'a malformed JSON body answers 400 in the error body',
    {
      feature: 'typescript/http-services',
      requirement: 'malformed-json-body',
      check: 'a-malformed-json-body-answers-400',
    },
    async () => {
      const baseUrl = await serve((plugin) => {
        plugin.register('/items', { default: endpoint(async (ctx) => ({ got: await ctx.body() })) }, 'POST');
      });

      const bad = await fetch(`${baseUrl}/items`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
        body: '{"name": ',
      });
      expect(bad.status).toBe(400);
      expect(await bad.json()).toEqual({ statusCode: 400, message: 'Malformed JSON body', error: 'Bad Request' });

      const good = await fetch(`${baseUrl}/items`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
        body: '{"name":"a"}',
      });
      expect(good.status).toBe(200);
      expect(await good.json()).toEqual({ got: { name: 'a' } });
    },
  );

  specTest(
    'a route that declares */* answers a type it does not name instead of 405',
    {
      feature: 'typescript/http-services',
      requirement: 'wildcard-accept',
      check: 'a-wildcard-route-matches-any-accept-behind-a-named-type',
    },
    async () => {
      const baseUrl = await serve((plugin, httpPlugin) => {
        plugin.register('/item', { GET: endpoint(() => ({ id: 1 })) });
        httpPlugin.get('/page', () => new HttpResponse('<p>page</p>', { headers: { 'Content-Type': 'text/html' } }), {
          accept: ['text/html', '*/*'],
        });
      });

      const res = await fetch(`${baseUrl}/page`, { headers: { Accept: 'text/markdown' } });
      expect(res.status).toBe(200);
      expect(await res.text()).toBe('<p>page</p>');

      // An API endpoint declares */* too: a type it cannot produce answers 406.
      const item = await fetch(`${baseUrl}/item`, { headers: { Accept: 'image/png' } });
      expect(item.status).toBe(406);
    },
  );
});
