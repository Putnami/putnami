import React from 'react';
import { describe, expect, it } from 'bun:test';
import { CSP_NONCE_CONTEXT_KEY, SecurityHeadersMiddleware, type HttpRequestContext } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import { restoreEnv } from '@putnami/utils';
import { buildHttpContext } from '../../../application/src/http/http-context.builder';
import type { StaticHandlerContext } from 'react-router';
import { runInContext } from '../../../runtime/src/context/context.utils';
import RootHtml from '../../src/ssr/default.html';
import {
  handleRenderError,
  pageRenderer,
  safeJsonForScript,
  sanitizeErrors,
  wrapInHtmlDocument,
} from '../../src/ssr/page.renderer';
import { SsrDocumentHelper } from '../../src/client/document/document-ssr.helper';
import { clientBootstrapEmitted } from '../../src/shared/context-slots';

// SSR render timeout the tests drive pageRenderer with. Shared so the abort-timer
// test can key off the same value instead of a bare literal that could drift.
const SSR_TIMEOUT_MS = 500;

function createMockContext(overrides: Record<string, unknown> = {}) {
  const req = overrides.req instanceof Request ? overrides.req : new Request('http://localhost/');
  const { req: _req, ...rest } = overrides;
  return Object.assign(buildHttpContext<HttpRequestContext>({ req }), { params: {}, statusCode: 0 }, rest) as never;
}

const createRouterContext = (overrides: Partial<StaticHandlerContext> = {}): StaticHandlerContext => ({
  basename: '/',
  location: { pathname: '/', search: '', hash: '', state: null, key: 'default' },
  matches: [],
  loaderData: {},
  actionData: null,
  errors: null,
  statusCode: 200,
  loaderHeaders: {},
  actionHeaders: {},
  _deepestRenderedBoundaryId: null,
  ...overrides,
});

async function renderRoute(
  route: Record<string, unknown>,
  ctxOverrides: Record<string, unknown> = {},
  basename?: string,
) {
  const handler = pageRenderer(
    () => [route as never],
    () => RootHtml,
    () => 'hydrate.js',
    SSR_TIMEOUT_MS,
    basename ? () => basename : undefined,
  );

  return await runInContext(createMockContext(ctxOverrides), async () => await handler());
}

