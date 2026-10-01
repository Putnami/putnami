import { afterAll, beforeAll, describe, expect, test } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { ClientRequestError, ClientServerError } from '../../src/runtime/errors';
import { HttpTransport } from '../../src/runtime/http-transport';

let server: ReturnType<typeof Bun.serve>;
let baseUrl: string;

beforeAll(() => {
  server = Bun.serve({
    port: 0,
    // biome-ignore lint/complexity/noExcessiveCognitiveComplexity: one local server enumerates the transport edge responses
    fetch(req) {
      const url = new URL(req.url);

      if (url.pathname === '/empty-204') {
        return new Response(null, { status: 204 });
      }

      if (url.pathname === '/plain-text') {
        return new Response('Hello world', {
          headers: { 'Content-Type': 'text/plain' },
        });
      }

      if (url.pathname === '/no-content-type-json') {
        // JSON body but no Content-Type header
        return new Response('{"parsed":true}');
      }

      if (url.pathname === '/no-content-type-text') {
        // Non-JSON body with no Content-Type
        return new Response('just text');
      }

      if (url.pathname === '/error-with-body') {
        return Response.json({ error: 'Validation failed', fields: { name: 'required' } }, { status: 422 });
      }

      if (url.pathname === '/error-plain-text') {
        return new Response('Service Unavailable', { status: 503 });
      }

      if (url.pathname === '/error-empty') {
        return new Response('', { status: 500 });
      }

      if (url.pathname === '/multi-param/a/b/c') {
        return Response.json({ ok: true });
      }

      if (url.pathname === '/encoded/hello%20world') {
        return Response.json({ decoded: true });
      }

      if (url.pathname === '/large-response') {
        const data = { items: Array.from({ length: 100 }, (_, i) => ({ id: i, name: `item-${i}` })) };
        return Response.json(data);
      }

      if (url.pathname === '/oversized-json-no-content-type') {
        // Valid JSON, but bigger than the 1 MiB speculative-parse cap and with
        // no JSON content-type — must NOT be speculatively parsed.
        const padding = 'x'.repeat(1024 * 1024 + 10);
        return new Response(JSON.stringify({ pad: padding }));
      }

      if (url.pathname === '/small-json-no-content-type') {
        // Valid JSON under the cap, no content-type — speculative parse applies.
        return new Response(JSON.stringify({ small: true }));
      }

      return new Response('Not found', { status: 404 });
    },
  });
  baseUrl = `http://localhost:${server.port}`;
});

afterAll(() => {
  server.stop(true);
});

describe('HttpTransport edge cases — response parsing', () => {
  test('handles 204 No Content (empty response)', async () => {
    const transport = new HttpTransport(baseUrl);
    const response = await transport.execute<undefined>({
      method: 'GET',
      path: '/empty-204',
      headers: new Headers(),
    });
    expect(response.status).toBe(204);
  });

  test('returns raw text when Content-Type is text/plain', async () => {
    const transport = new HttpTransport(baseUrl);
    const response = await transport.execute<string>({
      method: 'GET',
      path: '/plain-text',
      headers: new Headers(),
    });
    expect(response.status).toBe(200);
    expect(response.data).toBe('Hello world');
  });

  test('parses JSON from body without Content-Type', async () => {
    const transport = new HttpTransport(baseUrl);
    const response = await transport.execute<{ parsed: boolean }>({
      method: 'GET',
      path: '/no-content-type-json',
      headers: new Headers(),
    });
    expect(response.data.parsed).toBe(true);
  });

  test('returns raw text when body is not JSON and no Content-Type', async () => {
    const transport = new HttpTransport(baseUrl);
    const response = await transport.execute<string>({
      method: 'GET',
      path: '/no-content-type-text',
      headers: new Headers(),
    });
    expect(response.data).toBe('just text');
  });

  test('handles large JSON response', async () => {
    const transport = new HttpTransport(baseUrl);
    const response = await transport.execute<{ items: { id: number }[] }>({
      method: 'GET',
      path: '/large-response',
      headers: new Headers(),
    });
    expect(response.data.items).toHaveLength(100);
    expect(response.data.items[99].id).toBe(99);
  });

  specTest(
    'does not speculatively JSON-parse oversized non-JSON bodies',
    {
      feature: 'typescript/service-clients',
      requirement: 'transport-safety',
      check: 'an-oversized-non-json-body-is-not-speculatively-parsed',
    },
    async () => {
      const transport = new HttpTransport(baseUrl);
      const response = await transport.execute<unknown>({
        method: 'GET',
        path: '/oversized-json-no-content-type',
        headers: new Headers(),
      });
      // Over the cap with no JSON content-type → returned as raw text, not parsed.
      expect(typeof response.data).toBe('string');
    },
  );

  test('still speculatively parses small JSON bodies without content-type', async () => {
    const transport = new HttpTransport(baseUrl);
    const response = await transport.execute<{ small: boolean }>({
      method: 'GET',
      path: '/small-json-no-content-type',
      headers: new Headers(),
    });
    expect(response.data.small).toBe(true);
  });
});

