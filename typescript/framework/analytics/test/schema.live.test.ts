import { afterAll, afterEach, beforeAll, describe, expect, it } from 'bun:test';
import type { Plugin } from '@putnami/application';
import { createTestApp } from '@putnami/application/testing';
import {
  database,
  Migrator,
  provision,
  type ProvisionResult,
  type SQLSource,
  sqlSourceInline,
} from '@putnami/database';
import { type Logger, resetConfigLoader } from '@putnami/runtime';
import { analytics } from '../src/server/analytics.plugin';
import { createMigrationSource, scheduleRetentionDefinition } from '../src/server/sink/migrations';
import { createSink } from '../src/server/sink/sink';
import { pageViewRow, testConfig } from './utils/fixtures';
import { restoreProjectRoot, TEST_SECRET, useTempProjectRoot } from './utils/runtime';

// The named-schema path against a real Postgres. Runs only when
// DATABASE_TEST_BINDINGS names a server (CI service container or local
// server), like the database package's own provider test; unit runs pay no
// database cost. It carries no specTest, because a skipped attestation would
// block the enforce gate.
const RAW = process.env['DATABASE_TEST_BINDINGS']?.trim();
// The pg_cron job, against a server that has what a managed database has:
// pg_cron admin-installed in the database, a non-login owner role, and a login
// that is a member of it, starts as it, and may call cron.schedule_in_database.
// Same binding shape as DATABASE_TEST_BINDINGS.
const CRON_RAW = process.env['ANALYTICS_PG_CRON_TEST_BINDINGS']?.trim();
const CRON_DATASOURCE = 'analytics_cron';
const DATASOURCE = 'analytics_live';
const SCHEMA = 'analytics_own';
/** A second datasource, so its migration state is its own. */
const PLUGIN_DATASOURCE = 'analytics_plugin';
const PLUGIN_SCHEMA = 'analytics_moved';
const NOW = new Date('2026-09-02T10:30:00.000Z');

/** The server a test binding points at. */
function serverConnection(raw = RAW): unknown {
  const binding = JSON.parse(raw ?? '{}') as { databases?: Record<string, { connection?: unknown }> };
  return Object.values(binding.databases ?? {})[0]?.connection;
}

const silent = { error: () => {}, warn: () => {}, info: () => {}, debug: () => {} } as unknown as Logger;

