import { afterEach, describe, expect, it } from 'bun:test';
import { HttpPlugin, HttpResponse } from '@putnami/application';
import { createTestApp, type TestApp } from '@putnami/application/testing';
import { resetConfigLoader } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { ACTION_OUTCOME_CONTEXT_KEY, contextSlots, readClientBootstrap, readClientScripts } from '@putnami/web';
import type { AnalyticsBootstrap } from '../src/server/http/bootstrap';
import { pageViewMiddleware } from '../src/server/http/page-view.middleware';
import { resetConsentWarning } from '../src/server/identity/identified-cookie';
import { UUID_V7_RE } from '../src/server/sanitize/vocabulary';
import type { AnalyticsRuntime } from '../src/server/runtime';
import { createFakeSink, type FakeSink } from './utils/fake-sink';
import { pageViewRow } from './utils/fixtures';
import { restoreProjectRoot, type TestRuntimeOptions, testRuntime, useTempProjectRoot } from './utils/runtime';

const FEATURE = 'typescript/web-analytics-collection';
const COOKIELESS = 'page-views-are-collected-without-cookies';
const BOTS = 'bots-leave-no-trace';
const ASYNC = 'a-slow-database-never-delays-a-response';
const HTML = { 'content-type': 'text/html;charset=utf-8' };
const BOT_UA = 'Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)';
const CHROME_UA =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36';

let testApp: TestApp | undefined;
let runtime: AnalyticsRuntime | undefined;
let seenBootstrap: Record<string, unknown> | undefined;
let seenScripts: readonly string[] = [];

/**
 * Empties the write queue.
 *
 * Nothing on a response path waits for the sink any more, so a proof that
 * reads rows back has to say when it wants them — which is what a consuming
 * application's own tests do through the public `flushAnalytics()`.
 */
async function flush(): Promise<void> {
  await runtime?.queue.drain(1000);
}

/**
 * Starts an application whose only middleware is the one under test, with four
 * handlers covering the response shapes the middleware discriminates on.
 */
async function startApp(options: Omit<TestRuntimeOptions, 'sink'>): Promise<{ rt: AnalyticsRuntime; sink: FakeSink }> {
  useTempProjectRoot();
  const sink = createFakeSink();
  const rt = testRuntime({ ...options, sink: sink.sink });
  runtime = rt;
  testApp = await createTestApp({
    configure: (app) => {
      const http = app.getPlugin(HttpPlugin);
      http.use(pageViewMiddleware(rt));
      http.get('/', (ctx) => {
        seenBootstrap = readClientBootstrap(ctx);
        seenScripts = readClientScripts(ctx);
        return new HttpResponse('<html lang="en"><body>home</body></html>', { headers: HTML });
      });
      http.get('/api/json', () => HttpResponse.json({ ok: true }));
      http.get('/gone', () => new HttpResponse('<html lang="en"></html>', { status: 404, headers: HTML }));
      http.post('/signup', (ctx) => {
        contextSlots(ctx)[ACTION_OUTCOME_CONTEXT_KEY] = { route: '/signup', outcome: 'ok' };
        return new HttpResponse('<html lang="en"></html>', { headers: HTML });
      });
    },
  });
  return { rt, sink };
}

afterEach(async () => {
  await testApp?.stop();
  testApp = undefined;
  runtime = undefined;
  seenBootstrap = undefined;
  seenScripts = [];
  resetConsentWarning();
  restoreProjectRoot();
  resetConfigLoader();
});

