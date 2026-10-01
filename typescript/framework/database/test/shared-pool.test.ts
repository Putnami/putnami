import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import type { InferConfig } from '@putnami/runtime';
import { specTest } from '@putnami/spectest';
import postgres from 'postgres';
import { closeAllDatabases, closeDatabase, database, physicalPoolCount } from '../src/factory';
import { setDatabaseBindingOverride } from '../src/postgres/binding';
import type { PostgresConfig } from '../src/postgres/config';
import { createDatasourceClient, RESET_SEARCH_PATH_SQL, SET_SEARCH_PATH_SQL } from '../src/postgres/datasource-client';
import { __setPhysicalPoolFactoryForTests } from '../src/postgres/physical-pool';
import { setPrimaryDatasource } from '../src/primary-datasource';

const FEATURE = 'typescript/shared-datasource-pooling';

/**
 * A deterministic, in-memory stand-in for postgres.js: a callable `Sql` whose
 * `reserve()` hands out a callable connection that records every statement in
 * order, so the sharing, tuning, lifetime and per-operation search_path rules
 * are provable without a database. Queries settle on a timer so a cancel
 * "after execution" has an inner query to reach.
 */
interface FakeQuery extends PromiseLike<unknown> {
  text: string;
  strings: unknown;
  modes: string[];
  executed: boolean;
  cancelled: boolean;
  fail?: Error;
  handler: (q: FakeQuery) => void;
  resolve(value: unknown): void;
  reject(reason: unknown): void;
  execute(): FakeQuery;
  cancel(): void;
  values(): FakeQuery;
  raw(): FakeQuery;
  simple(): FakeQuery;
}

interface FakeSql {
  (...args: unknown[]): unknown;
  log: string[];
  queries: FakeQuery[];
  options: postgres.Options<Record<string, postgres.PostgresType>>;
  ended: boolean;
  failSet?: Error;
  reserve(): Promise<unknown>;
  begin(...args: unknown[]): Promise<unknown>;
  end(): Promise<void>;
  unsafe(query: string, params?: unknown[]): FakeQuery;
}

/**
 * A driver-shaped query: created undispatched with the handler of the `Sql`
 * that built it, dispatched once on the first then()/execute() through
 * whatever handler it carries at that moment — exactly the seam the logical
 * client re-points.
 */
function makeQuery(sql: FakeSql, text: string, handler: (q: FakeQuery) => void, fail?: Error): FakeQuery {
  let settle: { resolve(v: unknown): void; reject(e: unknown): void } = { resolve() {}, reject() {} };
  const promise = new Promise<unknown>((resolve, reject) => {
    settle = { resolve, reject };
  });
  const isSet = text.startsWith(`unsafe:${SET_SEARCH_PATH_SQL}`) || text === `unsafe:${RESET_SEARCH_PATH_SQL}`;
  // A set query gets NO blanket handler: the client must attach one itself,
  // otherwise a failing set surfaces here as an unhandled rejection.
  if (!isSet) {
    promise.catch(() => {});
  }
  const dispatch = () => {
    if (!q.executed) {
      q.executed = true;
      queueMicrotask(() => q.handler(q));
    }
  };
  const mode = (name: string) => {
    q.modes.push(name);
    return q;
  };
  const q = {
    text,
    strings: [text],
    modes: [] as string[],
    executed: false,
    cancelled: false,
    fail,
    handler,
    resolve: (value: unknown) => settle.resolve(value),
    reject: (reason: unknown) => settle.reject(reason),
    execute: () => {
      dispatch();
      return q;
    },
    cancel: () => {
      q.cancelled = true;
      sql.log.push(`cancel:${text}`);
      q.reject(Object.assign(new Error('canceling statement due to user request'), { code: '57014' }));
    },
    values: () => mode('values'),
    raw: () => mode('raw'),
    simple: () => mode('simple'),
  } as FakeQuery;
  // A driver Query is a Promise subclass; the fake becomes a thenable the same
  // lazy way — dispatched on its first then() — without declaring `then` in the
  // literal (Biome's noThenProperty).
  Object.defineProperty(q, 'then', {
    configurable: true,
    value: (onfulfilled?: ((v: unknown) => unknown) | null, onrejected?: ((e: unknown) => unknown) | null) => {
      if (isSet) {
        sql.log.push(`then:${text}`);
      }
      dispatch();
      return promise.then(onfulfilled, onrejected);
    },
  });
  sql.queries.push(q);
  return q;
}

