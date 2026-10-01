import type { SqlClient } from '@putnami/database';
import type { AnalyticsConfigValues } from '../analytics.config';

/** The daily `path` cardinality probe (body §D.10). */
export const PATH_COUNT_SQL =
  'SELECT count(*)::int AS n FROM analytics_daily_counter WHERE day = $1 AND dimension = $2';

/** How long one cardinality answer is reused, per instance. */
export const PATH_CAP_TTL_MS = 60_000;

/** The `path` cardinality valve of one instance. */
export interface PathCap {
  /** Whether new `path` keys must fold under `__overflow__` for `day`. */
  refresh(tx: SqlClient, day: string): Promise<boolean>;
}

/**
 * Builds the per-instance `path` cardinality cap.
 *
 * `path` is the one dimension a visitor controls: every other one is a closed
 * vocabulary or bounded by the wire contract, so only this one can turn a
 * crawler into a million counter rows. The probe costs one indexed count per
 * UTC day seen per minute per instance, and the answer is a valve — once a day
 * is over budget every path folds under one key.
 *
 * @param config - The resolved analytics configuration.
 * @param now - The clock, injected so the memoization window is testable.
 * @returns The cap.
 */
export function createPathCap(config: AnalyticsConfigValues, now: () => Date): PathCap {
  const byDay = new Map<string, { checkedAt: number; overflow: boolean }>();
  return {
    async refresh(tx: SqlClient, day: string): Promise<boolean> {
      const checkedAt = now().getTime();
      for (const [cachedDay, cached] of byDay) {
        if (checkedAt - cached.checkedAt >= PATH_CAP_TTL_MS) {
          byDay.delete(cachedDay);
        }
      }
      const cached = byDay.get(day);
      if (cached !== undefined && checkedAt - cached.checkedAt < PATH_CAP_TTL_MS) {
        return cached.overflow;
      }
      const rows = await tx.unsafe<{ n: number }[]>(PATH_COUNT_SQL, [day, 'path']);
      const overflow = (rows[0]?.n ?? 0) >= config.maxPathKeysPerDay;
      byDay.set(day, { checkedAt, overflow });
      return overflow;
    },
  };
}
