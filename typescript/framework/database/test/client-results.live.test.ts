import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { createDatasourceClient } from '../src/postgres/datasource-client';
import { createDriverPool } from '../src/postgres/driver-pool';
import type { SqlClient } from '../src/sql-client';
import { ensurePostgresContainer, isDockerAvailable, POSTGRES_SETUP_TIMEOUT_MS } from './utils/postgres-helper';

/**
 * The shape of the results the client hands out, read from `Bun.SQL` against
 * a real PostgreSQL: a result whose columns are already camelCase keeps
 * the driver's rows, and it still answers what every result answers — camelCase
 * keys, plain arrays, `count` and `command`. It carries no specTest: it skips
 * without a database, and a skipped attestation would block the enforce gate.
 */
const dockerAvailable = await isDockerAvailable();
const CONNECTION = { hostname: '127.0.0.1', port: 6432, database: 'test', username: 'test', password: 'test' };

let sql: SqlClient;

describe.skipIf(!dockerAvailable)('client results from Bun.SQL (live)', () => {
  beforeAll(async () => {
    await ensurePostgresContainer('sql-test');
    sql = createDatasourceClient(createDriverPool({ ...CONNECTION, max: 1 }), {
      datasource: 'client-results',
      searchPath: undefined,
      onEnd: async () => {},
    });
  }, POSTGRES_SETUP_TIMEOUT_MS);

  afterAll(async () => {
    await sql?.end();
  });

  it('answers camelCase columns as the driver returned them, with count and command', async () => {
    const rows = await sql`
      SELECT n AS "id", 'task ' || n AS "title", timestamptz '2026-01-01' AS "createdAt"
      FROM generate_series(1, ${20}::int) AS n
    `;
    expect(rows.count).toBe(20);
    expect(rows.command).toBe('SELECT');
    expect(rows).toHaveLength(20);
    expect(Object.keys(rows[0] as object)).toEqual(['id', 'title', 'createdAt']);
    expect(rows[19]).toEqual({ id: 20, title: 'task 20', createdAt: new Date('2026-01-01T00:00:00Z') });
  });

  it('answers the same shape whether or not its columns needed a copy', async () => {
    const camel = await sql`SELECT ${1}::int AS "userId"`;
    const snake = await sql`SELECT ${1}::int AS user_id`;
    expect(Reflect.ownKeys(camel)).toEqual(Reflect.ownKeys(snake));
    expect(Object.getPrototypeOf(camel)).toBe(Object.getPrototypeOf(snake));
    expect(camel.constructor).toBe(snake.constructor);
  });

  it('still turns the typed arrays of a camelCase result into plain arrays', async () => {
    const [row] = await sql`
      SELECT ${1}::int AS "id", ARRAY[1, 2]::int4[] AS "scores", ARRAY[0.5]::float8[] AS "ratios"
    `;
    expect(Array.isArray(row?.['scores'])).toBe(true);
    expect(row?.['scores']).toEqual([1, 2]);
    expect(Array.isArray(row?.['ratios'])).toBe(true);
    expect(row?.['ratios']).toEqual([0.5]);
  });

  it('still gives snake_case columns their camelCase keys', async () => {
    const [row] = await sql`SELECT ${7}::int AS user_id, 'x' AS display_name`;
    expect(row).toEqual({ userId: 7, displayName: 'x' });
  });
});