/** A connection handler: runs a query on the labelled connection, settling it a tick later. */
function connectionHandler(sql: FakeSql, label: string) {
  return (q: FakeQuery) => {
    sql.log.push(`${label}:${q.text}`);
    if (q.cancelled) {
      return;
    }
    setTimeout(() => (q.fail ? q.reject(q.fail) : q.resolve([{ ok: 1 }])), q.text.includes('pg_sleep') ? 5 : 1);
  };
}

function makeConnection(sql: FakeSql, label: string) {
  const handler = connectionHandler(sql, label);
  const conn = ((strings: unknown, ...args: unknown[]) => {
    if (Array.isArray(strings) && Array.isArray((strings as { raw?: unknown }).raw)) {
      return makeQuery(sql, (strings as string[]).join('?'), handler);
    }
    return { identifier: strings, args };
  }) as unknown as Record<string, unknown> & ((...args: unknown[]) => unknown);
  conn['unsafe'] = (query: string, params?: unknown[]) => {
    const fail = query === SET_SEARCH_PATH_SQL || query === RESET_SEARCH_PATH_SQL ? sql.failSet : undefined;
    return makeQuery(sql, `unsafe:${query}${params ? `:${JSON.stringify(params)}` : ''}`, handler, fail);
  };
  conn['release'] = () => sql.log.push(`${label}:release`);
  return conn;
}

function makeFakeSql(options: postgres.Options<Record<string, postgres.PostgresType>>): FakeSql {
  let poolHandler: (q: FakeQuery) => void = () => {};
  const sql = ((strings: unknown, ...args: unknown[]) => {
    if (Array.isArray(strings) && Array.isArray((strings as { raw?: unknown }).raw)) {
      return makeQuery(sql, (strings as string[]).join('?'), poolHandler);
    }
    if (typeof strings === 'string' && args.length === 0) {
      return { identifier: strings };
    }
    return { builder: [strings, ...args] };
  }) as FakeSql;
  poolHandler = connectionHandler(sql, 'pool');
  sql.log = [];
  sql.queries = [];
  sql.options = options;
  sql.ended = false;
  sql.reserve = async () => {
    sql.log.push('reserve');
    return makeConnection(sql, 'conn');
  };
  sql.begin = async (first: unknown, second?: unknown) => {
    const fn = (typeof first === 'function' ? first : second) as (tx: unknown) => unknown;
    sql.log.push(`begin:${typeof first === 'string' ? first : ''}`);
    const result = await fn(makeConnection(sql, 'tx'));
    sql.log.push('commit');
    return result;
  };
  sql.unsafe = (query, params) =>
    makeQuery(sql, `unsafe:${query}${params ? `:${JSON.stringify(params)}` : ''}`, poolHandler);
  sql.end = async () => {
    sql.ended = true;
    sql.log.push('end');
  };
  (sql as unknown as Record<string, unknown>)['json'] = (value: unknown) => ({ json: value });
  return sql;
}

const constructed: FakeSql[] = [];

function connection(host: string, params?: Record<string, string>) {
  return { host, port: 5432, database: 'app', user: 'svc', password: 'pw', ssl: false, ...(params ? { params } : {}) };
}

/** Five datasources: a, b, d, e on one connection; c on another database. */
const BINDING = JSON.stringify({
  protocolVersion: 1,
  databases: {
    shared_a: { engine: 'postgres', schema: 'app_a', connection: connection('fake-a.invalid') },
    shared_b: { engine: 'postgres', schema: 'app_b', connection: connection('fake-a.invalid') },
    other_c: { engine: 'postgres', schema: 'app_c', connection: connection('fake-c.invalid') },
    tune_d: { engine: 'postgres', schema: 'app_d', connection: connection('fake-a.invalid', { pool_max_conns: '4' }) },
    tune_e: { engine: 'postgres', schema: 'app_e', connection: connection('fake-a.invalid') },
  },
});

const cfg = (init: object) => init as InferConfig<typeof PostgresConfig>;

let previousOverride: string | undefined;
let base = 0;

