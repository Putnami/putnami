import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { runInContext } from '../../../runtime/src/context/context.utils';
import { BrowserDocumentHelper } from '../../src/client/document/document-browser.helper';
import { DocumentMetaProvider, useDocumentMeta } from '../../src/client/document/document-context';
import { SsrDocumentHelper } from '../../src/client/document/document-ssr.helper';
import { Favicon, HeaderLink, Lang, Meta, PageMeta, Script, Style, Title } from '../../src/client/document';
import * as clientIndex from '../../src/client';
import { FakeDocument } from '../utils/fake-document';

type BrowserGlobals = typeof globalThis & {
  document?: FakeDocument;
  window?: Record<string, unknown>;
};

function withBrowserDocument() {
  const fakeDocument = new FakeDocument();
  const globals = globalThis as BrowserGlobals;
  globals.document = fakeDocument;
  globals.window = {};
  return fakeDocument;
}

function clearBrowserDocument() {
  const globals = globalThis as BrowserGlobals;
  globals.document = undefined;
  globals.window = undefined;
}

describe('document runtime helpers', () => {
  beforeEach(() => {
    clearBrowserDocument();
  });

  afterEach(() => {
    clearBrowserDocument();
  });

  it('re-exports document helpers through the client entrypoint', () => {
    expect(clientIndex.PageMeta).toBe(PageMeta);
    expect(clientIndex.Title).toBe(Title);
    expect(clientIndex.Meta).toBe(Meta);
  });

  specTest(
    'deduplicates browser document tags and updates title/lang',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'document-isolation',
      check: 'browser-tags-are-de-duplicated-and-title-and-lang-updated',
    },
    () => {
      const fakeDocument = withBrowserDocument();
      const helper = new BrowserDocumentHelper();

      helper.title = 'Docs';
      helper.lang = 'fr';
      helper.addLink({ rel: 'icon', href: '/favicon.ico' });
      helper.addLink({ rel: 'icon', href: '/favicon.ico', crossOrigin: 'anonymous' });
      helper.addMeta({ name: 'description', content: 'Initial description' });
      helper.addMeta({ name: 'description', content: 'Updated description' });
      helper.addScript({ src: '/app.js', type: 'module' });
      helper.addScript({ src: '/app.js', type: 'module', crossOrigin: 'anonymous' });
      helper.addStyle({ children: 'body { color: red; }' });
      helper.addStyle({ children: 'body { color: red; }' });

      expect(fakeDocument.title).toBe('Docs');
      expect(fakeDocument.documentElement.lang).toBe('fr');
      expect(fakeDocument.head.querySelectorAll('link')).toHaveLength(1);
      expect(fakeDocument.head.querySelector('link[href="/favicon.ico"]')?.getAttribute('crossorigin')).toBe(
        'anonymous',
      );
      expect(fakeDocument.head.querySelectorAll('meta')).toHaveLength(1);
      expect(fakeDocument.head.querySelector('meta[name="description"]')?.getAttribute('content')).toBe(
        'Updated description',
      );
      expect(fakeDocument.head.querySelectorAll('script')).toHaveLength(1);
      expect(fakeDocument.head.querySelector('script[src="/app.js"]')?.getAttribute('crossorigin')).toBe('anonymous');
      expect(fakeDocument.head.querySelectorAll('style')).toHaveLength(1);
    },
  );

  it('shares SSR document metadata through React context', () => {
    const ssrMeta = { title: 'SSR title' };

    const Reader = () => {
      const value = useDocumentMeta();
      return React.createElement('pre', null, value?.title ?? 'missing');
    };

    const html = renderToStaticMarkup(
      React.createElement(DocumentMetaProvider, { meta: ssrMeta }, React.createElement(Reader)),
    );

    expect(html).toContain('SSR title');
  });

  specTest(
    'resolves SSR meta from the per-request AsyncLocalStorage context',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'document-isolation',
      check: 'ssr-meta-resolves-from-the-per-request-context',
    },
    () => {
      const meta = runInContext({} as never, () => {
        const helper = new SsrDocumentHelper();
        helper.title = 'Request title';
        // A second helper in the same request shares the same meta object.
        const sameRequestHelper = new SsrDocumentHelper();
        return sameRequestHelper.title;
      });

      expect(meta).toBe('Request title');
    },
  );

  specTest(
    'uses a fresh request-local meta when no request context exists',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'document-isolation',
      check: 'a-request-without-a-context-gets-a-fresh-request-local-meta',
    },
    () => {
      // Outside runInContext there is no ALS store; each helper must get its
      // own object instead of sharing a process-global slot.
      const first = new SsrDocumentHelper();
      first.title = 'First';
      const second = new SsrDocumentHelper();

      expect(first.title).toBe('First');
      expect(second.title).toBe('');
    },
  );

  specTest(
    'isolates SSR meta between interleaved concurrent requests',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'document-isolation',
      check: 'interleaved-requests-never-share-their-meta',
    },
    async () => {
      // Simulate two SSR renders whose async work interleaves. Each render runs
      // inside its own AsyncLocalStorage scope, so neither may observe or clear
      // the other's document meta.
      const tick = () => new Promise((resolve) => setTimeout(resolve, 0));

      const render = (title: string, extraDelay: boolean): Promise<{ title: string; head: string }> =>
        runInContext({} as never, async () => {
          const helper = new SsrDocumentHelper();
          helper.title = title;
          helper.addMeta({ name: 'request', content: title });
          // Yield control so the other request's work interleaves here.
          await tick();
          if (extraDelay) await tick();
          // After interleaving, a fresh helper in this request must still resolve
          // to the same per-request meta — not the other request's, not cleared.
          const observed = new SsrDocumentHelper();
          return { title: observed.title, head: observed.headHtml };
        });

      const [a, b] = await Promise.all([render('Request A', true), render('Request B', false)]);

      expect(a.title).toBe('Request A');
      expect(b.title).toBe('Request B');
      expect(a.head).toContain('<title>Request A</title>');
      expect(a.head).toContain('content="Request A"');
      expect(a.head).not.toContain('Request B');
      expect(b.head).toContain('<title>Request B</title>');
      expect(b.head).toContain('content="Request B"');
      expect(b.head).not.toContain('Request A');
    },
  );

  it('collects page metadata in SSR context', () => {
    const documentMeta: {
      title?: string;
      metas?: Record<string, string>[];
    } = {};

    renderToStaticMarkup(
      React.createElement(
        DocumentMetaProvider,
        { meta: documentMeta },
        React.createElement(PageMeta, {
          title: 'Getting Started',
          description: 'Setup guide',
          url: '/docs/getting-started',
          baseUrl: 'https://putnami.dev',
          siteName: 'Putnami',
          image: '/cover.png',
        }),
      ),
    );

    expect(documentMeta.title).toBe('Getting Started');
    expect(documentMeta.metas).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ name: 'description', content: 'Setup guide' }),
        expect.objectContaining({ property: 'og:site_name', content: 'Putnami' }),
        expect.objectContaining({ property: 'og:url', content: 'https://putnami.dev/docs/getting-started' }),
        expect.objectContaining({ name: 'twitter:image', content: '/cover.png' }),
      ]),
    );
  });

  it('falls back to the browser document helper outside SSR context', () => {
    const fakeDocument = withBrowserDocument();

    renderToStaticMarkup(
      React.createElement(
        React.Fragment,
        null,
        React.createElement(Title, null, 'Browser title'),
        React.createElement(Meta, { name: 'description', content: 'Browser meta' }),
      ),
    );
    Lang({ children: 'de' });
    HeaderLink({ rel: 'preload', href: '/fonts/docs.woff2' });
    Favicon({ href: '/favicon.svg' });
    Script({ src: '/bundle.js', type: 'module' });
    Style({ children: 'body { margin: 0; }' });

    expect(fakeDocument.title).toBe('Browser title');
    expect(fakeDocument.documentElement.lang).toBe('de');
    expect(fakeDocument.head.querySelector('meta[name="description"]')?.getAttribute('content')).toBe('Browser meta');
    expect(fakeDocument.head.querySelector('link[href="/fonts/docs.woff2"]')).toBeDefined();
    expect(fakeDocument.head.querySelector('link[href="/favicon.svg"]')?.getAttribute('type')).toBe('image/svg+xml');
    expect(fakeDocument.head.querySelector('script[src="/bundle.js"]')?.getAttribute('type')).toBe('module');
    expect(fakeDocument.head.querySelectorAll('style')).toHaveLength(1);
  });
});
