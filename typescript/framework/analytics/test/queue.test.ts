import { afterEach, describe, expect, it } from 'bun:test';
import { setCollector, TelemetryCollector } from '@putnami/application';
import { useLogger } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import type { EventRow } from '../src/server/sink/fold';
import { awaitFlush, createWriteQueue, FLUSH_BUDGET_MS, type WriteQueue } from '../src/server/sink/queue';
import { createFakeSink, type FakeSink } from './utils/fake-sink';
import { pageViewRow } from './utils/fixtures';

const FEATURE = 'typescript/web-analytics-collection';
const ASYNC = 'a-slow-database-never-delays-a-response';
const NOW = new Date('2026-09-02T10:00:00.000Z');

let collector: TelemetryCollector | undefined;

/** One row per index, so an order assertion reads as a sequence. */
function row(index: number): EventRow {
  return pageViewRow({ eventId: `01920000-0000-7000-8000-0000000000${index.toString().padStart(2, '0')}` });
}

/** A clock that jumps `stepMs` at every read, so the loop budget is reachable. */
function advancingClock(stepMs: number): () => Date {
  let elapsed = 0;
  return () => {
    elapsed += stepMs;
    return new Date(NOW.getTime() + elapsed);
  };
}

/** A queue over a fake sink, with the bounds the test cares about. */
function queueOn(
  options: { capacity?: number; batch?: number; flushIntervalMs?: number; now?: () => Date; failure?: Error } = {},
): { queue: WriteQueue; sink: FakeSink } {
  const sink = createFakeSink(options.failure);
  const queue = createWriteQueue({
    sink: sink.sink,
    capacity: options.capacity ?? 100,
    batch: options.batch ?? 10,
    flushIntervalMs: options.flushIntervalMs ?? 5000,
    now: options.now ?? (() => NOW),
    logger: useLogger('@putnami/analytics'),
  });
  return { queue, sink };
}

/** Every counter recorded since the collector was installed. */
function counters(): Record<string, number> {
  const totals: Record<string, number> = {};
  for (const bucket of collector?.drainAll() ?? []) {
    for (const [name, value] of Object.entries(bucket.counters)) {
      totals[name] = (totals[name] ?? 0) + value;
    }
  }
  return totals;
}

afterEach(() => {
  if (collector) {
    setCollector(undefined);
    collector = undefined;
  }
});

