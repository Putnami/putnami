import { incCounter, observeHistogram, setGauge } from '@putnami/application';
import type { Logger } from '@putnami/runtime';
import type { EventRow } from './fold';
import type { Sink } from './sink';

/**
 * How long one flush loop may keep writing before it yields.
 *
 * The loop is what turns a backlog into a sequence of batches, and it runs
 * inside somebody else's request. A budget is what keeps it from spending the
 * whole CPU window of a request that only wanted to render a page.
 */
export const FLUSH_BUDGET_MS = 1000;

/**
 * The write side of the queue, seen by the request paths.
 *
 * `enqueue` and `kick` return without waiting for anything: every caller of
 * the two is on a response path, and analytics must never make a visitor wait
 * for Postgres. `kick` hands back the flush it started so an *elected* request
 * can choose to wait for it, under a cap it owns — see
 * {@link awaitFlush}.
 */
export interface WriteQueue {
  /** Accepts rows for later writing; drops the oldest when over capacity. */
  enqueue(rows: EventRow[]): void;
  /**
   * Starts one flush when one is due and none is in flight; never awaits it.
   *
   * @returns The started flush, or undefined when nothing was started.
   */
  kick(): Promise<void> | undefined;
  /** Flushes until the queue is empty or the deadline passes. Never throws. */
  drain(deadlineMs: number): Promise<void>;
  /** How many rows are waiting. */
  size(): number;
}

/** What {@link createWriteQueue} needs from its host. */
export interface WriteQueueDeps {
  /** Where a flushed batch goes. */
  sink: Sink;
  /** The largest number of rows held in memory before the oldest are dropped. */
  capacity: number;
  /** The largest number of rows one `sink.write` carries. */
  batch: number;
  /** How long after the last flush a non-full queue becomes due again. */
  flushIntervalMs: number;
  /** The clock, injected so every time-dependent proof is deterministic. */
  now: () => Date;
  /** Where a dropped flush is reported. */
  logger: Logger;
}

/**
 * Builds the bounded, asynchronous write queue every server row goes through.
 *
 * Three rules make it safe to put in front of a response path:
 *
 * - **Bounded memory.** Past `capacity` the oldest rows are dropped and
 *   counted. A burst costs a fixed number of rows, never the instance.
 * - **One flush at a time.** `kick` coalesces, so a slow database can never
 *   accumulate connections or open transactions — the pressure stays here, in
 *   memory that is already capped, instead of moving into Postgres.
 * - **No retry.** Rows a failed write carried are dropped. Re-enqueuing them
 *   would build a poison loop that spends every later flush on the batch that
 *   already failed, and the sink has already counted them as
 *   `analytics.ingest.dropped.sink_error`.
 *
 * Losing events under pressure is the accepted trade: a measurement that can
 * delay a page is worse than a measurement with a gap.
 *
 * @param deps - The sink, the bounds, the clock, and the logger.
 * @returns The queue.
 */
