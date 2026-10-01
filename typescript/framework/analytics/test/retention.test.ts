import { describe, expect, it } from 'bun:test';
import type { Logger } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { createRetention, SWEEP_INTERVAL_MS } from '../src/server/sink/retention';
import { createSink, SWEEP_SQL } from '../src/server/sink/sink';
import { createFakeSql } from './utils/fake-sql';
import { pageViewRow, resultFor, testConfig } from './utils/fixtures';

const FEATURE = 'typescript/web-analytics-collection';
const REQUIREMENT = 'raw-events-expire';
const BOUNDED = 'sweep-runs-at-most-every-ten-minutes-and-is-bounded';

const START = new Date('2026-09-02T10:30:00.000Z').getTime();

describe('createRetention', () => {
  specTest(
    'sweeps at most once every ten minutes, a thousand rows at a time',
    { feature: FEATURE, requirement: REQUIREMENT, check: BOUNDED },
    async () => {
      let at = START;
      const retention = createRetention(testConfig(), () => new Date(at));

      // The first ingest after boot always sweeps, so a restart converges
      // instead of postponing.
      expect(retention.isDue()).toBe(true);
      at = START + SWEEP_INTERVAL_MS - 1;
      expect(retention.isDue()).toBe(false);
      at = START + SWEEP_INTERVAL_MS;
      expect(retention.isDue()).toBe(true);

      const row = pageViewRow();
      const fake = createFakeSql({ upsert: [resultFor(row)] });
      const sink = createSink({
        datasource: 'analytics',
        schema: () => 'public',
        config: testConfig(),
        logger: { error: () => {} } as unknown as Logger,
        now: () => new Date(START),
        connect: () => Promise.resolve(fake.sql),
      });

      await sink.write([row]);

      const deletes = fake.calls.filter((call) => call.query.startsWith('DELETE FROM'));
      expect(deletes).toHaveLength(4);
      // Bounded: an ingest never pays for a backlog, and the sweep runs inside
      // the batch's own transaction.
      expect(deletes.every((call) => call.query.includes('LIMIT 1000'))).toBe(true);
      expect(deletes.map((call) => call.args)).toEqual([
        ['2026-06-04'],
        ['2024-08-03'],
        ['2024-08-03'],
        ['2024-08-03'],
      ]);
      const verbs = fake.calls.map((call) => call.query.split(/\s+/)[0]);
      expect(verbs.indexOf('DELETE')).toBeLessThan(verbs.indexOf('COMMIT'));
    },
  );

  it('never sweeps in off or pg_cron mode', () => {
    for (const retentionMode of ['off', 'pg_cron'] as const) {
      const retention = createRetention(testConfig({ retentionMode }), () => new Date(START));

      expect(retention.isDue()).toBe(false);
      expect(retention.isDue()).toBe(false);
    }
  });

  it('cuts raw rows and aggregates at their own configured age', () => {
    const retention = createRetention(
      testConfig({ retentionRawDays: 30, retentionAggregateDays: 90 }),
      () => new Date(START),
    );

    expect(retention.cutoffs()).toEqual({ raw: '2026-08-03', aggregate: '2026-06-04' });
  });

  it('deletes the raw table on the raw cutoff and the three aggregates on the other', () => {
    expect(SWEEP_SQL).toHaveLength(4);
    expect(SWEEP_SQL[0]).toContain('DELETE FROM analytics_event');
    expect(SWEEP_SQL[1]).toContain('DELETE FROM analytics_daily_counter');
    expect(SWEEP_SQL[2]).toContain('DELETE FROM analytics_daily_visitor');
    expect(SWEEP_SQL[3]).toContain('DELETE FROM analytics_daily_session');
    // Each statement is prepared on its own, so each binds its cutoff as $1.
    expect(SWEEP_SQL.every((statement) => statement.includes('$1::date') && !statement.includes('$2'))).toBe(true);
  });
});