beforeEach(() => {
  constructed.length = 0;
  base = physicalPoolCount();
  __setPhysicalPoolFactoryForTests((options) => {
    const sql = makeFakeSql(options);
    constructed.push(sql);
    return sql as unknown as postgres.Sql;
  });
  previousOverride = setDatabaseBindingOverride(BINDING);
  setPrimaryDatasource(undefined);
});

afterEach(async () => {
  for (const name of ['shared_a', 'shared_b', 'other_c', 'tune_d', 'tune_e']) {
    await closeDatabase(name);
  }
  setDatabaseBindingOverride(previousOverride);
  __setPhysicalPoolFactoryForTests(undefined);
});

describe('shared physical pool — one pool per connection identity', () => {
  specTest(
    'two datasources on one connection share one physical pool; a third on another database opens a second',
    {
      feature: FEATURE,
      requirement: 'one-pool-per-physical-database',
      check: 'same-connection-identity-shares-one-physical-pool',
    },
    async () => {
      const a = await database('shared_a');
      const b = await database('shared_b');
      expect(constructed).toHaveLength(1);
      expect(physicalPoolCount() - base).toBe(1);
      expect(a).not.toBe(b);
      expect(await database('shared_a')).toBe(a);
    },
  );

  specTest(
    'distinct connection identities open distinct physical pools',
    {
      feature: FEATURE,
      requirement: 'one-pool-per-physical-database',
      check: 'distinct-connection-identities-open-distinct-pools',
    },
    async () => {
      await database('shared_a');
      await database('shared_b');
      await database('other_c');
      expect(constructed).toHaveLength(2);
      expect(physicalPoolCount() - base).toBe(2);
      expect(constructed[0].options.host).toBe('fake-a.invalid');
      expect(constructed[1].options.host).toBe('fake-c.invalid');
    },
  );

  specTest(
    'concurrent first opens of two datasources on one identity construct the pool once',
    { feature: FEATURE, requirement: 'one-pool-per-physical-database', check: 'concurrent-first-opens-connect-once' },
    async () => {
      const [a, b] = await Promise.all([database('shared_a'), database('shared_b')]);
      expect(constructed).toHaveLength(1);
      expect(physicalPoolCount() - base).toBe(1);
      expect(a).not.toBe(b);
    },
  );

  specTest(
    'the physical pool carries no search_path startup parameter',
    {
      feature: FEATURE,
      requirement: 'acquire-time-search-path',
      check: 'the-physical-pool-carries-no-startup-search-path',
    },
    async () => {
      await database('shared_a');
      const startup = (constructed[0].options.connection ?? {}) as Record<string, string>;
      expect(startup['search_path']).toBeUndefined();
      expect(startup['statement_timeout']).toBe('30000');
    },
  );
});

describe('shared physical pool — tuning must agree', () => {
  specTest(
    'a datasource declaring different tuning for the same database is a declaration error, not a second pool',
    {
      feature: FEATURE,
      requirement: 'tuning-agreement',
      check: 'different-tuning-on-one-database-is-a-declaration-error',
    },
    async () => {
      await database('shared_a');
      await expect(database('tune_d')).rejects.toThrow(
        'database: datasources "shared_a" and "tune_d" resolve to the same physical database but declare different pool tuning (poolSize 10 vs 4); a shared pool takes one tuning — declare it identically on every datasource of that database',
      );
      await expect(database('tune_e', cfg({ ...connection('fake-a.invalid'), idleTimeout: 5 }))).rejects.toThrow(
        'datasources "shared_a" and "tune_e" resolve to the same physical database but declare different pool tuning (idleTimeout 60 vs 5)',
      );
      expect(constructed).toHaveLength(1);
      expect(physicalPoolCount() - base).toBe(1);
      const failure = await database('tune_d').catch((err: Error) => err.message);
      expect(failure).not.toContain('pw');
      expect(failure).not.toContain('fake-a.invalid');
    },
  );
});

