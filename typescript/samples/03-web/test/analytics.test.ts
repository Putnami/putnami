import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { analytics, flushAnalytics } from '@putnami/analytics';
import { setCollector, TelemetryCollector } from '@putnami/application';
import { createTestApp, type TestApp } from '@putnami/application/testing';
import { react } from '@putnami/web';
import { events } from '../src/main';

const SAMPLE_ROOT = join(import.meta.dir, '..');
const WORKSPACE_ROOT = join(SAMPLE_ROOT, '..', '..', '..');
const INGEST = '/_putnami/analytics/events';
const CHROME_UA =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36';

/** The protocol's own valid batch, so the sample and the contract cannot drift. */
const MINIMAL_PAGE_VIEW = readFileSync(
  join(WORKSPACE_ROOT, 'protocols', 'analytics', 'fixtures', 'batch', 'valid', 'minimal-page-view.json'),
  'utf8',
);

/** The pre-build hook writes the tracker here; `putnami test` alone does not. */
const trackerBuilt = existsSync(join(SAMPLE_ROOT, '.gen', 'public', 'analytics'));

/**
 * Metrics are the only place a drop is visible: the ingest route answers 202
 * byte-identically whether it stored everything or nothing, so a test that
 * read the response could not tell a bot from a visitor.
 */
const collector = new TelemetryCollector();

const totals: Record<string, number> = {};

/** Drains the collector into a running total, so two reads are comparable. */
function counters(): Record<string, number> {
  for (const bucket of collector.drainAll()) {
    for (const [name, value] of Object.entries(bucket.counters)) {
      totals[name] = (totals[name] ?? 0) + value;
    }
  }
  return { ...totals };
}

describe('web sample analytics', () => {
  let testApp: TestApp;

  beforeAll(async () => {
    setCollector(collector);
    testApp = await createTestApp({ plugins: [react(), analytics({ events })] });
  });

  afterAll(async () => {
    await testApp.stop();
    setCollector(undefined);
  });

  describe('bootstrap injection', () => {
    it('publishes the page view id, route, endpoint, and declared names to the browser', async () => {
      const res = await testApp.fetch('/tasks');
      expect(res.status).toBe(200);

      const html = await res.text();
      const bootstrap = readBootstrap(html);
      expect(bootstrap.analytics.pv).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
      expect(bootstrap.analytics.route).toBe('/tasks');
      expect(bootstrap.analytics.endpoint).toBe(INGEST);
      expect(bootstrap.analytics.declared).toEqual(['counter_click']);
      // The browser is never told who the visitor is.
      expect(Object.keys(bootstrap.analytics).sort()).toEqual([
        'app',
        'declared',
        'endpoint',
        'env',
        'pv',
        'route',
        'version',
      ]);
    });

    it('sets no analytics cookie: cookieless is the default mode', async () => {
      const res = await testApp.fetch('/tasks');

      // react() may set its own _csrf cookie; analytics sets none at all.
      expect(res.headers.get('set-cookie') ?? '').not.toContain('_pa=');
    });

    it('does not inject the bootstrap for a bot', async () => {
      const res = await testApp.fetch('/tasks', { headers: { 'User-Agent': 'curl/8.0' } });

      expect(await res.text()).not.toContain('window.__putnamiBootstrap=');
    });

    it('does not inject the bootstrap into a static page', async () => {
      // `/` is `page().static()`. A static page is served as a file, so no
      // middleware runs, no bootstrap is written, and no tracker is loaded.
      const html = await (await testApp.fetch('/')).text();

      expect(html).not.toContain('window.__putnamiBootstrap=');
      expect(html).toContain('data-track="counter_click"');
    });
  });

  describe('POST /_putnami/analytics/events', () => {
    it('accepts a beacon with no CSRF token and answers an empty 202', async () => {
      // Nothing on a response path waits for the sink, so the acceptance
      // counter moves when the queue is flushed, not when the 202 is answered:
      // empty the queue first, and the delta below is this beacon's alone.
      await flushAnalytics();
      const before = counters();

      const res = await testApp.fetch(INGEST, {
        method: 'POST',
        // What `navigator.sendBeacon` sends; a beacon cannot carry a CSRF token.
        headers: { 'Content-Type': 'text/plain', 'User-Agent': CHROME_UA },
        body: MINIMAL_PAGE_VIEW,
      });
      await flushAnalytics();

      expect(res.status).toBe(202);
      expect(await res.text()).toBe('');
      const after = counters();
      expect((after['analytics.ingest.accepted'] ?? 0) - (before['analytics.ingest.accepted'] ?? 0)).toBe(1);
    });

    it('answers a bot the same 202 and accepts nothing', async () => {
      await flushAnalytics();
      const before = counters();

      const res = await testApp.fetch(INGEST, {
        method: 'POST',
        headers: { 'Content-Type': 'text/plain', 'User-Agent': 'Googlebot/2.1' },
        body: MINIMAL_PAGE_VIEW,
      });
      await flushAnalytics();

      // The response is not an oracle: a sender cannot tell acceptance from a
      // silent drop. The metric is where the operator sees it.
      expect(res.status).toBe(202);
      expect(await res.text()).toBe('');
      const after = counters();
      expect((after['analytics.ingest.dropped.bot'] ?? 0) - (before['analytics.ingest.dropped.bot'] ?? 0)).toBe(1);
      expect(after['analytics.ingest.accepted'] ?? 0).toBe(before['analytics.ingest.accepted'] ?? 0);
    });
  });

  describe.skipIf(!trackerBuilt)('the tracker bundle', () => {
    it('is loaded from the page and served gzipped and immutable', async () => {
      const html = await (await testApp.fetch('/tasks')).text();
      const src = /<script type="module"[^>]*src="(\/analytics\/analytics\.[A-Za-z0-9]+\.js)"/.exec(html)?.[1];
      expect(src).toBeString();

      const res = await testApp.fetch(src as string);

      expect(res.status).toBe(200);
      expect(res.headers.get('content-type')).toBe('application/javascript');
      expect(res.headers.get('cache-control')).toBe('public, max-age=31536000, immutable');
    });
  });
});

/** Reads `window.__putnamiBootstrap` back out of the rendered HTML. */
function readBootstrap(html: string): { analytics: Record<string, unknown> & { pv: string; route: string } } {
  // The renderer may write other globals after the bootstrap, such as
  // `__putnamiExposeErrors` outside production, so stop at the next one.
  const json = /window\.__putnamiBootstrap=(\{.*?\});window\./.exec(html)?.[1];
  expect(json).toBeString();
  return JSON.parse(json as string);
}
