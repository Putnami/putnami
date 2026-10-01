import type { SqlClient } from '@putnami/database';
import type { RawUpsertResult } from '../../src/server/sink/fold';

/** One statement the fake observed, with the parameters it was bound to. */
interface FakeCall {
  query: string;
  args: readonly unknown[];
}

/** What the fake answers, per statement kind. */
interface FakeSqlScript {
  /** The `RETURNING` rows of the raw upsert, or the error it raises. */
  upsert?: RawUpsertResult[] | Error;
  /** The daily `path` cardinality the cap's probe reads. */
  pathCount?: number | Record<string, number>;
}

/** The fake connection plus everything a test asserts on. */
export interface FakeSql {
  /** The stand-in handed to the sink in place of a pool. */
  sql: SqlClient;
  /** Every statement, in the order it was issued. */
  calls: FakeCall[];
  /** How many times `reserve()` was called. */
  reserves: () => number;
  /** How many times `release()` was called. */
  releases: () => number;
  /** The leading word of every statement, for order assertions. */
  verbs: () => string[];
  /** The single call whose SQL starts with `prefix`. */
  callStartingWith: (prefix: string) => FakeCall;
}

/** A postgres.js RowList stand-in: an array carrying the affected-row `count`. */
function rowsWithCount(count: number, data: Record<string, unknown>[] = []): Record<string, unknown>[] {
  const rows = [...data] as Record<string, unknown>[] & { count: number };
  rows.count = count;
  return rows;
}

/**
 * A deterministic, in-memory stand-in for the postgres.js connection.
 *
 * A library cannot be handed a database inside the Putnami gate — `test-env-up`
 * only plans for a project whose `infra/requirements.json` a runnable app
 * converged — so every proof in this package runs against this object instead.
 * It is the same shape as `typescript/framework/database/test/cas.test.ts`
 * uses: callable as a tag for `BEGIN`/`COMMIT`/`ROLLBACK`, `unsafe(query, args)`
 * routing on the leading verb, and `reserve()` returning itself so a statement
 * issued inside the transaction resolves this one connection.
 *
 * Result rows are camelCase because the framework's client applies a camel
 * transform to column names: the live round-trip in the sample app is what
 * proves that transform, not this fake.
 *
 * @param script - What the upsert and the cardinality probe answer.
 * @returns The fake connection and its recorded calls.
 */
export function createFakeSql(script: FakeSqlScript = {}): FakeSql {
  const calls: FakeCall[] = [];
  let reserved = 0;
  let released = 0;

  const handler = (query: string, args: readonly unknown[]): Record<string, unknown>[] => {
    if (query.startsWith('INSERT INTO analytics_event')) {
      if (script.upsert instanceof Error) {
        throw script.upsert;
      }
      return (script.upsert ?? []) as unknown as Record<string, unknown>[];
    }
    if (query.startsWith('SELECT count(*)')) {
      const configured = script.pathCount ?? 0;
      const count = typeof configured === 'number' ? configured : (configured[String(args[0])] ?? 0);
      return [{ n: count }];
    }
    const first = args[0];
    return rowsWithCount(Array.isArray(first) ? first.length : 0);
  };

  // biome-ignore lint/suspicious/noExplicitAny: a minimal postgres.Sql-shaped fake
  const sql = ((strings: TemplateStringsArray) => {
    calls.push({ query: strings.join('').trim(), args: [] });
    return Promise.resolve(rowsWithCount(0));
  }) as any;

  sql.unsafe = (query: string, args: readonly unknown[] = []) => {
    calls.push({ query, args });
    try {
      return Promise.resolve(handler(query, args));
    } catch (error) {
      return Promise.reject(error);
    }
  };
  sql.reserve = () => {
    reserved += 1;
    return Promise.resolve(sql);
  };
  sql.release = () => {
    released += 1;
  };

  return {
    sql: sql as SqlClient,
    calls,
    reserves: () => reserved,
    releases: () => released,
    verbs: () => calls.map((call) => call.query.split(/\s+/)[0] ?? ''),
    callStartingWith: (prefix: string) => {
      const found = calls.filter((call) => call.query.startsWith(prefix));
      if (found.length !== 1) {
        throw new Error(`expected exactly one statement starting with ${prefix}, got ${found.length}`);
      }
      return found[0] as FakeCall;
    },
  };
}
