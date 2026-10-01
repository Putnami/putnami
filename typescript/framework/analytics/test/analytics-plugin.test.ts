import { afterEach, describe, expect, it } from 'bun:test';
import { type Module, module, type Plugin } from '@putnami/application';
import { createTestApp, type TestApp } from '@putnami/application/testing';
import { type SQLSource, sqlSourceInline } from '@putnami/database';
import { resetConfigLoader } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { analytics, AnalyticsPlugin, hasSqlPlugin, startFlushTicker } from '../src/server/analytics.plugin';
import { MISSING_SECRET_MESSAGE } from '../src/server/analytics.config';
import { MIGRATION_NAMESPACE } from '../src/server/sink/migrations';
import type { WriteQueue } from '../src/server/sink/queue';
import { SET_SCHEMA_SQL } from '../src/server/sink/sink';
import { analyticsRuntime, NOT_INSTALLED_MESSAGE, setAnalyticsRuntime } from '../src/server/runtime';
import { createFakeSink } from './utils/fake-sink';
import { createFakeSql } from './utils/fake-sql';
import { restoreProjectRoot, TEST_SECRET, useTempProjectRoot } from './utils/runtime';

const FEATURE = 'typescript/web-analytics-collection';
const CONSENT = 'identified-mode-needs-consent';
const ASYNC = 'a-slow-database-never-delays-a-response';
const ENDPOINT = '/_putnami/analytics/events';
const CHROME_UA =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36';

const BATCH = JSON.stringify({
  protocolVersion: 1,
  sentAt: '2026-09-02T10:00:00.000Z',
  events: [
    {
      eventId: '01920000-0000-7000-8000-000000000001',
      name: 'page_view',
      clientTs: '2026-09-02T09:59:58.000Z',
      seq: 0,
      sessionId: '01920000-0000-7000-8000-0000000000aa',
      page: { path: '/' },
    },
  ],
});

/**
 * A stand-in shaped exactly like what `sql()` returns: the probe name plus the
 * infra contributor member. `sql()` exports no class, so plugin presence is a
 * structural question, and composing the real one would open a database this
 * package is not allowed to need.
 */
const fakeSqlPlugin: Plugin = {
  name: 'database',
  designInfraRequirements: () => [],
} as unknown as Plugin;

let testApp: TestApp | undefined;

function configure(analyticsSection: Record<string, unknown> = {}): void {
  process.env['CONFIG_DATA'] = JSON.stringify({ analytics: { secret: TEST_SECRET, ...analyticsSection } });
  useTempProjectRoot();
}

afterEach(async () => {
  await testApp?.stop();
  testApp = undefined;
  setAnalyticsRuntime(undefined);
  process.env['CONFIG_DATA'] = undefined;
  restoreProjectRoot();
  resetConfigLoader();
});

describe('the analytics plugin without a database', () => {
  it('serves, warns, and contributes no migration', async () => {
    configure();
    const plugin = analytics();

    testApp = await createTestApp({ plugins: [plugin] });
    const response = await testApp.fetch(ENDPOINT, {
      method: 'POST',
      headers: { 'content-type': 'application/json', 'user-agent': CHROME_UA },
      body: BATCH,
    });

    // An application that never asked for a database must not be given a
    // connection error in exchange for adding a measurement plugin.
    expect(response.status).toBe(202);
    expect(plugin.migrationSources()).toEqual([]);
    expect(analyticsRuntime().datasource).toBe('default');
  });
});

