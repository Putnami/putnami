import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { createDatasourceClient } from '../src/postgres/datasource-client';
import { createDriverPool } from '../src/postgres/driver-pool';
import type { SqlClient } from '../src/sql-client';
import { Column, DateIso, Int, Key, Repository, Table } from '../src/index';
import { __setTransactionConnectionFactory } from '../src/transaction';
import { ensurePostgresContainer, isDockerAvailable, POSTGRES_SETUP_TIMEOUT_MS } from './utils/postgres-helper';

/**
 * A find compiled once per shape reads, from `Bun.SQL` against a real
 * PostgreSQL, what the same find composed from fragments reads: the same rows
 * in the same order, for a point read, compared columns written in snake_case,
 * an ORDER BY, a LIMIT and an OFFSET. The composed twin of each find compares
 * with `{ equals }`, which takes the fragment path. It carries no specTest: it
 * skips without a database, and a skipped attestation would block the enforce
 * gate.
 */
const dockerAvailable = await isDockerAvailable();
const CONNECTION = { hostname: '127.0.0.1', port: 6432, database: 'test', username: 'test', password: 'test' };
const suffix = `${Date.now()}_${process.pid}`;
const schema = `find_shapes_${suffix}`;

const Notes = Table('find_notes', {
  id: Key(String),
  ownerId: Column(String),
  isDone: Column(Boolean),
  rank: Column(Int),
  createdAt: Column(DateIso, { columnName: 'created_at' }),
});

let sql: SqlClient;

describe.skipIf(!dockerAvailable)('a compiled find against PostgreSQL (live)', () => {
  beforeAll(async () => {
    await ensurePostgresContainer('sql-test');
    sql = createDatasourceClient(createDriverPool({ ...CONNECTION, max: 2 }), {
      datasource: 'find-shapes',
      searchPath: schema,
      onEnd: async () => {},
    });
    await sql.unsafe(`CREATE SCHEMA IF NOT EXISTS "${schema}"`);
    await sql.unsafe(
      `CREATE TABLE "${schema}".find_notes (id TEXT PRIMARY KEY, owner_id TEXT, is_done BOOLEAN, rank INT, created_at TIMESTAMPTZ)`,
    );
    await sql.unsafe(
      `INSERT INTO "${schema}".find_notes SELECT 'n' || g, 'o' || (g % 3), g % 2 = 0, g, timestamptz '2026-01-01' + g * interval '1 hour' FROM generate_series(1, 12) g`,
    );
    const client = Promise.resolve(sql);
    __setTransactionConnectionFactory(() => client);
  }, POSTGRES_SETUP_TIMEOUT_MS);

  afterAll(async () => {
    __setTransactionConnectionFactory(undefined);
    await sql?.unsafe(`DROP SCHEMA IF EXISTS "${schema}" CASCADE`);
    await sql?.end();
  });

  it('reads the rows the composed find reads', async () => {
    const notes = new Repository(Notes);
    expect(await notes.get({ id: 'n4' })).toEqual({
      id: 'n4',
      ownerId: 'o1',
      isDone: true,
      rank: 4,
      createdAt: '2026-01-01T04:00:00.000Z',
    });
    expect(await notes.get({ id: 'n4' })).toEqual(await notes.findOne({ id: { equals: 'n4' } } as never));
    expect(await notes.get({ id: 'missing' })).toBeUndefined();

    const shapes: [Record<string, unknown>, { limit?: number; offset?: number; orderBy?: string }][] = [
      [{ ownerId: 'o1' }, { orderBy: 'id' }],
      [{ ownerId: 'o2', isDone: false }, { orderBy: 'rank DESC' }],
      [{ isDone: true }, { orderBy: 'createdAt desc, id', limit: 3, offset: 1 }],
      [{}, { orderBy: 'rank', limit: 5, offset: 4 }],
    ];
    for (const [filters, options] of shapes) {
      const composedFilters = Object.fromEntries(
        Object.entries(filters).map(([key, value]) => [key, { equals: value }]),
      );
      const compiled = await notes.find(filters as never, options);
      const composed =
        Object.keys(filters).length > 0
          ? await notes.find(composedFilters as never, options)
          : await notes.find({ $and: [] } as never, options);
      expect(compiled.length).toBeGreaterThan(0);
      expect(compiled).toEqual(composed);
    }
  });
});
