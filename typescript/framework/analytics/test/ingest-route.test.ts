import { afterEach, describe, expect, it } from 'bun:test';
import { CsrfMiddleware, HttpPlugin, HttpResponse, setCollector, TelemetryCollector } from '@putnami/application';
import { createTestApp, type TestApp } from '@putnami/application/testing';
import { resetConfigLoader } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import type { AnalyticsConfigValues } from '../src/server/analytics.config';
import { canonicalTs, ingest, registerIngestRoute } from '../src/server/http/ingest.route';
import { MAX_BODY_BYTES } from '../src/server/sanitize/vocabulary';
import type { AnalyticsRuntime } from '../src/server/runtime';
import { fakeContext } from './utils/context';
import { createFakeSink, type FakeSink } from './utils/fake-sink';
import { pageViewRow } from './utils/fixtures';
import { restoreProjectRoot, testRuntime, useTempProjectRoot } from './utils/runtime';

const FEATURE = 'typescript/web-analytics-collection';
const ORACLE = 'acceptance-is-not-an-oracle';
const BOTS = 'bots-leave-no-trace';
const ASYNC = 'a-slow-database-never-delays-a-response';
const ENDPOINT = '/_putnami/analytics/events';
const JSON_TYPE = { 'content-type': 'application/json' };
const CHROME_UA =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36';
const BOT_UA = 'Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)';

const PAGE_VIEW = {
  eventId: '01920000-0000-7000-8000-000000000001',
  name: 'page_view',
  clientTs: '2026-09-02T09:59:58.000Z',
  seq: 3,
  sessionId: '01920000-0000-7000-8000-0000000000aa',
  engagementMs: 4200,
  viewportClass: 'lg',
  language: 'fr-FR',
  page: { path: '/docs/getting-started', route: '/docs/[...page]', referrer: 'https://news.ycombinator.com/' },
};

const ACTION = {
  eventId: '01920000-0000-7000-8000-000000000002',
  name: 'action',
  clientTs: '2026-09-02T09:59:59.000Z',
  seq: 4,
  sessionId: '01920000-0000-7000-8000-0000000000aa',
  action: 'signup_click',
  props: { plan: 'pro', seats: 12, trial: true },
};

const EVENTS = { signup_click: { plan: String, seats: Number, trial: Boolean } };

function body(events: unknown[], overrides: Record<string, unknown> = {}): string {
  return JSON.stringify({ protocolVersion: 1, sentAt: '2026-09-02T10:00:00.000Z', events, ...overrides });
}

let testApp: TestApp | undefined;
let collector: TelemetryCollector | undefined;
let runtime: AnalyticsRuntime | undefined;

/**
 * Empties the write queue.
 *
 * The route answers 202 the moment a batch is accepted into the queue, so a
 * proof that reads rows back has to ask for them.
 */
async function flush(): Promise<void> {
  await runtime?.queue.drain(1000);
}

/** Starts an app whose ingest route is registered exactly as the plugin does. */
async function startApp(
  config: Partial<AnalyticsConfigValues> = {},
  options: { csrf?: boolean; knownRoutes?: string[] } = {},
): Promise<FakeSink> {
  useTempProjectRoot();
  const sink = createFakeSink();
  const rt = testRuntime({
    sink: sink.sink,
    config,
    events: EVENTS,
    knownRoutes: options.knownRoutes ?? ['/', '/docs/[...page]'],
  });
  runtime = rt;
  testApp = await createTestApp({
    configure: (app) => {
      const http = app.getPlugin(HttpPlugin);
      if (options.csrf) {
        http.use(CsrfMiddleware());
        http.post('/guarded', () => new HttpResponse('ok'));
      }
      registerIngestRoute(http, rt);
    },
  });
  return sink;
}

/** Every counter this process recorded, summed across buckets. */
function counters(): Record<string, number> {
  const totals: Record<string, number> = {};
  for (const bucket of collector?.drain(Date.now() + 5000) ?? []) {
    for (const [name, value] of Object.entries(bucket.counters)) {
      totals[name] = (totals[name] ?? 0) + value;
    }
  }
  return totals;
}

