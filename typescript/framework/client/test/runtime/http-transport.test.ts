import { afterAll, beforeAll, describe, expect, test } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { ClientRequestError, ClientResponseSizeError, ClientServerError } from '../../src/runtime/errors';
import { HttpTransport } from '../../src/runtime/http-transport';

type RouteKey = `${string} ${string}`;
type RouteHandler = (req: Request, url: URL) => Response | Promise<Response>;

const routes: Record<RouteKey, RouteHandler> = {
  'GET /users': () => Response.json({ users: [{ id: '1', name: 'Alice' }] }),
  'GET /users/42': () => Response.json({ id: '42', name: 'Bob' }),
  'POST /users': (req) => req.json().then((body) => Response.json({ id: '2', ...body }, { status: 201 })),
  'GET /error/400': () => Response.json({ error: 'Bad request' }, { status: 400 }),
  'GET /error/500': () => Response.json({ error: 'Internal server error' }, { status: 500 }),
  'GET /search': (_req, url) => Response.json({ query: url.searchParams.get('q') }),
  'GET /search-values': (_req, url) =>
    Response.json({ empty: url.searchParams.get('empty'), tags: url.searchParams.getAll('tag') }),
  'GET /slow': () => new Promise((resolve) => setTimeout(() => resolve(Response.json({ done: true })), 500)),
  // Size-cap fixtures: plain-text bodies of an exact byte length ('a' = 1 byte UTF-8).
  'GET /size/128': () => new Response('a'.repeat(128), { headers: { 'Content-Type': 'text/plain' } }),
  'GET /size/129': () => new Response('a'.repeat(129), { headers: { 'Content-Type': 'text/plain' } }),
  'GET /size/large': () => new Response('a'.repeat(256 * 1024), { headers: { 'Content-Type': 'text/plain' } }),
};

let server: ReturnType<typeof Bun.serve>;
let baseUrl: string;

beforeAll(() => {
  server = Bun.serve({
    port: 0,
    fetch(req) {
      const url = new URL(req.url);
      const handler = routes[`${req.method} ${url.pathname}` as RouteKey];
      return handler ? handler(req, url) : new Response('Not found', { status: 404 });
    },
  });
  baseUrl = `http://localhost:${server.port}`;
});

afterAll(() => {
  server.stop(true);
});

describe('HttpTransport', () => {
  test('GET request with path', async () => {
    const transport = new HttpTransport(baseUrl);
    const response = await transport.execute<{ users: { id: string; name: string }[] }>({
      method: 'GET',
      path: '/users',
      headers: new Headers(),
    });

    expect(response.status).toBe(200);
    expect(response.data.users).toHaveLength(1);
    expect(response.data.users[0].name).toBe('Alice');
  });

  test('GET with path parameter substitution', async () => {
    const transport = new HttpTransport(baseUrl);
    const response = await transport.execute<{ id: string; name: string }>({
      method: 'GET',
      path: '/users/{id}',
      params: { id: '42' },
      headers: new Headers(),
    });

    expect(response.status).toBe(200);
    expect(response.data.id).toBe('42');
    expect(response.data.name).toBe('Bob');
  });

  test('GET with query parameters', async () => {
    const transport = new HttpTransport(baseUrl);
    const response = await transport.execute<{ query: string }>({
      method: 'GET',
      path: '/search',
      query: { q: 'hello world' },
      headers: new Headers(),
    });

    expect(response.status).toBe(200);
    expect(response.data.query).toBe('hello world');
  });

  test('POST with JSON body', async () => {
    const transport = new HttpTransport(baseUrl);
    const response = await transport.execute<{ id: string; name: string }>({
      method: 'POST',
      path: '/users',
      body: { name: 'Charlie' },
      headers: new Headers(),
    });

    expect(response.status).toBe(201);
    expect(response.data.name).toBe('Charlie');
  });

  test('throws ClientRequestError for 4xx', async () => {
    const transport = new HttpTransport(baseUrl);

    try {
      await transport.execute({
        method: 'GET',
        path: '/error/400',
        headers: new Headers(),
      });
      expect.unreachable('Should have thrown');
    } catch (error) {
      expect(error).toBeInstanceOf(ClientRequestError);
      expect((error as ClientRequestError).status).toBe(400);
    }
  });

  test('throws ClientServerError for 5xx', async () => {
    const transport = new HttpTransport(baseUrl);

    try {
      await transport.execute({
        method: 'GET',
        path: '/error/500',
        headers: new Headers(),
      });
      expect.unreachable('Should have thrown');
    } catch (error) {
      expect(error).toBeInstanceOf(ClientServerError);
      expect((error as ClientServerError).status).toBe(500);
    }
  });

  test('strips trailing slash from baseUrl', async () => {
    const transport = new HttpTransport(`${baseUrl}/`);
    const response = await transport.execute<{ users: unknown[] }>({
      method: 'GET',
      path: '/users',
      headers: new Headers(),
    });

    expect(response.status).toBe(200);
  });

  test('throws on missing path parameters', () => {
    const transport = new HttpTransport(baseUrl);

    expect(() =>
      transport.execute({
        method: 'GET',
        path: '/users/{id}',
        headers: new Headers(),
      }),
    ).toThrow('Missing path parameters');
  });

  test('preserves empty and repeated query parameters', async () => {
    const transport = new HttpTransport(baseUrl);
    const response = await transport.execute<{ empty: string; tags: string[] }>({
      method: 'GET',
      path: '/search-values',
      query: { empty: '', tag: ['first', 'second'], unused: undefined },
      headers: new Headers(),
    });

    expect(response.status).toBe(200);
    expect(response.data).toEqual({ empty: '', tags: ['first', 'second'] });
  });
});

