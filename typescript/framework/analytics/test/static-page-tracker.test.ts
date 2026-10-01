import { afterEach, describe, expect, it } from 'bun:test';
import { HttpPlugin, HttpResponse } from '@putnami/application';
import { createTestApp, type TestApp } from '@putnami/application/testing';
import { resetConfigLoader } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { markClientBootstrapEmitted } from '@putnami/web';
import { BOOTSTRAP_ATTRIBUTE as CLIENT_ATTRIBUTE, readBootstrap } from '../src/client/entry';
import type { AnalyticsBootstrap } from '../src/server/http/bootstrap';
import { BOOTSTRAP_ATTRIBUTE, injectTrackerTag } from '../src/server/http/inject';
import { pageViewMiddleware } from '../src/server/http/page-view.middleware';
import type { AnalyticsRuntime } from '../src/server/runtime';
import { type FakeBrowser, installFakeBrowser } from './utils/fake-browser';
import { createFakeSink, type FakeSink } from './utils/fake-sink';
import { restoreProjectRoot, testRuntime, useTempProjectRoot } from './utils/runtime';

const FEATURE = 'typescript/web-analytics-collection';
const STATIC = 'a-pre-rendered-page-reports-from-the-browser';
const HTML = { 'content-type': 'text/html;charset=utf-8' };
const TRACKER = '/analytics/analytics.abc123.js';
const CHROME_UA =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36';

let testApp: TestApp | undefined;
let runtime: AnalyticsRuntime | undefined;

/**
 * Starts an application with the two page shapes the injection discriminates.
 *
 * `/pre-rendered` answers with a document string and never touches the request
 * slots — what `staticServeHandler` does when it reads build output.
 * `/rendered` marks the slot the React renderer marks, so the middleware must
 * leave its body alone.
 */
async function startApp(): Promise<FakeSink> {
  useTempProjectRoot();
  const sink = createFakeSink();
  const rt = testRuntime({ sink: sink.sink, trackerUrl: TRACKER, events: { search: { hits: Number } } });
  runtime = rt;
  testApp = await createTestApp({
    configure: (app) => {
      const http = app.getPlugin(HttpPlugin);
      http.use(pageViewMiddleware(rt));
      http.get(
        '/pre-rendered',
        () => new HttpResponse('<html lang="en"><body><h1>Docs</h1></body></html>', { headers: HTML }),
      );
      http.get('/rendered', (ctx) => {
        markClientBootstrapEmitted(ctx);
        return new HttpResponse('<html lang="en"><body>rendered</body></html>', { headers: HTML });
      });
      http.get('/headless', () => new HttpResponse('<html lang="en"><p>no body tag</p></html>', { headers: HTML }));
    },
  });
  return sink;
}

/** The bootstrap the injected tag carries, parsed back out of the document. */
function injectedBootstrap(html: string): AnalyticsBootstrap {
  const raw = new RegExp(`${BOOTSTRAP_ATTRIBUTE}="([^"]*)"`).exec(html)?.[1];
  const decoded = (raw ?? '')
    .replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'")
    .replace(/&lt;/g, '<')
    .replace(/&gt;/g, '>')
    .replace(/&amp;/g, '&');
  return JSON.parse(decoded) as AnalyticsBootstrap;
}

afterEach(async () => {
  await testApp?.stop();
  testApp = undefined;
  runtime = undefined;
  restoreProjectRoot();
  resetConfigLoader();
});