export function createWriteQueue(deps: WriteQueueDeps): WriteQueue {
  const capacity = Math.max(1, deps.capacity);
  const batchSize = Math.max(1, deps.batch);
  const pending: EventRow[] = [];
  let inFlight: Promise<void> | undefined;
  // The epoch, so the very first kick is always due: an instance that has just
  // started has no flush to be measured against.
  let lastFlushAt = 0;

  /** Writes one batch, and drops it rather than retrying it when it fails. */
  async function writeOnce(): Promise<void> {
    const rows = pending.splice(0, batchSize);
    if (rows.length === 0) {
      return;
    }
    const startedAt = deps.now().getTime();
    try {
      await deps.sink.write(rows);
    } catch {
      // Counted and logged by the sink. Re-enqueuing would be a poison loop.
      deps.logger.debug(`analytics: dropped ${rows.length} queued row(s) after a sink failure`);
    }
    observeHistogram('analytics.queue.flush_ms', Math.max(0, deps.now().getTime() - startedAt));
  }

  /** Writes batch after batch until the queue is empty or the budget is spent. */
  async function flush(): Promise<void> {
    const startedAt = deps.now().getTime();
    do {
      // biome-ignore lint/performance/noAwaitInLoops: one flush at a time is
      // the point — concurrent batches would be concurrent transactions.
      await writeOnce();
    } while (pending.length > 0 && deps.now().getTime() - startedAt < FLUSH_BUDGET_MS);
    lastFlushAt = deps.now().getTime();
  }

  /** Starts the one flush, and clears the slot whatever it did. */
  function start(): Promise<void> {
    const running = flush()
      .catch(() => undefined)
      .then(() => {
        inFlight = undefined;
      });
    inFlight = running;
    return running;
  }

  return {
    enqueue(rows: EventRow[]): void {
      if (rows.length === 0) {
        return;
      }
      pending.push(...rows);
      const overflow = pending.length - capacity;
      if (overflow > 0) {
        // The oldest go: a queue that is behind is most useful holding what
        // just happened, and the rows at the front are the ones a reader is
        // least likely to miss.
        pending.splice(0, overflow);
        incCounter('analytics.ingest.dropped.overflow', overflow);
      }
    },

    kick(): Promise<void> | undefined {
      setGauge('analytics.queue.size', pending.length);
      if (inFlight || pending.length === 0) {
        return undefined;
      }
      const due = pending.length >= batchSize || deps.now().getTime() - lastFlushAt >= deps.flushIntervalMs;
      return due ? start() : undefined;
    },

    async drain(deadlineMs: number): Promise<void> {
      // The wall clock, not the injected one: this deadline exists to keep
      // `stop()` inside the SIGTERM grace period, and a clock a caller froze
      // would make it unbounded — a hanging stop is worse than a lost row.
      const deadline = Date.now() + Math.max(0, deadlineMs);
      try {
        while (Date.now() < deadline) {
          if (inFlight) {
            await until(inFlight, deadline);
          } else if (pending.length === 0) {
            return;
          } else {
            await until(start(), deadline);
          }
        }
      } catch {
        // A drain reports nothing: it is called from `stop()` and from an
        // operator's flush, and neither has anything to do with a failure the
        // sink has already counted.
      }
    },

    size(): number {
      return pending.length;
    },
  };
}

/**
 * Waits for the flush this request started, for at most `capMs`.
 *
 * This is the whole cost model of the elected request: one request every
 * `flushIntervalMs` pays for a batch, and it pays at most `flushWaitMs` of it.
 * A flush that outruns the cap is not cancelled — the response goes out and
 * the flush keeps whatever CPU the platform gives it, which on request-based
 * CPU may be none until the next request elects a flush again. That drift is
 * accepted; a page waiting on Postgres is not.
 *
 * @param flushing - What `kick()` returned, or undefined when it started none.
 * @param capMs - The longest this request may wait.
 * @returns A promise that settles at the flush or at the cap, and never rejects.
 */
export async function awaitFlush(flushing: Promise<void> | undefined, capMs: number): Promise<void> {
  if (!flushing) {
    return;
  }
  try {
    await until(flushing, Date.now() + Math.max(0, capMs));
  } catch {
    // A flush failure is the sink's to count and log, never the response's.
  }
}

/** Waits for a flush, or for the deadline, whichever comes first. */
async function until(flushing: Promise<void>, deadline: number): Promise<void> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const expiry = new Promise<void>((resolve) => {
    timer = setTimeout(resolve, Math.max(0, deadline - Date.now()));
    // Never keep a process alive for a deadline that only bounds a wait.
    if (timer && typeof timer === 'object' && 'unref' in timer) {
      (timer as { unref: () => void }).unref();
    }
  });
  try {
    await Promise.race([flushing, expiry]);
  } finally {
    clearTimeout(timer);
  }
}