afterEach(async () => {
  await testApp?.stop();
  testApp = undefined;
  runtime = undefined;
  if (collector) {
    setCollector(undefined);
    collector = undefined;
  }
  restoreProjectRoot();
  resetConfigLoader();
});

describe('the ingest route', () => {
  it('accepts a valid batch with an empty 202 and stores client rows', async () => {
    const sink = await startApp();

    const response = await testApp?.fetch(ENDPOINT, {
      method: 'POST',
      headers: { ...JSON_TYPE, 'user-agent': CHROME_UA },
      body: body([PAGE_VIEW, ACTION]),
    });
    await flush();

    expect(response.status).toBe(202);
    expect(await response.text()).toBe('');
    expect(sink.rows).toHaveLength(2);
    expect(sink.rows.every((row) => row.source === 'client')).toBe(true);
    const view = sink.named('page_view')[0]!;
    expect(view.route).toBe('/docs/[...page]');
    expect(view.path).toBe('/docs/getting-started');
    expect(view.sessionId).toBe(PAGE_VIEW.sessionId);
    expect(view.seq).toBe(3);
    expect(view.engagementMs).toBe(4200);
    expect(view.viewportClass).toBe('lg');
    expect(view.language).toBe('fr-FR');
    expect(view.referrerType).toBe('other');
    expect(view.statusCode).toBeNull();
    expect(view.renderMs).toBeNull();
    const action = sink.named('action')[0]!;
    expect(action.actionName).toBe('signup_click');
    expect(action.route).toBe('__none__');
    expect(action.path).toBeNull();
    expect(action.props).toEqual({ plan: 'pro', seats: 12, trial: true });
  });

  it('accepts a text/plain beacon body', async () => {
    const sink = await startApp();

    const response = await testApp?.fetch(ENDPOINT, {
      method: 'POST',
      headers: { 'content-type': 'text/plain;charset=UTF-8', 'user-agent': CHROME_UA },
      body: body([PAGE_VIEW]),
    });
    await flush();

    expect(response.status).toBe(202);
    expect(sink.rows).toHaveLength(1);
  });

  it('rejects a content type no beacon uses', async () => {
    const sink = await startApp();

    const response = await testApp?.fetch(ENDPOINT, {
      method: 'POST',
      headers: { 'content-type': 'text/html', 'user-agent': CHROME_UA },
      body: body([PAGE_VIEW]),
    });
    await flush();

    expect(response.status).toBe(400);
    expect(sink.batches).toHaveLength(0);
  });

  specTest(
    'answers 202 to a batch it stores nothing from',
    { feature: FEATURE, requirement: ORACLE, check: 'a-semantically-invalid-batch-still-answers-202' },
    async () => {
      const sink = await startApp();
      const accepted = await testApp?.fetch(ENDPOINT, {
        method: 'POST',
        headers: { ...JSON_TYPE, 'user-agent': CHROME_UA },
        body: body([PAGE_VIEW]),
      });

      const wrongVersion = await testApp?.fetch(ENDPOINT, {
        method: 'POST',
        headers: { ...JSON_TYPE, 'user-agent': CHROME_UA },
        body: body([PAGE_VIEW], { protocolVersion: 2 }),
      });
      const undeclared = await testApp?.fetch(ENDPOINT, {
        method: 'POST',
        headers: { ...JSON_TYPE, 'user-agent': CHROME_UA },
        body: body([{ ...ACTION, action: 'never_declared' }]),
      });
      await flush();

      // Byte-identical: an ingest that answered differently would be an oracle
      // for the declared vocabulary and the protocol version.
      expect([accepted.status, wrongVersion.status, undeclared.status]).toEqual([202, 202, 202]);
      expect([await accepted.text(), await wrongVersion.text(), await undeclared.text()]).toEqual(['', '', '']);
      // Only the first batch reached the sink.
      expect(sink.batches).toHaveLength(1);
      expect(sink.rows).toHaveLength(1);
    },
  );

  specTest(
    'answers 400 to a body that is not JSON',
    { feature: FEATURE, requirement: ORACLE, check: 'malformed-json-answers-400' },
    async () => {
      const sink = await startApp();

      const response = await testApp?.fetch(ENDPOINT, {
        method: 'POST',
        headers: { ...JSON_TYPE, 'user-agent': CHROME_UA },
        body: '{"protocolVersion":1,"events":[',
      });
      await flush();

      expect(response.status).toBe(400);
      expect(await response.text()).toBe('');
      expect(sink.batches).toHaveLength(0);
    },
  );

  specTest(
    'answers 413 to an oversized body',
    { feature: FEATURE, requirement: ORACLE, check: 'an-oversized-body-answers-413' },
    async () => {
      // Driven through `ingest` rather than a live socket: the handler refuses
      // without reading, so a real upload would still be in flight and the
      // proof would be measuring connection draining, not the rejection.
      const sink = createFakeSink();
      const rt = testRuntime({ sink: sink.sink, events: EVENTS });
      const oversized = 'x'.repeat(70_000);
      expect(oversized.length).toBeGreaterThan(MAX_BODY_BYTES);

      const declared = await ingest(
        fakeContext({
          headers: { ...JSON_TYPE, 'content-length': String(oversized.length), 'user-agent': CHROME_UA },
          body: oversized,
        }),
        rt,
      );
      const undeclared = await ingest(
        fakeContext({ headers: { ...JSON_TYPE, 'user-agent': CHROME_UA }, body: oversized }),
        rt,
      );

      await rt.queue.drain(1000);

      expect(declared.status).toBe(413);
      expect(undeclared.status).toBe(413);
      expect(sink.batches).toHaveLength(0);
    },
  );

  specTest(
    'stores nothing sent by a bot',
    { feature: FEATURE, requirement: BOTS, check: 'a-bot-user-agent-gets-no-bootstrap-and-no-row' },
    async () => {
      collector = new TelemetryCollector();
      setCollector(collector);
      const sink = await startApp();

      const response = await testApp?.fetch(ENDPOINT, {
        method: 'POST',
        headers: { ...JSON_TYPE, 'user-agent': BOT_UA },
        body: body([PAGE_VIEW]),
      });
      await flush();

      expect(response.status).toBe(202);
      expect(sink.batches).toHaveLength(0);
      expect(counters()['analytics.ingest.dropped.bot']).toBe(1);
    },
  );

  it('counts every drop reason without reporting it', async () => {
    collector = new TelemetryCollector();
    setCollector(collector);
    const sink = await startApp();

    await testApp?.fetch(ENDPOINT, {
      method: 'POST',
      headers: { ...JSON_TYPE, 'user-agent': CHROME_UA },
      body: body([PAGE_VIEW], { protocolVersion: 2 }),
    });
    await flush();

    expect(counters()['analytics.ingest.dropped.invalid_version']).toBe(1);
    expect(sink.batches).toHaveLength(0);
  });

  it('folds a route the application does not serve under __unknown__', async () => {
    const sink = await startApp({}, { knownRoutes: ['/'] });

    await testApp?.fetch(ENDPOINT, {
      method: 'POST',
      headers: { ...JSON_TYPE, 'user-agent': CHROME_UA },
      body: body([PAGE_VIEW]),
    });
    await flush();

    expect(sink.named('page_view')[0]?.route).toBe('__unknown__');
  });

  it('drops an undeclared property and keeps the declared ones', async () => {
    const sink = await startApp();

    await testApp?.fetch(ENDPOINT, {
      method: 'POST',
      headers: { ...JSON_TYPE, 'user-agent': CHROME_UA },
      body: body([{ ...ACTION, props: { plan: 'pro', secret_token: 'sk-live-1234' } }]),
    });
    await flush();

    expect(sink.named('action')[0]?.props).toEqual({ plan: 'pro' });
  });

  it('accepts the beacon under an app-wide CSRF guard', async () => {
    const sink = await startApp({}, { csrf: true });

    const guarded = await testApp?.fetch('/guarded', { method: 'POST' });
    const beacon = await testApp?.fetch(ENDPOINT, {
      method: 'POST',
      headers: { ...JSON_TYPE, 'user-agent': CHROME_UA },
      body: body([PAGE_VIEW]),
    });
    await flush();

    expect(guarded.status).toBe(403);
    expect(beacon.status).toBe(202);
    expect(sink.rows).toHaveLength(1);
  });

  it('rate limits one peer without telling it how close it was', async () => {
    const sink = await startApp({ rateLimitPerMinute: 2 });
    const send = () =>
      testApp?.fetch(ENDPOINT, {
        method: 'POST',
        headers: { ...JSON_TYPE, 'user-agent': CHROME_UA },
        body: body([PAGE_VIEW]),
      });

    const first = await send();
    const second = await send();
    const third = await send();
    await flush();

    expect([first.status, second.status]).toEqual([202, 202]);
    expect(third.status).toBe(429);
    expect(first.headers.get('ratelimit-remaining')).toBeNull();
    // Two accepted batches, one drained write: the queue is what turns a burst
    // into one round trip instead of one transaction per beacon.
    expect(sink.rows).toHaveLength(2);
    expect(sink.batches).toHaveLength(1);
  });

  it('answers 202 when the sink fails, and stores nothing', async () => {
    useTempProjectRoot();
    const failing = createFakeSink(new Error('connection refused'));
    const rt = testRuntime({ sink: failing.sink, events: EVENTS, knownRoutes: ['/docs/[...page]'] });
    runtime = rt;
    testApp = await createTestApp({ configure: (app) => registerIngestRoute(app.getPlugin(HttpPlugin), rt) });

    const response = await testApp.fetch(ENDPOINT, {
      method: 'POST',
      headers: { ...JSON_TYPE, 'user-agent': CHROME_UA },
      body: body([PAGE_VIEW]),
    });
    await flush();

    expect(response.status).toBe(202);
    expect(failing.rows).toHaveLength(0);
  });

  specTest(
    'answers 202 while the sink write of an earlier batch is still pending',
    { feature: FEATURE, requirement: ASYNC, check: 'the-ingest-route-answers-before-the-sink-settles' },
    async () => {
      const sink = await startApp();
      // A database that never answers, with a flush already in flight: the
      // worst moment a beacon could arrive in.
      sink.hold();
      runtime?.queue.enqueue([pageViewRow()]);
      runtime?.queue.kick();
      expect(sink.pending()).toBe(1);

      const response = await testApp?.fetch(ENDPOINT, {
        method: 'POST',
        headers: { ...JSON_TYPE, 'user-agent': CHROME_UA },
        body: body([PAGE_VIEW, ACTION]),
      });

      // Accepted, queued, answered — with the sink still unsettled and this
      // batch never handed to it. A burst of beacons therefore costs bounded
      // memory, not one open transaction per request.
      expect(response.status).toBe(202);
      expect(await response.text()).toBe('');
      expect(sink.pending()).toBe(1);
      expect(sink.rows).toHaveLength(0);
      expect(sink.batches).toHaveLength(1);
      expect(runtime?.queue.size()).toBe(2);

      sink.release();
      await flush();
      expect(sink.rows).toHaveLength(3);
    },
  );

  it('answers 405 to any other method on the path', async () => {
    await startApp();

    const response = await testApp?.fetch(ENDPOINT, { method: 'GET' });

    expect(response.status).toBe(405);
  });
});

describe('canonicalTs', () => {
  const received = new Date('2026-09-02T10:00:00.000Z');

  it('re-bases the client instant on the batch send time', () => {
    expect(
      canonicalTs(new Date('2026-09-02T09:59:50.000Z'), received, new Date('2026-09-02T09:59:55.000Z')).toISOString(),
    ).toBe('2026-09-02T09:59:55.000Z');
  });

  it('never lets a clock place an event in the future', () => {
    expect(
      canonicalTs(new Date('2030-01-01T00:00:00.000Z'), received, new Date('2026-09-02T09:59:59.000Z')).getTime(),
    ).toBe(received.getTime());
  });

  it('never lets a clock place an event before the retention window', () => {
    const stored = canonicalTs(new Date('2020-01-01T00:00:00.000Z'), received, new Date('2026-09-02T10:00:00.000Z'));

    expect(received.getTime() - stored.getTime()).toBe(24 * 60 * 60 * 1000);
  });

  it('falls back to the receive instant when the batch declared no send time', () => {
    expect(canonicalTs(new Date('2020-01-01T00:00:00.000Z'), received, null)).toEqual(received);
  });
});