describe('shared physical pool — lifetime', () => {
  specTest(
    'closing one datasource keeps the shared pool open for the other',
    {
      feature: FEATURE,
      requirement: 'shared-pool-lifetime',
      check: 'closing-one-datasource-keeps-the-shared-pool-open',
    },
    async () => {
      await database('shared_a');
      const b = await database('shared_b');
      await closeDatabase('shared_a');
      expect(constructed[0].ended).toBe(false);
      expect(physicalPoolCount() - base).toBe(1);
      constructed[0].log.length = 0;
      await b`SELECT 1`;
      expect(constructed[0].log).toEqual([
        'reserve',
        `then:unsafe:${SET_SEARCH_PATH_SQL}:["app_b"]`,
        `conn:unsafe:${SET_SEARCH_PATH_SQL}:["app_b"]`,
        'conn:SELECT 1',
        'conn:release',
      ]);
    },
  );

  specTest(
    'closing the last datasource ends the shared pool',
    {
      feature: FEATURE,
      requirement: 'shared-pool-lifetime',
      check: 'closing-the-last-datasource-ends-the-shared-pool',
    },
    async () => {
      await database('shared_a');
      await database('shared_b');
      await closeDatabase('shared_a');
      await closeDatabase('shared_b');
      expect(constructed[0].ended).toBe(true);
      expect(physicalPoolCount() - base).toBe(0);
      await database('shared_a');
      expect(constructed).toHaveLength(2);
      expect(physicalPoolCount() - base).toBe(1);
    },
  );

  specTest(
    'closeAllDatabases ends every physical pool and a later database() re-opens',
    {
      feature: FEATURE,
      requirement: 'shared-pool-lifetime',
      check: 'close-all-ends-every-pool-and-a-later-open-reconnects',
    },
    async () => {
      await database('shared_a');
      await database('shared_b');
      await database('other_c');
      await closeAllDatabases();
      expect(constructed.map((sql) => sql.ended)).toEqual([true, true]);
      expect(physicalPoolCount()).toBe(0);
      await closeAllDatabases();
      const a = await database('shared_a');
      expect(constructed).toHaveLength(3);
      expect(physicalPoolCount()).toBe(1);
      await a`SELECT 1`;
    },
  );

  specTest(
    'a closed datasource rejects its operations with an error and a later database() hands out a fresh client',
    { feature: FEATURE, requirement: 'shared-pool-lifetime', check: 'a-closed-datasource-fails-with-an-error' },
    async () => {
      const a = await database('shared_a');
      await a.end();
      await a.end();
      await expect(Promise.resolve(a`SELECT 1`)).rejects.toThrow('database: datasource "shared_a" is closed');
      await expect(a.reserve()).rejects.toThrow('is closed');
      await expect(a.begin(async () => 1)).rejects.toThrow('is closed');
      expect(physicalPoolCount() - base).toBe(0);
      const again = await database('shared_a');
      expect(again).not.toBe(a);
      expect(constructed).toHaveLength(2);
    },
  );
});