describe('the write queue', () => {
  specTest(
    'drops the oldest rows when it is full, and counts them',
    { feature: FEATURE, requirement: ASYNC, check: 'a-full-queue-drops-the-oldest-and-counts-it' },
    async () => {
      collector = new TelemetryCollector();
      setCollector(collector);
      const { queue, sink } = queueOn({ capacity: 3, batch: 3 });

      queue.enqueue([row(1), row(2)]);
      queue.enqueue([row(3), row(4), row(5)]);

      // Bounded memory is the whole point: a burst costs a fixed number of
      // rows, never the instance. The oldest go, because a reader is least
      // likely to miss the front of a backlog.
      expect(queue.size()).toBe(3);
      expect(counters()['analytics.ingest.dropped.overflow']).toBe(2);

      await queue.drain(1000);

      expect(sink.rows.map((stored) => stored.eventId)).toEqual([row(3).eventId, row(4).eventId, row(5).eventId]);
    },
  );

  it('never counts an overflow it did not have', () => {
    collector = new TelemetryCollector();
    setCollector(collector);
    const { queue } = queueOn({ capacity: 3 });

    queue.enqueue([row(1), row(2), row(3)]);
    queue.enqueue([]);

    expect(queue.size()).toBe(3);
    expect(counters()['analytics.ingest.dropped.overflow']).toBeUndefined();
  });

  it('coalesces: a second kick while one flush is in flight starts nothing', async () => {
    const { queue, sink } = queueOn({ batch: 1 });
    sink.hold();
    queue.enqueue([row(1), row(2)]);

    const first = queue.kick();
    const second = queue.kick();

    // One flush at a time, so a slow database can never accumulate
    // connections or open transactions.
    expect(first).toBeDefined();
    expect(second).toBeUndefined();
    expect(sink.batches).toHaveLength(1);
    expect(sink.pending()).toBe(1);
    sink.release();
    await first;
  });

  it('stops a flush loop at its budget instead of spending the whole request', async () => {
    // Every clock read jumps 600 ms, so the second turn of the loop is already
    // past the budget.
    const { queue, sink } = queueOn({ batch: 1, now: advancingClock(600) });
    queue.enqueue([row(1), row(2), row(3)]);

    await queue.kick();

    expect(FLUSH_BUDGET_MS).toBe(1000);
    expect(sink.batches).toHaveLength(1);
    expect(queue.size()).toBe(2);
  });

  it('writes batch after batch while it is under budget', async () => {
    const { queue, sink } = queueOn({ batch: 1 });
    queue.enqueue([row(1), row(2), row(3)]);

    await queue.kick();

    expect(sink.batches).toHaveLength(3);
    expect(queue.size()).toBe(0);
  });

  it('drops the rows of a failed write instead of retrying them', async () => {
    const { queue, sink } = queueOn({ batch: 1, failure: new Error('connection refused') });
    queue.enqueue([row(1), row(2)]);

    await queue.kick();

    // No poison loop: a batch that failed is gone, counted by the sink as
    // `dropped.sink_error`, and never spends a later flush.
    expect(sink.batches).toHaveLength(2);
    expect(sink.rows).toHaveLength(0);
    expect(queue.size()).toBe(0);
    await queue.drain(200);
    expect(sink.batches).toHaveLength(2);
  });

  it('starts nothing before the interval has elapsed', async () => {
    const { queue, sink } = queueOn({ batch: 10, flushIntervalMs: 5000 });
    queue.enqueue([row(1)]);
    await queue.kick();

    queue.enqueue([row(2)]);
    const second = queue.kick();

    // The clock is frozen, so no interval elapsed between the two: a request
    // arriving right after a flush pays nothing.
    expect(second).toBeUndefined();
    expect(sink.batches).toHaveLength(1);
  });

  it('is due as soon as a full batch is waiting', async () => {
    const { queue, sink } = queueOn({ batch: 2, flushIntervalMs: 5000 });
    queue.enqueue([row(1)]);
    await queue.kick();

    queue.enqueue([row(2), row(3)]);
    await queue.kick();

    expect(sink.batches).toHaveLength(2);
  });

  it('returns from drain at its deadline when the sink never settles', async () => {
    const { queue, sink } = queueOn({ batch: 1 });
    sink.hold();
    queue.enqueue([row(1), row(2)]);

    const startedAt = Date.now();
    await queue.drain(120);
    const elapsed = Date.now() - startedAt;

    // A hanging `stop()` is worse than a lost row: the deadline is honoured
    // whatever the database is doing, and the queue still holds what it holds.
    expect(elapsed).toBeGreaterThanOrEqual(100);
    expect(elapsed).toBeLessThan(2000);
    expect(queue.size()).toBe(1);
    expect(sink.rows).toHaveLength(0);
    sink.release();
  });

  it('returns from drain immediately when there is nothing to write', async () => {
    const { queue, sink } = queueOn();

    const startedAt = Date.now();
    await queue.drain(5000);

    expect(Date.now() - startedAt).toBeLessThan(500);
    expect(sink.batches).toHaveLength(0);
  });

  it('drains everything a burst left behind', async () => {
    const { queue, sink } = queueOn({ batch: 2, capacity: 100 });
    queue.enqueue([row(1), row(2), row(3), row(4), row(5)]);

    await queue.drain(2000);

    expect(sink.rows).toHaveLength(5);
    expect(queue.size()).toBe(0);
  });
});

describe('awaitFlush', () => {
  it('returns at the cap when the flush never settles', async () => {
    const never = new Promise<void>(() => undefined);

    const startedAt = Date.now();
    await awaitFlush(never, 80);

    expect(Date.now() - startedAt).toBeGreaterThanOrEqual(60);
    expect(Date.now() - startedAt).toBeLessThan(2000);
  });

  it('returns immediately when this request started no flush', async () => {
    const startedAt = Date.now();

    await awaitFlush(undefined, 5000);

    expect(Date.now() - startedAt).toBeLessThan(500);
  });
});
