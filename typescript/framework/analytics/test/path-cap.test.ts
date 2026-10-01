import { describe, expect, it } from 'bun:test';
import { NONE_KEY, OVERFLOW_KEY, planFold } from '../src/server/sink/fold';
import { createPathCap, PATH_CAP_TTL_MS, PATH_COUNT_SQL } from '../src/server/sink/path-cap';
import { createFakeSql } from './utils/fake-sql';
import { pageViewRow, resultFor, testConfig } from './utils/fixtures';

const START = new Date('2026-09-02T10:30:00.000Z').getTime();
const NO_OVERFLOW = new Set<string>();

describe('createPathCap', () => {
  it('reads the day cardinality at most once a minute', async () => {
    let at = START;
    const fake = createFakeSql({ pathCount: 10 });
    const cap = createPathCap(testConfig(), () => new Date(at));

    expect(await cap.refresh(fake.sql, '2026-09-02')).toBe(false);
    at = START + PATH_CAP_TTL_MS - 1;
    expect(await cap.refresh(fake.sql, '2026-09-02')).toBe(false);

    expect(fake.calls).toHaveLength(1);
    expect(fake.calls[0]).toEqual({ query: PATH_COUNT_SQL, args: ['2026-09-02', 'path'] });
  });

  it('reports overflow once the day reaches the configured key budget', async () => {
    const fake = createFakeSql({ pathCount: 2000 });
    const cap = createPathCap(testConfig({ maxPathKeysPerDay: 2000 }), () => new Date(START));

    expect(await cap.refresh(fake.sql, '2026-09-02')).toBe(true);
    expect(fake.calls).toHaveLength(1);
  });

  it('stays under budget while the day still has room', async () => {
    const fake = createFakeSql({ pathCount: 1999 });
    const cap = createPathCap(testConfig({ maxPathKeysPerDay: 2000 }), () => new Date(START));

    expect(await cap.refresh(fake.sql, '2026-09-02')).toBe(false);
  });

  it('reports no overflow for a day that has no path counter yet', async () => {
    const fake = createFakeSql({ pathCount: 0 });
    const cap = createPathCap(testConfig({ maxPathKeysPerDay: 1 }), () => new Date(START));

    expect(await cap.refresh(fake.sql, '2026-09-02')).toBe(false);
  });

  it('caches each UTC day independently across rollover and late batches', async () => {
    const fake = createFakeSql({ pathCount: { '2026-09-02': 2000, '2026-09-03': 10 } });
    const cap = createPathCap(testConfig({ maxPathKeysPerDay: 2000 }), () => new Date(START));

    expect(await cap.refresh(fake.sql, '2026-09-02')).toBe(true);
    expect(await cap.refresh(fake.sql, '2026-09-03')).toBe(false);
    expect(await cap.refresh(fake.sql, '2026-09-02')).toBe(true);

    expect(fake.calls.map((call) => call.args)).toEqual([
      ['2026-09-02', 'path'],
      ['2026-09-03', 'path'],
    ]);
  });

  it('folds new and existing path keys alike once it overflows', () => {
    const row = pageViewRow();

    const capped = planFold([row], [resultFor(row)], new Set([row.day]));
    const uncapped = planFold([row], [resultFor(row)], NO_OVERFLOW);

    expect(capped.counters.find((counter) => counter.dimension === 'path')?.key).toBe(OVERFLOW_KEY);
    expect(uncapped.counters.find((counter) => counter.dimension === 'path')?.key).toBe('/docs/getting-started');
    expect(OVERFLOW_KEY).not.toBe(NONE_KEY);
  });

  it('applies overflow by each row day in a mixed-day batch', () => {
    const older = pageViewRow({ eventId: '01920000-0000-7000-8000-000000000041', day: '2026-09-01' });
    const current = pageViewRow({ eventId: '01920000-0000-7000-8000-000000000042', day: '2026-09-02' });

    const plan = planFold([older, current], [resultFor(older), resultFor(current)], new Set([older.day]));
    const paths = plan.counters.filter((counter) => counter.dimension === 'path');

    expect(paths).toEqual([
      { day: older.day, dimension: 'path', key: OVERFLOW_KEY, count: 1 },
      { day: current.day, dimension: 'path', key: current.path, count: 1 },
    ]);
  });
});
