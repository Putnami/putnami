import { incCounter, observeHistogram } from '@putnami/application';
import { database, type ReservedSqlClient, type SqlClient } from '@putnami/database';
import type { Logger } from '@putnami/runtime';
import type { AnalyticsConfigValues } from '../analytics.config';
import { EVENT_PAGE_VIEW } from '../sanitize/vocabulary';
import { createDedupCache, type DedupCache } from './dedup-cache';
import { type CounterFold, type EventRow, type FoldPlan, planFold, type RawUpsertResult } from './fold';
import { createPathCap, type PathCap } from './path-cap';
import { DEFAULT_SCHEMA } from './migrations';
import { createRetention, type Retention } from './retention';

/**
 * S1 — the raw upsert (body §B.4).
 *
 * One `unnest` statement per batch, and the only statement that decides
 * anything: `(xmax = 0) AS inserted` tells the fold which rows are new. The
 * conflict branch never overwrites a value with a weaker one — it fills nulls
 * and takes the greater engagement — so a retried batch, or the client
 * re-sending the page view the server already recorded, enriches the row and
 * moves no counter.
 */
export const RAW_UPSERT_SQL = `INSERT INTO analytics_event (
  event_id, ts, day, name, source, app, env, app_version, visitor_id, visitor_kind, session_id, seq, user_id,
  route, path, referrer, referrer_type, utm_source, utm_medium, utm_campaign, utm_content, utm_term,
  status_code, render_ms, engagement_ms, action_name, outcome, props,
  browser, browser_major, os, device_type, language, viewport_class, country)
SELECT * FROM unnest(
  $1::uuid[], $2::timestamptz[], $3::date[], $4::text[], $5::text[], $6::text[], $7::text[], $8::text[], $9::text[], $10::text[],
  $11::uuid[], $12::integer[], $13::text[], $14::text[], $15::text[], $16::text[], $17::text[], $18::text[], $19::text[], $20::text[],
  $21::text[], $22::text[], $23::smallint[], $24::integer[], $25::integer[], $26::text[], $27::text[], $28::text[]::jsonb[],
  $29::text[], $30::smallint[], $31::text[], $32::text[], $33::text[], $34::text[], $35::text[])
ON CONFLICT (event_id) DO UPDATE SET
  session_id     = COALESCE(analytics_event.session_id, EXCLUDED.session_id),
  seq            = COALESCE(analytics_event.seq, EXCLUDED.seq),
  engagement_ms  = GREATEST(analytics_event.engagement_ms, EXCLUDED.engagement_ms),
  referrer       = COALESCE(analytics_event.referrer, EXCLUDED.referrer),
  referrer_type  = CASE WHEN analytics_event.referrer IS NULL AND EXCLUDED.referrer IS NOT NULL
                        THEN EXCLUDED.referrer_type ELSE analytics_event.referrer_type END,
  utm_source     = COALESCE(analytics_event.utm_source, EXCLUDED.utm_source),
  utm_medium     = COALESCE(analytics_event.utm_medium, EXCLUDED.utm_medium),
  utm_campaign   = COALESCE(analytics_event.utm_campaign, EXCLUDED.utm_campaign),
  utm_content    = COALESCE(analytics_event.utm_content, EXCLUDED.utm_content),
  utm_term       = COALESCE(analytics_event.utm_term, EXCLUDED.utm_term),
  language       = COALESCE(analytics_event.language, EXCLUDED.language),
  viewport_class = COALESCE(analytics_event.viewport_class, EXCLUDED.viewport_class)
RETURNING event_id, (xmax = 0) AS inserted, name, day, session_id, visitor_id, route, ts, engagement_ms`;

/**
 * S2 — the daily counters (body §B.4).
 *
 * The left operand of the addition is table-qualified on purpose: a bare
 * `count = count + EXCLUDED.count` is ambiguous in Postgres, which reads the
 * unqualified `count` as the excluded row's column and silently stops
 * accumulating.
 */
export const COUNTERS_SQL = `INSERT INTO analytics_daily_counter (day, dimension, key, count)
SELECT day, dimension, key, count
FROM unnest($1::date[], $2::text[], $3::text[], $4::bigint[]) AS rows(day, dimension, key, count)
ON CONFLICT (day, dimension, key)
DO UPDATE SET count = analytics_daily_counter.count + EXCLUDED.count`;