describe('datasource-bound client — search_path per operation', () => {
  function client(searchPath: string | undefined, onEnd = async () => {}) {
    const physical = makeFakeSql({});
    const sql = createDatasourceClient(physical as unknown as postgres.Sql, { datasource: 'ds', searchPath, onEnd });
    return { physical, sql };
  }

  specTest(
    'a tagged-template query reserves, sets search_path, runs the statement on that connection, and releases',
    {
      feature: FEATURE,
      requirement: 'acquire-time-search-path',
      check: 'the-set-precedes-the-statement-on-the-reserved-connection',
    },
    async () => {
      const { physical, sql } = client('tenant_a, public');
      const rows = await sql`SELECT v FROM t WHERE id = ${1}`;
      expect(rows).toEqual([{ ok: 1 }]);
      expect(physical.log).toEqual([
        'reserve',
        `then:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a, public"]`,
        `conn:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a, public"]`,
        'conn:SELECT v FROM t WHERE id = ?',
        'conn:release',
      ]);
    },
  );

  specTest(
    'a datasource without a search_path resets the connection to the server default before its statement',
    {
      feature: FEATURE,
      requirement: 'acquire-time-search-path',
      check: 'a-server-default-datasource-resets-search-path',
    },
    async () => {
      const { physical, sql } = client(undefined);
      await sql`SELECT 1`;
      expect(physical.log).toEqual([
        'reserve',
        `then:unsafe:${RESET_SEARCH_PATH_SQL}`,
        `conn:unsafe:${RESET_SEARCH_PATH_SQL}`,
        'conn:SELECT 1',
        'conn:release',
      ]);
    },
  );

  specTest(
    'a failed set rejects the operation with the set error and still releases the connection',
    {
      feature: FEATURE,
      requirement: 'acquire-time-search-path',
      check: 'a-failed-set-rejects-the-operation-and-releases-the-connection',
    },
    async () => {
      const { physical, sql } = client('tenant_a');
      physical.failSet = new Error('permission denied to set parameter');
      await expect(Promise.resolve(sql`SELECT 1`)).rejects.toThrow('permission denied to set parameter');
      expect(physical.log.at(-1)).toBe('conn:release');
      // The client attached a handler to the set's own promise: its rejection
      // is observed there, never left unhandled.
      expect(physical.log).toContain(`then:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a"]`);
      await expect(sql.reserve()).rejects.toThrow('permission denied to set parameter');
      expect(physical.log.at(-1)).toBe('conn:release');
    },
  );

  specTest(
    'unsafe goes through the same reserve, set, statement, release sequence',
    {
      feature: FEATURE,
      requirement: 'acquire-time-search-path',
      check: 'unsafe-sets-the-search-path-like-a-tagged-query',
    },
    async () => {
      const { physical, sql } = client('tenant_a');
      await sql.unsafe('SELECT $1::int', [7]);
      expect(physical.log).toEqual([
        'reserve',
        `then:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a"]`,
        `conn:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a"]`,
        'conn:unsafe:SELECT $1::int:[7]',
        'conn:release',
      ]);
    },
  );

  specTest(
    'reserve() hands out the pinned connection once its session carries the datasource search_path',
    {
      feature: FEATURE,
      requirement: 'datasource-bound-acquire',
      check: 'reserve-hands-out-a-connection-bound-to-the-datasource',
    },
    async () => {
      const { physical, sql } = client('tenant_a');
      const reserved = await sql.reserve();
      expect(physical.log).toEqual([
        'reserve',
        `then:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a"]`,
        `conn:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a"]`,
      ]);
      await reserved`BEGIN`;
      reserved.release();
      expect(physical.log.slice(3)).toEqual(['conn:BEGIN', 'conn:release']);
    },
  );

  specTest(
    'begin sets the search_path on the transaction connection before the callback runs, in both overloads',
    {
      feature: FEATURE,
      requirement: 'datasource-bound-acquire',
      check: 'begin-sets-the-search-path-before-the-callback',
    },
    async () => {
      const { physical, sql } = client('tenant_a');
      const result = await sql.begin(async (tx) => {
        physical.log.push('callback');
        await tx`INSERT INTO t VALUES (1)`;
        return 'done';
      });
      expect(result).toBe('done');
      expect(physical.log).toEqual([
        'begin:',
        `then:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a"]`,
        `tx:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a"]`,
        'callback',
        'tx:INSERT INTO t VALUES (1)',
        'commit',
      ]);
      physical.log.length = 0;
      const results = await sql.begin('read write', (tx) => [tx`SELECT 1`, tx`SELECT 2`]);
      expect(results).toHaveLength(2);
      expect(physical.log.slice(0, 3)).toEqual([
        'begin:read write',
        `then:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a"]`,
        `tx:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a"]`,
      ]);
      expect(physical.log.at(-1)).toBe('commit');
    },
  );

  specTest(
    'identifier and row builders forward to the physical Sql without touching a connection',
    {
      feature: FEATURE,
      requirement: 'datasource-bound-acquire',
      check: 'builders-forward-to-the-physical-pool-untouched',
    },
    async () => {
      const { physical, sql } = client('tenant_a');
      expect(sql('my_table')).toEqual({ identifier: 'my_table' });
      const rows = [{ a: 1 }];
      expect(sql(rows as never, 'a')).toEqual({ builder: [rows, 'a'] });
      expect(sql.json({ k: 1 })).toEqual({ json: { k: 1 } } as never);
      expect(physical.log).toEqual([]);
      expect(typeof sql).toBe('function');
    },
  );

  specTest(
    'cancel before and after execution reaches the inner query, and values/raw/simple modes reach it too',
    { feature: FEATURE, requirement: 'datasource-bound-acquire', check: 'cancel-and-modes-reach-the-inner-query' },
    async () => {
      const { physical, sql } = client('tenant_a');
      const early = sql`SELECT pg_sleep(1)`;
      early.cancel();
      await expect(Promise.resolve(early)).rejects.toMatchObject({ code: '57014' });
      // Cancelled before dispatch: rejected by the driver, no connection reserved.
      expect(physical.log).toEqual(['cancel:SELECT pg_sleep(1)']);

      physical.log.length = 0;
      const late = sql`SELECT pg_sleep(2)`.execute();
      await new Promise((resolve) => setTimeout(resolve, 0));
      late.cancel();
      await expect(Promise.resolve(late)).rejects.toMatchObject({ code: '57014' });
      expect(physical.log).toContain('cancel:SELECT pg_sleep(2)');
      await new Promise((resolve) => setTimeout(resolve, 10));
      expect(physical.log.at(-1)).toBe('conn:release');

      await sql`SELECT 1`.values();
      await sql`SELECT 2`.raw();
      await sql`SELECT 3`.simple();
      const modes = physical.queries.filter((q) => q.modes.length > 0).map((q) => q.modes);
      expect(modes).toEqual([['values'], ['raw'], ['simple']]);
    },
  );

  specTest(
    'end() releases the datasource handle once and marks the client closed',
    { feature: FEATURE, requirement: 'shared-pool-lifetime', check: 'end-releases-the-handle-once' },
    async () => {
      let released = 0;
      const { sql } = client('tenant_a', async () => {
        released++;
      });
      await sql.end();
      await sql.close();
      expect(released).toBe(1);
      await expect(Promise.resolve(sql`SELECT 1`)).rejects.toThrow('is closed');
    },
  );

  specTest(
    'a cancel that lands while the reserve is pending hands the connection back and issues nothing',
    {
      feature: FEATURE,
      requirement: 'datasource-bound-acquire',
      check: 'cancel-during-reserve-releases-the-connection',
    },
    async () => {
      const { physical, sql } = client('tenant_a');
      let grant: (() => void) | undefined;
      physical.reserve = () =>
        new Promise((resolve) => {
          physical.log.push('reserve');
          grant = () => resolve(makeConnection(physical, 'conn'));
        });
      const q = sql`SELECT pg_sleep(3)`;
      const outcome = Promise.resolve(q).then(
        () => 'resolved',
        (err: { code?: string }) => err.code,
      );
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(grant).toBeDefined();
      q.cancel();
      grant?.();
      await new Promise((resolve) => setTimeout(resolve, 5));
      expect(await outcome).toBe('57014');
      expect(physical.log).toEqual(['reserve', 'cancel:SELECT pg_sleep(3)', 'conn:release']);
    },
  );
});

