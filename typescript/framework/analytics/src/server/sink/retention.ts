import type { AnalyticsConfigValues } from '../analytics.config';
import { utcDay } from '../identity/visitor-hash';
import { MAX_RETENTION_DAYS, MIN_RETENTION_DAYS } from './migrations';

/** The shortest interval between two opportunistic sweeps, per instance. */
export const SWEEP_INTERVAL_MS = 600_000;

const DAY_MS = 86_400_000;

/**
 * Holds a retention window inside the same 1..3650 days the pg_cron migration
 * validates before it interpolates the value into SQL.
 *
 * Sweep mode reads the config directly, so without this a 0 or a negative count
 * would put the cutoff at or after today and every pass would delete live rows,
 * 1000 per table, silently — while the identical value under pg_cron refuses to
 * boot. A DELETE deserves the same floor whichever mode issues it.
 *
 * @param value - The configured number of days.
 * @returns The value, held inside the accepted window.
 */
function clampDays(value: number): number {
  if (!Number.isFinite(value)) return MIN_RETENTION_DAYS;
  return Math.min(MAX_RETENTION_DAYS, Math.max(MIN_RETENTION_DAYS, Math.trunc(value)));
}

/** The opportunistic sweep schedule of one instance. */
export interface Retention {
  /** Whether this ingest should carry the sweep, marking it as taken. */
  isDue(): boolean;
  /** The two UTC cutoff days, as `YYYY-MM-DD`. */
  cutoffs(): { raw: string; aggregate: string };
}

/**
 * Builds the per-instance sweep schedule (body §D.11).
 *
 * `isDue` marks the sweep as taken as it answers, so two concurrent batches
 * cannot both carry it. The first ingest after boot always sweeps, which is
 * what makes a restart converge instead of postponing.
 *
 * This is best effort by construction: a retention job that only runs while
 * traffic is arriving is not a retention guarantee. `retentionMode: 'pg_cron'`
 * is the guarantee, and `doc/privacy.md` says so.
 *
 * @param config - The resolved analytics configuration.
 * @param now - The clock, injected so the interval is testable.
 * @returns The schedule.
 */
export function createRetention(config: AnalyticsConfigValues, now: () => Date): Retention {
  let lastSweepAt: number | undefined;
  return {
    isDue(): boolean {
      if (config.retentionMode !== 'sweep') {
        return false;
      }
      const at = now().getTime();
      if (lastSweepAt !== undefined && at - lastSweepAt < SWEEP_INTERVAL_MS) {
        return false;
      }
      lastSweepAt = at;
      return true;
    },
    cutoffs(): { raw: string; aggregate: string } {
      const at = now().getTime();
      // Clamped to the same 1..3650 window the pg_cron migration validates. A
      // sweep is a DELETE: an unvalidated 0 or a negative day count would put
      // the cutoff at or after today and quietly delete live rows on every
      // pass, where the identical value under pg_cron refuses to boot.
      return {
        raw: utcDay(new Date(at - clampDays(config.retentionRawDays) * DAY_MS)),
        aggregate: utcDay(new Date(at - clampDays(config.retentionAggregateDays) * DAY_MS)),
      };
    },
  };
}
