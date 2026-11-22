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

// A pool of its own (another startup parameter, another identity), sized 4.
const ON_DEMAND_APP = `on_demand_${suffix}`;
const ON_DEMAND_CONNECTION = {
  ...CONNECTION,
  params: { pool_max_conns: '4', application_name: ON_DEMAND_APP },
};

const BINDING = JSON.stringify({
  protocolVersion: 1,
  databases: {
    on_demand: { engine: 'postgres', schema: 'public', connection: ON_DEMAND_CONNECTION },
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

  it('runs builders and nested fragments on the connection of the statement, with camelCase rows and SQLSTATE codes', async () => {
    // The Bun.SQL behaviors the client relies on (doc/adr/0003 and 0005): a
    // builder made by the pool and a query used as a fragment both work inside
    // a statement of a reserved connection.
    const a = await database('shared_a');
    await a`CREATE TABLE items (item_id int PRIMARY KEY, display_name text, note text DEFAULT 'from-default')`;
    const rows = [
      { itemId: 1, displayName: 'one', note: undefined },
      { itemId: 2, displayName: 'two', note: 'given' },
    ];
    await a`INSERT INTO ${a('items')} ${a(rows, 'itemId', 'displayName', 'note')}`;

    const where = a`${a('itemId')} IN ${a([1, 2])}`;
    const found = await a`SELECT * FROM items WHERE ${where} ORDER BY item_id`;
    // Keys come back camelCase, and an undefined field was written as NULL.
    expect<unknown[]>([...found]).toEqual([
      { itemId: 1, displayName: 'one', note: null },
      { itemId: 2, displayName: 'two', note: 'given' },
    ]);
    expect(found.count).toBe(2);
    expect(found.command).toBe('SELECT');

    const duplicate = await Promise.resolve(a`INSERT INTO items (item_id) VALUES (1)`).catch((error: unknown) => error);
    expect(duplicate).toMatchObject({ code: '23505', driverCode: 'ERR_POSTGRES_SERVER_ERROR' });

    const reserved = await a.reserve();
    try {
      const [row] = await reserved`SELECT ${reserved('displayName')} FROM items WHERE ${a`item_id = ${2}`}`;
      expect<unknown>(row).toEqual({ displayName: 'two' });
    } finally {
      reserved.release();
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

  it('opens connections as concurrent demand needs them, up to poolSize, and reads arrays as plain arrays', async () => {
    const sql = await database('on_demand');
    const backends = async (): Promise<number> => {
      const [row] = await admin<{ n: number }[]>`
        SELECT count(*)::int AS n FROM pg_stat_activity WHERE application_name = ${ON_DEMAND_APP}
      `;
      return row?.n ?? -1;
    };
    try {
      for (let i = 0; i < 3; i++) {
        await sql`SELECT 1`;
      }
      expect(await backends()).toBe(1);

      await Promise.all(Array.from({ length: 3 }, () => sql`SELECT pg_sleep(0.2)`));
      expect(await backends()).toBe(3);

      await Promise.all(Array.from({ length: 8 }, () => sql`SELECT pg_sleep(0.1)`));
      expect(await backends()).toBe(4);

      // A bound parameter selects the binary protocol, where the driver reads
      // int4[] and float4[] as typed arrays; the client hands out plain ones.
      const [row] = await sql`
        SELECT ARRAY[1, 2]::int4[] AS nums, ARRAY[0.5]::float4[] AS halves, ARRAY[0.5]::float8[] AS ratios
        WHERE ${1}::int = 1
      `;
      for (const column of ['nums', 'halves', 'ratios']) {
        expect(Array.isArray(row?.[column])).toBe(true);
      }
      expect(row?.['nums']).toEqual([1, 2]);
      expect(row?.['halves']).toEqual([0.5]);
      expect(row?.['ratios']).toEqual([0.5]);
    } finally {
      await closeDatabase('on_demand');
    }
  }, 60_000);
});
