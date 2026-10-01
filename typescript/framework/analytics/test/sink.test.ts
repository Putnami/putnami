import { describe, expect, it } from 'bun:test';
import type { Logger } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { createNoopSink } from '../src/server/sink/noop';
import { OVERFLOW_KEY } from '../src/server/sink/fold';
import {
  COUNTERS_SQL,
  createSink,
  RAW_UPSERT_SQL,
  SINK_FAILURE_LOG_INTERVAL_MS,
  SESSION_ENGAGEMENT_SQL,
  SESSIONS_SQL,
  SET_SCHEMA_SQL,
  VISITORS_SQL,
} from '../src/server/sink/sink';
import { createFakeSql, type FakeSql } from './utils/fake-sql';
import { actionRow, pageViewRow, resultFor, testConfig } from './utils/fixtures';

const FEATURE = 'typescript/web-analytics-collection';
const REQUIREMENT = 'a-retried-event-is-stored-once';
const ENRICHES = 'the-raw-upsert-only-enriches-on-conflict';
const INSERTED_ONLY = 'counters-fold-only-inserted-rows';

const NOW = new Date('2026-09-02T10:30:00.000Z');

/** Captures what the sink reported, without any log plumbing. */
function fakeLogger(): { logger: Logger; entries: { message: unknown; params: unknown[] }[] } {
  const entries: { message: unknown; params: unknown[] }[] = [];
  const logger = {
    error: (message: unknown, ...params: unknown[]) => {
      entries.push({ message, params });
    },
  } as unknown as Logger;
  return { logger, entries };
}

/** Labels one recorded statement, so an order assertion reads like body §B.4. */
function label(query: string): string {
  if (query.startsWith('INSERT INTO analytics_event')) return 'S1';
  if (query.startsWith('INSERT INTO analytics_daily_counter')) return 'S2';
  if (query.startsWith('INSERT INTO analytics_daily_visitor')) return 'S3';
  if (query.startsWith('INSERT INTO analytics_daily_session')) return 'S4';
  if (query.startsWith('UPDATE analytics_daily_session')) return 'S5';
  if (query.startsWith('DELETE FROM')) return 'S6';
  if (query.startsWith('SELECT count(*)')) return 'path-cap';
  if (query === SET_SCHEMA_SQL) return 'S0';
  return query;
}

function sequence(fake: FakeSql): string[] {
  return fake.calls.map((call) => label(call.query));
}

function sinkOn(fake: FakeSql, overrides: Parameters<typeof testConfig>[0] = {}) {
  const { logger, entries } = fakeLogger();
  const sink = createSink({
    datasource: 'analytics',
    schema: () => 'public',
    config: testConfig({ retentionMode: 'off', ...overrides }),
    logger,
    now: () => NOW,
    connect: () => Promise.resolve(fake.sql),
  });
  return { sink, entries };
}

