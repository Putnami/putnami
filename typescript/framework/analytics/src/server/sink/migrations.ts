import { type SQLDefinition, type SQLSource, sqlSourceInline } from '@putnami/database';
import type { AnalyticsConfigValues } from '../analytics.config';

/** Migration namespace of every analytics definition (body §B). */
export const MIGRATION_NAMESPACE = 'putnami-analytics';

/** The schema the tables live in when nothing on their datasource declares one. */
export const DEFAULT_SCHEMA = 'public';

/** A lowercase unquoted SQL identifier, the only schema name the runner's `search_path` keeps as written. */
const SCHEMA_PATTERN = /^[a-z_][a-z0-9_]{0,62}$/;

/** The prefix Postgres reserves for its own schemas; `CREATE SCHEMA pg_x` is refused. */
const RESERVED_SCHEMA_PREFIX = 'pg_';
/** The SQL-standard catalog schema, never a place for application tables. */
const INFORMATION_SCHEMA = 'information_schema';

/** The pg_cron job name of the retention sweep, before the database name. */
const RETENTION_JOB_PREFIX = 'putnami-analytics-retention-';

/** The smallest accepted retention window, in days. */
export const MIN_RETENTION_DAYS = 1;
/** The largest accepted retention window, in days (ten years). */
export const MAX_RETENTION_DAYS = 3650;

/**
 * The raw event table (body §B.1).
 *
 * The column list *is* the privacy boundary: there is no column for an IP
 * address, a raw `User-Agent`, a query string, a page title, or a form field,
 * so none of them can be stored by an accident upstream. `day` is written by
 * TypeScript rather than derived by Postgres, because the UTC day of an event
 * is decided by the same clock that decided the visitor hash.
 */
export const CREATE_ANALYTICS_EVENT: SQLDefinition = {
  name: '001_create_analytics_event',
  sql: `CREATE TABLE IF NOT EXISTS analytics_event (
    event_id       uuid        PRIMARY KEY,
    received_at    timestamptz NOT NULL DEFAULT now(),
    ts             timestamptz NOT NULL,
    day            date        NOT NULL,
    name           text        NOT NULL,
    source         text        NOT NULL,
    app            text        NOT NULL,
    env            text        NOT NULL,
    app_version    text,
    visitor_id     text        NOT NULL,
    visitor_kind   text        NOT NULL,
    session_id     uuid,
    seq            integer,
    user_id        text,
    route          text        NOT NULL,
    path           text,
    referrer       text,
    referrer_type  text        NOT NULL,
    utm_source     text,
    utm_medium     text,
    utm_campaign   text,
    utm_content    text,
    utm_term       text,
    status_code    smallint,
    render_ms      integer,
    engagement_ms  integer     NOT NULL DEFAULT 0,
    action_name    text,
    outcome        text,
    props          jsonb       NOT NULL DEFAULT '{}'::jsonb,
    browser        text        NOT NULL,
    browser_major  smallint,
    os             text        NOT NULL,
    device_type    text        NOT NULL,
    language       text,
    viewport_class text,
    country        text
);
CREATE INDEX IF NOT EXISTS idx_analytics_event_day ON analytics_event (day);
CREATE INDEX IF NOT EXISTS idx_analytics_event_session ON analytics_event (session_id) WHERE session_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_analytics_event_user ON analytics_event (user_id, day) WHERE user_id IS NOT NULL;`,
  down: 'DROP TABLE IF EXISTS analytics_event;',
};

/**
 * The three daily aggregates (body §B.2).
 *
 * Bounce, entry, and exit are derived at read time from `page_views = 1`,
 * `first_route`, and `last_route`; storing them would be a second copy of the
 * same fact that a late-arriving event could contradict.
 */