describe('the analytics plugin with a database', () => {
  it('contributes the analytics migration source', async () => {
    configure({ datasource: 'default' });
    const plugin = analytics();

    testApp = await createTestApp({ plugins: [fakeSqlPlugin, plugin] });

    const sources = plugin.migrationSources();
    expect(sources).toHaveLength(1);
    expect(sources[0]?.namespace).toBe(MIGRATION_NAMESPACE);
  });

  it('puts the tables in the schema sql() declares for the resolved datasource', async () => {
    // `marketing` is declared in no binding, so warmup resolves to `default`,
    // and the schema has to be the one `default` lives in.
    configure({ datasource: 'marketing' });
    const plugin = analytics();
    const sqlOnDefault = { ...fakeSqlPlugin, primaryDatasource: { name: 'default', schema: 'tenant' } } as Plugin;

    testApp = await createTestApp({ plugins: [sqlOnDefault, plugin] });

    const [source] = plugin.migrationSources();
    expect(source?.datasource).toBe('default');
    expect(source?.schema).toBe('tenant');
  });

  it('writes in the schema its migration was built in, even when the resolution later moves', async () => {
    // A workload source puts `default` in a named schema, until it stops
    // contributing: re-resolved at the first write, the schema would be
    // `public`, where the migration created no table.
    let contributing = true;
    const workload = {
      name: 'workload-migrations',
      migrationSources: (): SQLSource[] =>
        contributing
          ? [
              sqlSourceInline({
                namespace: 'app',
                datasource: { name: 'default', schema: 'marketing' },
                definitions: [{ name: '001_noop', sql: 'SELECT 1;' }],
              }),
            ]
          : [],
    } as unknown as Plugin;
    const fake = createFakeSql();
    configure({ datasource: 'default' });
    const plugin = analytics({ __connect: () => Promise.resolve(fake.sql) });
    testApp = await createTestApp({ plugins: [fakeSqlPlugin, workload, plugin] });

    // What prepare() does: read the sources, which is when the schema is fixed.
    expect(plugin.migrationSources()[0]?.schema).toBe('marketing');
    contributing = false;
    const accepted = await testApp.fetch(ENDPOINT, {
      method: 'POST',
      headers: { 'content-type': 'application/json', 'user-agent': CHROME_UA },
      body: BATCH,
    });
    expect(accepted.status).toBe(202);
    // Stopping drains the queue through the plugin's own Postgres sink.
    await testApp.stop();
    testApp = undefined;

    expect(fake.callStartingWith(SET_SCHEMA_SQL).args).toEqual(['marketing']);
  });

  it('terminates when a second contributor reads every source while it builds its own', async () => {
    // Shaped like analytics: it walks the tree and reads every contributor's
    // sources, analytics' included, with its own re-entry guard. Each guard
    // stops only its own plugin, so the walk ends only if both hold.
    let root: Module | undefined;
    let reading = false;
    const lazy = {
      name: 'lazy-migrations',
      warmup: async (owner: Module) => {
        root = owner.getRoot();
      },
      migrationSources: (): SQLSource[] => {
        if (reading || !root) {
          return [];
        }
        reading = true;
        try {
          for (const { plugin } of root.collectPlugins()) {
            (plugin as Partial<{ migrationSources(): unknown[] }>).migrationSources?.();
          }
        } finally {
          reading = false;
        }
        return [
          sqlSourceInline({
            namespace: 'lazy',
            datasource: { name: 'default', schema: 'marketing' },
            definitions: [{ name: '001_noop', sql: 'SELECT 1;' }],
          }),
        ];
      },
    } as unknown as Plugin;
    configure({ datasource: 'default' });
    const plugin = analytics();
    testApp = await createTestApp({ plugins: [fakeSqlPlugin, lazy, plugin] });

    const [source] = plugin.migrationSources();

    expect(source?.schema).toBe('marketing');
  });

  it('detects the SQL plugin anywhere in the module tree', async () => {
    configure();
    const plugin = analytics();

    testApp = await createTestApp({ plugins: [module('data').use(fakeSqlPlugin), plugin] });

    expect(plugin.migrationSources()).toHaveLength(1);
  });
});