/** S3 — the daily unique visitors (body §B.4). */
export const VISITORS_SQL = `INSERT INTO analytics_daily_visitor (day, visitor_id)
SELECT day, visitor_id FROM unnest($1::date[], $2::text[]) AS rows(day, visitor_id)
ON CONFLICT (day, visitor_id) DO NOTHING`;

/**
 * S4 — the daily sessions (body §B.4).
 *
 * The arrays carry the values `planFold` already aggregated per `(day,
 * session_id)` — a page-view count, the first and the last route, the earliest
 * and the latest instant — rather than one row per event with `1` and the same
 * route twice. `ON CONFLICT … DO UPDATE` cannot touch one row twice in a
 * single statement, so a batch with two page views of one session has to
 * arrive pre-folded; carrying the constants of a per-event shape would then
 * count that batch as a single page view.
 */
export const SESSIONS_SQL = `INSERT INTO analytics_daily_session (day, session_id, visitor_id, first_route, last_route, page_views, engagement_ms, started_at, ended_at)
SELECT day, session_id, visitor_id, first_route, last_route, page_views, 0, started_at, ended_at
FROM unnest($1::date[], $2::uuid[], $3::text[], $4::text[], $5::text[], $6::integer[], $7::timestamptz[], $8::timestamptz[])
  AS rows(day, session_id, visitor_id, first_route, last_route, page_views, started_at, ended_at)
ON CONFLICT (day, session_id) DO UPDATE SET
  page_views  = analytics_daily_session.page_views + EXCLUDED.page_views,
  first_route = CASE WHEN EXCLUDED.started_at < analytics_daily_session.started_at THEN EXCLUDED.first_route ELSE analytics_daily_session.first_route END,
  started_at  = LEAST(analytics_daily_session.started_at, EXCLUDED.started_at),
  last_route  = CASE WHEN EXCLUDED.ended_at >= analytics_daily_session.ended_at THEN EXCLUDED.last_route ELSE analytics_daily_session.last_route END,
  ended_at    = GREATEST(analytics_daily_session.ended_at, EXCLUDED.ended_at)`;

/**
 * S5 — the session engagement recompute (body §B.4).
 *
 * Recomputing the total from the raw rows is idempotent and needs no "value
 * before this batch" plumbing, which is what makes a duplicate delivery of an
 * engagement re-send harmless. `idx_analytics_event_session` keeps it an index
 * scan.
 */
export const SESSION_ENGAGEMENT_SQL = `UPDATE analytics_daily_session s
SET engagement_ms = agg.total
FROM (
  SELECT e.day, e.session_id, COALESCE(SUM(e.engagement_ms), 0)::bigint AS total
  FROM analytics_event e
  WHERE (e.day, e.session_id) IN (SELECT day, session_id FROM unnest($1::date[], $2::uuid[]) AS t(day, session_id))
    AND e.name = 'page_view'
  GROUP BY e.day, e.session_id
) agg
WHERE s.day = agg.day AND s.session_id = agg.session_id`;

/**
 * S6 — the opportunistic retention sweep (body §B.4).
 *
 * Four statements run one by one, each bounded to a thousand `ctid`s so an
 * ingest never pays for a backlog, and each taking its own cutoff as `$1`:
 * they are separate prepared statements, so a shared `$2` would not bind.
 * The first takes the raw cutoff, the other three the aggregate one.
 */
export const SWEEP_SQL: readonly string[] = [
  'DELETE FROM analytics_event WHERE ctid IN (SELECT ctid FROM analytics_event WHERE day < $1::date LIMIT 1000)',
  'DELETE FROM analytics_daily_counter WHERE ctid IN (SELECT ctid FROM analytics_daily_counter WHERE day < $1::date LIMIT 1000)',
  'DELETE FROM analytics_daily_visitor WHERE ctid IN (SELECT ctid FROM analytics_daily_visitor WHERE day < $1::date LIMIT 1000)',
  'DELETE FROM analytics_daily_session WHERE ctid IN (SELECT ctid FROM analytics_daily_session WHERE day < $1::date LIMIT 1000)',
];

