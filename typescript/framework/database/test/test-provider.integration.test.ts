import { describe, expect, it } from 'bun:test';
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { emitMigrationBundle } from '@putnami/migration';
import { resetConfigLoader } from '@putnami/runtime';
import { SQL } from 'bun';
import { closeAllDatabases, database } from '../src/factory';
import { poolClient } from '../src/postgres/sql-query';
import type { SqlClient } from '../src/sql-client';
import { SQLSource } from '../src/migrations/sql-source';
import {
  type DatabaseBindingEntry,
  type DatabaseConnectionSpec,
  type DatabaseTestBinding,
  parseTestBinding,
} from '../src/postgres/binding';
import {
  connectionDatabase,
  isolatedPrefix,
  isolationOf,
  provision,
  setConnectionDatabase,
} from '../src/test-provider';
import { isolatedSuffix, ORPHAN_AGE_MS } from '../src/test-provider-reclaim';

// Live provisioning against an externally provided Postgres. Runs only when
// DATABASE_TEST_BINDINGS is injected (CI service container / local server), so
// unit runs — including this container's — pay no database cost.
//
// It provisions, then proves each provisioned datasource is reachable through
// the workload's own `database(name)` factory (which the provider points at the
// isolated databases via DATABASE_BINDINGS) and that the owning schema is the
// live search_path — the end-to-end contract, identical in spirit to the Go
// provider's integration test.
const HAS_BINDING = !!process.env['DATABASE_TEST_BINDINGS']?.trim();

