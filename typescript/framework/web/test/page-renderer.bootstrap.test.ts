import React from 'react';
import { describe, expect, it } from 'bun:test';
import type { HttpRequestContext } from '@putnami/application';
import type { StaticHandlerContext } from 'react-router';
import { buildHttpContext } from '../../application/src/http/http-context.builder';
import { runInContext } from '../../runtime/src/context/context.utils';
import RootHtml from '../src/ssr/default.html';
import { pageRenderer, wrapInHtmlDocument } from '../src/ssr/page.renderer';
import { SsrDocumentHelper } from '../src/client/document/document-ssr.helper';
import {
  mergeClientBootstrap,
  pushClientScript,
  readClientBootstrap,
  readClientScripts,
} from '../src/shared/context-slots';

const NONCE = 'test-nonce-123';

const routerContext = (): StaticHandlerContext => ({
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
});

/** Minimal stand-in for the React server stream: nothing to read, already ready. */
const emptyStream = () =>
  ({
    allReady: Promise.resolve(),
    getReader: () => ({
      read: async () => ({ done: true, value: undefined }),
    }),
  }) as never;

async function renderShell(options: { bootstrap?: Record<string, unknown>; clientScripts?: readonly string[] }) {
  const readable = await wrapInHtmlDocument(emptyStream(), new SsrDocumentHelper(), {
    timeoutId: setTimeout(() => {}, 1000),
    context: routerContext(),
    hydrateScript: 'hydrate.js',
    nonce: NONCE,
    ...options,
  });
  return await new Response(readable).text();
}

describe('page renderer client bootstrap slot', () => {
  it('emits no bootstrap global when no plugin wrote to the slot', async () => {
    expect(await renderShell({})).not.toContain('__putnamiBootstrap');
    // An empty object is the same statement as "nothing was written".
    expect(await renderShell({ bootstrap: {} })).not.toContain('__putnamiBootstrap');
  });

  it('serializes the bootstrap inside the nonce-stamped script before the hydration data', async () => {
    const html = await renderShell({ bootstrap: { plugin: { pv: 'x' } } });

    expect(html).toContain(`<script nonce="${NONCE}">`);
    expect(html).toContain('window.__putnamiBootstrap={"plugin":{"pv":"x"}};');

    const bootstrapAt = html.indexOf('window.__putnamiBootstrap');
    const hydrationAt = html.indexOf('window.__staticRouterHydrationData');
    const scriptOpenAt = html.indexOf(`<script nonce="${NONCE}">`);
    const scriptCloseAt = html.indexOf('</script>', scriptOpenAt);
    expect(bootstrapAt).toBeGreaterThan(scriptOpenAt);
    expect(bootstrapAt).toBeLessThan(hydrationAt);
    expect(hydrationAt).toBeLessThan(scriptCloseAt);
  });

  it('escapes a bootstrap value that would otherwise close the script tag', async () => {
    const html = await renderShell({ bootstrap: { note: '</script><img src=x onerror=alert(1)>' } });

    // The raw sequence must not appear: it would end the inline script early and
    // hand the rest of the payload to the HTML parser as markup.
    expect(html).not.toContain('</script><img');
    expect(html).toContain('\\u003c/script\\u003e');
    // The document still closes its own script exactly once before the body.
    expect(html).toContain('window.__staticRouterHydrationData');
  });

  it('emits one nonce-stamped module tag per client script after the hydrate tag', async () => {
    const html = await renderShell({ clientScripts: ['/a.js', 'b.js'] });

    const hydrateTag = `<script type="module" nonce="${NONCE}" src="/hydrate.js"></script>`;
    const aTag = `<script type="module" nonce="${NONCE}" src="/a.js"></script>`;
    // A relative URL is normalized to an absolute path.
    const bTag = `<script type="module" nonce="${NONCE}" src="/b.js"></script>`;

    expect(html.split(aTag).length - 1).toBe(1);
    expect(html.split(bTag).length - 1).toBe(1);
    expect(html.indexOf(aTag)).toBeGreaterThan(html.indexOf(hydrateTag));
    expect(html.indexOf(bTag)).toBeGreaterThan(html.indexOf(aTag));
    expect(html).not.toContain('<script type="module" src=');
  });

  it('escapes a client script URL so it cannot break out of the src attribute', async () => {
    const html = await renderShell({ clientScripts: ['/t.js"></script><script>alert(1)</script>'] });

    expect(html).not.toContain('"></script><script>alert(1)');
    expect(html).toContain('&quot;');
  });

  it('reads both slots off the request context when rendering a page', async () => {
    const ctx = buildHttpContext<HttpRequestContext>({ req: new Request('http://localhost/') });
    Object.assign(ctx, { params: {}, statusCode: 0 });

    mergeClientBootstrap(ctx, { plugin: { pv: 'x' } });
    mergeClientBootstrap(ctx, { plugin: { pv: 'y' }, other: 1 });
    pushClientScript(ctx, '/tracker.js');
    pushClientScript(ctx, '/tracker.js');

    // Later writers win per top-level key; the URL list stays deduplicated.
    expect(readClientBootstrap(ctx)).toEqual({ plugin: { pv: 'y' }, other: 1 });
    expect(readClientScripts(ctx)).toEqual(['/tracker.js']);

    const handler = pageRenderer(
      () => [{ path: '/', element: React.createElement('div', null, 'Hello') } as never],
      () => RootHtml,
      () => 'hydrate.js',
      500,
    );
    const response = await runInContext(ctx as never, async () => await handler());
    const html = await response.get().text();

    expect(html).toContain('window.__putnamiBootstrap={"plugin":{"pv":"y"},"other":1};');
    expect(html).toContain('src="/tracker.js"></script>');
  });

  it('returns empty defaults when nothing wrote to the slots', () => {
    const ctx = buildHttpContext<HttpRequestContext>({ req: new Request('http://localhost/') });

    expect(readClientBootstrap(ctx)).toBeUndefined();
    expect(readClientScripts(ctx)).toEqual([]);
  });
});
