import { NO_HOST } from '../enrich/referrer';
import {
  type Browser,
  type DIMENSIONS,
  type DeviceType,
  EVENT_ACTION,
  EVENT_FORM_SUBMIT,
  EVENT_PAGE_VIEW,
  type OS,
  type ReferrerType,
  type ViewportClass,
} from '../sanitize/vocabulary';

/** One daily counter dimension (body §B.2). */
export type Dimension = (typeof DIMENSIONS)[number];

/** The counter key a missing dimension value folds under. */
export const NONE_KEY = NO_HOST;

/** The counter key new `path` values fold under once the daily cap is reached. */
export const OVERFLOW_KEY = '__overflow__';

/**
 * One row of `analytics_event` (body §B.1) as TypeScript hands it to the sink:
 * one property per column, `received_at` excepted because Postgres defaults it.
 *
 * Everything here is already sanitized and classified — the sink writes, it
 * does not decide. `props` is the declared action payload, serialized to JSON
 * on the way to the `jsonb[]` parameter.
 */
export interface EventRow {
  eventId: string;
  ts: Date;
  day: string;
  name: string;
  source: string;
  app: string;
  env: string;
  appVersion: string | null;
  visitorId: string;
  visitorKind: string;
  sessionId: string | null;
  seq: number | null;
  userId: string | null;
  route: string;
  path: string | null;
  referrer: string | null;
  referrerType: ReferrerType;
  utmSource: string | null;
  utmMedium: string | null;
  utmCampaign: string | null;
  utmContent: string | null;
  utmTerm: string | null;
  statusCode: number | null;
  renderMs: number | null;
  engagementMs: number;
  actionName: string | null;
  outcome: string | null;
  props: Record<string, unknown>;
  browser: Browser;
  browserMajor: number | null;
  os: OS;
  deviceType: DeviceType;
  language: string | null;
  viewportClass: ViewportClass | null;
  country: string | null;
}

/**
 * One row of the raw upsert's `RETURNING` clause.
 *
 * `inserted` is `xmax = 0`: true when the statement created the row, false
 * when it took the conflict branch and only enriched it. That single boolean
 * is the whole dedup story — a retried batch, or the client re-sending the
 * server-rendered page view, reaches the fold with `inserted: false` and moves
 * no counter.
 *
 * The property names are camelCase because the framework's postgres.js client
 * applies a camel transform to result columns (`event_id` arrives as
 * `eventId`); `inserted` has no underscore and is unchanged.
 */
export interface RawUpsertResult {
  eventId: string;
  inserted: boolean;
  name: string;
  /**
   * The stored UTC day. Declared `string | Date` because the driver decodes a
   * `date` column into a JS `Date`: binding that value straight back into the
   * next statement's `$1::date[]` makes the driver declare the parameter
   * `timestamptz`, and Postgres refuses the cast.
   */
  day: string | Date;
  sessionId: string | null;
  visitorId: string;
  route: string;
  ts: Date;
  engagementMs: number;
}

/** One `analytics_daily_counter` contribution. */
export interface CounterFold {
  day: string;
  dimension: Dimension;
  key: string;
  count: number;
}

/** One `analytics_daily_visitor` contribution. */
export interface VisitorFold {
  day: string;
  visitorId: string;
}

/** One pre-folded `analytics_daily_session` contribution. */
export interface SessionFold {
  day: string;
  sessionId: string;
  visitorId: string;
  firstRoute: string;
  lastRoute: string;
  pageViews: number;
  startedAt: Date;
  endedAt: Date;
}

/** A `(day, session_id)` pair whose engagement total has to be recomputed. */
export interface TouchedSession {
  day: string;
  sessionId: string;
}

/** Everything statements S2 to S5 need, already aggregated and ordered. */
export interface FoldPlan {
  counters: CounterFold[];
  visitors: VisitorFold[];
  sessions: SessionFold[];
  touchedSessions: TouchedSession[];
}

/**
 * Turns one written batch into the daily aggregate work it implies.
 *
 * Pure by construction: the sink hands it the rows it wrote and the upsert's
 * verdict on each, and gets back four ordered arrays. Two properties are load
 * bearing and both are tested:
 *
 * - Only `inserted` rows move a counter, a visitor, or a session. Enrichment
 *   of an existing row is not a new observation.
 * - Duplicate `(day, session_id)` pairs are folded here, in memory, because
 *   `ON CONFLICT … DO UPDATE` cannot update the same row twice in one
 *   statement — a batch carrying two page views of one session would abort it.
 *
 * Every array is emitted in sorted key order, so two runs over the same batch
 * bind the same SQL parameters in the same positions.
 *
 * @param rows - The rows handed to the raw upsert.
 * @param results - The upsert's `RETURNING` rows.
 * @param pathOverflowDays - UTC days whose `path` cardinality cap is reached.
 * @returns The counters, visitors, sessions, and touched sessions.
 */