describe('provision (live)', () => {
  it.skipIf(!HAS_BINDING)('provisions reachable datasources with the owning schema on search_path', async () => {
    const result = await provision({ sources: [] });
    try {
      const names = Object.keys(result.binding.databases ?? {});
      expect(names.length).toBeGreaterThan(0);

      for (const name of names) {
        const entry = result.binding.databases?.[name];
        const sql = await database(name);
        const [{ one }] = await sql<{ one: number }[]>`SELECT 1 AS one`;
        expect(one).toBe(1);
        if (entry?.schema) {
          // The client returns row keys in camelCase: the search_path column
          // is searchPath.
          const [{ searchPath }] = await sql<{ searchPath: string }[]>`SHOW search_path`;
          const onPath = searchPath
            .split(',')
            .map((p) => p.trim().replace(/^["']|["']$/g, ''))
            .includes(entry?.schema);
          expect(onPath).toBe(true);
        }
      }
    } finally {
      await closeAllDatabases();
      await result.cleanup();
    }
  });

  it.skipIf(!HAS_BINDING)('keeps the provisioned databases authoritative over a managed config binding', async () => {
    const result = await provision({ sources: [] });
    // A managed `database` config-section binding appearing in the test
    // environment (here via CONFIG_DATA) must NOT re-route the suite: the
    // provider pins its provisioned binding above both deploy transports.
    process.env['CONFIG_DATA'] = JSON.stringify({
      database: {
        protocolVersion: 1,
        databases: {
          [Object.keys(result.binding.databases ?? {})[0] as string]: {
            engine: 'postgres',
            schema: 'managed',
            connection: { host: 'managed-db.invalid', port: 5432, database: 'managed', user: 'x', password: 'x' },
          },
        },
      },
    });
    resetConfigLoader();
    try {
      for (const name of Object.keys(result.binding.databases ?? {})) {
        const entry = result.binding.databases?.[name];
        const sql = await database(name);
        const [{ db }] = await sql<{ db: string }[]>`SELECT current_database() AS db`;
        // The provisioned isolated database, not the managed one.
        expect(db).toBe(entry?.connection?.database ?? '');
      }
    } finally {
      delete process.env.CONFIG_DATA;
      resetConfigLoader();
      await closeAllDatabases();
      await result.cleanup();
    }
  });
});

// Reclaiming what a killed suite left behind, against a real server. It needs a
// binding that provisions isolated databases and drops them: schema isolation
// creates no database, and keepDatabases means the server dies with the run.
const LIVE_BINDING = HAS_BINDING ? parseTestBinding(process.env['DATABASE_TEST_BINDINGS'] as string) : undefined;
const RECLAIMS = !!LIVE_BINDING && isolationOf(LIVE_BINDING) === 'database' && !LIVE_BINDING.keepDatabases;

function adminFor(conn: DatabaseConnectionSpec, db?: string): SqlClient {
  const target = { ...conn };
  if (db) {
    setConnectionDatabase(target, db);
  }
  if (target.dsn) {
    return poolClient(new SQL({ url: target.dsn, max: 1 }));
  }
  return poolClient(
    new SQL({
      hostname: target.host,
      port: target.port,
      database: target.database,
      username: target.user,
      password: target.password,
      tls: target.ssl ? 'require' : false,
      max: 1,
    }),
  );
}

describe('provision reclaims orphans (live)', () => {
  it.skipIf(!RECLAIMS)("drops a killed suite's database and never a live suite's", async () => {
    const tb = LIVE_BINDING as DatabaseTestBinding;
    const entry = Object.values(tb.databases ?? {})[0] as DatabaseBindingEntry;
    const conn = entry.connection as DatabaseConnectionSpec;
    const prefix = isolatedPrefix(connectionDatabase(conn) || 'reclaim');
    const admin = adminFor(conn);
    const old = new Date(Date.now() - 2 * ORPHAN_AGE_MS);
    const orphan = prefix + isolatedSuffix(old);
    const live = prefix + isolatedSuffix(old);
    const young = prefix + isolatedSuffix(new Date(Date.now() - 60_000));
    const exists = async (name: string): Promise<boolean> =>
      (await admin`SELECT 1 FROM pg_database WHERE datname = ${name}`).length > 0;
    let liveClient: SqlClient | undefined;
    try {
      for (const name of [orphan, live, young]) {
        await admin.unsafe(`CREATE DATABASE "${name}"`);
      }
      // A live suite holding a connection on its database.
      liveClient = adminFor(conn, live);
      await liveClient`SELECT 1`;

      const result = await provision({
        sources: [],
        binding: { protocolVersion: 1, mode: 'require', isolation: 'database', databases: { reclaim: entry } },
      });
      const provisioned = result.binding.databases?.['reclaim']?.connection?.database as string;

      expect(await exists(orphan)).toBe(false);
      expect(await exists(live)).toBe(true);
      expect(await exists(young)).toBe(true);

      await result.cleanup();
      expect(await exists(provisioned)).toBe(false);
    } finally {
      await liveClient?.end();
      for (const name of [orphan, live, young]) {
        await admin.unsafe(`DROP DATABASE IF EXISTS "${name}" WITH (FORCE)`).catch(() => {});
      }
      await admin.end();
    }
  });
});

describe("provision applies the project's migration bundle (live)", () => {
  const tasks = new SQLSource({
    namespace: 'tasks',
    datasource: { name: 'default', schema: 'public' },
    definitions: [
      { name: '20260101000000_create_tasks', sql: 'CREATE TABLE tasks (id INT PRIMARY KEY, title TEXT NOT NULL);' },
    ],
  });

  // The binding `putnami test` passes, on the live server, with the isolation
  // under test.
  function bindingFor(isolation: 'database' | 'schema'): DatabaseTestBinding {
    const tb = LIVE_BINDING as DatabaseTestBinding;
    const entry = Object.values(tb.databases ?? {})[0] as DatabaseBindingEntry;
    return {
      protocolVersion: 1,
      mode: 'require',
      isolation,
      applyMigrations: true,
      databases: { default: { ...entry, schema: 'public' } },
    };
  }

  // Each isolation runs where the live server's binding uses it: a server set
  // up for database isolation need not let its role create schemas in the
  // maintenance database.
  for (const isolation of ['database', 'schema'] as const) {
    it.skipIf(!LIVE_BINDING || isolationOf(LIVE_BINDING) !== isolation)(
      `migrates each suite's ${isolation} with no sources passed`,
      async () => {
        const projectRoot = mkdtempSync(join(tmpdir(), 'provision-live-'));
        await emitMigrationBundle([tasks], 'tasks-api', projectRoot);

        // Two suites in turn, as two test processes of one run: the second must
        // not read the first's migrations as already applied.
        for (const title of ['first suite', 'second suite']) {
          const result = await provision({ binding: bindingFor(isolation), projectRoot });
          const schema = result.binding.databases?.['default']?.schema as string;
          try {
            const sql = await database('default');
            await sql`INSERT INTO tasks (id, title) VALUES (1, ${title})`;
            const rows = await sql<{ title: string }[]>`SELECT title FROM tasks`;
            expect(rows.map((r) => r.title)).toEqual([title]);
            const where = await sql<{ schema: string }[]>`
            SELECT table_schema AS schema FROM information_schema.tables WHERE table_name = 'tasks'`;
            expect(where.map((r) => r.schema)).toEqual([schema]);
          } finally {
            await closeAllDatabases();
            await result.cleanup();
          }
        }
      },
    );
  }
});