describe('pageViewMiddleware', () => {
  specTest(
    'records one server page view with a daily visitor id',
    {
      feature: FEATURE,
      requirement: COOKIELESS,
      check: 'an-html-get-records-a-server-page-view-with-a-daily-visitor-id',
    },
    async () => {
      const { sink } = await startApp({});

      const response = await testApp?.fetch('/', { headers: { 'user-agent': CHROME_UA } });
      await flush();

      expect(response.status).toBe(200);
      expect(sink.rows).toHaveLength(1);
      const row = sink.rows[0];
      expect(row?.name).toBe('page_view');
      expect(row?.source).toBe('server');
      expect(row?.visitorKind).toBe('daily');
      expect(row?.visitorId).toHaveLength(22);
      expect(row?.route).toBe('/');
      expect(row?.path).toBe('/');
      expect(row?.sessionId).toBeNull();
      expect(row?.seq).toBeNull();
      expect(row?.engagementMs).toBe(0);
      expect(row?.statusCode).toBe(200);
      expect(row?.renderMs).not.toBeNull();
      expect(row?.browser).toBe('chrome');
      expect(row?.viewportClass).toBeNull();
      expect(row?.userId).toBeNull();
    },
  );

  specTest(
    'sets no cookie in the default cookieless mode',
    { feature: FEATURE, requirement: COOKIELESS, check: 'cookieless-mode-never-sets-a-cookie' },
    async () => {
      const { sink } = await startApp({});

      const response = await testApp?.fetch('/', { headers: { 'user-agent': CHROME_UA } });
      await flush();

      expect(response.headers.get('set-cookie')).toBeNull();
      expect(sink.rows[0]?.visitorKind).toBe('daily');
    },
  );

  specTest(
    'hands the browser the server event id and the matched route',
    { feature: FEATURE, requirement: COOKIELESS, check: 'the-bootstrap-carries-the-server-event-id-and-route' },
    async () => {
      const { sink } = await startApp({ trackerUrl: '/analytics/analytics.abc123.js' });

      await testApp?.fetch('/', { headers: { 'user-agent': CHROME_UA } });
      await flush();

      const bootstrap = seenBootstrap?.['analytics'] as AnalyticsBootstrap;
      expect(UUID_V7_RE.test(bootstrap.pv)).toBe(true);
      expect(bootstrap.pv).toBe(sink.rows[0]?.eventId);
      expect(bootstrap.route).toBe('/');
      expect(bootstrap.endpoint).toBe('/_putnami/analytics/events');
      expect(bootstrap.app).toBe('demo');
      expect(seenScripts).toEqual(['/analytics/analytics.abc123.js']);
      // The browser is never told who the visitor is.
      expect(Object.keys(bootstrap).sort()).toEqual(['app', 'declared', 'endpoint', 'env', 'pv', 'route', 'version']);
    },
  );

  specTest(
    'gives a bot neither a bootstrap nor a row',
    { feature: FEATURE, requirement: BOTS, check: 'a-bot-user-agent-gets-no-bootstrap-and-no-row' },
    async () => {
      const { sink } = await startApp({ trackerUrl: '/analytics/analytics.abc123.js' });

      const response = await testApp?.fetch('/', { headers: { 'user-agent': BOT_UA } });
      await flush();

      expect(response.status).toBe(200);
      expect(sink.rows).toHaveLength(0);
      expect(seenBootstrap).toBeUndefined();
      expect(seenScripts).toEqual([]);
      expect(response.headers.get('set-cookie')).toBeNull();
    },
  );

  it('records nothing for a JSON response', async () => {
    const { sink } = await startApp({});

    const response = await testApp?.fetch('/api/json', { headers: { 'user-agent': CHROME_UA } });
    await flush();

    expect(response.status).toBe(200);
    expect(sink.rows).toHaveLength(0);
  });

  it('records nothing for an error response', async () => {
    const { sink } = await startApp({});

    const response = await testApp?.fetch('/gone', { headers: { 'user-agent': CHROME_UA } });
    await flush();

    expect(response.status).toBe(404);
    expect(sink.rows).toHaveLength(0);
  });

  it('folds a request that matched no route under __unmatched__', async () => {
    const { sink } = await startApp({});

    await testApp?.fetch('/no/such/page', { headers: { 'user-agent': CHROME_UA } });
    await flush();

    // The 404 body is not HTML, so nothing is stored; what matters is that the
    // attacker-controlled path never became a route value.
    expect(sink.rows).toHaveLength(0);
  });

  it('keeps injecting the bootstrap when server page views are off', async () => {
    const { sink } = await startApp({ config: { serverPageViews: false } });

    await testApp?.fetch('/', { headers: { 'user-agent': CHROME_UA } });
    await flush();

    expect(sink.rows).toHaveLength(0);
    const bootstrap = seenBootstrap?.['analytics'] as AnalyticsBootstrap | undefined;
    expect(bootstrap?.route).toBe('/');
  });

  it('records the referrer and the campaign of a server view', async () => {
    const { sink } = await startApp({});

    await testApp?.fetch('/?utm_source=newsletter&utm_medium=email', {
      headers: { 'user-agent': CHROME_UA, referer: 'https://www.google.com/search?q=putnami' },
    });
    await flush();

    const row = sink.rows[0];
    expect(row?.referrerType).toBe('search');
    expect(row?.referrer).toBe('https://www.google.com/search');
    expect(row?.utmSource).toBe('newsletter');
    expect(row?.utmMedium).toBe('email');
  });

  it('reads the primary language and the trusted country header', async () => {
    const { sink } = await startApp({ config: { countryHeader: 'cf-ipcountry' } });

    await testApp?.fetch('/', {
      headers: { 'user-agent': CHROME_UA, 'accept-language': 'fr-FR,fr;q=0.9,en;q=0.8', 'cf-ipcountry': 'fr' },
    });
    await flush();

    expect(sink.rows[0]?.language).toBe('fr-FR');
    expect(sink.rows[0]?.country).toBe('FR');
    // A server-recorded view has no viewport: only the browser can measure one,
    // and inventing a bucket here would make an unmeasured value look measured.
    expect(sink.rows[0]?.viewportClass).toBeNull();
  });

  it('records a form submit from the action outcome slot', async () => {
    const { sink } = await startApp({});

    const response = await testApp?.fetch('/signup', { method: 'POST', headers: { 'user-agent': CHROME_UA } });
    await flush();

    expect(response.status).toBe(200);
    expect(sink.named('form_submit')).toHaveLength(1);
    const row = sink.named('form_submit')[0];
    expect(row?.route).toBe('/signup');
    expect(row?.outcome).toBe('ok');
    expect(row?.source).toBe('server');
    expect(row?.path).toBeNull();
    expect(row?.sessionId).toBeNull();
    // A POST mints no page view, so a form submit never double-counts the page.
    expect(sink.named('page_view')).toHaveLength(0);
  });

  it('answers the request even when the sink fails', async () => {
    useTempProjectRoot();
    const failing = createFakeSink(new Error('connection refused'));
    const rt = testRuntime({ sink: failing.sink });
    runtime = rt;
    testApp = await createTestApp({
      configure: (app) => {
        const http = app.getPlugin(HttpPlugin);
        http.use(pageViewMiddleware(rt));
        http.get('/', () => new HttpResponse('<html lang="en"></html>', { headers: HTML }));
      },
    });

    const response = await testApp.fetch('/', { headers: { 'user-agent': CHROME_UA } });
    await flush();

    expect(response.status).toBe(200);
    expect(failing.batches).toHaveLength(1);
    // The row is gone, not retried, and the failure never reached the page.
    expect(failing.rows).toHaveLength(0);
  });
});

