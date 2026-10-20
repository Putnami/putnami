import { afterEach, beforeEach, describe, expect, it, mock } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { actionHandler as clientActionHandler } from '../../src/client/form/action.handler';
import { layoutLoaderHandler, pageLoaderHandler } from '../../src/client/form/loader.handler';
import { setRouterRoutes } from '../../src/client/router/router-routes';

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
        headers: { Accept: 'application/json', 'X-Putnami-Loader': '1' },
        signal: expect.any(AbortSignal),
      });
      expect(response).toBeInstanceOf(Response);
    });

    it('keeps the query string after the JSON suffix', async () => {
      await pageLoaderHandler()({
        request: new Request('http://localhost/tasks?page=2&sort=name'),
        params: {},
        unstable_pattern: '/tasks',
        context: undefined,
      });

      expect(fetchMock).toHaveBeenCalledWith('http://localhost/tasks.json?page=2&sort=name', {
        headers: { Accept: 'application/json', 'X-Putnami-Loader': '1' },
        signal: expect.any(AbortSignal),
      });
    });

    describe('redirects', () => {
      const navigate = () =>
        pageLoaderHandler()({
          request: new Request('http://localhost/app/tasks/7'),
          params: {},
          context: undefined,
        }) as Promise<Response>;

      it('returns a router redirect, resolved against the page URL, when the loader redirects', async () => {
        fetchMock.mockImplementationOnce(
          async () =>
            new Response(null, {
              status: 204,
              headers: { 'X-Putnami-Redirect': '../login?from=7', 'X-Putnami-Redirect-Status': '307' },
            }),
        );

        const result = await navigate();

        expect(result.status).toBe(307);
        expect(result.headers.get('Location')).toBe('http://localhost/app/login?from=7');
      });

      it('keeps a redirect to another origin absolute', async () => {
        fetchMock.mockImplementationOnce(
          async () =>
            new Response(null, {
              status: 204,
              headers: { 'X-Putnami-Redirect': 'https://idp.example.com/authorize' },
            }),
        );

        const result = await navigate();

        expect(result.status).toBe(302);
        expect(result.headers.get('Location')).toBe('https://idp.example.com/authorize');
      });

      it('redirects to the page fetch already followed when a redirect came before the loader route', async () => {
        fetchMock.mockImplementationOnce(async () => {
          // fetch keeps `Accept: application/json` when it follows, so the page answers a JSON error.
          const followed = new Response(JSON.stringify({ error: 'Method Not Allowed' }), {
            status: 405,
            headers: { 'Content-Type': 'application/json' },
          });
          Object.defineProperty(followed, 'redirected', { value: true });
          Object.defineProperty(followed, 'url', { value: 'http://localhost/login' });
          return followed;
        });

        const result = await navigate();

        expect(result.status).toBe(302);
        expect(result.headers.get('Location')).toBe('http://localhost/login');
      });

      it('keeps the data of a JSON endpoint that fetch reached through a redirect', async () => {
        const followed = new Response(JSON.stringify({ id: 7 }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
        Object.defineProperty(followed, 'redirected', { value: true });
        Object.defineProperty(followed, 'url', { value: 'http://localhost/app/tasks/7/.json' });
        fetchMock.mockImplementationOnce(async () => followed);

        expect(await navigate()).toBe(followed);
      });

      describe('to a page the router does not render', () => {
        const documentLoad = {
          feature: 'typescript/web-application-delivery',
          requirement: 'redirect-outside-the-router',
          check: 'a-redirect-to-a-page-the-router-does-not-render-loads-a-document',
        };
        const inRouter = {
          feature: 'typescript/web-application-delivery',
          requirement: 'redirect-outside-the-router',
          check: 'a-redirect-to-a-router-page-stays-in-the-router',
        };
        const answerRedirect = (location: string) =>
          fetchMock.mockImplementationOnce(
            async () => new Response(null, { status: 204, headers: { 'X-Putnami-Redirect': location } }),
          );
        const answerFollowedPage = (url: string) =>
          fetchMock.mockImplementationOnce(async () => {
            const followed = new Response('<!DOCTYPE html><html></html>', {
              status: 200,
              headers: { 'Content-Type': 'text/html' },
            });
            Object.defineProperty(followed, 'redirected', { value: true });
            Object.defineProperty(followed, 'url', { value: url });
            return followed;
          });
        const isDocumentLoad = (result: Response) => result.headers.get('X-Remix-Reload-Document') === 'true';

        beforeEach(() => {
          setRouterRoutes(
            [
              {
                path: '/',
                children: [
                  { path: 'tasks/:id', id: 'root-tasks-id-page' },
                  { path: 'docs/*', id: 'root-docs-*-page' },
                  { path: '*' },
                ],
              },
            ],
            '/app',
          );
        });

        afterEach(() => {
          setRouterRoutes(undefined);
        });

        specTest('loads a document for a redirect to a server-only path', documentLoad, async () => {
          answerRedirect('/app/login');

          const result = await navigate();

          expect(isDocumentLoad(result)).toBe(true);
          expect(result.headers.get('Location')).toBe('http://localhost/app/login');
        });

        specTest(
          'loads a document for a followed redirect that ended on a server-only page',
          documentLoad,
          async () => {
            answerFollowedPage('http://localhost/app/login');

            const result = await navigate();

            expect(isDocumentLoad(result)).toBe(true);
            expect(result.headers.get('Location')).toBe('http://localhost/app/login');
          },
        );

        it('loads a document for a redirect outside the basename', async () => {
          answerRedirect('/login');

          expect(isDocumentLoad(await navigate())).toBe(true);
        });

        specTest('stays in the router for a redirect to a router page', inRouter, async () => {
          answerRedirect('/app/tasks/8');

          const result = await navigate();

          expect(isDocumentLoad(result)).toBe(false);
          expect(result.headers.get('Location')).toBe('http://localhost/app/tasks/8');
        });

        specTest('stays in the router for a followed redirect that ended on a router page', inRouter, async () => {
          answerFollowedPage('http://localhost/app/docs/intro');

          const result = await navigate();

          expect(isDocumentLoad(result)).toBe(false);
          expect(result.headers.get('Location')).toBe('http://localhost/app/docs/intro');
        });

        it('leaves a redirect to another origin to the router', async () => {
          answerRedirect('https://idp.example.com/authorize');

          const result = await navigate();

          expect(isDocumentLoad(result)).toBe(false);
          expect(result.headers.get('Location')).toBe('https://idp.example.com/authorize');
        });
      });

      it('resolves a layout loader redirect against the page URL, not the layout endpoint', async () => {
        fetchMock.mockImplementationOnce(
          async () => new Response(null, { status: 204, headers: { 'X-Putnami-Redirect': 'login' } }),
        );

        const result = (await layoutLoaderHandler('/docs')({
          request: new Request('http://localhost/docs/guides/start'),
          params: {},
          context: undefined,
        })) as Response;

        expect(result.headers.get('Location')).toBe('http://localhost/docs/guides/login');
      });
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
        headers: { Accept: 'application/json', 'X-Putnami-Loader': '1' },
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
        headers: { Accept: 'application/json', 'X-Putnami-Loader': '1' },
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
        headers: { Accept: 'application/json', 'X-Putnami-Loader': '1' },
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
        headers: { Accept: 'application/json', 'X-Putnami-Loader': '1' },
        signal: expect.any(AbortSignal),
      });
    });

    it('keeps the query string after the layout JSON suffix', async () => {
      await layoutLoaderHandler('/docs')({
        request: new Request('http://localhost/docs/guide?lang=fr'),
        params: {},
        unstable_pattern: '/docs/*',
        context: undefined,
      });

      expect(fetchMock).toHaveBeenCalledWith('http://localhost/docs-layout.json?lang=fr', {
        headers: { Accept: 'application/json', 'X-Putnami-Loader': '1' },
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
        headers: { Accept: 'application/json', 'X-Putnami-Loader': '1' },
        signal: expect.any(AbortSignal),
      });
    });
    describe('under a dynamic segment', () => {
      const fillsParams = {
        feature: 'typescript/web-application-delivery',
        requirement: 'layout-loader-endpoint',
        check: 'a-layout-endpoint-fills-its-dynamic-segments-from-the-matched-params',
      };
      const expectLayoutFetch = (url: string) =>
        expect(fetchMock).toHaveBeenCalledWith(url, {
          headers: { Accept: 'application/json', 'X-Putnami-Loader': '1' },
          signal: expect.any(AbortSignal),
        });

      specTest('fills and encodes a dynamic segment from the matched params', fillsParams, async () => {
        await layoutLoaderHandler('/w/:workspace')({
          request: new Request('http://localhost/w/a%20b/settings'),
          params: { workspace: 'a b' },
          context: undefined,
        });

        expectLayoutFetch('http://localhost/w/a%20b-layout.json');
      });

      specTest('fills nested dynamic segments and encodes a slash in a value', fillsParams, async () => {
        await layoutLoaderHandler('/w/:workspace/p/:project')({
          request: new Request('http://localhost/w/acme/p/x%2Fy/runs'),
          params: { workspace: 'acme', project: 'x/y' },
          context: undefined,
        });

        expectLayoutFetch('http://localhost/w/acme/p/x%2Fy-layout.json');
      });

      specTest('keeps the basename and the query string around the filled path', fillsParams, async () => {
        (globalThis as typeof globalThis & { window?: { __basename?: string } }).window = { __basename: '/app' };

        await layoutLoaderHandler('/w/:workspace')({
          request: new Request('http://localhost/app/w/acme/settings?tab=2'),
          params: { workspace: 'acme' },
          context: undefined,
        });

        expectLayoutFetch('http://localhost/app/w/acme-layout.json?tab=2');
      });

      it('fills the unstable_pattern fallback and drops its catch-all', async () => {
        await layoutLoaderHandler()({
          request: new Request('http://localhost/w/acme/docs/intro'),
          params: { workspace: 'acme', '*': 'docs/intro' },
          unstable_pattern: '/w/:workspace/*',
          context: undefined,
        });

        expectLayoutFetch('http://localhost/w/acme-layout.json');
      });

      it('drops an optional segment that has no value', async () => {
        await layoutLoaderHandler('/w/:workspace?')({
          request: new Request('http://localhost/w'),
          params: {},
          context: undefined,
        });

        expectLayoutFetch('http://localhost/w-layout.json');
      });

      specTest(
        'rejects without a request when a required param is missing',
        {
          feature: 'typescript/web-application-delivery',
          requirement: 'layout-loader-endpoint',
          check: 'a-layout-endpoint-without-a-required-param-is-never-requested',
        },
        async () => {
          const load = layoutLoaderHandler('/w/:workspace')({
            request: new Request('http://localhost/w/acme'),
            params: {},
            context: undefined,
          });

          await expect(load).rejects.toThrow('Missing ":workspace" param');
          expect(fetchMock).not.toHaveBeenCalled();
        },
      );
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
        expect(init?.headers).toEqual({ 'X-Putnami-Action': '1', 'X-CSRF-Token': 'token-123' });
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

    const submit = () =>
      clientActionHandler({
        request: new Request('http://localhost/app/tasks/7', { method: 'POST', body: new FormData() }),
      } as never);

    it('returns a router redirect, resolved against the action URL, when the action redirects', async () => {
      fetchMock.mockImplementationOnce(
        async () =>
          new Response(null, {
            status: 204,
            headers: { 'X-Putnami-Redirect': '../done?from=7', 'X-Putnami-Redirect-Status': '303' },
          }),
      );

      const result = (await submit()) as Response;

      expect(result).toBeInstanceOf(Response);
      expect(result.status).toBe(303);
      expect(result.headers.get('Location')).toBe('http://localhost/app/done?from=7');
    });

    it('keeps a redirect to another origin absolute', async () => {
      fetchMock.mockImplementationOnce(
        async () =>
          new Response(null, {
            status: 204,
            headers: { 'X-Putnami-Redirect': 'https://idp.example.com/authorize' },
          }),
      );

      const result = (await submit()) as Response;

      expect(result.status).toBe(302);
      expect(result.headers.get('Location')).toBe('https://idp.example.com/authorize');
    });

    it('redirects to the page fetch already followed when a redirect came before the action route', async () => {
      fetchMock.mockImplementationOnce(async () => {
        const followed = new Response('<!doctype html>', { status: 200, headers: { 'Content-Type': 'text/html' } });
        Object.defineProperty(followed, 'redirected', { value: true });
        Object.defineProperty(followed, 'url', { value: 'http://localhost/login' });
        return followed;
      });

      const result = (await submit()) as Response;

      expect(result.status).toBe(302);
      expect(result.headers.get('Location')).toBe('http://localhost/login');
    });

    it('keeps the data of an action that a followed 307 re-sent the POST to', async () => {
      fetchMock.mockImplementationOnce(async () => {
        const followed = new Response(JSON.stringify({ saved: true }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
        Object.defineProperty(followed, 'redirected', { value: true });
        Object.defineProperty(followed, 'url', { value: 'http://localhost/app/tasks/7/' });
        return followed;
      });

      expect(await submit()).toEqual({ status: 200, ok: true, saved: true });
    });

    it('falls back to 302 for a status React Router does not follow', async () => {
      fetchMock.mockImplementationOnce(
        async () =>
          new Response(null, {
            status: 204,
            headers: { 'X-Putnami-Redirect': '/tasks', 'X-Putnami-Redirect-Status': '300' },
          }),
      );

      const result = (await submit()) as Response;

      expect(result.status).toBe(302);
      expect(result.headers.get('Location')).toBe('http://localhost/tasks');
    });
  });
});