describe('HttpTransport edge cases — error responses', () => {
  test('carries JSON body on 4xx error', async () => {
    const transport = new HttpTransport(baseUrl, 'test-service');

    try {
      await transport.execute({
        method: 'GET',
        path: '/error-with-body',
        headers: new Headers(),
      });
      expect.unreachable();
    } catch (error) {
      expect(error).toBeInstanceOf(ClientRequestError);
      const err = error as ClientRequestError;
      expect(err.status).toBe(422);
      expect(err.service).toBe('test-service');
      expect(err.responseBody).toEqual({ error: 'Validation failed', fields: { name: 'required' } });
    }
  });

  test('carries text body on 5xx error', async () => {
    const transport = new HttpTransport(baseUrl, 'test-service');

    try {
      await transport.execute({
        method: 'GET',
        path: '/error-plain-text',
        headers: new Headers(),
      });
      expect.unreachable();
    } catch (error) {
      expect(error).toBeInstanceOf(ClientServerError);
      const err = error as ClientServerError;
      expect(err.status).toBe(503);
      expect(err.responseBody).toBe('Service Unavailable');
    }
  });

  test('handles empty body on 500 error', async () => {
    const transport = new HttpTransport(baseUrl, 'test-service');

    try {
      await transport.execute({
        method: 'GET',
        path: '/error-empty',
        headers: new Headers(),
      });
      expect.unreachable();
    } catch (error) {
      expect(error).toBeInstanceOf(ClientServerError);
      expect((error as ClientServerError).status).toBe(500);
    }
  });

  test('error message includes status code and text', async () => {
    const transport = new HttpTransport(baseUrl);

    try {
      await transport.execute({
        method: 'GET',
        path: '/error-with-body',
        headers: new Headers(),
      });
      expect.unreachable();
    } catch (error) {
      expect((error as ClientRequestError).message).toContain('422');
    }
  });

  test('error includes method and path', async () => {
    const transport = new HttpTransport(baseUrl, 'my-svc');

    try {
      await transport.execute({
        method: 'GET',
        path: '/error/400',
        headers: new Headers(),
      });
      expect.unreachable();
    } catch (error) {
      const err = error as ClientRequestError;
      expect(err.method).toBe('GET /error/400');
    }
  });
});

describe('HttpTransport edge cases — path parameters', () => {
  test('encodes special characters in path parameters', async () => {
    const transport = new HttpTransport(baseUrl);
    const response = await transport.execute<{ decoded: boolean }>({
      method: 'GET',
      path: '/encoded/{name}',
      params: { name: 'hello world' },
      headers: new Headers(),
    });
    expect(response.status).toBe(200);
    expect(response.data.decoded).toBe(true);
  });

  test('throws on multiple missing path parameters', () => {
    const transport = new HttpTransport(baseUrl);

    expect(() =>
      transport.execute({
        method: 'GET',
        path: '/users/{id}/posts/{postId}',
        headers: new Headers(),
      }),
    ).toThrow('{id}');
  });

  test('skips query param construction when query object is empty', async () => {
    const transport = new HttpTransport(baseUrl);
    const response = await transport.execute<{ parsed: boolean }>({
      method: 'GET',
      path: '/no-content-type-json',
      // Empty query object — should not add ? to URL
      query: {},
      headers: new Headers(),
    });
    expect(response.status).toBe(200);
    expect(response.data.parsed).toBe(true);
  });

  test('GET request does not send body', async () => {
    const transport = new HttpTransport(baseUrl);
    // GET with body — the body should be ignored per HTTP semantics
    const response = await transport.execute<{ parsed: boolean }>({
      method: 'GET',
      path: '/no-content-type-json',
      body: { ignored: true },
      headers: new Headers(),
    });
    expect(response.status).toBe(200);
  });
});

describe('HttpTransport constructor', () => {
  test('defaults serviceName to empty string', async () => {
    const transport = new HttpTransport(baseUrl);

    try {
      await transport.execute({
        method: 'GET',
        path: '/error/400',
        headers: new Headers(),
      });
      expect.unreachable();
    } catch (error) {
      expect((error as ClientRequestError).service).toBe('');
    }
  });

  test('sets custom serviceName', async () => {
    const transport = new HttpTransport(baseUrl, 'my-service');

    try {
      await transport.execute({
        method: 'GET',
        path: '/error/400',
        headers: new Headers(),
      });
      expect.unreachable();
    } catch (error) {
      expect((error as ClientRequestError).service).toBe('my-service');
    }
  });
});
