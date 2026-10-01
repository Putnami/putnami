import type { EventRow } from '../../src/server/sink/fold';
import type { Sink, SinkCounts } from '../../src/server/sink/sink';

/** A sink that records what it was asked to write, and writes nothing. */
export interface FakeSink {
  /** The stand-in handed to the runtime. */
  sink: Sink;
  /** Every row, in the order it arrived. */
  rows: EventRow[];
  /** One entry per `write()` call, so a test can count round trips. */
  batches: EventRow[][];
  /** Rows of one event name. */
  named: (name: string) => EventRow[];
  /** Stops settling `write`: every call from now on stays pending. */
  hold: () => void;
  /** Settles every held write and answers immediately again. */
  release: () => void;
  /** How many `write` calls have not settled. */
  pending: () => number;
  /** Adds a real delay to every write, so a proof can order two instants. */
  delay: (ms: number) => void;
  /** When the last write settled, or 0. */
  settledAt: () => number;
}

/**
 * Builds the sink every proof in this slice writes through.
 *
 * No test in this package opens a database: a library never gets one inside
 * the gate, and a measurement layer that needed live Postgres to prove it does
 * not store an IP address would be proving nothing a reviewer could re-run.
 *
 * `hold()` is what makes the asynchronous write path provable: a write that
 * never settles is a database that never answers, and the proofs assert what
 * the response did while it was still pending.
 *
 * @param failure - When set, every write rejects with it.
 * @returns The fake sink and what it observed.
 */
export function createFakeSink(failure?: Error): FakeSink {
  const rows: EventRow[] = [];
  const batches: EventRow[][] = [];
  const waiting: (() => void)[] = [];
  let held = false;
  let delayMs = 0;
  let settledAt = 0;

  const settle = (batch: EventRow[], resolve: (counts: SinkCounts) => void): void => {
    rows.push(...batch);
    settledAt = Date.now();
    resolve({ inserted: batch.length, enriched: 0 });
  };

  const sink: Sink = {
    write(batch: EventRow[]): Promise<SinkCounts> {
      batches.push([...batch]);
      if (failure) {
        return Promise.reject(failure);
      }
      if (held) {
        return new Promise<SinkCounts>((resolve) => {
          waiting.push(() => settle(batch, resolve));
        });
      }
      if (delayMs > 0) {
        return new Promise<SinkCounts>((resolve) => {
          setTimeout(() => settle(batch, resolve), delayMs);
        });
      }
      return new Promise<SinkCounts>((resolve) => settle(batch, resolve));
    },
  };

  return {
    sink,
    rows,
    batches,
    named: (name: string) => rows.filter((row) => row.name === name),
    hold: () => {
      held = true;
    },
    release: () => {
      held = false;
      for (const resume of waiting.splice(0)) {
        resume();
      }
    },
    pending: () => waiting.length,
    delay: (ms: number) => {
      delayMs = ms;
    },
    settledAt: () => settledAt,
  };
}