/**
 * S0: points the transaction at the tables' schema. Transaction-local, so the
 * datasource's own `search_path` is back in force for the next borrower. It is
 * written right behind `BEGIN`, so it costs no round trip of its own.
 */
export const SET_SCHEMA_SQL = "SELECT set_config('search_path', $1, true)";

/** The SQLSTATE of a missing relation: the tables are not in the schema the batch runs in. */
const UNDEFINED_TABLE = '42P01';

/** One `unnest` parameter: the values of a single column, in row order. */
type ColumnArray = readonly (string | number | boolean | Date | null)[];

/**
 * How often one instance reports a sink failure.
 *
 * A database outage fails every flush, and a log line per failed batch turns
 * one incident into a log bill. The counter keeps counting every drop; only
 * the line is rate limited.
 */
export const SINK_FAILURE_LOG_INTERVAL_MS = 60_000;

/** How many rows one batch created, and how many it only enriched. */
export interface SinkCounts {
  inserted: number;
  enriched: number;
}

/** The write side of analytics ingestion. */
export interface Sink {
  /** Writes one accepted batch and reports what it created. */
  write(rows: EventRow[]): Promise<SinkCounts>;
}

/** What {@link createSink} needs from its host. */
export interface SinkDeps {
  /** The datasource resolved by `resolveAnalyticsDatasource`. */
  datasource: string;
  /**
   * The schema the migration put the tables in. Read at the first write, once
   * every plugin has contributed its sources, and kept. Defaults to `public`.
   */
  schema?: () => string;
  /** The resolved analytics configuration. */
  config: AnalyticsConfigValues;
  /** Where a sink failure is reported. */
  logger: Logger;
  /** The clock, injected so retention and the path cap are testable. */
  now: () => Date;
  /** How to reach the pool; defaults to `database` from `@putnami/database`. */
  connect?: (name: string) => Promise<SqlClient>;
  /**
   * The per-instance dedup cache. The plugin passes the one it published on
   * the runtime so an instance has exactly one memory of what it wrote; a
   * second cache would make the two disagree about which ids are in flight.
   */
  dedup?: DedupCache;
}

const EMPTY: SinkCounts = { inserted: 0, enriched: 0 };

/**
 * Builds the Postgres sink: one transaction per accepted batch.
 *
 * The transaction runs in the tables' schema whatever `search_path` the
 * datasource binds: an explicit `analytics.schema` need not match it.
 *
 * The transaction is opened by hand (`reserve`, `BEGIN`, `COMMIT`/`ROLLBACK`,
 * `release`) rather than through `sql.begin(fn)`, mirroring the framework's own
 * transaction machinery, so the reserved connection is released on every path.
 *
 * A failure is a counted drop, never a leaked payload: the log line carries the
 * error message and the batch size, and nothing else — the rows are the data
 * this package exists to keep out of logs.
 *
 * @param deps - The datasource, configuration, logger, clock, and pool seam.
 * @returns The sink.
 */
export function createSink(deps: SinkDeps): Sink {
  const connect = deps.connect ?? ((name: string) => database(name));
  const retention = createRetention(deps.config, deps.now);
  const pathCap = createPathCap(deps.config, deps.now);
  const dedup = deps.dedup ?? createDedupCache();
  let schema: string | undefined;
  // The epoch, so the first failure of a process is always reported.
  let lastFailureLoggedAt = 0;

  return {
    async write(rows: EventRow[]): Promise<SinkCounts> {
      if (rows.length === 0) {
        return EMPTY;
      }
      const batch = dedup.filter(rows);
      // `duplicate` is a declared drop reason, so it has to be counted: an
      // operator watching this counter is the only way a cache that drops too
      // much becomes visible, since the sender is answered 202 either way.
      if (batch.length < rows.length) {
        incCounter('analytics.ingest.dropped.duplicate', rows.length - batch.length);
      }
      if (batch.length === 0) {
        return EMPTY;
      }
      const startedAt = deps.now().getTime();
      const sql = await connect(deps.datasource);
      const tx = await sql.reserve();
      let counts: SinkCounts;
      try {
        schema ??= deps.schema?.() ?? DEFAULT_SCHEMA;
        // Written back to back on the reserved connection: the set rides the
        // same round trip as BEGIN.
        await Promise.all([tx`BEGIN`, tx.unsafe(SET_SCHEMA_SQL, [schema])]);
        counts = await writeBatch(tx, batch, pathCap, retention);
        await tx`COMMIT`;
      } catch (error) {
        await rollbackQuietly(tx);
        const at = deps.now().getTime();
        if (at - lastFailureLoggedAt >= SINK_FAILURE_LOG_INTERVAL_MS) {
          lastFailureLoggedAt = at;
          deps.logger.error('analytics.sink.failed', failureFields(error, rows.length, schema));
        }
        incCounter('analytics.ingest.dropped.sink_error', rows.length);
        throw error;
      } finally {
        tx.release();
      }
      // Only client-written ids are remembered. A server-written page view is
      // deliberately re-sent by the tracker under the same id, carrying the
      // session, viewport, language, referrer and campaign the server could not
      // know — and that first re-send carries no engagement, so remembering the
      // server's id would make the cache drop the enrichment before it reached
      // the upsert that exists to merge it.
      dedup.remember(batch.filter((row) => row.source === 'client').map((row) => row.eventId));
      observeHistogram('analytics.sink.write_ms', deps.now().getTime() - startedAt);
      incCounter('analytics.ingest.accepted', counts.inserted);
      return counts;
    },
  };
}