describe('createSink', () => {
  const first = pageViewRow({ eventId: '01920000-0000-7000-8000-000000000021' });
  const second = pageViewRow({
    eventId: '01920000-0000-7000-8000-000000000022',
    route: '/pricing',
    ts: new Date('2026-09-02T10:05:00.000Z'),
  });
  const third = actionRow({ eventId: '01920000-0000-7000-8000-000000000023' });
  const batch = [first, second, third];

  it('issues the batch as one transaction and releases the connection', async () => {
    const fake = createFakeSql({ upsert: batch.map((row) => resultFor(row)) });
    const { sink } = sinkOn(fake);

    const counts = await sink.write(batch);

    expect(sequence(fake)).toEqual(['BEGIN', 'S0', 'path-cap', 'S1', 'S2', 'S3', 'S4', 'S5', 'COMMIT']);
    expect(fake.reserves()).toBe(1);
    expect(fake.releases()).toBe(1);
    expect(counts).toEqual({ inserted: 3, enriched: 0 });
  });

  it('binds thirty-five parallel arrays of equal length to the raw upsert', async () => {
    const fake = createFakeSql({ upsert: batch.map((row) => resultFor(row)) });
    const { sink } = sinkOn(fake);

    await sink.write(batch);

    const args = fake.callStartingWith('INSERT INTO analytics_event').args;
    expect(args).toHaveLength(35);
    for (const column of args) {
      expect(Array.isArray(column)).toBe(true);
      expect(column as unknown[]).toHaveLength(3);
    }
    // The `jsonb[]` parameter carries text, not objects.
    expect(args[27]).toEqual(['{}', '{}', '{"plan":"pro"}']);
    expect(args[0]).toEqual(batch.map((row) => row.eventId));
  });

  it('binds every instant as an ISO-8601 string, never as a Date', async () => {
    const fake = createFakeSql({ upsert: batch.map((row) => resultFor(row)) });
    const { sink } = sinkOn(fake);

    await sink.write(batch);

    // The driver types an array parameter from its *first element*, so an array
    // of `Date` declares `$2` a scalar `timestamptz` and Postgres refuses
    // `$2::timestamptz[]` — every batch is lost against a real database. The
    // fake accepts anything, which is exactly why this is asserted here.
    const upsert = fake.callStartingWith('INSERT INTO analytics_event').args;
    expect(upsert[1]).toEqual(batch.map((row) => row.ts.toISOString()));
    const sessions = fake.callStartingWith('INSERT INTO analytics_daily_session').args;
    for (const column of [sessions[6], sessions[7]] as unknown[][]) {
      expect(column.every((value) => typeof value === 'string')).toBe(true);
    }
  });

  it('binds the declared properties as text and casts them to jsonb in the statement', () => {
    // A bare `$28::jsonb[]` stores every payload as a JSON *string*: the driver
    // escapes each element of an array whose type it cannot infer, so
    // `props ->> 'plan'` reads null on every row ever written.
    expect(RAW_UPSERT_SQL).toContain('$28::text[]::jsonb[]');
    expect(RAW_UPSERT_SQL).not.toContain('$28::jsonb[]');
  });

  specTest(
    'enriches an already-stored event instead of counting it again',
    { feature: FEATURE, requirement: REQUIREMENT, check: ENRICHES },
    () => {
      expect(RAW_UPSERT_SQL).toContain('ON CONFLICT (event_id) DO UPDATE SET');
      expect(RAW_UPSERT_SQL).toContain('GREATEST(analytics_event.engagement_ms, EXCLUDED.engagement_ms)');
      expect(RAW_UPSERT_SQL).toContain('COALESCE(analytics_event.session_id, EXCLUDED.session_id)');
      // `xmax = 0` is the whole dedup story: it tells the fold which rows the
      // statement created, so a retried batch moves nothing.
      expect(RAW_UPSERT_SQL).toContain('(xmax = 0) AS inserted');
      // Nothing in the conflict branch overwrites a stored value with a weaker
      // one, so replaying a batch cannot lose data either.
      expect(RAW_UPSERT_SQL).not.toContain('DO UPDATE SET route');
    },
  );

  specTest(
    'keeps an enriched row out of the counters, visitors, and sessions it wrote',
    { feature: FEATURE, requirement: REQUIREMENT, check: INSERTED_ONLY },
    async () => {
      const fake = createFakeSql({
        upsert: [resultFor(first), resultFor(second, false), resultFor(third)],
      });
      const { sink } = sinkOn(fake);

      const counts = await sink.write(batch);

      expect(counts).toEqual({ inserted: 2, enriched: 1 });
      const counters = fake.callStartingWith('INSERT INTO analytics_daily_counter').args;
      expect(counters[2] as string[]).not.toContain('/pricing');
      const visitors = fake.callStartingWith('INSERT INTO analytics_daily_visitor').args;
      // Both page views share one visitor, so the enriched one leaves no trace.
      expect(visitors[0] as string[]).toHaveLength(1);
      const sessions = fake.callStartingWith('INSERT INTO analytics_daily_session').args;
      expect(sessions[5] as number[]).toEqual([1]);
      // The enriched row is still a page view of a session whose engagement
      // total changed, so it reaches S5.
      const touched = fake.callStartingWith('UPDATE analytics_daily_session').args;
      expect(touched[1] as string[]).toEqual([first.sessionId as string]);
      // The counter upsert accumulates on the stored row, not on the excluded
      // one: an unqualified `count` would read as EXCLUDED.count in Postgres.
      expect(COUNTERS_SQL).toContain('DO UPDATE SET count = analytics_daily_counter.count + EXCLUDED.count');
    },
  );

  it('rolls back, reports the failure without the payload, and rethrows', async () => {
    const fake = createFakeSql({ upsert: new Error('deadlock detected') });
    const { sink, entries } = sinkOn(fake);

    await expect(sink.write(batch)).rejects.toThrow('deadlock detected');

    expect(sequence(fake)).toEqual(['BEGIN', 'S0', 'path-cap', 'S1', 'ROLLBACK']);
    expect(fake.releases()).toBe(1);
    expect(entries).toHaveLength(1);
    expect(entries[0]?.message).toBe('analytics.sink.failed');
    expect(entries[0]?.params).toEqual([{ error: 'deadlock detected', rows: 3 }]);
    // Nothing that reached the sink may reach a log line.
    const serialized = JSON.stringify(entries);
    expect(serialized).not.toContain(first.visitorId);
    expect(serialized).not.toContain(first.eventId);
    expect(serialized).not.toContain('/docs/getting-started');
  });

  it('reports one failure a minute, and counts every one of them', async () => {
    const fake = createFakeSql({ upsert: new Error('deadlock detected') });
    const { sink, entries } = sinkOn(fake);

    await expect(sink.write(batch)).rejects.toThrow('deadlock detected');
    await expect(sink.write(batch)).rejects.toThrow('deadlock detected');

    // An outage fails every flush the queue starts. One line per failed batch
    // would turn one incident into a log bill, so the line is rate limited and
    // the `dropped.sink_error` counter is what carries the volume.
    expect(entries).toHaveLength(1);
  });

  it("runs every batch in the tables' schema, resolved once", async () => {
    const fake = createFakeSql({ upsert: batch.map((row) => resultFor(row)) });
    let resolutions = 0;
    const sink = createSink({
      datasource: 'marketing',
      schema: () => {
        resolutions += 1;
        return 'marketing';
      },
      config: testConfig({ retentionMode: 'off' }),
      logger: fakeLogger().logger,
      now: () => NOW,
      connect: () => Promise.resolve(fake.sql),
    });

    await sink.write([first]);
    await sink.write([second]);

    const sets = fake.calls.filter((call) => call.query === SET_SCHEMA_SQL);
    expect(sets.map((call) => call.args)).toEqual([['marketing'], ['marketing']]);
    // Transaction-local: the datasource's own search path is back after COMMIT.
    expect(SET_SCHEMA_SQL).toContain(', true)');
    expect(resolutions).toBe(1);
  });

  it('writes to public when the host names no schema', async () => {
    const fake = createFakeSql({ upsert: batch.map((row) => resultFor(row)) });
    const sink = createSink({
      datasource: 'analytics',
      config: testConfig({ retentionMode: 'off' }),
      logger: fakeLogger().logger,
      now: () => NOW,
      connect: () => Promise.resolve(fake.sql),
    });

    await sink.write([first]);

    expect(fake.calls.filter((call) => call.query === SET_SCHEMA_SQL).map((call) => call.args)).toEqual([['public']]);
  });

  it('names the schema when the tables are missing from it', async () => {
    const missing = Object.assign(new Error('relation "analytics_event" does not exist'), { code: '42P01' });
    const fake = createFakeSql({ upsert: missing });
    const { logger, entries } = fakeLogger();
    const sink = createSink({
      datasource: 'marketing',
      schema: () => 'marketing',
      config: testConfig({ retentionMode: 'off' }),
      logger,
      now: () => NOW,
      connect: () => Promise.resolve(fake.sql),
    });

    await expect(sink.write(batch)).rejects.toThrow('does not exist');

    // The schema is fixed at the first migration: a line that names it is how
    // an operator learns the resolved schema moved away from the tables.
    expect(entries[0]?.params).toEqual([
      { error: 'relation "analytics_event" does not exist', rows: 3, schema: 'marketing' },
    ]);
  });

  it('reports again once the rate-limit window has passed', async () => {
    const fake = createFakeSql({ upsert: new Error('deadlock detected') });
    const { logger, entries } = fakeLogger();
    let at = NOW.getTime();
    const sink = createSink({
      datasource: 'analytics',
      schema: () => 'public',
      config: testConfig({ retentionMode: 'off' }),
      logger,
      now: () => new Date(at),
      connect: () => Promise.resolve(fake.sql),
    });

    await expect(sink.write(batch)).rejects.toThrow('deadlock detected');
    at += SINK_FAILURE_LOG_INTERVAL_MS;
    await expect(sink.write(batch)).rejects.toThrow('deadlock detected');

    // Rate limited, never silenced: an outage that outlives the window is
    // reported again, so a log reader sees it is still going.
    expect(entries).toHaveLength(2);
  });

  it('skips a statement the batch produced no rows for', async () => {
    const form = pageViewRow({ eventId: '01920000-0000-7000-8000-000000000031', sessionId: null });
    const fake = createFakeSql({ upsert: [resultFor(form, false)] });
    const { sink } = sinkOn(fake);

    const counts = await sink.write([form]);

    expect(sequence(fake)).toEqual(['BEGIN', 'S0', 'path-cap', 'S1', 'COMMIT']);
    expect(counts).toEqual({ inserted: 0, enriched: 1 });
  });

  it('never opens a transaction for an empty batch', async () => {
    const fake = createFakeSql();
    const { sink } = sinkOn(fake);

    expect(await sink.write([])).toEqual({ inserted: 0, enriched: 0 });
    expect(fake.calls).toHaveLength(0);
  });

  it('drops a batch this instance already wrote before reaching the database', async () => {
    const fake = createFakeSql({ upsert: [resultFor(first)] });
    const { sink } = sinkOn(fake);

    await sink.write([first]);
    const before = fake.calls.length;
    const counts = await sink.write([first]);

    expect(counts).toEqual({ inserted: 0, enriched: 0 });
    expect(fake.calls).toHaveLength(before);
  });

  it('skips the path-cardinality probe for a batch with no page view', async () => {
    const fake = createFakeSql({ upsert: [resultFor(third)] });
    const { sink } = sinkOn(fake);

    await sink.write([third]);

    expect(sequence(fake)).toEqual(['BEGIN', 'S0', 'S1', 'S2', 'S3', 'COMMIT']);
  });

  it('probes and applies path overflow for every page-view day in the batch', async () => {
    const older = pageViewRow({ eventId: '01920000-0000-7000-8000-000000000041', day: '2026-09-01' });
    const rows = [older, first, second];
    const fake = createFakeSql({
      upsert: rows.map((row) => resultFor(row)),
      pathCount: { '2026-09-01': 2000, '2026-09-02': 10 },
    });
    const { sink } = sinkOn(fake);

    await sink.write(rows);

    expect(fake.calls.filter((call) => call.query.startsWith('SELECT count(*)')).map((call) => call.args)).toEqual([
      ['2026-09-01', 'path'],
      ['2026-09-02', 'path'],
    ]);
    const counters = fake.callStartingWith('INSERT INTO analytics_daily_counter').args as string[][];
    const paths = (counters[1] ?? []).flatMap((dimension, index) =>
      dimension === 'path' ? [{ day: counters[0]?.[index], key: counters[2]?.[index] }] : [],
    );
    expect(paths).toEqual([
      { day: older.day, key: OVERFLOW_KEY },
      { day: first.day, key: first.path },
    ]);
  });
});