describe('a pre-rendered page', () => {
  specTest(
    'is served with the tracker tag the renderer never wrote',
    { feature: FEATURE, requirement: STATIC, check: 'a-pre-rendered-page-is-served-with-the-tracker-tag' },
    async () => {
      await startApp();

      const response = await testApp?.fetch('/pre-rendered', { headers: { 'user-agent': CHROME_UA } });
      const html = await response.text();

      expect(html).toContain(`<script type="module" src="${TRACKER}"`);
      // Before the closing tag, so the parser reaches the content first and a
      // deferred module script finds the document it measures already parsed.
      expect(html.indexOf('<script type="module"')).toBeLessThan(html.indexOf('</body>'));
      expect(html).toContain('<h1>Docs</h1>');
    },
  );

  specTest(
    'hands the browser the same page-view id the server recorded',
    { feature: FEATURE, requirement: STATIC, check: 'the-injected-tag-carries-the-recorded-page-view-id' },
    async () => {
      const sink = await startApp();

      const response = await testApp?.fetch('/pre-rendered', { headers: { 'user-agent': CHROME_UA } });
      const bootstrap = injectedBootstrap(await response.text());
      await runtime?.queue.drain(1000);

      // One page view per load: the browser enriches the row that already
      // exists instead of minting a competing one.
      expect(sink.rows).toHaveLength(1);
      expect(bootstrap.pv).toBe(sink.rows[0]?.eventId as string);
      expect(bootstrap.route).toBe('/pre-rendered');
      expect(bootstrap.endpoint).toBe('/_putnami/analytics/events');
      expect(bootstrap.declared).toEqual(['search']);
      // The browser is never told who the visitor is.
      expect(Object.keys(bootstrap).sort()).toEqual(['app', 'declared', 'endpoint', 'env', 'pv', 'route', 'version']);
    },
  );

  specTest(
    'boots the tracker from the injected tag',
    { feature: FEATURE, requirement: STATIC, check: 'the-tracker-boots-from-the-injected-tag' },
    () => {
      const bootstrap: AnalyticsBootstrap = {
        pv: '0192f0c0-1234-7abc-8def-0123456789ab',
        route: '/docs/[...page]',
        endpoint: '/_putnami/analytics/events',
        app: 'putnami.dev',
        env: 'prod',
        version: null,
        declared: ['search'],
      };
      const html = injectTrackerTag('<html lang="en"><body>docs</body></html>', bootstrap, TRACKER) as string;
      // A browser resolves the entities before script reads the attribute, so
      // the fake carries the decoded value the injection produced.
      const attribute = JSON.stringify(injectedBootstrap(html));
      const fake: FakeBrowser = installFakeBrowser({ injectedTag: { [BOOTSTRAP_ATTRIBUTE]: attribute } });

      try {
        expect(readBootstrap()).toEqual(bootstrap);
      } finally {
        fake.uninstall();
      }
    },
  );
});

describe('a rendered page', () => {
  specTest(
    'is not given a second tracker tag',
    { feature: FEATURE, requirement: STATIC, check: 'a-rendered-page-is-not-injected-twice' },
    async () => {
      await startApp();

      const response = await testApp?.fetch('/rendered', { headers: { 'user-agent': CHROME_UA } });

      // The renderer already emitted the bootstrap and the script tag; a
      // second one would install a second tracker on the same document.
      expect(await response.text()).toBe('<html lang="en"><body>rendered</body></html>');
    },
  );
});

describe('injectTrackerTag', () => {
  const bootstrap: AnalyticsBootstrap = {
    pv: '0192f0c0-1234-7abc-8def-0123456789ab',
    route: '/docs/[...page]',
    endpoint: '/_putnami/analytics/events',
    app: 'demo',
    env: 'test',
    version: null,
    declared: [],
  };

  it('escapes the quotes and the angle brackets of the payload', () => {
    const html = injectTrackerTag('<html lang="en"><body></body></html>', bootstrap, TRACKER) as string;

    // The JSON sits in an attribute of a start tag: a raw quote would end the
    // attribute and a raw `<` would let a resynchronising parser find a tag.
    expect(html).not.toContain('{"pv"');
    expect(html).toContain('&quot;pv&quot;');
    expect(injectedBootstrap(html)).toEqual(bootstrap);
  });

  it('leaves a document with no closing body tag alone', () => {
    expect(injectTrackerTag('<html lang="en"><p>fragment</p></html>', bootstrap, TRACKER)).toBeUndefined();
  });

  it('answers a document with no closing body tag unchanged', async () => {
    await startApp();

    const response = await testApp?.fetch('/headless', { headers: { 'user-agent': CHROME_UA } });

    expect(await response.text()).toBe('<html lang="en"><p>no body tag</p></html>');
  });
});

describe('the injected attribute', () => {
  it('is the one the browser entry looks up', () => {
    // The client cannot import the server constant — nothing in the bundle may
    // reach `src/server/**` — so the two copies are pinned here instead.
    expect(CLIENT_ATTRIBUTE).toBe(BOOTSTRAP_ATTRIBUTE);
  });
});