/** Runs S1 to S6 on an open transaction. */
async function writeBatch(
  tx: ReservedSqlClient,
  batch: EventRow[],
  pathCap: PathCap,
  retention: Retention,
): Promise<SinkCounts> {
  const overflowDays = await refreshPathCap(tx, batch, pathCap);
  const results = (await tx.unsafe<RawUpsertResult[]>(RAW_UPSERT_SQL, upsertParams(batch))) as RawUpsertResult[];
  const plan = planFold(batch, results, overflowDays);
  await applyFold(tx, plan);
  if (retention.isDue()) {
    await sweep(tx, retention.cutoffs());
  }
  const inserted = results.filter((result) => result.inserted).length;
  return { inserted, enriched: results.length - inserted };
}

/** Runs S2 to S5, skipping any statement the batch gave no rows for. */
async function applyFold(tx: ReservedSqlClient, plan: FoldPlan): Promise<void> {
  if (plan.counters.length > 0) {
    await tx.unsafe(COUNTERS_SQL, counterParams(plan.counters));
  }
  if (plan.visitors.length > 0) {
    await tx.unsafe(VISITORS_SQL, [plan.visitors.map((v) => v.day), plan.visitors.map((v) => v.visitorId)]);
  }
  if (plan.sessions.length > 0) {
    await tx.unsafe(SESSIONS_SQL, sessionParams(plan));
  }
  if (plan.touchedSessions.length > 0) {
    const touched = plan.touchedSessions;
    await tx.unsafe(SESSION_ENGAGEMENT_SQL, [touched.map((t) => t.day), touched.map((t) => t.sessionId)]);
  }
}

/** Runs the four bounded delete statements of the opportunistic sweep. */
async function sweep(tx: ReservedSqlClient, cutoffs: { raw: string; aggregate: string }): Promise<void> {
  for (const [index, statement] of SWEEP_SQL.entries()) {
    // biome-ignore lint/performance/noAwaitInLoops: the four bounded deletes are
    // one transaction's ordered tail; issuing them concurrently on one reserved
    // connection is not possible.
    await tx.unsafe(statement, [index === 0 ? cutoffs.raw : cutoffs.aggregate]);
  }
}

/** Probes each page-view day's `path` cardinality in deterministic order. */
async function refreshPathCap(tx: ReservedSqlClient, batch: EventRow[], pathCap: PathCap): Promise<Set<string>> {
  const days = [...new Set(batch.filter((row) => row.name === EVENT_PAGE_VIEW).map((row) => row.day))].sort();
  const overflow = new Set<string>();
  for (const day of days) {
    // biome-ignore lint/performance/noAwaitInLoops: one reserved connection
    // owns this transaction, so its per-day probes must be issued in order.
    if (await pathCap.refresh(tx, day)) {
      overflow.add(day);
    }
  }
  return overflow;
}

