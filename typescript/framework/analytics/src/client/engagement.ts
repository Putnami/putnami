/** Engagement is capped at 24 hours (protocol `MaxEngagementMs`). */
export const MAX_ENGAGEMENT_MS = 86_400_000;

/** The visible-time accumulator of the current page view (body §D.14). */
export interface Engagement {
  /** Starts a fresh accumulation for a new page view. */
  reset(): void;
  /** Stops counting; the tab went hidden or the page is going away. */
  pause(): void;
  /** Resumes counting; the tab came back. */
  resume(): void;
  /** Milliseconds the current view has been visible, capped and rounded. */
  total(): number;
}

/**
 * Reads the monotonic clock, falling back to the wall clock.
 *
 * `performance.now()` cannot be moved by a clock adjustment, so a user whose
 * machine syncs its time mid-visit does not produce an hour of engagement.
 * It is read through `globalThis` on every call: the module is imported by
 * tests that install a fake clock, and a captured reference would freeze the
 * real one.
 */
function clock(): number {
  const performance = (globalThis as { performance?: { now(): number } }).performance;
  return typeof performance?.now === 'function' ? performance.now() : Date.now();
}

/**
 * Creates the engagement accumulator for one page view (body §D.14).
 *
 * It counts *visible* time, not elapsed time: a tab left open in the
 * background for an hour reports the seconds it was actually looked at. The
 * accumulated value rides on a re-send of the page view under the same event
 * id, and the sink takes `GREATEST`, so re-sending a smaller number can never
 * shrink a stored one.
 *
 * @returns The accumulator, already running.
 */
export function createEngagement(): Engagement {
  let accumulated = 0;
  let startedAt: number | undefined = clock();

  return {
    reset(): void {
      accumulated = 0;
      startedAt = clock();
    },
    pause(): void {
      if (startedAt !== undefined) {
        accumulated += clock() - startedAt;
        startedAt = undefined;
      }
    },
    resume(): void {
      if (startedAt === undefined) {
        startedAt = clock();
      }
    },
    total(): number {
      const live = startedAt === undefined ? 0 : clock() - startedAt;
      return Math.min(MAX_ENGAGEMENT_MS, Math.max(0, Math.round(accumulated + live)));
    },
  };
}
