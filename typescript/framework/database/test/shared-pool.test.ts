import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import type { InferConfig } from '@putnami/runtime';
import { specTest } from '@putnami/spectest';
import type { SQL } from 'bun';
import { closeAllDatabases, closeDatabase, database, physicalPoolCount } from '../src/factory';
import { setDatabaseBindingOverride } from '../src/postgres/binding';
import type { PostgresConfig } from '../src/postgres/config';
import type { ConnectionOptions } from '../src/postgres/options';
import { createDatasourceClient, RESET_SEARCH_PATH_SQL, SET_SEARCH_PATH_SQL } from '../src/postgres/datasource-client';
import { createDriverPool, type DriverPoolOptions } from '../src/postgres/driver-pool';
import { __setPhysicalPoolFactoryForTests, assertBunVersion, MINIMUM_BUN_VERSION } from '../src/postgres/physical-pool';
import { setPrimaryDatasource } from '../src/primary-datasource';

const FEATURE = 'typescript/shared-datasource-pooling';

/**
 * A deterministic, in-memory stand-in for `Bun.SQL`: a callable pool whose
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
  options: ConnectionOptions;
  ended: boolean;
  failSet?: Error;
  reserve(): Promise<unknown>;
  begin(...args: unknown[]): Promise<unknown>;
  end(): Promise<void>;
  unsafe(query: string, params?: unknown[]): FakeQuery;
}

/**
 * A driver-shaped query: created undispatched with the handler of the
 * connection that built it, dispatched once on the first then()/execute().
 */
