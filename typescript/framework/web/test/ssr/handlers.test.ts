import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { HttpResponse } from '@putnami/application';
import { HttpException } from '@putnami/runtime';
import { runInContext } from '../../../runtime/src/context/context.utils';
import { actionHandler } from '../../src/ssr/handlers/action.handler';
import { loaderHandler } from '../../src/ssr/handlers/loader.handler';

describe('SSR Handlers', () => {
  describe('loaderHandler', () => {
    it('is a function that wraps handlers', () => {
      expect(typeof loaderHandler).toBe('function');
    });

    it('returns a function when called with a handler', () => {
      const handler = () => ({ data: 'test' });
      const wrappedHandler = loaderHandler(handler);

      expect(typeof wrappedHandler).toBe('function');
    });

    it('accepts handlers that return objects', () => {
      const handler = () => ({ users: [{ id: 1 }] });
      const wrapped = loaderHandler(handler);

      expect(typeof wrapped).toBe('function');
    });

    it('accepts handlers that return arrays', () => {
      const handler = () => [1, 2, 3];
      const wrapped = loaderHandler(handler);

      expect(typeof wrapped).toBe('function');
    });

    it('accepts handlers that return primitives', () => {
      const handler = () => 'string value';
      const wrapped = loaderHandler(handler);

      expect(typeof wrapped).toBe('function');
    });
  });

  describe('actionHandler', () => {
    it('is a function that wraps handlers', () => {
      expect(typeof actionHandler).toBe('function');
    });

    it('returns an async function when called', () => {
      const handler = () => ({ success: true });
      const wrappedHandler = actionHandler(handler);

      expect(typeof wrappedHandler).toBe('function');
    });
  });
});

describe('actionHandler internal logic', () => {
  const createContext = () =>
    ({
      req: new Request('http://localhost/test'),
      headers: new Headers(),
      params: {},
      queryParams: () => ({}),
      body: async () => undefined,
      statusCode: 0,
    }) as never;

  it('HttpResponse.json creates proper JSON response', () => {
    const data = { id: 1, name: 'test' };
    const response = HttpResponse.json(data);

    expect(response).toBeInstanceOf(HttpResponse);
  });

  it('HttpResponse with undefined body creates 204 response', () => {
    const response = new HttpResponse(undefined, { status: 204 });

    expect(response.get().status).toBe(204);
  });

  it('HttpResponse can create with status code', () => {
    const response = new HttpResponse('{"error":"not found"}', { status: 404 });

    expect(response.get().status).toBe(404);
  });

  it('HttpException stores status and response', () => {
    const exception = new HttpException('Not found', 404);

    expect(exception.getStatus()).toBe(404);
    expect(exception.getResponse()).toBe('Not found');
  });

  it('HttpException can be created with different status codes', () => {
    const badRequest = new HttpException('Bad request', 400);
    const serverError = new HttpException('Server error', 500);

    expect(badRequest.getStatus()).toBe(400);
    expect(serverError.getStatus()).toBe(500);
  });

  it('HttpException extends Error', () => {
    const exception = new HttpException('Test error', 500);

    expect(exception).toBeInstanceOf(Error);
  });

  specTest(
    'returns HttpResponse and native Response objects unchanged',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'ssr-response',
      check: 'an-http-response-passes-through-unchanged',
    },
    async () => {
      const httpResponse = new HttpResponse('ok', { status: 201 });
      const nativeResponse = new Response('ok', { status: 202 });

      const wrappedHttp = actionHandler(() => httpResponse);
      const wrappedNative = actionHandler(() => nativeResponse);

      await expect(runInContext(createContext(), async () => wrappedHttp())).resolves.toBe(httpResponse);
      await expect(runInContext(createContext(), async () => wrappedNative())).resolves.toBe(nativeResponse);
    },
  );

  specTest(
    'returns 204 for empty results and JSON for arrays or objects',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'ssr-response',
      check: 'a-handler-result-is-serialized-to-json-or-204',
    },
    async () => {
      const emptyWrapped = actionHandler(() => undefined);
      const objectWrapped = actionHandler(() => ({ ok: true }));
      const arrayWrapped = actionHandler(() => [1, 2, 3]);

      const emptyResponse = await runInContext(createContext(), async () => emptyWrapped());
      const objectResponse = await runInContext(createContext(), async () => objectWrapped());
      const arrayResponse = await runInContext(createContext(), async () => arrayWrapped());

      expect(emptyResponse.status).toBe(204);
      expect(await objectResponse.get().json()).toEqual({ ok: true });
      expect(await arrayResponse.get().json()).toEqual([1, 2, 3]);
    },
  );

  it('returns undefined for primitive values', async () => {
    const wrapped = actionHandler(() => 'primitive');
    await expect(runInContext(createContext(), async () => wrapped())).resolves.toBeUndefined();
  });

  specTest(
    'converts HttpException to JSON responses and rethrows unknown errors',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'ssr-response',
      check: 'an-http-exception-becomes-a-json-response-and-an-unknown-error-rethrows',
    },
    async () => {
      const httpWrapped = actionHandler(() => {
        throw new HttpException({ error: 'missing' }, 404);
      });
      const unknownWrapped = actionHandler(() => {
        throw new Error('boom');
      });

      const response = await runInContext(createContext(), async () => httpWrapped());
      expect(response.status).toBe(404);
      expect(await response.get().json()).toEqual({ error: 'missing' });

      await expect(runInContext(createContext(), async () => unknownWrapped())).rejects.toThrow('boom');
    },
  );
});

describe('handler response types', () => {
  it('HttpResponse can wrap Response objects', async () => {
    const response = HttpResponse.json({ data: 'test' });
    const native = response.get();

    expect(native).toBeInstanceOf(Response);
    expect(native.status).toBe(200);
  });

  it('HttpResponse preserves headers', () => {
    const response = new HttpResponse('body', {
      status: 200,
      headers: { 'X-Custom': 'value' },
    });

    const native = response.get();
    expect(native.headers.get('X-Custom')).toBe('value');
  });
});
