import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import {
  CREATE_ANALYTICS_DAILY,
  CREATE_ANALYTICS_EVENT,
  createMigrationSource,
  MAX_RETENTION_DAYS,
  MIGRATION_NAMESPACE,
  scheduleRetentionDefinition,
} from '../src/server/sink/migrations';
import { testConfig } from './utils/fixtures';

const FEATURE = 'typescript/web-analytics-collection';
const REQUIREMENT = 'raw-events-expire';
const PG_CRON = 'pg-cron-mode-contributes-the-scheduling-migration';

describe('createMigrationSource', () => {
  it('contributes the two table migrations on the resolved datasource', () => {
    const source = createMigrationSource('analytics', testConfig(), 'public');

    expect(source.namespace).toBe(MIGRATION_NAMESPACE);
    expect(source.datasource).toBe('analytics');
    expect(source.schema).toBe('public');
    expect(source.definitions.map((definition) => definition.name)).toEqual([
      `${MIGRATION_NAMESPACE}/001_create_analytics_event`,
      `${MIGRATION_NAMESPACE}/002_create_analytics_daily`,
    ]);
  });

  it('reports the datasource as an infra database requirement', () => {
    const source = createMigrationSource('default', testConfig({ datasource: 'default' }), 'public');

    expect(source.infraDatabase()).toEqual({ name: 'default', engine: 'postgres', schemas: ['public'] });
  });

  it('omits the scheduling migration in sweep and off modes', () => {
    for (const retentionMode of ['sweep', 'off'] as const) {
      const source = createMigrationSource('analytics', testConfig({ retentionMode }), 'public');

      expect(source.definitions).toHaveLength(2);
    }
  });

  specTest(
    'contributes the scheduling migration only in pg_cron mode',
    { feature: FEATURE, requirement: REQUIREMENT, check: PG_CRON },
    () => {
      const source = createMigrationSource(
        'analytics',
        testConfig({ retentionMode: 'pg_cron', retentionRawDays: 45, retentionAggregateDays: 400 }),
        'public',
      );

      expect(source.definitions).toHaveLength(3);
      const scheduling = source.definitions[2];
      expect(scheduling?.name).toBe(`${MIGRATION_NAMESPACE}/003_schedule_analytics_retention`);
      const sql = scheduling?.sql ?? '';
      // `cron.schedule` would schedule the job in whatever database pg_cron was
      // installed in; only `schedule_in_database` targets this one.
      expect(sql).toContain('cron.schedule_in_database(');
      expect(sql).not.toContain('cron.schedule(');
      // Scheduling runs as the authenticated login, not the non-login owner the
      // migration assumed, or pg_cron cannot start its background worker.
      expect(sql.indexOf('SET LOCAL ROLE NONE;')).toBeLessThan(sql.indexOf('cron.schedule_in_database('));
      expect(sql).toContain('CREATE OR REPLACE FUNCTION analytics_expire()');
      expect(sql).toContain("'putnami-analytics-retention-' || current_database()");
      expect(sql).toContain("'15 0 * * *'");
      expect(sql).toContain("(now() AT TIME ZONE 'UTC')::date - 45");
      expect(sql).toContain("(now() AT TIME ZONE 'UTC')::date - 400");
      // A missing extension is a hard failure: silently skipping the job would
      // report a retention guarantee nothing enforces.
      expect(sql).toContain("SELECT 1 FROM pg_extension WHERE extname = 'pg_cron'");
      expect(scheduling?.down).toContain('cron.unschedule');
      expect(scheduling?.down).toContain('DROP FUNCTION IF EXISTS analytics_expire();');
    },
  );
});