export const CREATE_ANALYTICS_DAILY: SQLDefinition = {
  name: '002_create_analytics_daily',
  sql: `CREATE TABLE IF NOT EXISTS analytics_daily_counter (
    day       date   NOT NULL,
    dimension text   NOT NULL,
    key       text   NOT NULL,
    count     bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (day, dimension, key)
);
CREATE TABLE IF NOT EXISTS analytics_daily_visitor (
    day        date NOT NULL,
    visitor_id text NOT NULL,
    PRIMARY KEY (day, visitor_id)
);
CREATE TABLE IF NOT EXISTS analytics_daily_session (
    day           date        NOT NULL,
    session_id    uuid        NOT NULL,
    visitor_id    text        NOT NULL,
    first_route   text        NOT NULL,
    last_route    text        NOT NULL,
    page_views    integer     NOT NULL DEFAULT 0,
    engagement_ms bigint      NOT NULL DEFAULT 0,
    started_at    timestamptz NOT NULL,
    ended_at      timestamptz NOT NULL,
    PRIMARY KEY (day, session_id)
);`,
  down: `DROP TABLE IF EXISTS analytics_daily_session; DROP TABLE IF EXISTS analytics_daily_visitor; DROP TABLE IF EXISTS analytics_daily_counter;`,
};

/**
 * Builds the `pg_cron` retention migration (body §B.3).
 *
 * The two day counts are interpolated into the migration text rather than
 * bound as parameters: a `CREATE FUNCTION` body is not a prepared statement,
 * so there is nothing to bind to. They are configuration integers, never user
 * input, and {@link assertDays} rejects anything that is not a whole number of
 * days inside a ten-year window before it reaches the text.
 *
 * The migration fails hard when `pg_cron` is absent. That is the contract of
 * the mode, not a defect: silently omitting the job would couple expiry to
 * request-serving uptime while still reporting a retention guarantee.
 *
 * The function pins its own `search_path` and the job names it qualified, so
 * both carry the tables' schema. The job name carries the schema too, so two
 * workloads that share one database in two schemas each keep their own job.
 * `public` stays bare and unsuffixed, which keeps the text a workload on the
 * default schema already applied byte for byte.
 *
 * @param rawDays - How long a raw event row is kept.
 * @param aggregateDays - How long a daily aggregate row is kept.
 * @param schema - The schema the tables live in.
 * @returns The `003_schedule_analytics_retention` definition.
 * @throws Error when either count is not an integer in 1..3650, or the schema
 *   is not a lowercase identifier.
 */
export function scheduleRetentionDefinition(
  rawDays: number,
  aggregateDays: number,
  schema: string = DEFAULT_SCHEMA,
): SQLDefinition {
  const raw = assertDays(rawDays, 'retentionRawDays');
  const aggregate = assertDays(aggregateDays, 'retentionAggregateDays');
  const inDefault = assertSchema(schema) === DEFAULT_SCHEMA;
  const qualifier = inDefault ? DEFAULT_SCHEMA : `"${schema}"`;
  const jobName = `'${RETENTION_JOB_PREFIX}' || current_database()${inDefault ? '' : ` || '-${schema}'`}`;
  return {
    name: '003_schedule_analytics_retention',
    sql: `DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_cron') THEN
        RAISE EXCEPTION
            'pg_cron must be admin-installed in the analytics database'
            USING ERRCODE = '55000';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_roles
        WHERE rolname = session_user AND rolcanlogin
    ) THEN
        RAISE EXCEPTION
            'pg_cron session user % must be a login role', session_user
            USING ERRCODE = '55000';
    END IF;
END
$$;

CREATE OR REPLACE FUNCTION analytics_expire() RETURNS void
LANGUAGE sql SECURITY DEFINER
SET search_path = ${qualifier}, pg_temp AS $$
WITH
raw AS (DELETE FROM analytics_event WHERE day < (now() AT TIME ZONE 'UTC')::date - ${raw}),
counters AS (DELETE FROM analytics_daily_counter WHERE day < (now() AT TIME ZONE 'UTC')::date - ${aggregate}),
visitors AS (DELETE FROM analytics_daily_visitor WHERE day < (now() AT TIME ZONE 'UTC')::date - ${aggregate})
DELETE FROM analytics_daily_session WHERE day < (now() AT TIME ZONE 'UTC')::date - ${aggregate}
$$;

-- The managed migration login assumes the non-login database owner role.
-- Return to the authenticated login for scheduling so pg_cron can start its
-- background worker without granting LOGIN to the owner.
SET LOCAL ROLE NONE;

SELECT cron.schedule_in_database(
    ${jobName},
    '15 0 * * *',
    'SELECT ${qualifier}.analytics_expire()',
    current_database()
);

SELECT analytics_expire();`,
    down: `SET LOCAL ROLE NONE;

SELECT cron.unschedule(${jobName})
WHERE EXISTS (
    SELECT 1 FROM cron.job
    WHERE jobname = ${jobName}
);

DROP FUNCTION IF EXISTS analytics_expire();`,
  };
}

