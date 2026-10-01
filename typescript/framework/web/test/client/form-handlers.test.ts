import { afterEach, beforeEach, describe, expect, it, mock } from 'bun:test';
import { actionHandler as clientActionHandler } from '../../src/client/form/action.handler';
import { layoutLoaderHandler, pageLoaderHandler } from '../../src/client/form/loader.handler';

describe('Client Form Handlers', () => {
  const originalFetch = globalThis.fetch;
  const fetchMock = mock<typeof fetch>(async () => new Response(JSON.stringify({ ok: true }), { status: 200 }));

  beforeEach(() => {
    fetchMock.mockClear();
    globalThis.fetch = fetchMock;
    Reflect.deleteProperty(globalThis, 'document');
    Reflect.deleteProperty(globalThis, 'window');
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
    Reflect.deleteProperty(globalThis, 'document');
    Reflect.deleteProperty(globalThis, 'window');
  });

  describe('pageLoaderHandler', () => {
    it('is a function', () => {
      expect(typeof pageLoaderHandler).toBe('function');
    });

    it('returns a function when called', () => {
      const handler = pageLoaderHandler();
      expect(typeof handler).toBe('function');
    });

    it('fetches the page JSON endpoint', async () => {
      const handler = pageLoaderHandler();
      const response = await handler({
        request: new Request('http://localhost/tasks'),
        params: {},
        unstable_pattern: '/tasks',
        context: undefined,
      });

      expect(fetchMock).toHaveBeenCalledWith('http://localhost/tasks.json', {
        headers: { Accept: 'application/json' },
        signal: expect.any(AbortSignal),
      });
      expect(response).toBeInstanceOf(Response);
    });
  });

  describe('layoutLoaderHandler', () => {
    it('is a function', () => {
      expect(typeof layoutLoaderHandler).toBe('function');
    });

    it('returns a function when called', () => {
      const handler = layoutLoaderHandler();
      expect(typeof handler).toBe('function');
    });

    it('uses the route pattern for layout JSON requests', async () => {
      const handler = layoutLoaderHandler();

      await handler({
        request: new Request('http://localhost/docs/getting-started'),
        params: {},
        unstable_pattern: '/docs/*',
        context: undefined,
      });

      expect(fetchMock).toHaveBeenCalledWith('http://localhost/docs-layout.json', {
        headers: { Accept: 'application/json' },
        signal: expect.any(AbortSignal),
      });
    });

    it('derives the layout URL from the framework-supplied pattern, not unstable_pattern', async () => {
      // The generator threads the framework-owned route pattern into the handler.
      // It must win over React Router's experimental `unstable_pattern` so the
      // derivation survives that field being renamed/removed in a 7.x minor.
      const handler = layoutLoaderHandler('/docs');

      await handler({
        request: new Request('http://localhost/docs/getting-started'),
        params: {},
        // Deliberately wrong/foreign to prove it is NOT the source of truth.
        unstable_pattern: '/SHOULD-NOT-BE-USED/*',
        context: undefined,
      });

      expect(fetchMock).toHaveBeenCalledWith('http://localhost/docs-layout.json', {
        headers: { Accept: 'application/json' },
        signal: expect.any(AbortSignal),
      });
    });

    it('derives the layout URL from the framework pattern even when unstable_pattern is absent', async () => {
      // A future react-router could drop `unstable_pattern` entirely; the
      // framework pattern must still produce the correct endpoint.
      const handler = layoutLoaderHandler('/docs');

      await handler({
        request: new Request('http://localhost/docs/getting-started'),
        params: {},
        context: undefined,
      });

      expect(fetchMock).toHaveBeenCalledWith('http://localhost/docs-layout.json', {
        headers: { Accept: 'application/json' },
        signal: expect.any(AbortSignal),
      });
    });

    it('prepends the mounted basename so a prefixed app hits the right endpoint', async () => {
      // App mounted under `/app`: the JSON endpoints are served under that
      // prefix too, so the reconstructed pattern-based URL must include it.
      (globalThis as typeof globalThis & { window?: { __basename?: string } }).window = { __basename: '/app' };
      const handler = layoutLoaderHandler('/docs');

      await handler({
        request: new Request('http://localhost/app/docs/getting-started'),
        params: {},
        unstable_pattern: '/docs/*',
        context: undefined,
      });

      expect(fetchMock).toHaveBeenCalledWith('http://localhost/app/docs-layout.json', {
        headers: { Accept: 'application/json' },
        signal: expect.any(AbortSignal),
      });
    });

    it('falls back to unstable_pattern when the framework pattern is not supplied', async () => {
      // Older generated bundles call `layoutLoaderHandler()` without a pattern;
      // the experimental field remains a last-resort fallback.
      const handler = layoutLoaderHandler();

      await handler({
        request: new Request('http://localhost/docs/getting-started'),
        params: {},
        unstable_pattern: '/docs/*',
        context: undefined,
      });

      expect(fetchMock).toHaveBeenCalledWith('http://localhost/docs-layout.json', {
        headers: { Accept: 'application/json' },
        signal: expect.any(AbortSignal),
      });
    });
  });

  describe('clientActionHandler', () => {
    it('is a function', () => {
      expect(typeof clientActionHandler).toBe('function');
    });

    it('is an ActionFunction type', () => {
      // ActionFunction takes an object with request property
      expect(typeof clientActionHandler).toBe('function');
    });

    it('posts form data and includes the CSRF header when present', async () => {
      Object.defineProperty(globalThis, 'document', {
        configurable: true,
        value: { cookie: '_csrf=token-123' },
      });
      fetchMock.mockImplementationOnce(async (_input, init) => {
        expect(init?.method).toBe('POST');
        expect(init?.headers).toEqual({ 'X-CSRF-Token': 'token-123' });
        expect(init?.body).toBeInstanceOf(FormData);
        return new Response(JSON.stringify({ saved: true }), { status: 201 });
      });

      const formData = new FormData();
      formData.set('name', 'Ada');
      const result = await clientActionHandler({
        request: new Request('http://localhost/tasks', {
          method: 'POST',
          body: formData,
        }),
      } as never);

      expect(result).toEqual({ status: 201, ok: true, saved: true });
    });

    it('returns structured error payloads for non-ok responses', async () => {
      fetchMock.mockImplementationOnce(
        async () => new Response(JSON.stringify({ message: 'invalid' }), { status: 422 }),
      );

      const formData = new FormData();
      formData.set('name', 'Ada');
      const result = await clientActionHandler({
        request: new Request('http://localhost/tasks', {
          method: 'POST',
          body: formData,
        }),
      } as never);

      expect(result).toEqual({ status: 422, ok: false, message: 'invalid' });
    });
  });
});
