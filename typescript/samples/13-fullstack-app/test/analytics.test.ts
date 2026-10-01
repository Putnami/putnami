import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import net from 'node:net';
import { analytics, flushAnalytics } from '@putnami/analytics';
import { config } from '@putnami/application';
import { createTestApp, type TestApp } from '@putnami/application/testing';
import { database, provideRepository, repositoryToken, sql } from '@putnami/database';
import { events } from '@putnami/events';
import { react } from '@putnami/web';
import { analyticsEvents } from '../src/analytics';
import seedTask from '../src/events/project-created.seed-task.on';
import syncProject from '../src/events/task-status.sync-project.on';
import { migrations } from '../src/main';
import { ProjectService, projects } from '../src/projects';
import { TaskService, Tasks, tasks } from '../src/tasks';

// Skip tests when PostgreSQL is not reachable (requires running database on port 6543)
const dbAvailable = await new Promise<boolean>((resolve) => {
  const socket = new net.Socket();
  socket.setTimeout(1000);
  socket.connect(6543, 'localhost', () => {
    socket.destroy();
    resolve(true);
  });
  socket.on('error', () => {
    socket.destroy();
    resolve(false);
  });
  socket.on('timeout', () => {
    socket.destroy();
    resolve(false);
  });
});

const INGEST = '/_putnami/analytics/events';
const CHROME_UA =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36';
/** One client-owned session for the whole run, and one client-minted view. */
const SESSION_ID = '01920000-0000-7000-8000-0000000000aa';
const TASKS_VIEW_ID = '01920000-0000-7000-8000-0000000000bb';
/** The view queued by the last test, which only `stop()` can land. */
const DRAINED_VIEW_ID = '01920000-0000-7000-8000-0000000000cc';
const ANALYTICS_TABLES = [
  'analytics_event',
  'analytics_daily_counter',
  'analytics_daily_visitor',
  'analytics_daily_session',
];

/** The UTC day every aggregate is keyed on. */
const today = new Date().toISOString().slice(0, 10);

type Client = Awaited<ReturnType<typeof database>>;