/**
 * Builds the migration source the plugin contributes.
 *
 * `SQLSource.infraDatabase()` then reports `{ name, engine: 'postgres',
 * schemas: [schema] }`, which is what puts the resolved datasource into a
 * consuming workload's `infra/requirements.json` — the library itself never
 * asks for a database. The runner applies `schema` as the `search_path` of
 * every definition, so the unqualified DDL lands in it.
 *
 * @param datasource - The datasource resolved by `resolveAnalyticsDatasource`.
 * @param config - The resolved analytics configuration.
 * @param schema - The schema the tables live in, from `resolveAnalyticsSchema`.
 * @returns The migration source, with `003` only in `pg_cron` mode.
 * @throws Error when the schema is not a lowercase identifier, or is reserved.
 */
export function createMigrationSource(
  datasource: string,
  config: AnalyticsConfigValues,
  schema: string = DEFAULT_SCHEMA,
): SQLSource {
  const definitions: SQLDefinition[] = [CREATE_ANALYTICS_EVENT, CREATE_ANALYTICS_DAILY];
  if (config.retentionMode === 'pg_cron') {
    definitions.push(scheduleRetentionDefinition(config.retentionRawDays, config.retentionAggregateDays, schema));
  }
  return sqlSourceInline({
    namespace: MIGRATION_NAMESPACE,
    datasource: { name: datasource, schema: assertSchema(schema) },
    definitions,
  });
}

/**
 * Reports whether the tables can live in `schema`.
 *
 * The runner and the sink both hand the name to `search_path` as written,
 * where an unquoted name is folded to lowercase; only a lowercase identifier
 * names the same schema in the DDL, the job, and every write. The system
 * schemas are refused: Postgres owns them.
 *
 * @param schema - A candidate schema name.
 * @returns True for a lowercase identifier that is not a system schema.
 */
export function isAnalyticsSchema(schema: string): boolean {
  return SCHEMA_PATTERN.test(schema) && !schema.startsWith(RESERVED_SCHEMA_PREFIX) && schema !== INFORMATION_SCHEMA;
}

/** Rejects a schema the tables could not be found in again. */
function assertSchema(schema: string): string {
  if (!isAnalyticsSchema(schema)) {
    throw new Error(
      `analytics: schema must be a lowercase SQL identifier (a-z, 0-9, _; at most 63 characters) outside pg_* and information_schema, got ${JSON.stringify(schema)}`,
    );
  }
  return schema;
}

/** Rejects a retention window that is not a whole number of days in 1..3650. */
function assertDays(value: number, field: string): number {
  if (!Number.isInteger(value) || value < MIN_RETENTION_DAYS || value > MAX_RETENTION_DAYS) {
    throw new Error(
      `analytics: ${field} must be an integer between ${MIN_RETENTION_DAYS} and ${MAX_RETENTION_DAYS}, got ${value}`,
    );
  }
  return value;
}