export function planFold(
  rows: EventRow[],
  results: RawUpsertResult[],
  pathOverflowDays: ReadonlySet<string>,
): FoldPlan {
  const byId = new Map(rows.map((row) => [row.eventId, row]));
  const counters = new Map<string, CounterFold>();
  const visitors = new Map<string, VisitorFold>();
  const sessions = new Map<string, SessionFold>();
  const touched = new Map<string, TouchedSession>();

  for (const result of results) {
    // The stored `(day, session_id)` is the upsert's, not the batch's: the
    // conflict branch keeps the session id the row already had.
    if (result.name === EVENT_PAGE_VIEW && result.sessionId !== null) {
      const day = dayOf(result.day);
      touched.set(`${day}|${result.sessionId}`, { day, sessionId: result.sessionId });
    }
    const row = byId.get(result.eventId);
    if (row === undefined || !result.inserted) {
      continue;
    }
    countRow(counters, row, pathOverflowDays.has(row.day));
    visitors.set(`${row.day}|${row.visitorId}`, { day: row.day, visitorId: row.visitorId });
    if (row.name === EVENT_PAGE_VIEW && row.sessionId !== null) {
      foldSession(sessions, row, row.sessionId);
    }
  }

  return {
    counters: sortedValues(counters),
    visitors: sortedValues(visitors),
    sessions: sortedValues(sessions),
    touchedSessions: sortedValues(touched),
  };
}

/**
 * Derives the `referrer_host` counter key from the stored referrer.
 *
 * An internal referrer is stored app-relative and has no host to count, so it
 * folds under `__none__`; the visit is still classified by `referrer_type`.
 * The row is the only input: no host is kept beside the referrer column.
 */
export function referrerHostOf(referrer: string | null): string {
  if (referrer === null || referrer === '' || referrer.startsWith('/')) {
    return NONE_KEY;
  }
  try {
    const host = new URL(referrer).host.toLowerCase().replace(/^www\./, '');
    return host === '' ? NONE_KEY : host;
  } catch {
    return NONE_KEY;
  }
}

/** Adds every counter one inserted row contributes. */
function countRow(counters: Map<string, CounterFold>, row: EventRow, pathOverflow: boolean): void {
  bump(counters, row.day, 'event', row.name);
  if (row.name === EVENT_PAGE_VIEW) {
    countPageView(counters, row, pathOverflow);
    return;
  }
  if (row.name === EVENT_ACTION) {
    bump(counters, row.day, 'action', row.actionName ?? NONE_KEY);
    return;
  }
  if (row.name === EVENT_FORM_SUBMIT) {
    bump(counters, row.day, 'form_submit', `${row.route}|${row.outcome ?? NONE_KEY}`);
  }
}

/** Adds the twelve page-view dimensions of body §B.2. */
function countPageView(counters: Map<string, CounterFold>, row: EventRow, pathOverflow: boolean): void {
  const day = row.day;
  bump(counters, day, 'route', row.route);
  // The cap is a safety valve, not a feature: once a day is over its path
  // budget every path folds under one key, existing ones included. A v1 that
  // distinguished them would need the day's whole key set in memory.
  bump(counters, day, 'path', pathOverflow ? OVERFLOW_KEY : (row.path ?? NONE_KEY));
  bump(counters, day, 'referrer_host', referrerHostOf(row.referrer));
  bump(counters, day, 'referrer_type', row.referrerType);
  bump(counters, day, 'utm_source', row.utmSource ?? NONE_KEY);
  bump(counters, day, 'utm_medium', row.utmMedium ?? NONE_KEY);
  bump(counters, day, 'utm_campaign', row.utmCampaign ?? NONE_KEY);
  bump(counters, day, 'country', row.country ?? NONE_KEY);
  bump(counters, day, 'device_type', row.deviceType);
  bump(counters, day, 'browser', row.browser);
  bump(counters, day, 'os', row.os);
  bump(counters, day, 'language', row.language ?? NONE_KEY);
}

/** Adds one to the `(day, dimension, key)` counter. */
function bump(counters: Map<string, CounterFold>, day: string, dimension: Dimension, key: string): void {
  const mapKey = `${day}|${dimension}|${key}`;
  const existing = counters.get(mapKey);
  if (existing === undefined) {
    counters.set(mapKey, { day, dimension, key, count: 1 });
    return;
  }
  existing.count += 1;
}

/** Folds one page view into its session, keeping the first and last route. */
function foldSession(sessions: Map<string, SessionFold>, row: EventRow, sessionId: string): void {
  const mapKey = `${row.day}|${sessionId}`;
  const existing = sessions.get(mapKey);
  if (existing === undefined) {
    sessions.set(mapKey, {
      day: row.day,
      sessionId,
      visitorId: row.visitorId,
      firstRoute: row.route,
      lastRoute: row.route,
      pageViews: 1,
      startedAt: row.ts,
      endedAt: row.ts,
    });
    return;
  }
  existing.pageViews += 1;
  if (row.ts < existing.startedAt) {
    existing.startedAt = row.ts;
    existing.firstRoute = row.route;
  }
  if (row.ts >= existing.endedAt) {
    existing.endedAt = row.ts;
    existing.lastRoute = row.route;
  }
}

/** The `YYYY-MM-DD` spelling of a day the driver may have decoded as a `Date`. */
function dayOf(day: string | Date): string {
  return typeof day === 'string' ? day : day.toISOString().slice(0, 10);
}

/** Emits a map's values in sorted key order, so SQL parameters are stable. */
function sortedValues<T>(entries: Map<string, T>): T[] {
  return [...entries.keys()].sort().map((key) => entries.get(key) as T);
}
