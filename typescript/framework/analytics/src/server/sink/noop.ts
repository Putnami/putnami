import { incCounter } from '@putnami/application';
import type { EventRow } from './fold';
import type { Sink, SinkCounts } from './sink';

/**
 * Builds the sink used when the application composes no `SqlPlugin`.
 *
 * It counts and returns. Nothing here reaches `database()`, because an
 * application that never asked for a database must not be given a connection
 * error in exchange for adding an analytics plugin — the measurement is
 * optional, the application is not.
 *
 * @returns A sink that records acceptance and stores nothing.
 */
export function createNoopSink(): Sink {
  return {
    write(rows: EventRow[]): Promise<SinkCounts> {
      incCounter('analytics.ingest.accepted', rows.length);
      return Promise.resolve({ inserted: rows.length, enriched: 0 });
    },
  };
}
