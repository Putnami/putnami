import { describe, expect, it } from 'bun:test';
import type { SQL } from 'bun';
import { normalizeError, poolClient } from '../src/postgres/sql-query';

type DriverResult = unknown[] & { count: number; command: string };

function result(rows: unknown[], command = 'SELECT'): DriverResult {
  return Object.assign(rows, { count: rows.length, command });
}

/**
 * A driver stand-in whose every statement answers `answer`, so the shape the
 * client hands out can be asserted without a database. A mode method records
 * itself and keeps the same answer.
 */
function fakeDriver(answer: unknown) {
  const modes: string[] = [];
  const driver = (_strings: TemplateStringsArray) => {
    const query = Promise.resolve(answer) as Promise<unknown> & Record<string, unknown>;
    for (const mode of ['execute', 'values', 'raw', 'simple']) {
      query[mode] = () => {
        if (mode !== 'execute') modes.push(mode);
        return query;
      };
    }
    return query;
  };
  Object.assign(driver, { reserve: async () => driver, release: () => {}, end: async () => {} });
  return { sql: poolClient(driver as unknown as SQL), modes };
}

describe('client results', () => {
  it('gives row keys in camelCase and turns typed arrays into plain arrays, keeping count and command', async () => {
    const { sql } = fakeDriver(
      result([
        { user_id: 1, scores: new Int32Array([1, 2]), ratios: new Float32Array([0.5]), blob: new Uint8Array([7]) },
      ]),
    );
    const rows = await sql`SELECT 1`;
    expect(rows.count).toBe(1);
    expect(rows.command).toBe('SELECT');
    const [row] = rows;
    expect(row?.['userId']).toBe(1);
    expect(row?.['scores']).toEqual([1, 2]);
    expect(Array.isArray(row?.['scores'])).toBe(true);
    expect(row?.['ratios']).toEqual([0.5]);
    // A byte buffer stays one.
    expect(row?.['blob']).toBeInstanceOf(Uint8Array);
  });

  it('keeps the count and command of each nested result of a multi-statement values() query', async () => {
    const nested = result([result([[1, new Int32Array([3])]], 'SELECT'), result([], 'UPDATE')]);
    const { sql, modes } = fakeDriver(nested);
    const outcome = (await sql`SELECT 1; UPDATE t SET x = 1`.simple().values()) as unknown as DriverResult[];
    expect(modes).toEqual(['values', 'simple']);
    expect(outcome).toHaveLength(2);
    expect(outcome[0]?.command).toBe('SELECT');
    expect(outcome[0]?.count).toBe(1);
    expect(outcome[0]?.[0]).toEqual([1, [3]]);
    expect(outcome[1]?.command).toBe('UPDATE');
  });

  it('keeps the rows of a single values() query as arrays of plain values', async () => {
    const { sql } = fakeDriver(result([[1, new Int32Array([4, 5])]]));
    const rows = await sql`SELECT 1`.values();
    expect(rows).toEqual([[1, [4, 5]]] as never);
    expect(rows.count).toBe(1);
  });
});

describe('server errors', () => {
  it('carry the SQLSTATE in code and keep the driver code', () => {
    const error = Object.assign(new Error('duplicate key'), { code: 'ERR_POSTGRES_SERVER_ERROR', errno: '23505' });
    expect(normalizeError(error)).toBe(error);
    expect(error.code).toBe('23505');
    expect((error as unknown as { driverCode: string }).driverCode).toBe('ERR_POSTGRES_SERVER_ERROR');
    // Normalizing twice changes nothing.
    normalizeError(error);
    expect((error as unknown as { driverCode: string }).driverCode).toBe('ERR_POSTGRES_SERVER_ERROR');
  });

  it('leave a client-side error code alone', () => {
    const error = Object.assign(new Error('refused'), { code: 'ERR_POSTGRES_CONNECTION_REFUSED' });
    normalizeError(error);
    expect(error.code).toBe('ERR_POSTGRES_CONNECTION_REFUSED');
  });
});