describe('driver internals pinned by src/postgres/datasource-client.ts', () => {
  it('postgres.js Query still exposes handler, executed, resolve, reject, cancelled and strings, and then() is a no-op once executed', async () => {
    // A real driver instance that never connects: creating a query dispatches
    // nothing, so the shape can be asserted without a database.
    const sql = postgres({
      host: '127.0.0.1',
      port: 1,
      database: 'x',
      username: 'x',
      password: 'x',
      max: 1,
      connect_timeout: 1,
    });
    try {
      for (const q of [sql`select 1`, sql.unsafe('select 1')] as unknown as Record<string, unknown>[]) {
        const why = 'postgres.js changed its Query internals; update src/postgres/datasource-client.ts (dispatch/bind)';
        expect(q instanceof Promise, why).toBe(true);
        expect(typeof q['handler'], why).toBe('function');
        expect(q['executed'], why).toBe(false);
        expect(typeof q['resolve'], why).toBe('function');
        expect(typeof q['reject'], why).toBe('function');
        expect(q['cancelled'], why).toBeNull();
        expect(Array.isArray(q['strings']), why).toBe(true);
        let dispatched = 0;
        q['handler'] = () => {
          dispatched++;
        };
        q['executed'] = true;
        (q as unknown as Promise<unknown>).then(undefined, () => {});
        await new Promise((resolve) => setTimeout(resolve, 5));
        expect(dispatched, `${why}: then() must not dispatch an executed query`).toBe(0);
      }
    } finally {
      await sql.end({ timeout: 0 }).catch(() => {});
    }
  });
});
