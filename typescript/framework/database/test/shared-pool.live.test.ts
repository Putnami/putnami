import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { runInContext } from '@putnami/runtime';
import { closeAllDatabases, closeDatabase, database, physicalPoolCount } from '../src/factory';
import { setDatabaseBindingOverride } from '../src/postgres/binding';
import { ensurePostgresContainer, isDockerAvailable, POSTGRES_SETUP_TIMEOUT_MS } from './utils/postgres-helper';
import { setPrimaryDatasource } from '../src/primary-datasource';
import type { SqlClient } from '../src/sql-client';
import { runInTransaction, useTxConnection } from '../src/transaction';

/**
 * The behavior proof of the shared physical pool against a real PostgreSQL,
 * mirroring go/framework/database/shared_pool_live_test.go: two datasources on
 * one database, each owning a schema with a same-named table holding a
 * different row, share ONE physical pool (physicalPoolCount() === 1) and —
 * under concurrent, interleaved use on a pool small enough that connections
 * change hands between the two datasources — every unqualified query resolves
 * in its own datasource's schema, through the logical client (autocommit),
 * runInTransaction/useTxConnection, and reserve(). It deliberately carries no
 * specTest: it skips without a database, and a skipped attestation would block
 * the enforce gate.
 */
const ADMIN = 'sql-test';
const dockerAvailable = await isDockerAvailable();

const suffix = `${Date.now()}_${process.pid}`;
const schemaA = `shared_pool_a_${suffix}`;
const schemaB = `shared_pool_b_${suffix}`;

const CONNECTION = {
  host: '127.0.0.1',
  port: 6432,
  database: 'test',
  user: 'test',
  password: 'test',
  ssl: false,
  // poolSize 3 through the binding's pool parameter, so eight workers must
  // hand connections back and forth between the two datasources.
  params: { pool_max_conns: '3' },
};

const BINDING = JSON.stringify({
  protocolVersion: 1,
  databases: {
    shared_admin: { engine: 'postgres', schema: 'public', connection: CONNECTION },
    shared_a: { engine: 'postgres', schema: schemaA, connection: CONNECTION },
    shared_b: { engine: 'postgres', schema: schemaB, connection: CONNECTION },
  },
});

let previousOverride: string | undefined;
let admin: SqlClient;

describe.skipIf(!dockerAvailable)('shared physical pool — two datasources, one database (live)', () => {
  beforeAll(async () => {
    await ensurePostgresContainer(ADMIN);
    // Every earlier suite's pool on this database was opened with the default
    // tuning; end them so the binding below opens the shared pool with its own.
    await closeAllDatabases();
    setPrimaryDatasource(undefined);
    previousOverride = setDatabaseBindingOverride(BINDING);

    admin = await database('shared_admin');
    for (const [schema, row] of [
      [schemaA, 'row-of-a'],
      [schemaB, 'row-of-b'],
    ]) {
      await admin.unsafe(`CREATE SCHEMA "${schema}"`);
      await admin.unsafe(`CREATE TABLE "${schema}".t (v text NOT NULL)`);
      await admin.unsafe(`INSERT INTO "${schema}".t (v) VALUES ($1)`, [row]);
    }
  }, POSTGRES_SETUP_TIMEOUT_MS);

  afterAll(async () => {
    try {
      const cleanup = await database('shared_admin');
      for (const schema of [schemaA, schemaB]) {
        await cleanup.unsafe(`DROP SCHEMA IF EXISTS "${schema}" CASCADE`);
      }
    } finally {
      await closeAllDatabases();
      setDatabaseBindingOverride(previousOverride);
    }
  });

  it('serves every unqualified query in its own schema under interleaved concurrent use', async () => {
    const a = await database('shared_a');
    const b = await database('shared_b');
    expect(a).not.toBe(b);
    expect(physicalPoolCount()).toBe(1);

    type Source = { name: string; sql: SqlClient; schema: string; row: string };
    const sources: Source[] = [
      { name: 'shared_a', sql: a, schema: schemaA, row: 'row-of-a' },
      { name: 'shared_b', sql: b, schema: schemaB, row: 'row-of-b' },
    ];

    const check = async (source: Source, path: string, sql: SqlClient) => {
      const [{ v }] = await sql`SELECT v FROM t`;
      if (v !== source.row) {
        throw new Error(
          `${path}: SELECT v FROM t on ${source.schema} = ${JSON.stringify(v)}, want ${source.row} (wrong schema)`,
        );
      }
      const [row] = await sql`SHOW search_path`;
      const searchPath = String(row['searchPath'] ?? row['search_path']).replace(/"/g, '');
      if (searchPath !== source.schema) {
        throw new Error(`${path}: search_path on ${source.schema} = ${searchPath}, want ${source.schema}`);
      }
    };

    const round = async (source: Source) => {
      // Autocommit through the logical client.
      await check(source, 'autocommit', source.sql);
      // Inside the request transaction: the reserved connection was bound to
      // this datasource when the transaction opened.
      await runInContext({}, () =>
        runInTransaction(async () => {
          const tx = await useTxConnection(source.name, 'write');
          await check(source, 'runInTransaction', tx);
        }),
      );
      // The advanced surface: a pinned connection the caller drives itself.
      const reserved = await source.sql.reserve();
      try {
        await check(source, 'reserve', reserved);
      } finally {
        reserved.release();
      }
    };

    const workers = 8;
    const iterations = 40;
    const failures: string[] = [];
    await Promise.all(
      Array.from({ length: workers }, async (_, g) => {
        for (let i = 0; i < iterations; i++) {
          try {
            await round(sources[(g + i) % 2]);
          } catch (err) {
            failures.push(`worker ${g} iteration ${i}: ${err instanceof Error ? err.message : String(err)}`);
            return;
          }
        }
      }),
    );
    expect(failures).toEqual([]);
    expect(physicalPoolCount()).toBe(1);

    // Closing one datasource keeps the other working on the shared pool.
    await closeDatabase('shared_a');
    expect(physicalPoolCount()).toBe(1);
    await check(sources[1], 'after closing a', b);
    await expect(Promise.resolve(a`SELECT 1`)).rejects.toThrow('is closed');
  }, 120_000);
});