describe('the fold statements', () => {
  it('bind their arrays positionally to the columns they unnest', () => {
    expect(VISITORS_SQL).toContain('unnest($1::date[], $2::text[])');
    expect(SESSION_ENGAGEMENT_SQL).toContain('unnest($1::date[], $2::uuid[])');
    // S4 carries the aggregates `planFold` computed, not one row per event:
    // `ON CONFLICT` cannot update one row twice in a single statement.
    expect(SESSIONS_SQL).toContain('$6::integer[]');
    expect(SESSIONS_SQL).toContain('page_views  = analytics_daily_session.page_views + EXCLUDED.page_views');
  });
});

describe('createNoopSink', () => {
  it('counts the batch and never reaches a database', async () => {
    const sink = createNoopSink();

    expect(await sink.write([pageViewRow(), actionRow()])).toEqual({ inserted: 2, enriched: 0 });
    expect(await sink.write([])).toEqual({ inserted: 0, enriched: 0 });
  });
});

describe('the dedup cache and the server-written row', () => {
  const PV = '01920000-0000-7000-8000-0000000000f1';

  // The tracker re-sends the server's page view under the server's own event
  // id, carrying the session, viewport, language, referrer and campaign the
  // server could not know — and that first re-send carries no engagement.
  // Remembering the server-written id would make the cache drop exactly that
  // row before it reached the upsert whose whole purpose is to merge it, while
  // the sender is still answered 202 and discards the batch it acked.
  it('lets the client enrich a server-written page view that carries no engagement', async () => {
    const serverRow = pageViewRow({ eventId: PV, source: 'server', sessionId: null, engagementMs: 0 });
    const enrichment = pageViewRow({
      eventId: PV,
      source: 'client',
      sessionId: '01920000-0000-7000-8000-0000000000f2',
      engagementMs: 0,
    });

    const fake = createFakeSql({ upsert: [resultFor(serverRow)] });
    const { sink } = sinkOn(fake);
    await sink.write([serverRow]);

    fake.calls.length = 0;
    await sink.write([enrichment]);

    expect(sequence(fake)).toContain('S1');
  });

  it('still short-circuits a replayed client batch', async () => {
    const clientRow = pageViewRow({ eventId: PV, source: 'client', engagementMs: 0 });

    const fake = createFakeSql({ upsert: [resultFor(clientRow)] });
    const { sink } = sinkOn(fake);
    await sink.write([clientRow]);

    fake.calls.length = 0;
    await sink.write([clientRow]);

    expect(sequence(fake)).not.toContain('S1');
  });
});