describe('page.renderer', () => {
  describe('pageRenderer', () => {
    it('renders an HTML document for a simple route', async () => {
      const response = await renderRoute({
        path: '/',
        element: React.createElement('div', null, 'Hello SSR'),
      });

      expect(response.status).toBe(200);
      const html = await response.get().text();
      expect(html).toContain('<div>Hello SSR</div>');
      expect(html).toContain('src="/hydrate.js"');
      expect(html).toContain('window.__staticRouterHydrationData = {"loaderData":{},"actionData":null,"errors":null};');
    });

    it('marks the request context as carrying the client bootstrap', async () => {
      const ctx = createMockContext();
      const handler = pageRenderer(
        () => [{ path: '/', element: React.createElement('div', null, 'Rendered') } as never],
        () => RootHtml,
        () => 'hydrate.js',
        SSR_TIMEOUT_MS,
      );

      // Unset until the renderer runs: a plugin reads the slot to tell a
      // document that carries what it published from one that does not — a
      // pre-rendered page never reaches this code.
      expect(clientBootstrapEmitted(ctx)).toBe(false);
      const response = await runInContext(ctx, async () => await handler());
      await response.get().text();

      expect(clientBootstrapEmitted(ctx)).toBe(true);
    });

    it('stamps the matching nonce on hydration scripts and publishes it on the request context', async () => {
      const ctx = createMockContext();
      const handler = pageRenderer(
        () => [{ path: '/', element: React.createElement('div', null, 'Nonced') } as never],
        () => RootHtml,
        () => 'hydrate.js',
        500,
      );
      const response = await runInContext(ctx, async () => await handler());

      // get() may only be called once — it marks the body as used.
      const built = response.get();
      const nonce = (ctx as Record<string, unknown>)[CSP_NONCE_CONTEXT_KEY] as string;
      expect(nonce.length).toBeGreaterThan(0);
      expect(built.headers.get('content-security-policy')).toBeNull();

      // The same nonce must appear on both framework-emitted script tags so a
      // strict CSP does not block hydration.
      const html = await built.text();
      expect(html).toContain(`<script nonce="${nonce}">`);
      expect(html).toContain(`<script type="module" nonce="${nonce}" src="/hydrate.js"></script>`);
      // The framework script is the only one that carries hydration data: a
      // second, unnonced copy inside #root is blocked by the CSP and is markup
      // the client router does not render, so hydration mismatches.
      const scriptTags = html.match(/<script\b[^>]*>/g) ?? [];
      expect(scriptTags.length).toBeGreaterThan(0);
      for (const tag of scriptTags) expect(tag).toContain(`nonce="${nonce}"`);
      expect(html.split('__staticRouterHydrationData').length - 1).toBe(1);
    });

    specTest(
      'stamps the nonce on the script React writes to move a streamed boundary into place',
      {
        feature: 'typescript/web-application-delivery',
        requirement: 'csrf-and-csp',
        check: 'a-script-react-streams-carries-the-per-request-nonce',
      },
      async () => {
        // A lazy page above 12,800 bytes inside a layout element: React streams
        // its boundary after the shell, with an inline script that moves it
        // into place.
        const lines = Array.from({ length: 400 }, (_, index) =>
          React.createElement('p', { key: index }, `Line ${index} of a page longer than one stream chunk.`),
        );
        const LongPage = React.lazy(async () => ({ default: () => React.createElement('div', null, lines) }));
        const ctx = createMockContext();
        const handler = pageRenderer(
          () => [
            {
              path: '/',
              element: React.createElement(
                'main',
                null,
                React.createElement(React.Suspense, { fallback: null }, React.createElement(LongPage)),
              ),
            } as never,
          ],
          () => RootHtml,
          () => 'hydrate.js',
          SSR_TIMEOUT_MS,
        );
        const response = await runInContext(ctx, async () => await handler());
        const html = await response.get().text();
        const nonce = (ctx as Record<string, unknown>)[CSP_NONCE_CONTEXT_KEY] as string;

        const root = html.slice(html.indexOf('<div id="root">'));
        const streamed = root.match(/<script\b[^>]*>/g) ?? [];
        expect(root).toContain('<!--$?-->');
        expect(streamed.length).toBeGreaterThan(0);
        for (const tag of html.match(/<script\b[^>]*>/g) ?? []) expect(tag).toContain(`nonce="${nonce}"`);
      },
    );

    it('lets security headers preserve an explicit CSP while adding the SSR nonce', async () => {
      const ctx = createMockContext();
      const handler = pageRenderer(
        () => [{ path: '/', element: React.createElement('div', null, 'Nonced') } as never],
        () => RootHtml,
        () => 'hydrate.js',
        500,
      );
      const middleware = SecurityHeadersMiddleware({
        contentSecurityPolicy: "default-src 'self'; script-src 'self' https://cdn.example.com",
      });

      const response = await runInContext(ctx, async () => await middleware(ctx, async () => await handler()));
      if (!response) throw new Error('expected SSR response');
      const built = response.get();
      const csp = built.headers.get('content-security-policy');
      const nonce = (ctx as Record<string, unknown>)[CSP_NONCE_CONTEXT_KEY] as string;

      expect(csp).toBeTruthy();
      expect(csp).toContain('https://cdn.example.com');
      expect(csp).toContain(`'nonce-${nonce}'`);
    });

    it('injects basename and security context when available', async () => {
      const response = await renderRoute(
        {
          path: '/',
          element: React.createElement('div', null, 'Secure page'),
        },
        {
          req: new Request('http://localhost/docs'),
          user: { sub: 'user-1', roles: ['admin'], scope: 'docs:read docs:write' },
        },
        '/docs',
      );

      const html = await response.get().text();
      expect(html).toContain('window.__basename="\\/docs"'.replace('\\/', '/'));
      expect(html).toContain('"authenticated":true');
      expect(html).toContain('"roles":["admin"]');
      expect(html).toContain('"scopes":["docs:read","docs:write"]');
    });

    it('returns redirect responses thrown by loaders', async () => {
      const response = await renderRoute(
        {
          path: '/from',
          loader: () => {
            throw Response.redirect('http://localhost/to', 302);
          },
          element: React.createElement('div', null, 'Redirect'),
        },
        {
          req: new Request('http://localhost/from'),
        },
      );

      expect(response.status).toBe(302);
      expect(response.redirected).toBe(true);
      expect(response.get().headers.get('location')).toBe('http://localhost/to');
    });

    it('renders route errors with the router status code', async () => {
      const response = await renderRoute(
        {
          path: '/missing',
          loader: () => {
            throw new Response('missing', { status: 404 });
          },
          element: React.createElement('div', null, 'Missing'),
        },
        {
          req: new Request('http://localhost/missing'),
        },
      );

      expect(response.status).toBe(404);
      const html = await response.get().text();
      expect(html).toContain('Unexpected Application Error');
      expect(html).toContain('"status":404');
    });

    it('falls back to the request context status code when the router stays at 200', async () => {
      const response = await renderRoute(
        {
          path: '/',
          element: React.createElement('div', null, 'Created'),
        },
        {
          statusCode: 201,
        },
      );

      expect(response.status).toBe(201);
    });

    it('returns a formatted 500 response when rendering throws', async () => {
      const response = await renderRoute({
        path: '/',
        element: React.createElement(() => {
          throw new Error('boom');
        }),
      });

      expect(response.status).toBe(500);
      const html = await response.get().text();
      expect(html).toContain('<h1>Something went wrong</h1>');
      expect(html).toContain('Error: boom');
    });

    it('clears the abort timeout when rendering throws before streaming', async () => {
      // renderRoute drives pageRenderer with ssrTimeout=SSR_TIMEOUT_MS, so the
      // abort timer is the setTimeout with that delay. On the error path
      // wrapInHtmlDocument never runs, so this timer must be released in the
      // catch, not leaked for the window.
      const realSetTimeout = globalThis.setTimeout;
      const realClearTimeout = globalThis.clearTimeout;
      const abortTimerIds = new Set<unknown>();
      const cleared = new Set<unknown>();
      globalThis.setTimeout = ((fn: TimerHandler, delay?: number, ...rest: unknown[]) => {
        const id = realSetTimeout(fn, delay, ...(rest as []));
        if (delay === SSR_TIMEOUT_MS) abortTimerIds.add(id);
        return id;
      }) as typeof globalThis.setTimeout;
      globalThis.clearTimeout = ((id?: number | ReturnType<typeof setTimeout>) => {
        cleared.add(id);
        return realClearTimeout(id);
      }) as typeof globalThis.clearTimeout;

      try {
        const response = await renderRoute({
          path: '/',
          element: React.createElement(() => {
            throw new Error('boom');
          }),
        });

        expect(response.status).toBe(500);
        expect(abortTimerIds.size).toBeGreaterThan(0);
        for (const id of abortTimerIds) {
          expect(cleared.has(id)).toBe(true);
        }
      } finally {
        globalThis.setTimeout = realSetTimeout;
        globalThis.clearTimeout = realClearTimeout;
      }
    });
  });

  describe('wrapInHtmlDocument', () => {
    it('streams the HTML shell, hydration data, and React body', async () => {
      const helper = new SsrDocumentHelper();
      helper.title = 'Docs';
      const chunks = ['<main>', 'Body', '</main>'];
      let index = 0;
      const readable = await wrapInHtmlDocument(
        {
          allReady: Promise.resolve(),
          getReader: () => ({
            read: async () =>
              index < chunks.length
                ? { done: false, value: new TextEncoder().encode(chunks[index++]) }
                : { done: true, value: undefined },
          }),
        } as never,
        helper,
        {
          timeoutId: setTimeout(() => {}, 1000),
          context: createRouterContext({
            loaderData: { root: { title: 'Docs' } },
          }),
          hydrateScript: 'hydrate.js',
          basename: '/docs',
          securityContext: { authenticated: true, roles: ['admin'], scopes: ['docs:read'] },
          nonce: 'test-nonce-123',
          clientFetchTimeout: 5000,
        },
      );

      const html = await new Response(readable).text();
      expect(html).toContain('<title>Docs</title>');
      expect(html).toContain('<div id="root"><main>Body</main></div>');
      expect(html).toContain('window.__basename="/docs";');
      expect(html).toContain('"authenticated":true');
      expect(html).toContain('window.__reactClientFetchTimeoutMs=5000;');
      expect(html).toContain('src="/hydrate.js"');
    });

    it('injects the error-disclosure flag outside production and omits it in production', async () => {
      // The browser has no NODE_ENV of its own, so this flag is the only thing
      // that lets the hydrated boundary match what the server streamed. It must
      // be absent in production: the client guard fails closed without it.
      const emptyStream = () =>
        ({
          allReady: Promise.resolve(),
          getReader: () => ({ read: async () => ({ done: true, value: undefined }) }),
        }) as never;
      const render = async () => {
        const readable = await wrapInHtmlDocument(emptyStream(), new SsrDocumentHelper(), {
          timeoutId: setTimeout(() => {}, 1000),
          context: createRouterContext(),
          hydrateScript: 'hydrate.js',
        });
        return new Response(readable).text();
      };

      const previousNodeEnv = process.env.NODE_ENV;
      const previousService = process.env.K_SERVICE;
      try {
        delete process.env.K_SERVICE;
        process.env.NODE_ENV = 'development';
        expect(await render()).toContain('window.__putnamiExposeErrors=true;');

        process.env.NODE_ENV = 'production';
        expect(await render()).not.toContain('__putnamiExposeErrors');
      } finally {
        restoreEnv('NODE_ENV', previousNodeEnv);
        restoreEnv('K_SERVICE', previousService);
      }
    });

    it('omits the client fetch timeout global when not configured', async () => {
      const readable = await wrapInHtmlDocument(
        {
          allReady: Promise.resolve(),
          getReader: () => ({
            read: async () => ({ done: true, value: undefined }),
          }),
        } as never,
        new SsrDocumentHelper(),
        {
          timeoutId: setTimeout(() => {}, 1000),
          context: createRouterContext(),
          hydrateScript: 'hydrate.js',
          nonce: 'test-nonce-123',
        },
      );

      const html = await new Response(readable).text();
      expect(html).not.toContain('__reactClientFetchTimeoutMs');
    });

    it('stamps the nonce on the inline hydration and module bootstrap scripts', async () => {
      const readable = await wrapInHtmlDocument(
        {
          allReady: Promise.resolve(),
          getReader: () => ({
            read: async () => ({ done: true, value: undefined }),
          }),
        } as never,
        new SsrDocumentHelper(),
        {
          timeoutId: setTimeout(() => {}, 1000),
          context: createRouterContext(),
          hydrateScript: 'hydrate.js',
          nonce: 'test-nonce-123',
        },
      );

      const html = await new Response(readable).text();
      // Inline hydration data script carries the nonce
      expect(html).toContain('<script nonce="test-nonce-123">');
      // Module bootstrap script carries the nonce
      expect(html).toContain('<script type="module" nonce="test-nonce-123" src="/hydrate.js"></script>');
      // No framework script is emitted without a nonce
      expect(html).not.toContain('<script>');
      expect(html).not.toContain('<script type="module" src=');
    });

    it('omits the nonce attribute when no nonce is provided', async () => {
      const readable = await wrapInHtmlDocument(
        {
          allReady: Promise.resolve(),
          getReader: () => ({
            read: async () => ({ done: true, value: undefined }),
          }),
        } as never,
        new SsrDocumentHelper(),
        {
          timeoutId: setTimeout(() => {}, 1000),
          context: createRouterContext(),
          hydrateScript: 'hydrate.js',
        },
      );

      const html = await new Response(readable).text();
      expect(html).not.toContain('nonce=');
      expect(html).toContain('<script>');
      expect(html).toContain('<script type="module" src="/hydrate.js"></script>');
    });

    it('writes a fallback error footer when streaming fails', async () => {
      const logger = { error: () => undefined };
      const readable = await wrapInHtmlDocument(
        {
          allReady: Promise.resolve(),
          getReader: () => ({
            read: async () => {
              throw new Error('stream-failed');
            },
          }),
        } as never,
        new SsrDocumentHelper(),
        {
          timeoutId: setTimeout(() => {}, 1000),
          context: createRouterContext(),
          hydrateScript: 'hydrate.js',
          logger,
        },
      );

      const html = await new Response(readable).text();
      expect(html).toContain('<!-- SSR error -->');
      expect(html).toContain('</body></html>');
    });

    it('keeps a hostile loader return value from breaking out of the hydration <script>', async () => {
      // End-to-end guard: whatever a loader returns is serialized into the inline
      // window.__staticRouterHydrationData script, so a break-out payload there
      // must never reach the rendered HTML verbatim.
      const ls = String.fromCharCode(0x20_28);
      const hostile = `</script><script>alert(1)</script>${ls}`;
      const readable = await wrapInHtmlDocument(
        {
          allReady: Promise.resolve(),
          getReader: () => ({
            read: async () => ({ done: true, value: undefined }),
          }),
        } as never,
        new SsrDocumentHelper(),
        {
          timeoutId: setTimeout(() => {}, 1000),
          context: createRouterContext({
            loaderData: { root: { evil: hostile } },
          }),
          hydrateScript: 'hydrate.js',
          nonce: 'test-nonce-123',
        },
      );

      const html = await new Response(readable).text();
      // The only </script> in the document closes the framework's own tags;
      // the loader payload must be escaped, not echoed literally.
      expect(html).toContain('window.__staticRouterHydrationData =');
      expect(html).not.toContain('</script><script>alert(1)</script>');
      expect(html).not.toContain(ls);
      // The escaped form is present and the value round-trips out of the blob.
      const match = html.match(/window\.__staticRouterHydrationData = (.+?);<\/script>/s);
      if (!match) throw new Error('expected hydration data blob in rendered HTML');
      expect(JSON.parse(match[1]).loaderData).toEqual({ root: { evil: hostile } });
    });
  });

  describe('handleRenderError', () => {
    it('returns an escaped development error response', async () => {
      const response = handleRenderError(new Error('<boom>'));
      const html = await response.get().text();

      expect(response.status).toBe(500);
      expect(html).toContain('&lt;boom&gt;');
      expect(html).not.toContain('<boom>');
    });
  });

  describe('safeJsonForScript', () => {
    it('escapes </script> in string values', () => {
      const result = safeJsonForScript({ value: '</script><script>alert(1)</script>' });
      expect(result).not.toContain('</script>');
      expect(result).not.toContain('<script>');
      expect(JSON.parse(result)).toEqual({ value: '</script><script>alert(1)</script>' });
    });

    it('escapes </script> in nested objects', () => {
      const result = safeJsonForScript({ a: { b: '</script>' } });
      expect(result).not.toContain('</script>');
      expect(JSON.parse(result)).toEqual({ a: { b: '</script>' } });
    });

    it('escapes <!-- comment injection', () => {
      const result = safeJsonForScript({ value: '<!--' });
      expect(result).not.toContain('<!--');
      expect(JSON.parse(result)).toEqual({ value: '<!--' });
    });

    it('escapes the U+2028 line separator that breaks <script> embedding', () => {
      // U+2028 is a JS line terminator; an unescaped one would truncate the
      // inline <script> statement. Build it from a charcode to keep source ASCII.
      const sep = String.fromCharCode(0x20_28);
      const value = { note: `before${sep}after` };
      const result = safeJsonForScript(value);
      // The raw separator must not survive into the emitted script source...
      expect(result).not.toContain(sep);
      expect(result).toContain('\\u2028');
      // ...but the value must still round-trip back to the original.
      expect(JSON.parse(result)).toEqual(value);
    });

    it('escapes the U+2029 paragraph separator that breaks <script> embedding', () => {
      const sep = String.fromCharCode(0x20_29);
      const value = { note: `before${sep}after` };
      const result = safeJsonForScript(value);
      expect(result).not.toContain(sep);
      expect(result).toContain('\\u2029');
      expect(JSON.parse(result)).toEqual(value);
    });

    it('keeps a hostile loader payload inert yet round-trippable', () => {
      const ls = String.fromCharCode(0x20_28);
      const ps = String.fromCharCode(0x20_29);
      const hostile = { x: '</script><script>alert(1)</script>', y: `a${ls}b${ps}c` };
      const result = safeJsonForScript(hostile);
      // Nothing left that could break out of the surrounding inline <script>.
      expect(result).not.toContain('</script>');
      expect(result).not.toContain('<script>');
      expect(result).not.toContain(ls);
      expect(result).not.toContain(ps);
      expect(JSON.parse(result)).toEqual(hostile);
    });

    it('preserves normal JSON values', () => {
      const data = { roles: ['admin', 'user'], count: 42, active: true };
      const result = safeJsonForScript(data);
      expect(JSON.parse(result)).toEqual(data);
    });

    it('handles string primitive', () => {
      const result = safeJsonForScript('hello</script>');
      expect(result).not.toContain('</script>');
      expect(JSON.parse(result)).toBe('hello</script>');
    });

    it('handles null and numbers', () => {
      expect(safeJsonForScript(null)).toBe('null');
      expect(safeJsonForScript(42)).toBe('42');
    });
  });

  describe('sanitizeErrors', () => {
    it('returns undefined for undefined and null for null', () => {
      expect(sanitizeErrors(undefined)).toBeUndefined();
      expect(sanitizeErrors(null)).toBeNull();
    });

    it('strips stack traces and internal fields from Error objects', () => {
      const err = new Error('Something failed');
      err.stack = 'Error: Something failed\n    at /app/src/secret/path.ts:42';
      (err as { cause?: string }).cause = 'internal db timeout';

      const result = sanitizeErrors({ root: err });

      expect(result).toEqual({ root: { message: 'Something failed', status: undefined } });
    });

    it('preserves status from HttpException-like errors', () => {
      const err = Object.assign(new Error('Not found'), { status: 404 });

      const result = sanitizeErrors({ route: err });

      expect(result).toEqual({ route: { message: 'Not found', status: 404 } });
    });

    it('passes through non-Error values unchanged', () => {
      const result = sanitizeErrors({ key: 'plain string' });

      expect(result).toEqual({ key: 'plain string' });
    });
  });
});