describe('the analytics plugin at warmup', () => {
  specTest(
    'refuses to start in identified mode without a consent gate',
    { feature: FEATURE, requirement: CONSENT, check: 'no-cookie-before-consent' },
    async () => {
      configure({ mode: 'identified' });

      // Fail closed at startup: a persistent cookie nobody gated is a consent
      // violation an operator has to see before the first request, not after.
      expect(createTestApp({ plugins: [analytics()] })).rejects.toThrow(
        'analytics: mode "identified" requires a consent(ctx) callback',
      );
    },
  );

  it('starts in identified mode once a consent gate is supplied', async () => {
    configure({ mode: 'identified' });

    testApp = await createTestApp({ plugins: [analytics({ consent: () => false })] });

    expect(analyticsRuntime().config.mode).toBe('identified');
  });

  it('refuses to start without any server-side secret', async () => {
    process.env['CONFIG_DATA'] = JSON.stringify({ analytics: {} });
    useTempProjectRoot();

    expect(createTestApp({ plugins: [analytics()] })).rejects.toThrow(MISSING_SECRET_MESSAGE);
  });

  it('registers nothing when disabled', async () => {
    configure({ enabled: false });

    testApp = await createTestApp({ plugins: [analytics()] });
    const response = await testApp.fetch(ENDPOINT, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: BATCH,
    });

    expect(response.status).toBe(404);
    expect(() => analyticsRuntime()).toThrow(NOT_INSTALLED_MESSAGE);
  });

  it('reads programmatic options underneath the configuration file', async () => {
    configure();

    testApp = await createTestApp({ plugins: [analytics({ rateLimitPerMinute: 7, events: { ping: {} } })] });

    expect(analyticsRuntime().config.rateLimitPerMinute).toBe(7);
    expect(analyticsRuntime().declared.has('ping')).toBe(true);
  });

  it('moves the ingest endpoint with the owning module base path', async () => {
    configure();
    const plugin = analytics();

    testApp = await createTestApp({ plugins: [module('tracked').path('/app').use(plugin)] });

    expect(analyticsRuntime().endpoint).toBe('/app/_putnami/analytics/events');
    const moved = await testApp.fetch('/app/_putnami/analytics/events', {
      method: 'POST',
      headers: { 'content-type': 'application/json', 'user-agent': CHROME_UA },
      body: BATCH,
    });
    expect(moved.status).toBe(202);
  });

  specTest(
    'drains the queue on stop, and returns at its deadline when the sink never settles',
    { feature: FEATURE, requirement: ASYNC, check: 'stop-drains-within-its-deadline' },
    async () => {
      // 1. What a running application queued is written by the drain.
      configure();
      const sink = createFakeSink();
      testApp = await createTestApp({ plugins: [analytics({ __sink: sink.sink })] });
      const accepted = await testApp.fetch(ENDPOINT, {
        method: 'POST',
        headers: { 'content-type': 'application/json', 'user-agent': CHROME_UA },
        body: BATCH,
      });

      // Answered without the sink being touched: nothing on a response path
      // waits for the database.
      expect(accepted.status).toBe(202);
      expect(sink.batches).toHaveLength(0);

      await testApp.stop();
      testApp = undefined;

      // SIGTERM reaches `stop()` through `installSignalHandlers`, so this is
      // where a scale-to-zero instance lands what it was holding.
      expect(sink.rows).toHaveLength(1);
      expect(sink.rows[0]?.source).toBe('client');
      expect(() => analyticsRuntime()).toThrow(NOT_INSTALLED_MESSAGE);

      // 2. A database that never answers costs the deadline, not the shutdown:
      // `installSignalHandlers` force-exits at 10 000 ms, and a `stop()` that
      // hung until then would be worse than the rows it was trying to save.
      configure({ flushDeadlineMs: 150 });
      const hung = createFakeSink();
      hung.hold();
      testApp = await createTestApp({ plugins: [analytics({ __sink: hung.sink })] });
      await testApp.fetch(ENDPOINT, {
        method: 'POST',
        headers: { 'content-type': 'application/json', 'user-agent': CHROME_UA },
        body: BATCH,
      });

      const startedAt = Date.now();
      await testApp.stop();
      const elapsed = Date.now() - startedAt;
      testApp = undefined;

      expect(elapsed).toBeGreaterThanOrEqual(120);
      expect(elapsed).toBeLessThan(5000);
      expect(hung.rows).toHaveLength(0);
      hung.release();
    },
  );

  it('drives the queue from an unref-ed ticker, and clears it on stop', async () => {
    configure({ flushIntervalMs: 1234 });
    const sink = createFakeSink();
    const realSetInterval = globalThis.setInterval;
    const realClearInterval = globalThis.clearInterval;
    let ticker: unknown;
    let unrefs = 0;
    let cleared = 0;
    // Only the analytics ticker is observed: every other interval the
    // framework installs passes straight through to the real function.
    globalThis.setInterval = ((handler: () => void, timeout?: number, ...args: unknown[]) => {
      const timer = realSetInterval(handler, timeout, ...args);
      if (timeout === 1234) {
        ticker = timer;
        const unref = (timer as { unref?: () => unknown }).unref?.bind(timer);
        (timer as { unref: () => unknown }).unref = () => {
          unrefs += 1;
          return unref?.();
        };
      }
      return timer;
    }) as unknown as typeof setInterval;
    globalThis.clearInterval = ((handle?: unknown) => {
      if (handle !== undefined && handle === ticker) {
        cleared += 1;
      }
      return realClearInterval(handle as Parameters<typeof clearInterval>[0]);
    }) as unknown as typeof clearInterval;

    try {
      testApp = await createTestApp({ plugins: [analytics({ __sink: sink.sink })] });

      // Unref-ed, so the ticker never by itself keeps a process alive — which
      // is what makes it safe to install one at warmup in every application.
      expect(ticker).toBeDefined();
      expect(unrefs).toBe(1);

      await testApp.stop();
      testApp = undefined;

      expect(cleared).toBe(1);
    } finally {
      globalThis.setInterval = realSetInterval;
      globalThis.clearInterval = realClearInterval;
    }
  });
});

describe('startFlushTicker', () => {
  /** A queue that only counts kicks. */
  function countingQueue(): { queue: WriteQueue; kicks: () => number } {
    let kicks = 0;
    const queue: WriteQueue = {
      enqueue: () => undefined,
      kick: () => {
        kicks += 1;
        return undefined;
      },
      drain: () => Promise.resolve(),
      size: () => 0,
    };
    return { queue, kicks: () => kicks };
  }

  const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

  it('kicks the queue on every tick until it is stopped', async () => {
    const { queue, kicks } = countingQueue();

    const stop = startFlushTicker(queue, 5);
    await sleep(60);
    const ticked = kicks();
    stop();
    await sleep(40);

    // The ticker is the trigger that does not need a request: it covers the
    // keep-warm ping and the routes that bypass the middleware chain.
    expect(ticked).toBeGreaterThan(0);
    expect(kicks()).toBe(ticked);
  });
});

describe('hasSqlPlugin', () => {
  it('ignores a plugin that only borrows the probe name', () => {
    const impostor = { name: 'database' } as Plugin;
    const root = module('root').use(impostor);

    expect(hasSqlPlugin(root)).toBe(false);
    expect(hasSqlPlugin(module('root').use(fakeSqlPlugin))).toBe(true);
  });
});

describe('analytics()', () => {
  it('builds the plugin with no options at all', () => {
    expect(analytics()).toBeInstanceOf(AnalyticsPlugin);
  });
});