describe.skipIf(!RAW)('analytics in a named schema (live)', () => {
  let provisioned: ProvisionResult;

  beforeAll(async () => {
    // The datasource's own search path stays `public`: every write below finds
    // the tables only through the schema the sink sets for its transaction.
    provisioned = await provision({
      binding: {
        protocolVersion: 1,
        mode: 'require',
        isolation: 'database',
        databases: {
          [DATASOURCE]: { engine: 'postgres', schema: 'public', connection: serverConnection() },
          [PLUGIN_DATASOURCE]: { engine: 'postgres', schema: 'public', connection: serverConnection() },
        } as never,
      },
    });
    await (await database(DATASOURCE)).unsafe(`CREATE SCHEMA ${SCHEMA}`);
    await (await database(PLUGIN_DATASOURCE)).unsafe(`CREATE SCHEMA ${PLUGIN_SCHEMA}`);
    // Every test below reads or writes these tables, so none depends on another
    // having run first.
    const source = createMigrationSource(DATASOURCE, testConfig({ datasource: DATASOURCE }), SCHEMA);
    await new Migrator(DATASOURCE, source.definitions, SCHEMA).up();
  }, 60_000);

  afterEach(() => {
    process.env['CONFIG_DATA'] = undefined;
    restoreProjectRoot();
    resetConfigLoader();
  });

  afterAll(async () => {
    await provisioned?.cleanup();
  }, 60_000);

  it('creates the tables in the schema, writes there, and sweeps there', async () => {
    const config = testConfig({ datasource: DATASOURCE, retentionMode: 'sweep' });
    const sink = createSink({ datasource: DATASOURCE, schema: () => SCHEMA, config, logger: silent, now: () => NOW });
    const current = pageViewRow({ day: '2026-09-02' });
    // Older than the 90-day raw window: the sweep in the same transaction
    // deletes it, in the schema, or the count below is 2.
    const expired = pageViewRow({
      eventId: '01920000-0000-7000-8000-0000000000ff',
      sessionId: '01920000-0000-7000-8000-0000000000fe',
      ts: new Date('2020-01-01T10:00:00.000Z'),
      day: '2020-01-01',
    });
    const counts = await sink.write([current, expired]);

    expect(counts.inserted).toBe(2);
    const sql = await database(DATASOURCE);
    const rows = await sql.unsafe<{ day: string }[]>(`SELECT day::text AS day FROM ${SCHEMA}.analytics_event`);
    expect(rows.map((row) => row.day)).toEqual(['2026-09-02']);
    const [{ table }] = await sql.unsafe<{ table: string | null }[]>(
      "SELECT to_regclass('public.analytics_event')::text AS table",
    );
    expect(table).toBeNull();
  }, 60_000);

  it('runs the pg_cron retention function in the schema from any search path', async () => {
    // The image has no pg_cron, so the job itself is not scheduled here: this
    // runs the function the job calls, exactly as 003 defines it, from a
    // connection whose search path is `public`, through the job's qualifier.
    const text = scheduleRetentionDefinition(90, 760, SCHEMA).sql;
    const create = text.slice(
      text.indexOf('CREATE OR REPLACE FUNCTION'),
      text.indexOf('$$;', text.indexOf('LANGUAGE sql')) + 3,
    );
    const sql = await database(DATASOURCE);
    await sql.unsafe(`SET search_path = ${SCHEMA}; ${create}; RESET search_path;`);
    await sql.unsafe(
      `INSERT INTO ${SCHEMA}.analytics_daily_visitor (day, visitor_id) VALUES ('2020-01-01', 'expired'), (current_date, 'current')`,
    );

    await sql.unsafe(`SELECT "${SCHEMA}".analytics_expire()`);

    const rows = await sql.unsafe<{ visitorId: string }[]>(`SELECT visitor_id FROM ${SCHEMA}.analytics_daily_visitor`);
    const visitors = rows.map((row) => row.visitorId);
    expect(visitors).toContain('current');
    expect(visitors).not.toContain('expired');
  }, 60_000);

  it('writes where its migration put the tables, even when the resolution later moves', async () => {
    // A workload source that puts the datasource in a named schema, until it
    // stops contributing: re-resolved at the first write, the schema would be
    // `public`, where no table exists.
    let contributing = true;
    const workload = {
      name: 'workload-migrations',
      migrationSources: (): SQLSource[] =>
        contributing
          ? [
              sqlSourceInline({
                namespace: 'app',
                datasource: { name: PLUGIN_DATASOURCE, schema: PLUGIN_SCHEMA },
                definitions: [{ name: '001_noop', sql: 'SELECT 1;' }],
              }),
            ]
          : [],
    } as unknown as Plugin;
    const sqlPlugin = { name: 'database', designInfraRequirements: () => [] } as unknown as Plugin;
    const plugin = analytics();
    process.env['CONFIG_DATA'] = JSON.stringify({ analytics: { secret: TEST_SECRET, datasource: PLUGIN_DATASOURCE } });
    useTempProjectRoot();
    const app = await createTestApp({ plugins: [sqlPlugin, workload, plugin] });
    try {
      // What prepare() does: read the sources, then apply them.
      const [source] = plugin.migrationSources();
      expect(source?.schema).toBe(PLUGIN_SCHEMA);
      await new Migrator(PLUGIN_DATASOURCE, source?.definitions ?? [], PLUGIN_SCHEMA).up();
      contributing = false;

      const response = await app.fetch('/_putnami/analytics/events', {
        method: 'POST',
        headers: { 'content-type': 'application/json', 'user-agent': CHROME_UA },
        body: BATCH,
      });
      expect(response.status).toBe(202);
    } finally {
      // Stopping drains the queue through the sink.
      await app.stop();
    }

    const sql = await database(PLUGIN_DATASOURCE);
    const rows = await sql.unsafe<{ count: number }[]>(
      `SELECT count(*)::int AS count FROM ${PLUGIN_SCHEMA}.analytics_event`,
    );
    expect(rows[0]?.count).toBe(1);
  }, 60_000);
});

