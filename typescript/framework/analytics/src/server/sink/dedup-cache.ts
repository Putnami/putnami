import type { EventRow } from './fold';

/** How many event ids one instance remembers. */
export const DEDUP_CAPACITY = 10_000;

/** The per-instance recently-written event ids. */
export interface DedupCache {
  /** Drops rows this instance already wrote, engagement re-sends excepted. */
  filter(rows: EventRow[]): EventRow[];
  /** Records the ids of a committed batch, evicting the oldest past capacity. */
  remember(ids: Iterable<string>): void;
}

/**
 * Builds the per-instance dedup cache (body §D.16).
 *
 * It is an optimization, never the truth: the database's `ON CONFLICT
 * (event_id)` is what makes a retry idempotent, and this cache only spares it
 * the round trip. That is why a miss is harmless and why the filter lets any
 * row carrying engagement through — an engagement re-send repeats the event id
 * on purpose, and dropping it here would lose the only value it carries.
 *
 * @param capacity - How many ids to remember.
 * @returns The cache.
 */
export function createDedupCache(capacity = DEDUP_CAPACITY): DedupCache {
  const seen = new Map<string, true>();
  return {
    filter(rows: EventRow[]): EventRow[] {
      return rows.filter((row) => row.engagementMs > 0 || !seen.has(row.eventId));
    },
    remember(ids: Iterable<string>): void {
      for (const id of ids) {
        // Re-inserting after a delete moves the id to the end of the insertion
        // order, which is what makes the eviction below least-recently-written.
        if (seen.has(id)) {
          seen.delete(id);
        }
        seen.set(id, true);
        if (seen.size > capacity) {
          seen.delete(seen.keys().next().value as string);
        }
      }
    },
  };
}
