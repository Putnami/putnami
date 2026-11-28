import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { createDatasourceClient } from '../src/postgres/datasource-client';
import { createDriverPool } from '../src/postgres/driver-pool';
import type { SqlClient } from '../src/sql-client';
import { Column, DateIso, Int, Key, Optional, Repository, RepositoryError, Table } from '../src/index';
import { __setTransactionConnectionFactory } from '../src/transaction';
import { ensurePostgresContainer, isDockerAvailable, POSTGRES_SETUP_TIMEOUT_MS } from './utils/postgres-helper';

/**
 * A save compiled once per shape writes and returns, from `Bun.SQL` against a
 * real PostgreSQL, what the same save composed from fragments writes and
 * returns: an insert, an update on conflict, a key-only save, and the errors
 * of a constraint and of a column the table lacks. The composed twin of each
 * save is `saveMany` of the one entity on a twin table, which still composes
 * its statement from fragments. It carries no specTest: it skips without a
 * database, and a skipped attestation would block the enforce gate.
 */
const dockerAvailable = await isDockerAvailable();
const CONNECTION = { hostname: '127.0.0.1', port: 6432, database: 'test', username: 'test', password: 'test' };
const suffix = `${Date.now()}_${process.pid}`;
const schema = `save_shapes_${suffix}`;

const columns = {
  id: Key(String),
  title: Column(String),
  ownerId: Column(Optional(String)),
  rank: Column(Optional(Int)),
  createdAt: Column(DateIso, { columnName: 'created_at', default: 'NOW()' }),
};
const Compiled = Table('save_notes_compiled', columns);
const Composed = Table('save_notes_composed', columns);
/** A column the table declares and the database lacks. */
const ghost = { ...columns, ghost: Column(Optional(String)) };
const CompiledGhost = Table('save_notes_compiled', ghost);
const ComposedGhost = Table('save_notes_composed', ghost);

let sql: SqlClient;

/** What a save settles to: the saved entity, or the code and SQLSTATE of its error. */
async function outcome(pending: Promise<unknown>): Promise<unknown> {
  try {
    return { saved: await pending };
  } catch (error) {
    const cause = (error as RepositoryError).cause as { code?: unknown } | undefined;
    return { error: (error as RepositoryError).code, sqlstate: cause?.code };
  }
}

describe.skipIf(!dockerAvailable)('a compiled save against PostgreSQL (live)', () => {
  beforeAll(async () => {
    await ensurePostgresContainer('sql-test');
    sql = createDatasourceClient(createDriverPool({ ...CONNECTION, max: 2 }), {
      datasource: 'save-shapes',
      searchPath: schema,
      onEnd: async () => {},
    });
    await sql.unsafe(`CREATE SCHEMA IF NOT EXISTS "${schema}"`);
    for (const table of ['save_notes_compiled', 'save_notes_composed']) {
      await sql.unsafe(
        `CREATE TABLE "${schema}".${table} (id TEXT PRIMARY KEY, title TEXT UNIQUE, owner_id TEXT, rank INT NOT NULL DEFAULT 0, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
      );
    }
    const client = Promise.resolve(sql);
    __setTransactionConnectionFactory(() => client);
  }, POSTGRES_SETUP_TIMEOUT_MS);

  afterAll(async () => {
    __setTransactionConnectionFactory(undefined);
    await sql?.unsafe(`DROP SCHEMA IF EXISTS "${schema}" CASCADE`);
    await sql?.end();
  });

  it('writes and returns what the composed save writes and returns', async () => {
    const compiled = new Repository(Compiled);
    const composed = new Repository(Composed);
    const saves: Record<string, unknown>[] = [
      { id: 'n1', title: 'First', ownerId: 'o1', rank: 3, createdAt: '2026-01-01T00:00:00.000Z' },
      { rank: 4, id: 'n2', title: 'Second', createdAt: '2026-01-02T00:00:00.000Z' },
      // An update on conflict, of some columns, one of them to NULL.
      { id: 'n1', title: 'First again', ownerId: undefined },
      // Only the key: the existing row comes back unchanged.
      { id: 'n2' },
      // A constraint of the table the row breaks: NOT NULL, then UNIQUE.
      { id: 'n3', title: 'Third', rank: null, createdAt: '2026-01-03T00:00:00.000Z' },
      { id: 'n3', title: 'Second', createdAt: '2026-01-03T00:00:00.000Z' },
    ];
    const outcomes: unknown[] = [];
    for (const entity of saves) {
      const one = await outcome(compiled.save(entity as never));
      const twin = await outcome(composed.saveMany([entity as never]).then((rows) => rows[0]));
      expect(one).toEqual(twin);
      outcomes.push(one);
    }
    expect(outcomes.slice(4)).toEqual([
      { error: 'NOT_NULL_VIOLATION', sqlstate: '23502' },
      { error: 'UNIQUE_VIOLATION', sqlstate: '23505' },
    ]);
    expect(outcomes.slice(0, 4)).toEqual([
      { saved: { id: 'n1', title: 'First', ownerId: 'o1', rank: 3, createdAt: '2026-01-01T00:00:00.000Z' } },
      { saved: { id: 'n2', title: 'Second', ownerId: null, rank: 4, createdAt: '2026-01-02T00:00:00.000Z' } },
      { saved: { id: 'n1', title: 'First again', ownerId: null, rank: 3, createdAt: '2026-01-01T00:00:00.000Z' } },
      { saved: { id: 'n2', title: 'Second', ownerId: null, rank: 4, createdAt: '2026-01-02T00:00:00.000Z' } },
    ]);
    const rows = async (table: string) =>
      (await sql.unsafe(`SELECT id, title, owner_id, rank, created_at FROM "${schema}".${table} ORDER BY id`)).map(
        (row) => ({ ...row }),
      );
    expect(await rows('save_notes_compiled')).toEqual(await rows('save_notes_composed'));
    expect(await rows('save_notes_compiled')).toHaveLength(2);
  });

  it('fails on a column the table lacks as the composed save does', async () => {
    const entity = { id: 'g1', title: 'Ghost', rank: 1, ghost: 'boo' };
    const one = await outcome(new Repository(CompiledGhost).save(entity));
    const twin = await outcome(new Repository(ComposedGhost).saveMany([entity]).then((saved) => saved[0]));
    expect(one).toEqual({ error: 'SAVE_ERROR', sqlstate: '42703' });
    expect(one).toEqual(twin);
    const error = await new Repository(CompiledGhost).save(entity).catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(RepositoryError);
    expect((error as Error).message).toBe('Failed to save entity');
  });
});