describe('HttpTransport response-size cap (Go parity)', () => {
  specTest(
    'body exactly at the cap is accepted',
    {
      feature: 'typescript/service-clients',
      requirement: 'transport-safety',
      check: 'a-body-exactly-at-the-cap-is-accepted',
    },
    async () => {
      const transport = new HttpTransport(baseUrl, '', 128);
      const response = await transport.execute<string>({
        method: 'GET',
        path: '/size/128',
        headers: new Headers(),
      });

      expect(response.status).toBe(200);
      expect((response.data as string).length).toBe(128);
    },
  );

  specTest(
    'body one byte over the cap fails with ClientResponseSizeError',
    {
      feature: 'typescript/service-clients',
      requirement: 'transport-safety',
      check: 'a-body-one-byte-over-the-cap-fails-with-a-typed-size-error',
    },
    async () => {
      const transport = new HttpTransport(baseUrl, '', 128);

      try {
        await transport.execute({ method: 'GET', path: '/size/129', headers: new Headers() });
        expect.unreachable('Should have thrown');
      } catch (error) {
        expect(error).toBeInstanceOf(ClientResponseSizeError);
        expect((error as ClientResponseSizeError).maxResponseSize).toBe(128);
      }
    },
  );

  test('an explicit override is honored (rejects a body the default would accept)', async () => {
    const transport = new HttpTransport(baseUrl, '', 64);

    await expect(
      transport.execute({ method: 'GET', path: '/size/128', headers: new Headers() }),
    ).rejects.toBeInstanceOf(ClientResponseSizeError);
  });

  test('the default cap (unset ⇒ 32 MiB) passes a moderately large body', async () => {
    // 256 KiB is well under the 32 MiB default → must succeed without a cap error.
    const transport = new HttpTransport(baseUrl);
    const response = await transport.execute<string>({
      method: 'GET',
      path: '/size/large',
      headers: new Headers(),
    });

    expect(response.status).toBe(200);
    expect((response.data as string).length).toBe(256 * 1024);
  });

  test('a cap of 0 selects the 32 MiB default (Go parity)', async () => {
    const transport = new HttpTransport(baseUrl, '', 0);
    const response = await transport.execute<string>({
      method: 'GET',
      path: '/size/128',
      headers: new Headers(),
    });

    expect(response.status).toBe(200);
    expect((response.data as string).length).toBe(128);
  });
});

describe('HttpTransport timeout and abort', () => {
  test('AbortSignal.timeout aborts a slow request', async () => {
    const transport = new HttpTransport(baseUrl);

    try {
      await transport.execute({
        method: 'GET',
        path: '/slow',
        headers: new Headers(),
        signal: AbortSignal.timeout(50),
      });
      expect.unreachable('Should have thrown');
    } catch (error) {
      expect(error).toBeDefined();
      expect((error as Error).name).toBe('TimeoutError');
    }
  });

  specTest(
    'manual AbortController cancels a request',
    {
      feature: 'typescript/service-clients',
      requirement: 'cancellation',
      check: 'a-manual-abort-cancels-an-in-flight-request',
    },
    async () => {
      const transport = new HttpTransport(baseUrl);
      const controller = new AbortController();

      // Abort after a short delay
      setTimeout(() => controller.abort(), 50);

      try {
        await transport.execute({
          method: 'GET',
          path: '/slow',
          headers: new Headers(),
          signal: controller.signal,
        });
        expect.unreachable('Should have thrown');
      } catch (error) {
        expect(error).toBeDefined();
        expect((error as Error).name).toBe('AbortError');
      }
    },
  );

  specTest(
    'pre-aborted signal rejects immediately',
    {
      feature: 'typescript/service-clients',
      requirement: 'cancellation',
      check: 'a-pre-aborted-signal-rejects-at-the-transport',
    },
    async () => {
      const transport = new HttpTransport(baseUrl);

      try {
        await transport.execute({
          method: 'GET',
          path: '/users',
          headers: new Headers(),
          signal: AbortSignal.abort(),
        });
        expect.unreachable('Should have thrown');
      } catch (error) {
        expect(error).toBeDefined();
        expect((error as Error).name).toBe('AbortError');
      }
    },
  );

  test('successful request with signal that does not fire', async () => {
    const transport = new HttpTransport(baseUrl);
    const controller = new AbortController();

    const response = await transport.execute<{ users: unknown[] }>({
      method: 'GET',
      path: '/users',
      headers: new Headers(),
      signal: controller.signal,
    });

    expect(response.status).toBe(200);
  });
});