function makeQuery(sql: FakeSql, text: string, handler: (q: FakeQuery) => void, fail?: Error): FakeQuery {
  let settle: { resolve(v: unknown): void; reject(e: unknown): void } = { resolve() {}, reject() {} };
  const promise = new Promise<unknown>((resolve, reject) => {
    settle = { resolve, reject };
  });
  const isSet = text.startsWith(`unsafe:${SET_SEARCH_PATH_SQL}`) || text === RESET_SEARCH_PATH_SQL;
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

/**
 * A single-connection member: what `Bun.SQL` with `max: 1` is to the driver
 * pool. Its template calls and `unsafe` run on its one connection, labelled
 * `conn`; `RESET search_path` fails with `failSet` like the set does.
 */
function makeFakeMember(pool: FakeSql): FakeSql {
  const handler = connectionHandler(pool, 'conn');
  const member = ((strings: unknown, ...args: unknown[]) => {
    if (Array.isArray(strings) && Array.isArray((strings as { raw?: unknown }).raw)) {
      const text = (strings as string[]).join('?');
      return makeQuery(pool, text, handler, text === RESET_SEARCH_PATH_SQL ? pool.failSet : undefined);
    }
    if (typeof strings === 'string' && args.length === 0) {
      return { identifier: strings };
    }
    return { builder: [strings, ...args] };
  }) as FakeSql;
  member.unsafe = (query, params) => {
    const fail = query === SET_SEARCH_PATH_SQL ? pool.failSet : undefined;
    return makeQuery(pool, `unsafe:${query}${params ? `:${JSON.stringify(params)}` : ''}`, handler, fail);
  };
  member.reserve = async () => {
    pool.log.push('reserve');
    return makeConnection(pool, 'conn');
  };
  member.begin = async (first: unknown, second?: unknown) => {
    const fn = (typeof first === 'function' ? first : second) as (tx: unknown) => unknown;
    pool.log.push(`begin:${typeof first === 'string' ? first : ''}`);
    const result = await fn(makeConnection(pool, 'tx'));
    pool.log.push('commit');
    return result;
  };
  member.end = async () => {};
  (member as unknown as Record<string, unknown>)['json'] = (value: unknown) => ({ json: value });
  return member;
}

/** A fake physical pool: the package's own driver pool over fake members, with one shared log. */
type FakePool = FakeSql & {
  /** The members the pool created, in order. */
  members: FakeSql[];
  /** The options each member was created with, in order. */
  memberOptions: DriverPoolOptions[];
  /** Closes the connection of member `index`, as the driver reports an idle timeout or a server error. */
  closeConnection(index?: number): void;
};

function makeFakeSql(options: ConnectionOptions): FakePool {
  const members: FakeSql[] = [];
  const memberOptions: DriverPoolOptions[] = [];
  let pool: FakePool;
  const driver = createDriverPool({ max: 2 }, (created) => {
    memberOptions.push(created);
    const member = makeFakeMember(pool);
    members.push(member);
    return member as unknown as SQL;
  });
  pool = driver as unknown as FakePool;
  pool.log = [];
  pool.queries = [];
  pool.options = options;
  pool.ended = false;
  pool.members = members;
  pool.memberOptions = memberOptions;
  pool.closeConnection = (index = 0) => {
    (memberOptions[index] as { onclose?: (error: unknown) => void }).onclose?.(new Error('connection closed'));
  };
  const end = driver.end.bind(driver);
  pool.end = async () => {
    pool.ended = true;
    pool.log.push('end');
    await end();
  };
  return pool;
}

const constructed: FakePool[] = [];

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
    return sql as unknown as SQL;
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
        `then:unsafe:${SET_SEARCH_PATH_SQL}:["app_b"]`,
        `conn:unsafe:${SET_SEARCH_PATH_SQL}:["app_b"]`,
        'conn:SELECT 1',
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

describe('datasource-bound client — search_path per connection', () => {
  function client(
    searchPath: string | undefined,
    onEnd = async () => {},
    physical = makeFakeSql({} as ConnectionOptions),
  ) {
    const sql = createDatasourceClient(physical as unknown as SQL, { datasource: 'ds', searchPath, onEnd });
    return { physical, sql };
  }

  const tick = (ms = 0) => new Promise((resolve) => setTimeout(resolve, ms));

  specTest(
    'the first query on a connection sets search_path before its statement; the next one sends its statement alone',
    {
      feature: FEATURE,
      requirement: 'acquire-time-search-path',
      check: 'a-connection-is-set-once-then-reused-without-a-set',
    },
    async () => {
      const { physical, sql } = client('tenant_a, public');
      const rows = await sql`SELECT v FROM t WHERE id = ${1}`;
      expect<unknown>(rows).toEqual([{ ok: 1 }]);
      expect(physical.log).toEqual([
        `then:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a, public"]`,
        `conn:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a, public"]`,
        'conn:SELECT v FROM t WHERE id = ?',
      ]);
      physical.log.length = 0;
      await sql`SELECT v FROM t WHERE id = ${2}`;
      await sql.unsafe('SELECT 3');
      expect(physical.log).toEqual(['conn:SELECT v FROM t WHERE id = ?', 'conn:unsafe:SELECT 3']);
      expect(physical.members).toHaveLength(1);
    },
  );

  specTest(
    'a datasource without a search_path resets the connection to the server default once, before its first statement',
    {
      feature: FEATURE,
      requirement: 'acquire-time-search-path',
      check: 'a-server-default-datasource-resets-search-path',
    },
    async () => {
      const { physical, sql } = client(undefined);
      await sql`SELECT 1`;
      await sql`SELECT 2`;
      expect(physical.log).toEqual([
        `then:${RESET_SEARCH_PATH_SQL}`,
        `conn:${RESET_SEARCH_PATH_SQL}`,
        'conn:SELECT 1',
        'conn:SELECT 2',
      ]);
    },
  );

  specTest(
    'datasources that alternate on one connection set their own path each time the connection changes hands',
    {
      feature: FEATURE,
      requirement: 'acquire-time-search-path',
      check: 'alternating-datasources-set-the-path-on-each-switch',
    },
    async () => {
      const { physical, sql: a } = client('app_a');
      const { sql: b } = client('app_b', async () => {}, physical);
      await a`SELECT 'a1'`;
      await b`SELECT 'b1'`;
      await a`SELECT 'a2'`;
      await a`SELECT 'a3'`;
      expect(physical.log.filter((line) => line.startsWith('conn:'))).toEqual([
        `conn:unsafe:${SET_SEARCH_PATH_SQL}:["app_a"]`,
        "conn:SELECT 'a1'",
        `conn:unsafe:${SET_SEARCH_PATH_SQL}:["app_b"]`,
        "conn:SELECT 'b1'",
        `conn:unsafe:${SET_SEARCH_PATH_SQL}:["app_a"]`,
        "conn:SELECT 'a2'",
        "conn:SELECT 'a3'",
      ]);
    },
  );

  specTest(
    'a connection that closed forgets its path, so the next query on it sets the path again',
    {
      feature: FEATURE,
      requirement: 'acquire-time-search-path',
      check: 'a-closed-connection-is-set-again',
    },
    async () => {
      const { physical, sql } = client('tenant_a');
      await sql`SELECT 1`;
      physical.closeConnection(0);
      physical.log.length = 0;
      await sql`SELECT 2`;
      expect(physical.log.filter((line) => line.startsWith('conn:'))).toEqual([
        `conn:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a"]`,
        'conn:SELECT 2',
      ]);
    },
  );

  specTest(
    'a connection that closes while its set runs is set again before the statement',
    {
      feature: FEATURE,
      requirement: 'acquire-time-search-path',
      check: 'a-close-during-the-set-sets-the-path-again',
    },
    async () => {
      const { physical, sql } = client('tenant_a');
      const pending = sql`SELECT 1`.execute();
      await tick();
      // The set is in flight: the driver reports the connection closed under it.
      physical.closeConnection(0);
      await pending;
      expect(physical.log.filter((line) => line.startsWith('conn:'))).toEqual([
        `conn:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a"]`,
        `conn:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a"]`,
        'conn:SELECT 1',
      ]);
    },
  );

  specTest(
    'a connection that closes during every set rejects the operation without sending its statement',
    {
      feature: FEATURE,
      requirement: 'acquire-time-search-path',
      check: 'a-connection-closing-during-each-set-rejects-the-operation',
    },
    async () => {
      const { physical, sql } = client('tenant_a');
      const pending = Promise.resolve(sql`SELECT 1`);
      await tick();
      // Every later set closes the connection under it, as the first one does now.
      const member = physical.members[0] as FakeSql;
      const unsafe = member.unsafe.bind(member);
      member.unsafe = (query, params) => {
        const sent = unsafe(query, params);
        physical.closeConnection(0);
        return sent;
      };
      physical.closeConnection(0);
      await expect(pending).rejects.toThrow('the connection closed while its search_path was being set');
      expect(physical.log).not.toContain('conn:SELECT 1');
    },
  );

  specTest(
    'a failed set rejects the operation before its statement is sent, and the connection is set again next time',
    {
      feature: FEATURE,
      requirement: 'acquire-time-search-path',
      check: 'a-failed-set-rejects-the-operation-before-its-statement',
    },
    async () => {
      const { physical, sql } = client('tenant_a');
      physical.failSet = new Error('permission denied to set parameter');
      await expect(Promise.resolve(sql`SELECT 1`)).rejects.toThrow('permission denied to set parameter');
      expect(physical.log).not.toContain('conn:SELECT 1');
      expect(physical.log).toContain(`then:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a"]`);
      await expect(sql.reserve()).rejects.toThrow('permission denied to set parameter');
      expect(physical.log.at(-1)).toBe('conn:release');

      physical.failSet = undefined;
      physical.log.length = 0;
      await sql`SELECT 2`;
      expect(physical.log.filter((line) => line.startsWith('conn:'))).toEqual([
        `conn:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a"]`,
        'conn:SELECT 2',
      ]);
    },
  );

  specTest(
    'unsafe sets the path like a tagged query',
    {
      feature: FEATURE,
      requirement: 'acquire-time-search-path',
      check: 'unsafe-sets-the-search-path-like-a-tagged-query',
    },
    async () => {
      const { physical, sql } = client('tenant_a');
      await sql.unsafe('SELECT $1::int', [7]);
      expect(physical.log).toEqual([
        `then:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a"]`,
        `conn:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a"]`,
        'conn:unsafe:SELECT $1::int:[7]',
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
      // What ran on the reserved connection may have changed its path: the
      // next query on that connection sets it again.
      physical.log.length = 0;
      await sql`SELECT 1`;
      expect(physical.log.filter((line) => line.startsWith('conn:'))).toEqual([
        `conn:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a"]`,
        'conn:SELECT 1',
      ]);
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
      // A rollback undoes a set made inside the transaction: the connection's
      // path is unknown afterwards.
      expect(physical.members).toHaveLength(1);
      physical.log.length = 0;
      const results = await sql.begin('read write', (tx) => [tx`SELECT 1`, tx`SELECT 2`]);
      expect(results).toHaveLength(2);
      expect(physical.log.slice(0, 3)).toEqual([
        'begin:read write',
        `then:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a"]`,
        `tx:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a"]`,
      ]);
      expect(physical.log.at(-1)).toBe('commit');
      physical.log.length = 0;
      await sql`SELECT 3`;
      expect(physical.log.filter((line) => line.startsWith('conn:'))).toEqual([
        `conn:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a"]`,
        'conn:SELECT 3',
      ]);
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
      expect<unknown>(sql('my_table')).toEqual({ identifier: 'my_table' });
      const rows = [{ a: 1 }];
      expect<unknown>(sql(rows as never, 'a')).toEqual({ builder: [rows, 'a'] });
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
      // Cancelled before dispatch: rejected without reaching the driver, no connection leased.
      expect(physical.log).toEqual([]);
      expect(physical.members).toHaveLength(0);

      await sql`SELECT 0`;
      physical.log.length = 0;
      const late = sql`SELECT pg_sleep(2)`.execute();
      await tick();
      late.cancel();
      await expect(Promise.resolve(late)).rejects.toMatchObject({ code: '57014' });
      expect(physical.log).toContain('cancel:SELECT pg_sleep(2)');

      await sql`SELECT 1`.values();
      await sql`SELECT 2`.raw();
      await sql`SELECT 3`.simple();
      const modes = physical.queries.filter((q) => q.modes.length > 0).map((q) => q.modes);
      expect(modes).toEqual([['values'], ['raw'], ['simple']]);
      // The cancelled query gave its connection back: every query ran on one.
      expect(physical.members).toHaveLength(1);
    },
  );

  specTest(
    'a query used as a fragment runs on the connection of the query that holds it, and is never dispatched itself',
    {
      feature: FEATURE,
      requirement: 'datasource-bound-acquire',
      check: 'a-fragment-runs-on-the-outer-querys-connection',
    },
    async () => {
      const { physical, sql } = client('tenant_a');
      const where = sql`id = ${1}`;
      await sql`SELECT v FROM t WHERE ${where}`;
      expect(physical.log).toEqual([
        `then:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a"]`,
        `conn:unsafe:${SET_SEARCH_PATH_SQL}:["tenant_a"]`,
        'conn:SELECT v FROM t WHERE ?',
      ]);
      // The fragment was built on the leased connection, as a value of the
      // statement, and never executed on its own.
      const fragment = physical.queries.find((q) => q.text === 'id = ?');
      expect(fragment?.executed).toBe(false);
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
      // The datasource client also answers close().
      await sql.close();
      expect(released).toBe(1);
      await expect(Promise.resolve(sql`SELECT 1`)).rejects.toThrow('is closed');
    },
  );

  specTest(
    'a cancel that lands while the query waits for a connection hands the connection back and issues nothing',
    {
      feature: FEATURE,
      requirement: 'datasource-bound-acquire',
      check: 'cancel-during-reserve-releases-the-connection',
    },
    async () => {
      const { physical, sql } = client('tenant_a');
      // The pool holds two connections: hold both.
      const first = await sql.reserve();
      const second = await sql.reserve();
      physical.log.length = 0;
      const q = sql`SELECT pg_sleep(3)`;
      const outcome = Promise.resolve(q).then(
        () => 'resolved',
        (err: { code?: string }) => err.code,
      );
      await tick();
      q.cancel();
      first.release();
      await tick(5);
      expect(await outcome).toBe('57014');
      // Cancelled while it waited: the connection goes back unused, and the
      // statement never reaches the driver.
      expect(physical.log).toEqual(['conn:release']);
      second.release();
      await sql`SELECT 1`;
      expect(physical.members).toHaveLength(2);
    },
  );
});

describe('datasource-bound client — runs only on the package pool', () => {
  specTest(
    'a client over another pool rejects its operations with an error naming the requirement',
    {
      feature: FEATURE,
      requirement: 'acquire-time-search-path',
      check: 'a-client-over-another-pool-fails',
    },
    async () => {
      const foreign = Object.assign(() => undefined, { reserve: async () => ({}) });
      const sql = createDatasourceClient(foreign as unknown as SQL, {
        datasource: 'ds',
        searchPath: 'a',
        onEnd: async () => {},
      });
      await expect(Promise.resolve(sql`SELECT 1`)).rejects.toThrow('runs on a pool built by createDriverPool');
    },
  );
});

describe('physical pool — Bun floor', () => {
  it('rejects a Bun release older than 1.4.5 with an error naming both versions', () => {
    expect(() => assertBunVersion('1.4.0')).toThrow(
      '@putnami/database needs Bun 1.4.5 or later; this process runs Bun 1.4.0',
    );
    expect(() => assertBunVersion('1.4.5')).not.toThrow();
    expect(() => assertBunVersion('1.5.0')).not.toThrow();
    expect(() => assertBunVersion('1.4.6-canary.3')).not.toThrow();
    expect(() => assertBunVersion('1.4.5-canary.1')).toThrow('this process runs Bun 1.4.5-canary.1');
    expect(MINIMUM_BUN_VERSION).toBe('1.4.5');
  });
});
