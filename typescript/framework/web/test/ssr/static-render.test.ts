import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import React from 'react';
import type { RouteObject } from 'react-router';
import { Style } from '../../src/client/document/style';
import { loaderHandler } from '../../src/ssr/handlers';
import { inlineDeferredBoundaries, renderStaticDocument } from '../../src/ssr/static-render';
import { isStaticRenderViolation } from '../../src/ssr/static';

function Page() {
  return React.createElement('main', null, 'hello static');
}

describe('renderStaticDocument', () => {
  specTest(
    'renders a route to a complete zero-JS HTML document',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'island-hydration',
      check: 'a-static-route-renders-a-zero-javascript-document',
    },
    async () => {
      const routes: RouteObject[] = [{ path: '/', element: React.createElement(Page) }];
      const result = await renderStaticDocument({ routes, route: '/', pathname: '/' });

      expect(result.status).toBe(200);
      expect(result.html).toContain('<!DOCTYPE html>');
      expect(result.html).toContain('hello static');
      expect(result.html).toContain('<div id="root">');
      // Zero JS: no hydration bootstrap, no hydration data script.
      expect(result.html).not.toContain('hydrate.main');
      expect(result.html).not.toContain('__staticRouterHydrationData');
      expect(result.html).not.toContain('<script type="module"');
    },
  );

  it('bakes static loader data into the HTML', async () => {
    function DataPage() {
      // Read loader data via the static router context.
      return React.createElement('p', null, 'data page');
    }
    const routes: RouteObject[] = [
      {
        path: '/',
        id: 'root',
        element: React.createElement(DataPage),
        loader: loaderHandler(() => ({ value: 42 })),
      },
    ];
    const result = await renderStaticDocument({ routes, route: '/', pathname: '/' });
    expect(result.html).toContain('data page');
    expect(result.status).toBe(200);
  });

  specTest(
    'throws StaticRenderViolation when a loader reads request data',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'explicit-render-mode',
      check: 'a-static-render-that-reads-request-data-raises-a-violation',
    },
    async () => {
      const routes: RouteObject[] = [
        {
          path: '/',
          id: 'root',
          element: React.createElement(Page),
          loader: loaderHandler((ctx) => ({ u: (ctx as unknown as { user: unknown }).user })),
        },
      ];

      let caught: unknown;
      try {
        await renderStaticDocument({ routes, route: '/dash', pathname: '/' });
      } catch (e) {
        caught = e;
      }
      expect(isStaticRenderViolation(caught)).toBe(true);
    },
  );

  specTest(
    'captures AsyncLocalStorage document-helper head contributions (Style) in the static head',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'ssr-response',
      check: 'a-lazy-head-contribution-reaches-the-rendered-head',
    },
    async () => {
      function HeadPage() {
        // The Style helper registers via documentHelper() (AsyncLocalStorage), so
        // it only reaches the head when the render runs inside the request context.
        Style({ children: '.docs-island{display:block}' });
        return React.createElement('main', null, 'with head');
      }
      const routes: RouteObject[] = [{ path: '/', element: React.createElement(HeadPage) }];
      const result = await renderStaticDocument({ routes, route: '/', pathname: '/' });

      expect(result.html).toContain('with head');
      expect(result.html).toMatch(/<style[^>]*>\.docs-island\{display:block\}<\/style>/);
      // It lands in the <head>, not the body.
      expect(result.html.split('</head>')[0]).toContain('.docs-island{display:block}');
    },
  );

  it('injects head and body extras', async () => {
    const routes: RouteObject[] = [{ path: '/', element: React.createElement(Page) }];
    const result = await renderStaticDocument({
      routes,
      route: '/',
      pathname: '/',
      headExtra: '<link rel="stylesheet" href="/x.css">',
      bodyExtra: '<script src="/island.js"></script>',
    });
    expect(result.html).toContain('<link rel="stylesheet" href="/x.css"></head>');
    expect(result.html).toContain('<script src="/island.js"></script></body>');
  });
});

describe('inlineDeferredBoundaries', () => {
  it('inlines an out-of-order Suspense boundary into its placeholder', () => {
    // React streams a boundary that resolves after the shell as a template
    // placeholder plus a trailing hidden block, spliced by an inline script.
    const html =
      '<div id="shell">' +
      '<div class="c"><!--$--><!--$?--><template id="B:0"></template><!--/$--><!--/$--></div>' +
      '<footer>F</footer>' +
      '</div>' +
      '<div hidden id="S:0"><main><h1>Hi</h1></main></div>' +
      '<script>$RB=[];$RV=function(a){};</script>' +
      '<script>$RC("B:0","S:0")</script>';

    const out = inlineDeferredBoundaries(html);

    // No streaming artifacts remain.
    expect(out).not.toContain('hidden id="S:0"');
    expect(out).not.toContain('<template id="B:0"');
    expect(out).not.toContain('$RC');
    expect(out).not.toContain('$RB');
    // The content is spliced in-order (before the footer) and appears once.
    expect(out).toContain('<main><h1>Hi</h1></main>');
    expect(out.indexOf('<main>')).toBeLessThan(out.indexOf('<footer>'));
    expect(out.match(/<main>/g)?.length).toBe(1);
  });

  it('handles nested boundaries and nested divs within the content', () => {
    const html =
      '<section><!--$?--><template id="B:0"></template><!--/$--></section>' +
      '<div hidden id="S:0"><div class="wrap"><!--$?--><template id="B:1"></template><!--/$--></div></div>' +
      '<div hidden id="S:1"><p>deep</p></div>' +
      '<script>$RC("B:0","S:0")</script><script>$RC("B:1","S:1")</script>';

    const out = inlineDeferredBoundaries(html);

    expect(out).toContain('<div class="wrap">');
    expect(out).toContain('<p>deep</p>');
    expect(out).not.toContain('<template id="B:');
    expect(out).not.toContain('hidden id="S:');
    expect(out).not.toContain('$RC');
    expect(out.match(/<p>deep<\/p>/g)?.length).toBe(1);
  });

  it('leaves shell-only HTML (no deferred boundaries) untouched', () => {
    const html = '<main>hello world</main>';
    expect(inlineDeferredBoundaries(html)).toBe(html);
  });
});