describe.skipIf(!CRON_RAW)('analytics pg_cron retention in a named schema (live)', () => {
  let provisioned: ProvisionResult;
  /** A fresh schema in the pg_cron database, created by the owner role. */
  let schema = '';
  let migrator: Migrator | undefined;

  beforeAll(async () => {
    provisioned = await provision({
      binding: {
        protocolVersion: 1,
        mode: 'require',
        isolation: 'schema',
        databases: {
          [CRON_DATASOURCE]: { engine: 'postgres', schema: 'marketing', connection: serverConnection(CRON_RAW) },
        } as never,
      },
    });
    schema = provisioned.binding.databases[CRON_DATASOURCE]?.schema ?? '';
  }, 60_000);

  afterAll(async () => {
    // Migration state is kept per datasource, not per schema: dropping the
    // schema alone would leave the next run's 001 already applied.
    await migrator?.reset();
    await provisioned?.cleanup();
  }, 60_000);

  it("schedules the job as the login, runs it with the login's rights, and unschedules it", async () => {
    const config = testConfig({ datasource: CRON_DATASOURCE, retentionMode: 'pg_cron' });
    migrator = new Migrator(
      CRON_DATASOURCE,
      createMigrationSource(CRON_DATASOURCE, config, schema).definitions,
      schema,
    );
    await migrator.up();
    const sql = await database(CRON_DATASOURCE);
    const [{ login, db }] = await sql.unsafe<{ login: string; db: string }[]>(
      'SELECT session_user::text AS login, current_database()::text AS db',
    );
    const jobName = `putnami-analytics-retention-${db}-${schema}`;

    // cron.job shows a role its own jobs, and only the login may read it.
    const jobsNamed = (name: string) =>
      sql.begin(async (tx) => {
        await tx.unsafe('SET LOCAL ROLE NONE');
        return tx.unsafe<{ command: string; username: string }[]>(
          'SELECT command, username FROM cron.job WHERE jobname = $1',
          [name],
        );
      });

    // 003 returned to the login before scheduling, so the job runs as it.
    const jobs = await jobsNamed(jobName);
    expect(jobs).toEqual([{ command: `SELECT "${schema}".analytics_expire()`, username: login }]);

    // What the background worker does at 00:15: the job's command, as the
    // login, from its default search path. A schema the login cannot use
    // fails here, where `public` never would.
    await sql.unsafe(
      `INSERT INTO "${schema}".analytics_daily_visitor (day, visitor_id) VALUES ('2020-01-01', 'expired'), (current_date, 'current')`,
    );
    await sql.begin(async (tx) => {
      await tx.unsafe('SET LOCAL ROLE NONE');
      await tx.unsafe('SET LOCAL search_path = DEFAULT');
      await tx.unsafe(jobs[0]?.command ?? '');
    });
    const visitors = await sql.unsafe<{ visitorId: string }[]>(
      `SELECT visitor_id FROM "${schema}".analytics_daily_visitor`,
    );
    expect(visitors.map((row) => row.visitorId)).toEqual(['current']);

    // 003's down unschedules exactly this schema's job.
    await migrator.rollback();
    expect(await jobsNamed(jobName)).toEqual([]);
  }, 60_000);
});

const CHROME_UA =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36';

const BATCH = JSON.stringify({
  protocolVersion: 1,
  sentAt: '2026-09-02T10:00:00.000Z',
  events: [
    {
      eventId: '01920000-0000-7000-8000-000000000001',
      name: 'page_view',
      clientTs: '2026-09-02T09:59:58.000Z',
      seq: 0,
      sessionId: '01920000-0000-7000-8000-0000000000aa',
      page: { path: '/' },
    },
  ],
});