describe.skipIf(!dbAvailable)('fullstack-app analytics', () => {
  let testApp: TestApp;
  let db: Client;
  let pageViewId: string;
  let projectId: string;
  let taskId: string;
  let csrfToken: string;

  const rows = async <T>(text: string, params: unknown[] = []): Promise<T[]> =>
    (await db.unsafe(text, params as never[])) as unknown as T[];

  const count = async (table: string, where = 'true', params: unknown[] = []): Promise<number> => {
    const [row] = await rows<{ count: number }>(`SELECT COUNT(*)::int AS count FROM ${table} WHERE ${where}`, params);
    return Number(row?.count ?? 0);
  };

  const counter = async (dimension: string, key: string): Promise<number> => {
    const [row] = await rows<{ count: string }>(
      'SELECT count FROM analytics_daily_counter WHERE day = $1 AND dimension = $2 AND key = $3',
      [today, dimension, key],
    );
    return row ? Number(row.count) : 0;
  };

  /** Everything the four tables hold, ordered, as one comparable value. */
  const snapshot = async (): Promise<string> => {
    const events = await rows(
      `SELECT event_id, name, source, route, path, session_id, seq, engagement_ms, action_name, outcome, props
       FROM analytics_event ORDER BY event_id`,
    );
    const counters = await rows(
      'SELECT day, dimension, key, count FROM analytics_daily_counter ORDER BY day, dimension, key',
    );
    const visitors = await rows('SELECT day, visitor_id FROM analytics_daily_visitor ORDER BY day, visitor_id');
    const sessions = await rows(
      `SELECT day, session_id, first_route, last_route, page_views, engagement_ms
       FROM analytics_daily_session ORDER BY day, session_id`,
    );
    // `count` and `engagement_ms` arrive as BigInt from Postgres.
    return JSON.stringify({ events, counters, visitors, sessions }, (_key, value) =>
      typeof value === 'bigint' ? value.toString() : value,
    );
  };

  /** A page view the browser sends, in the wire shape of the analytics protocol. */
  const beacon = (event: Record<string, unknown>): string =>
    JSON.stringify({
      protocolVersion: 1,
      sentAt: new Date().toISOString(),
      events: [{ clientTs: new Date().toISOString(), sessionId: SESSION_ID, ...event }],
    });

  const post = (body: string, userAgent = CHROME_UA): Promise<Response> =>
    testApp.fetch(INGEST, {
      method: 'POST',
      // What `navigator.sendBeacon` sends. The route is CSRF-exempt by contract.
      headers: { 'Content-Type': 'text/plain', 'User-Agent': userAgent },
      body,
    });

  /**
   * Writes what the queue is holding.
   *
   * Nothing on a response path waits for Postgres, so every assertion that
   * reads a row back asks for it first. `flushAnalytics()` is the public API
   * for exactly this — a consuming application's tests, and an operator.
   */
  const flush = (): Promise<void> => flushAnalytics();

  beforeAll(async () => {
    testApp = await createTestApp({
      plugins: [
        config(),
        sql({ autoApply: true }),
        migrations,
        events({ autoScan: false, handlers: [seedTask, syncProject] }),
        projects(),
        tasks(),
        react(),
        // After sql(): the analytics tables come from this plugin's migration.
        analytics({ events: analyticsEvents }),
      ],
      configure: (app) => {
        app.provide(ProjectService);
        app.register(provideRepository(Tasks));
        app.provide(TaskService, { deps: [repositoryToken(Tasks)] });
      },
    });
    db = await database('default');
    // `fullstack_db` is shared across runs; start from an empty measurement.
    for (const table of ANALYTICS_TABLES) {
      await db.unsafe(`DELETE FROM ${table}`);
    }
  });

  afterAll(async () => {
    // `stop()` leaves the pool open, so the tables are still reachable after
    // the drain proof below stopped the application.
    await testApp?.stop();
    if (db) {
      for (const table of ANALYTICS_TABLES) {
        await db.unsafe(`DELETE FROM ${table}`);
      }
    }
  });

  it('resolves the analytics datasource to default and creates its four tables', async () => {
    // `analytics.datasource` is `analytics`, and no `database.analytics` block
    // exists, so the resolution falls back to the primary datasource.
    for (const table of ANALYTICS_TABLES) {
      expect(await count(table)).toBe(0);
    }
    // The migration namespace is the plugin's, and it ran in the `default`
    // datasource this sample declares.
    const applied = await rows<{ name: string }>(
      "SELECT name FROM migration.migrations WHERE name LIKE 'putnami-analytics/%' AND success = 1 ORDER BY name",
    );
    expect(applied.map((row) => row.name)).toEqual([
      'putnami-analytics/001_create_analytics_event',
      'putnami-analytics/002_create_analytics_daily',
    ]);
  });

  it('records a server page view, its route counter, and the day visitor', async () => {
    const res = await testApp.fetch('/projects', { headers: { 'User-Agent': CHROME_UA } });
    expect(res.status).toBe(200);
    const html = await res.text();
    expect(html).toContain('<!DOCTYPE html');
    await flush();

    const [row] = await rows<{ eventId: string; route: string; path: string; sessionId: string | null }>(
      "SELECT event_id, route, path, session_id FROM analytics_event WHERE name = 'page_view' AND source = 'server'",
    );
    expect(row).toBeDefined();
    expect(row.route).toBe('/projects');
    expect(row.path).toBe('/projects');
    // The server never knows the session: that is the browser's own record.
    expect(row.sessionId).toBeNull();
    expect(await counter('route', '/projects')).toBe(1);
    expect(await counter('event', 'page_view')).toBe(1);
    expect(await count('analytics_daily_visitor', 'day = $1', [today])).toBe(1);

    // The id the server put in the HTML is the id it wrote to the database.
    // Other globals may follow the bootstrap, such as `__putnamiExposeErrors`
    // outside production, so stop at the next one.
    pageViewId = JSON.parse(/window\.__putnamiBootstrap=(\{.*?\});window\./.exec(html)?.[1] as string).analytics.pv;
    expect(pageViewId).toBe(row.eventId);
  });

  it('enriches the server row from a re-send that carries no engagement', async () => {
    // This is the tracker's FIRST re-send, and the shape it actually sends: the
    // session, viewport, language and campaign the server could not know, with
    // no engagement yet because the visitor has not left the page. A server-side
    // dedup cache that remembered the server's own id would drop exactly this
    // row while still answering 202, and the browser would ack and discard it —
    // the enrichment would be lost unless a later engagement beacon arrived.
    const res = await post(
      beacon({
        eventId: pageViewId,
        name: 'page_view',
        seq: 0,
        page: { path: '/projects', route: '/projects' },
      }),
    );
    expect(res.status).toBe(202);
    await flush();

    const [row] = await rows<{ sessionId: string | null; engagementMs: number }>(
      'SELECT session_id, engagement_ms FROM analytics_event WHERE event_id = $1',
      [pageViewId],
    );
    expect(row.sessionId).toBe(SESSION_ID);
    expect(row.engagementMs).toBe(0);
    // Enriched, not duplicated.
    expect(await count('analytics_event')).toBe(1);
    expect(await counter('route', '/projects')).toBe(1);
  });

  it('enriches the server row when the browser re-sends the same event id', async () => {
    const res = await post(
      beacon({
        eventId: pageViewId,
        name: 'page_view',
        seq: 0,
        engagementMs: 4200,
        page: { path: '/projects', route: '/projects' },
      }),
    );
    expect(res.status).toBe(202);
    await flush();

    const [row] = await rows<{ sessionId: string; engagementMs: number; source: string; renderMs: number | null }>(
      'SELECT session_id, engagement_ms, source, render_ms FROM analytics_event WHERE event_id = $1',
      [pageViewId],
    );
    // The conflict branch fills nulls and raises engagement; it never rewrites
    // what the server already decided.
    expect(row.sessionId).toBe(SESSION_ID);
    expect(row.engagementMs).toBe(4200);
    expect(row.source).toBe('server');
    expect(row.renderMs).not.toBeNull();
    // One page view, seen twice: the row was enriched, not created.
    expect(await count('analytics_event')).toBe(1);
    expect(await counter('route', '/projects')).toBe(1);
    expect(await counter('event', 'page_view')).toBe(1);
  });

  it('moves nothing when the exact same batch arrives a second time', async () => {
    const batch = beacon({
      eventId: pageViewId,
      name: 'page_view',
      seq: 0,
      engagementMs: 4200,
      page: { path: '/projects', route: '/projects' },
    });
    const before = await snapshot();

    const res = await post(batch);
    await flush();

    expect(res.status).toBe(202);
    // The dedup proof: every row of all four tables is byte-identical after a
    // replay. `ON CONFLICT (event_id)` is what makes it true — the in-memory
    // cache lets an engagement re-send through on purpose, so this batch really
    // did reach Postgres and really was absorbed there.
    expect(await snapshot()).toBe(before);
  });

  it('counts a client-minted page view of a second route in the same session', async () => {
    const res = await post(
      beacon({
        eventId: TASKS_VIEW_ID,
        name: 'page_view',
        seq: 1,
        engagementMs: 1500,
        page: { path: '/tasks', route: '/tasks' },
      }),
    );
    expect(res.status).toBe(202);
    await flush();

    const [row] = await rows<{ source: string; route: string; sessionId: string; seq: number }>(
      'SELECT source, route, session_id, seq FROM analytics_event WHERE event_id = $1',
      [TASKS_VIEW_ID],
    );
    expect(row.source).toBe('client');
    expect(row.route).toBe('/tasks');
    expect(row.seq).toBe(1);
    expect(await counter('route', '/tasks')).toBe(1);
    expect(await counter('route', '/projects')).toBe(1);

    const [session] = await rows<{ pageViews: number; firstRoute: string; lastRoute: string; engagementMs: string }>(
      'SELECT page_views, first_route, last_route, engagement_ms FROM analytics_daily_session WHERE session_id = $1',
      [SESSION_ID],
    );
    expect(session).toBeDefined();
    expect(session.firstRoute).toBe('/tasks');
    expect(session.lastRoute).toBe('/tasks');
    // One, not two. A session row is opened only by a page view the upsert
    // *inserted*; enriching the server-rendered view above filled its
    // `session_id` but did not create the session. `page_views` therefore
    // counts client-minted views only.
    expect(session.pageViews).toBe(1);
    // The engagement total is recomputed from the raw rows of the session, so
    // it carries the enriched server view as well as this client one.
    expect(Number(session.engagementMs)).toBe(5700);
  });

  it('records a declared action tracked from an API handler', async () => {
    const project = await testApp.fetch('/api/projects', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: 'Analytics sample', description: 'Proves the full loop' }),
    });
    expect(project.status).toBe(201);
    projectId = (await project.json()).project.id;

    const created = await testApp.fetch('/api/tasks', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'User-Agent': CHROME_UA },
      body: JSON.stringify({ projectId, title: 'Measure the launch', priority: 3 }),
    });
    expect(created.status).toBe(201);
    taskId = (await created.json()).task.id;
    await flush();

    const [row] = await rows<{ source: string; actionName: string; route: string; priority: string }>(
      `SELECT source, action_name, route, props ->> 'priority' AS priority
       FROM analytics_event WHERE name = 'action'`,
    );
    expect(row.source).toBe('server');
    expect(row.actionName).toBe('task_created');
    // An action has no page, and `route` is NOT NULL.
    expect(row.route).toBe('__none__');
    expect(row.priority).toBe('3');
    expect(await counter('action', 'task_created')).toBe(1);
  });

  it('records the outcome of a web form action', async () => {
    const page = await testApp.fetch('/tasks', { headers: { 'User-Agent': CHROME_UA } });
    expect(page.status).toBe(200);
    csrfToken = /_csrf=([^;]+)/.exec(page.headers.get('set-cookie') ?? '')?.[1] as string;
    expect(csrfToken).toBeString();
    expect(await page.text()).toContain('name="_csrf"');

    const submitted = await testApp.fetch('/tasks', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/x-www-form-urlencoded',
        Cookie: `_csrf=${csrfToken}`,
        'User-Agent': CHROME_UA,
      },
      body: new URLSearchParams({ _csrf: csrfToken, taskId }),
    });
    expect(submitted.status).toBe(200);
    await flush();

    const [row] = await rows<{ outcome: string; route: string; source: string }>(
      "SELECT outcome, route, source FROM analytics_event WHERE name = 'form_submit'",
    );
    expect(row.outcome).toBe('ok');
    expect(row.route).toBe('/tasks');
    expect(row.source).toBe('server');
    expect(await counter('form_submit', '/tasks|ok')).toBe(1);
  });

  it('records nothing for a bot', async () => {
    await flush();
    const before = await snapshot();

    const res = await testApp.fetch('/projects', { headers: { 'User-Agent': 'Googlebot/2.1' } });
    await flush();

    expect(res.status).toBe(200);
    expect(await snapshot()).toBe(before);
  });

  it('writes what the queue was still holding when the application stops', async () => {
    // A server page view and a beacon, neither of which waited for Postgres.
    // With a minute-long flush period nothing else can land them: what proves
    // they are stored is the drain inside `stop()`, which is where SIGTERM
    // arrives before a scale-to-zero instance disappears.
    const serverViewsBefore = await count('analytics_event', "name = 'page_view' AND source = 'server'");

    const page = await testApp.fetch('/projects', { headers: { 'User-Agent': CHROME_UA } });
    expect(page.status).toBe(200);
    const beaconRes = await post(
      beacon({
        eventId: DRAINED_VIEW_ID,
        name: 'page_view',
        seq: 2,
        engagementMs: 900,
        page: { path: '/projects', route: '/projects' },
      }),
    );
    expect(beaconRes.status).toBe(202);
    expect(await count('analytics_event', 'event_id = $1', [DRAINED_VIEW_ID])).toBe(0);

    await testApp.stop();

    expect(await count('analytics_event', 'event_id = $1', [DRAINED_VIEW_ID])).toBe(1);
    expect(await count('analytics_event', "name = 'page_view' AND source = 'server'")).toBe(serverViewsBefore + 1);
  });
});