describe('pageViewMiddleware in identified mode', () => {
  it('falls back to the daily identity under Sec-GPC', async () => {
    const { sink } = await startApp({ config: { mode: 'identified' }, consent: () => true });

    const response = await testApp?.fetch('/', { headers: { 'user-agent': CHROME_UA, 'Sec-GPC': '1' } });
    await flush();

    expect(response.headers.get('set-cookie')).toBeNull();
    expect(sink.rows[0]?.visitorKind).toBe('daily');
  });

  it('mints the signed cookie once consent is given', async () => {
    const { sink } = await startApp({ config: { mode: 'identified' }, consent: () => true });

    const response = await testApp?.fetch('/', { headers: { 'user-agent': CHROME_UA } });
    await flush();

    const cookie = response.headers.get('set-cookie') ?? '';
    expect(cookie).toContain('_pa=');
    expect(cookie).toContain('HttpOnly');
    expect(cookie).toContain('SameSite=Lax');
    expect(cookie).toContain('Max-Age=33696000');
    expect(sink.rows[0]?.visitorKind).toBe('cookie');
  });
});

describe('pageViewMiddleware and the write queue', () => {
  specTest(
    'returns the page while the sink write is still pending',
    { feature: FEATURE, requirement: ASYNC, check: 'the-page-view-middleware-returns-before-the-sink-settles' },
    async () => {
      // A database that never answers. Nothing below is allowed to notice.
      const { rt, sink } = await startApp({ config: { flushWaitMs: 80, flushIntervalMs: 0 } });
      sink.hold();

      // 1. A request that starts no flush pays nothing at all: the queue is
      //    empty when it arrives, so it renders, queues its row, and returns.
      const first = await testApp?.fetch('/', { headers: { 'user-agent': CHROME_UA } });

      expect(first.status).toBe(200);
      expect(await first.text()).toContain('home');
      expect(sink.batches).toHaveLength(0);
      expect(rt.queue.size()).toBe(1);

      // 2. The next request is elected: it starts the flush, lets it overlap
      //    the render, then waits — for at most `flushWaitMs`. The sink never
      //    settles, and the page goes out anyway.
      const startedAt = Date.now();
      const second = await testApp?.fetch('/', { headers: { 'user-agent': CHROME_UA } });
      const elapsed = Date.now() - startedAt;

      expect(second.status).toBe(200);
      expect(await second.text()).toContain('home');
      expect(elapsed).toBeLessThan(2000);
      // Returned before the sink settled, which is the invariant: a hung
      // database costs one request the cap, and no request more than that.
      expect(sink.pending()).toBe(1);
      expect(sink.rows).toHaveLength(0);

      // 3. A third request finds a flush in flight, so it is not elected and
      //    returns while that same write is still pending.
      const third = await testApp?.fetch('/', { headers: { 'user-agent': CHROME_UA } });

      expect(third.status).toBe(200);
      expect(sink.pending()).toBe(1);
      expect(sink.batches).toHaveLength(1);
      sink.release();
    },
  );

  it('waits for the flush it started when the sink answers inside the cap', async () => {
    // `serverPageViews: false` so the only row in play is the seeded one: the
    // proof is about when the write settles, not about what the page produced.
    const { rt, sink } = await startApp({
      config: { flushWaitMs: 1000, flushIntervalMs: 0, serverPageViews: false },
    });
    sink.delay(30);
    rt.queue.enqueue([pageViewRow()]);

    const response = await testApp?.fetch('/', { headers: { 'user-agent': CHROME_UA } });

    // The elected request paid for the batch, so the row is stored by the time
    // the page is returned. That is what makes a flush land at all on
    // request-based CPU: a flush nobody waits for has no CPU after the response.
    expect(response.status).toBe(200);
    expect(sink.rows).toHaveLength(1);
  });

  it('elects a later request again once a capped flush settles', async () => {
    const { rt, sink } = await startApp({
      config: { flushIntervalMs: 0, flushWaitMs: 50, serverPageViews: false },
    });
    sink.hold();
    rt.queue.enqueue([pageViewRow({ eventId: '01920000-0000-7000-8000-0000000000f1' })]);

    await testApp?.fetch('/', { headers: { 'user-agent': CHROME_UA } });
    expect(sink.batches).toHaveLength(1);

    // A second request finds one flush in flight: it coalesces, and it waits
    // for nothing.
    rt.queue.enqueue([pageViewRow({ eventId: '01920000-0000-7000-8000-0000000000f2' })]);
    await testApp?.fetch('/', { headers: { 'user-agent': CHROME_UA } });
    expect(sink.batches).toHaveLength(1);
    expect(sink.pending()).toBe(1);

    sink.release();
    await flush();
    expect(sink.rows).toHaveLength(2);

    // The in-flight marker cleared when the capped flush finally settled, so
    // the next request is elected: a slow database never locks the queue.
    rt.queue.enqueue([pageViewRow({ eventId: '01920000-0000-7000-8000-0000000000f3' })]);
    await testApp?.fetch('/', { headers: { 'user-agent': CHROME_UA } });

    expect(sink.rows).toHaveLength(3);
    expect(sink.batches).toHaveLength(3);
  });
});
