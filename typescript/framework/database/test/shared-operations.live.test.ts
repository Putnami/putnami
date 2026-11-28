import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { SQL } from 'bun';
import { createDatasourceClient } from '../src/postgres/datasource-client';
import { createDriverPool, type DriverPoolOptions } from '../src/postgres/driver-pool';
import type { SqlClient } from '../src/sql-client';
import { ensurePostgresContainer, isDockerAvailable, POSTGRES_SETUP_TIMEOUT_MS } from './utils/postgres-helper';

/**
 * The live proof of shared operations (doc/adr/0007): statements of several
 * operations follow one another on one connection, and none of them runs under
 * another path than its datasource's — across a lifetime retirement, a failing
 * neighbour, a cancelled neighbour and a connection the server terminates. A
 * same-named table in `public` holds another row, so a statement that ran on a
 * session at the server default would read it. It carries no specTest: it
 * skips without a database, and a skipped attestation would block the enforce
 * gate.
 */
const dockerAvailable = await isDockerAvailable();
const suffix = `${Date.now()}_${process.pid}`;
const schema = `shared_ops_${suffix}`;
const table = `t_${suffix}`;
const CONNECTION = { hostname: '127.0.0.1', port: 6432, database: 'test', username: 'test', password: 'test' };

let admin: SQL;

/** A statement a member sent, and when. */
interface Sent {
  text: string;
  at: number;
}

/** A datasource client bound to `schema` over a pool of its own; `sent` records each statement a member sends. */
function client(max: number, maxLifetime?: number, sent?: Sent[]): SqlClient {
  const record = (options: DriverPoolOptions): SQL => {
    const member = new SQL(options);
    if (!sent) return member;
    return new Proxy(member, {
      get(target, property) {
        if (property === 'unsafe') {
          return (text: string, parameters?: unknown[]) => {
            sent.push({ text, at: performance.now() });
            return target.unsafe(text, parameters as never);
          };
        }
        const value = Reflect.get(target, property, target);
        return typeof value === 'function' ? value.bind(target) : value;
      },
    });
  };
  const physical = createDriverPool({ ...CONNECTION, max, maxLifetime } as never, record);
  return createDatasourceClient(physical as never, {
    datasource: 'shared_ops',
    searchPath: schema,
    onEnd: async () => {
      await physical.end();
    },
  });
}

/** The backend that serves `sql`'s next statement, and the row an unqualified statement reads there. */
async function readOn(sql: SqlClient, extra = ''): Promise<{ v: string; pid: number } | string> {
  try {
    const [row] = await sql.unsafe(`SELECT v, pg_backend_pid() AS pid${extra} FROM ${table}`);
    return row as { v: string; pid: number };
  } catch (error) {
    return `error:${(error as { code?: string }).code ?? 'unknown'}`;
  }
}

/** Reads the row an unqualified statement resolves to, or the error code it fails with. */
async function read(sql: SqlClient, extra = ''): Promise<string> {
  try {
    const [row] = await sql.unsafe(`SELECT v${extra} FROM ${table}`);
    return String((row as { v: string }).v);
  } catch (error) {
    return `error:${(error as { code?: string }).code ?? 'unknown'}`;
  }
}