describe('createMigrationSource in a named schema', () => {
  it('declares the schema, so the runner applies it as the search path', () => {
    const source = createMigrationSource('marketing', testConfig(), 'marketing');

    expect(source.schema).toBe('marketing');
    expect(source.infraDatabase()).toEqual({ name: 'marketing', engine: 'postgres', schemas: ['marketing'] });
  });

  it('points the retention function and its job at the schema', () => {
    const sql = scheduleRetentionDefinition(90, 760, 'marketing').sql;

    // A SECURITY DEFINER function pinned to `public` would delete from tables
    // that are not there, and the job would call a function that is not there.
    expect(sql).toContain('SET search_path = "marketing", pg_temp AS $$');
    expect(sql).toContain(`'SELECT "marketing".analytics_expire()'`);
    expect(sql).not.toContain('public');
  });

  it('names the job after the schema, so two schemas in one database keep two jobs', () => {
    const { sql, down } = scheduleRetentionDefinition(90, 760, 'marketing');
    const job = `'putnami-analytics-retention-' || current_database() || '-marketing'`;

    expect(sql).toContain(`cron.schedule_in_database(\n    ${job},`);
    expect(down).toContain(`cron.unschedule(${job})`);
    expect(down).toContain(`WHERE jobname = ${job}`);
  });

  it('keeps public bare', () => {
    const sql = scheduleRetentionDefinition(90, 760, 'public').sql;

    expect(sql).toContain('SET search_path = public, pg_temp AS $$');
    expect(sql).toContain(`'SELECT public.analytics_expire()'`);
    expect(sql).toContain(`'putnami-analytics-retention-' || current_database(),`);
  });

  it('defaults to public when no schema is given', () => {
    expect(scheduleRetentionDefinition(90, 760)).toEqual(scheduleRetentionDefinition(90, 760, 'public'));
    expect(createMigrationSource('analytics', testConfig()).schema).toBe('public');
  });

  it('rejects a schema that is not a lowercase identifier', () => {
    for (const schema of [
      '',
      'Marketing',
      'mark eting',
      'a"b',
      "a'b",
      '1st',
      'x'.repeat(64),
      'app, public',
      'pg_catalog',
      'pg_toast',
      'information_schema',
    ]) {
      expect(() => createMigrationSource('marketing', testConfig(), schema)).toThrow(
        'analytics: schema must be a lowercase SQL identifier',
      );
      expect(() => scheduleRetentionDefinition(90, 760, schema)).toThrow(
        'analytics: schema must be a lowercase SQL identifier',
      );
    }
  });
});

describe('scheduleRetentionDefinition', () => {
  it('accepts the bounds of the accepted window', () => {
    expect(() => scheduleRetentionDefinition(1, MAX_RETENTION_DAYS, 'public')).not.toThrow();
  });

  it('rejects a day count outside 1..3650 or not a whole number', () => {
    for (const [raw, aggregate] of [
      [0, 760],
      [-1, 760],
      [90, 3651],
      [1.5, 760],
      [90, Number.NaN],
    ] as const) {
      expect(() => scheduleRetentionDefinition(raw, aggregate, 'public')).toThrow(/between 1 and 3650/);
    }
  });
});

describe('the table definitions', () => {
  it('names no column that could carry an address, a header, or a query string', () => {
    const sql = CREATE_ANALYTICS_EVENT.sql;

    expect(sql).toContain('event_id       uuid        PRIMARY KEY');
    expect(sql).not.toContain('ip');
    expect(sql).not.toContain('user_agent');
    expect(sql).not.toContain('query');
    expect(CREATE_ANALYTICS_EVENT.down).toBe('DROP TABLE IF EXISTS analytics_event;');
  });

  it('keys each aggregate on the day so retention is a range delete', () => {
    const sql = CREATE_ANALYTICS_DAILY.sql;

    expect(sql).toContain('PRIMARY KEY (day, dimension, key)');
    expect(sql).toContain('PRIMARY KEY (day, visitor_id)');
    expect(sql).toContain('PRIMARY KEY (day, session_id)');
    expect(CREATE_ANALYTICS_DAILY.down).toContain('DROP TABLE IF EXISTS analytics_daily_session;');
  });
});