/**
 * The thirty-five parallel arrays of S1, in the column order of body §B.4.
 *
 * Every instant is bound as an ISO-8601 string, never as a `Date`. The driver
 * infers a parameter's type from the *first element* of an array it is handed,
 * so an array of `Date` is declared `timestamptz` while the statement casts
 * `$2::timestamptz[]` — Postgres answers `cannot cast type timestamp with time
 * zone to timestamp with time zone[]` and the whole batch is lost. A string
 * infers as unspecified, which lets the explicit cast decide, and that is what
 * every other column here already relies on.
 */
function upsertParams(rows: EventRow[]): ColumnArray[] {
  return [
    rows.map((r) => r.eventId),
    rows.map((r) => r.ts.toISOString()),
    rows.map((r) => r.day),
    rows.map((r) => r.name),
    rows.map((r) => r.source),
    rows.map((r) => r.app),
    rows.map((r) => r.env),
    rows.map((r) => r.appVersion),
    rows.map((r) => r.visitorId),
    rows.map((r) => r.visitorKind),
    rows.map((r) => r.sessionId),
    rows.map((r) => r.seq),
    rows.map((r) => r.userId),
    rows.map((r) => r.route),
    rows.map((r) => r.path),
    rows.map((r) => r.referrer),
    rows.map((r) => r.referrerType),
    rows.map((r) => r.utmSource),
    rows.map((r) => r.utmMedium),
    rows.map((r) => r.utmCampaign),
    rows.map((r) => r.utmContent),
    rows.map((r) => r.utmTerm),
    rows.map((r) => r.statusCode),
    rows.map((r) => r.renderMs),
    rows.map((r) => r.engagementMs),
    rows.map((r) => r.actionName),
    rows.map((r) => r.outcome),
    // Serialized here, and bound as `text[]` cast to `jsonb[]` in the statement.
    // A bare `$28::jsonb[]` stores every payload as a JSON *string* — the
    // driver escapes each element of an array whose type it cannot infer, so
    // `props ->> 'plan'` would be null on every row ever written.
    rows.map((r) => JSON.stringify(r.props)),
    rows.map((r) => r.browser),
    rows.map((r) => r.browserMajor),
    rows.map((r) => r.os),
    rows.map((r) => r.deviceType),
    rows.map((r) => r.language),
    rows.map((r) => r.viewportClass),
    rows.map((r) => r.country),
  ];
}

/** The four parallel arrays of S2. */
function counterParams(counters: CounterFold[]): ColumnArray[] {
  return [
    counters.map((c) => c.day),
    counters.map((c) => c.dimension),
    counters.map((c) => c.key),
    counters.map((c) => c.count),
  ];
}

/**
 * The eight parallel arrays of S4, already folded per `(day, session_id)`.
 *
 * `started_at` and `ended_at` are ISO-8601 strings for the same reason the raw
 * upsert's `ts` is: see {@link upsertParams}.
 */
function sessionParams(plan: FoldPlan): ColumnArray[] {
  const sessions = plan.sessions;
  return [
    sessions.map((s) => s.day),
    sessions.map((s) => s.sessionId),
    sessions.map((s) => s.visitorId),
    sessions.map((s) => s.firstRoute),
    sessions.map((s) => s.lastRoute),
    sessions.map((s) => s.pageViews),
    sessions.map((s) => s.startedAt.toISOString()),
    sessions.map((s) => s.endedAt.toISOString()),
  ];
}

/** Rolls back without masking the failure that caused it. */
async function rollbackQuietly(tx: ReservedSqlClient): Promise<void> {
  try {
    await tx`ROLLBACK`;
  } catch {
    // The original error is the one worth reporting; a rollback that fails on
    // an already-broken connection adds nothing.
  }
}

/**
 * What a sink failure line carries: the error and the batch size, plus the
 * schema when the tables are missing from it. The schema is fixed at the first
 * migration, so a workload whose resolved schema moved afterwards writes to a
 * schema that has no tables; the line names it so an operator can pin
 * `analytics.schema` back.
 */
function failureFields(error: unknown, rows: number, schema: string | undefined): Record<string, unknown> {
  const fields: Record<string, unknown> = { error: messageOf(error), rows };
  if ((error as { code?: unknown } | null)?.code === UNDEFINED_TABLE) {
    fields['schema'] = schema;
  }
  return fields;
}

/** The message of a thrown value, whatever it is. */
function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