describe.skipIf(!dockerAvailable)('shared operations on one connection (live)', () => {
  beforeAll(async () => {
    await ensurePostgresContainer('sql-test');
    admin = new SQL({ ...CONNECTION, max: 1 });
    await admin.unsafe(`CREATE SCHEMA "${schema}"`);
    await admin.unsafe(`CREATE TABLE "${schema}"."${table}" (v text NOT NULL)`);
    await admin.unsafe(`INSERT INTO "${schema}"."${table}" (v) VALUES ('row-of-schema')`);
    await admin.unsafe(`CREATE TABLE public."${table}" (v text NOT NULL)`);
    await admin.unsafe(`INSERT INTO public."${table}" (v) VALUES ('row-of-public')`);
  }, POSTGRES_SETUP_TIMEOUT_MS);

  afterAll(async () => {
    await admin.unsafe(`DROP SCHEMA IF EXISTS "${schema}" CASCADE`);
    await admin.unsafe(`DROP TABLE IF EXISTS public."${table}"`);
    await admin.close();
  });

  it('runs no shared statement under another path when a connection reaches its lifetime', async () => {
    const sql = client(1, 1);
    try {
      // A stream of short statements, several in flight on the only connection, runs across its
      // one-second lifetime: statements keep finishing, so only the lifetime rule can keep a late
      // one off the retiring connection.
      const answers: Array<{ v: string; pid: number } | string> = [];
      const inFlight = new Set<Promise<void>>();
      const start = performance.now();
      while (performance.now() - start < 1600) {
        if (inFlight.size < 4) {
          const running: Promise<void> = readOn(sql, ', pg_sleep(0.01)::text AS slept').then((answer) => {
            answers.push(answer);
            inFlight.delete(running);
          });
          inFlight.add(running);
        }
        await Bun.sleep(2);
      }
      await Promise.all(inFlight);
      expect(answers.length).toBeGreaterThan(40);
      for (const answer of answers) expect(answer).toMatchObject({ v: 'row-of-schema' });
      // The connection was retired and replaced during the stream.
      expect(new Set(answers.map((answer) => (answer as { pid: number }).pid)).size).toBeGreaterThan(1);
    } finally {
      await sql.end();
    }
  });

  it('fails only the statement that fails, or is cancelled, among shared neighbours', async () => {
    const sql = client(1);
    try {
      expect(await read(sql)).toBe('row-of-schema');
      const neighbours = [read(sql), read(sql, ', (1/0)::text AS boom'), read(sql)];
      expect(await Promise.all(neighbours)).toEqual(['row-of-schema', 'error:22012', 'row-of-schema']);

      const running = sql.unsafe(`SELECT v, pg_sleep(1)::text FROM ${table}`);
      const runningAnswer = Promise.resolve(running).then(
        () => 'finished',
        (error: { code?: string }) => `error:${error.code}`,
      );
      await Bun.sleep(50);
      const queued = sql.unsafe(`SELECT v, pg_sleep(0.2)::text FROM ${table}`);
      const queuedAnswer = Promise.resolve(queued).then(
        () => 'finished',
        (error: { code?: string }) => `cancelled:${error.code}`,
      );
      const after = read(sql);
      await Bun.sleep(50);
      queued.cancel();
      expect(await runningAnswer).toBe('finished');
      expect(await queuedAnswer).toStartWith('cancelled:');
      expect(await after).toBe('row-of-schema');

      // Cancelling the running statement fails it alone: the statement that joined it answers.
      await sql.end();
      const sent: Sent[] = [];
      const shared = client(1, undefined, sent);
      expect(await read(shared)).toBe('row-of-schema');
      const head = shared.unsafe(`SELECT v, pg_sleep(1)::text FROM ${table}`);
      const headAnswer = Promise.resolve(head).then(
        () => 'finished',
        (error: { code?: string }) => `cancelled:${error.code}`,
      );
      await Bun.sleep(20);
      const behind = readOn(shared);
      await Bun.sleep(50);
      const cancelledAt = performance.now();
      head.cancel();
      expect(await headAnswer).toStartWith('cancelled:');
      expect(await behind).toMatchObject({ v: 'row-of-schema' });
      // The statement behind was already sent on the connection when the running one was cancelled.
      const behindSent = sent.find((entry) => entry.text.includes('pg_backend_pid()'));
      expect(behindSent?.at).toBeLessThan(cancelledAt);
      await shared.end();
    } finally {
      await sql.end();
    }
  });

  it('runs no statement under another path after the server terminates a shared connection', async () => {
    const sql = client(1);
    try {
      const [{ pid }] = (await sql.unsafe('SELECT pg_backend_pid() AS pid')) as unknown as [{ pid: number }];
      const long = read(sql, ', pg_sleep(1)::text AS slept');
      await Bun.sleep(20);
      const joined = Array.from({ length: 5 }, () => read(sql));
      await Bun.sleep(20);
      await admin.unsafe('SELECT pg_terminate_backend($1)', [pid]);
      const answers = [await long, ...(await Promise.all(joined))];
      // In flight on the terminated session, the long statement and those that joined it all fail.
      for (const answer of answers) expect(answer).toStartWith('error:');
      // The next statements run on a new session, with the path set again.
      expect(await Promise.all([read(sql), read(sql), read(sql)])).toEqual([
        'row-of-schema',
        'row-of-schema',
        'row-of-schema',
      ]);
    } finally {
      await sql.end();
    }
  });
});
