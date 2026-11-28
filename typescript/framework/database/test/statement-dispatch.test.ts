import { afterEach, describe, expect, it } from 'bun:test';
import { resetConfigLoader, runInContext } from '@putnami/runtime';
import type { SQL } from 'bun';
import { QueryAbortError } from '../src/abort';
import { closeAllDatabases, database } from '../src/factory';
import { setDatabaseBindingOverride } from '../src/postgres/binding';
import { createDatasourceClient, SET_SEARCH_PATH_SQL } from '../src/postgres/datasource-client';
import { createDriverPool, leaseMember } from '../src/postgres/driver-pool';
import { __setPhysicalPoolFactoryForTests } from '../src/postgres/physical-pool';
import type { SqlClient } from '../src/sql-client';
import { __setTransactionConnectionFactory, useTxConnection } from '../src/transaction';

/**
 * The path from a query to its statement: how many turns it takes, and that
 * every failure on the way still rejects the query and gives its member back.
 */

/** A driver query as `Bun.SQL` makes one: a Promise subclass that runs on its first then() or execute(). */
class FakeQuery extends Promise<unknown> {
  static override get [Symbol.species]() {
    return Promise;
  }
  executed = false;
  readonly settle: (value: unknown) => void;
  readonly fail: (reason: unknown) => void;

  constructor(readonly text: string) {
    let settle!: (value: unknown) => void;
    let fail!: (reason: unknown) => void;
    super((resolve, reject) => {
      settle = resolve;
      fail = reject;
    });
    this.settle = settle;
    this.fail = fail;
  }

  // biome-ignore lint/suspicious/noThenProperty: a driver query is a promise
  override then<A = unknown, B = never>(
    onfulfilled?: ((value: unknown) => A | PromiseLike<A>) | null,
    onrejected?: ((reason: unknown) => B | PromiseLike<B>) | null,
  ): Promise<A | B> {
    this.execute();
    return super.then(onfulfilled, onrejected);
  }

  execute(): this {
    this.executed = true;
    return this;
  }
  values(): this {
    return this;
  }
  raw(): this {
    return this;
  }
  simple(): this {
    return this;
  }
  cancel(): void {
    this.fail(Object.assign(new Error('canceling statement due to user request'), { code: '57014' }));
  }
}

/** Single-connection fake members that record every query they build, in order. */
function fakeMembers() {
  const sent: FakeQuery[] = [];
  const factory = (): SQL => {
    const record = (query: FakeQuery) => {
      sent.push(query);
      return query;
    };
    const member = (strings: TemplateStringsArray, ..._values: unknown[]) => record(new FakeQuery(strings.join('?')));
    return Object.assign(member, {
      unsafe: (text: string) => record(new FakeQuery(text)),
      close: async () => {},
      end: async () => {},
    }) as unknown as SQL;
  };
  return { sent, factory };
}

/** A pool of one member whose session holds `path`, idle. */
async function primedPool(path: string) {
  const { sent, factory } = fakeMembers();
  const pool = createDriverPool({ max: 1 }, factory);
  const member = await leaseMember(pool, path);
  if (!member) throw new Error('no lease');
  member.session.searchPath = path;
  member.release();
  return { pool, sent };
}

function clientOf(pool: SQL, searchPath: string): SqlClient {
  return createDatasourceClient(pool, { datasource: 'app', searchPath, onEnd: async () => {} });
}

/** Whether the member of `pool` is free: a lease alone is granted at once, not queued. */
function memberIsFree(pool: SQL): boolean {
  const lease = leaseMember(pool);
  if (lease === undefined || lease instanceof Promise) {
    lease?.then((member) => member.release());
    return false;
  }
  lease.release();
  return true;
}

describe('a datasource query reaches its statement', () => {
  it('sends the statement in the turn it is executed when the free member holds its path, with no set', async () => {
    const { pool, sent } = await primedPool('app');
    const query = clientOf(pool, 'app')`SELECT 1`;
    query.execute();

    expect(sent.map((q) => [q.text, q.executed])).toEqual([['SELECT 1', true]]);
    (sent[0] as FakeQuery).settle(Object.assign([{ created_at: 1 }], { count: 1, command: 'SELECT' }));
    expect([...((await query) as unknown[])]).toEqual([{ createdAt: 1 }]);
    expect(memberIsFree(pool)).toBe(true);
  });

  it('gives the member back once a statement sent in that turn fails, and rejects with its error', async () => {
    const { pool, sent } = await primedPool('app');
    const query = clientOf(pool, 'app')`SELECT 1`;
    query.execute();
    const failure = Object.assign(new Error('duplicate key'), { errno: '23505' });
    (sent[0] as FakeQuery).fail(failure);

    const error = await Promise.resolve(query).then(
      () => undefined,
      (reason: unknown) => reason,
    );
    expect(error).toBe(failure);
    expect((error as { code?: unknown }).code).toBe('23505');
    expect(memberIsFree(pool)).toBe(true);
  });

  it('sets the path first, then sends the statement, when the free member holds another path', async () => {
    const { pool, sent } = await primedPool('other');
    const query = clientOf(pool, 'app')`SELECT 1`;
    query.execute();

    expect(sent.map((q) => q.text)).toEqual([SET_SEARCH_PATH_SQL]);
    (sent[0] as FakeQuery).settle([]);
    await Bun.sleep(0);
    expect(sent.map((q) => q.text)).toEqual([SET_SEARCH_PATH_SQL, 'SELECT 1']);
    (sent[1] as FakeQuery).settle([]);
    await query;
    expect(memberIsFree(pool)).toBe(true);
  });

  it('rejects, and sends nothing, when the query is cancelled while it waits for a member', async () => {
    const { pool, sent } = await primedPool('app');
    const held = leaseMember(pool);
    if (held === undefined || held instanceof Promise) throw new Error('the member should be free');
    const query = clientOf(pool, 'app')`SELECT 1`;
    query.execute();
    query.cancel();
    held.release();

    const error = await Promise.resolve(query).then(
      () => undefined,
      (reason: unknown) => reason,
    );
    expect((error as { code?: unknown }).code).toBe('57014');
    expect(sent).toEqual([]);
    expect(memberIsFree(pool)).toBe(true);
  });

  it('rejects the query, and never throws, when the pool cannot open a member', async () => {
    const pool = createDriverPool({ max: 1 }, () => {
      throw new Error('cannot open a connection');
    });
    const query = clientOf(pool, 'app')`SELECT 1`;

    expect(() => query.execute()).not.toThrow();
    const error = await Promise.resolve(query).then(
      () => undefined,
      (reason: unknown) => reason,
    );
    expect((error as Error).message).toBe('cannot open a connection');
  });
});

describe('useTxConnection outside a transaction', () => {
  afterEach(() => {
    __setTransactionConnectionFactory(undefined);
  });

  it('answers the connection source promise itself', async () => {
    const client = (() => undefined) as unknown as SqlClient;
    const source = Promise.resolve(client);
    __setTransactionConnectionFactory(() => source);

    const answered = useTxConnection('db', 'read');
    expect(answered).toBe(source);
    expect(await answered).toBe(client);
  });

  it('rejects, and never throws, when the connection source throws', async () => {
    __setTransactionConnectionFactory(() => {
      throw new Error('no connection source');
    });

    let answered: Promise<SqlClient> | undefined;
    expect(() => {
      answered = useTxConnection('db', 'read');
    }).not.toThrow();
    await expect(answered as Promise<SqlClient>).rejects.toThrow('no connection source');
  });

  it('rejects, and never throws, when the request was aborted', async () => {
    __setTransactionConnectionFactory(() => Promise.reject(new Error('the source should not be asked')));

    const answered = runInContext({ signal: AbortSignal.abort() }, () =>
      useTxConnection('db', 'write'),
    ) as Promise<SqlClient>;
    await expect(answered).rejects.toBeInstanceOf(QueryAbortError);
  });
});

describe('database() for an open datasource', () => {
  let previousOverride: string | undefined;

  afterEach(async () => {
    await closeAllDatabases();
    __setPhysicalPoolFactoryForTests(undefined);
    setDatabaseBindingOverride(previousOverride);
    delete process.env.CONFIG_DATA;
    resetConfigLoader();
  });

  function configure(sections: unknown): void {
    previousOverride = setDatabaseBindingOverride(undefined);
    process.env.CONFIG_DATA = JSON.stringify({ database: sections });
    resetConfigLoader();
    // A pool that answers its first ping; no query of these tests runs on it.
    __setPhysicalPoolFactoryForTests(
      (options) => Object.assign(() => undefined, { end: async () => {}, options }) as unknown as SQL,
    );
  }

  it('answers one settled promise of its client, and a new one once it was closed', async () => {
    configure({ 'dispatch-open': { host: '127.0.0.1', port: 5432, database: 'app', user: 'app', password: 'app' } });
    const client = await database('dispatch-open');

    const first = database('dispatch-open');
    expect(database('dispatch-open')).toBe(first);
    expect(await first).toBe(client);

    await client.end();
    const reopened = await database('dispatch-open');
    expect(reopened).not.toBe(client);
    expect(database('dispatch-open')).not.toBe(first);
  });

  it('rejects, and never throws, when the configuration of the datasource is invalid', async () => {
    configure({ 'dispatch-broken': { host: '127.0.0.1', port: 'not-a-port' } });

    let answered: Promise<SqlClient> | undefined;
    expect(() => {
      answered = database('dispatch-broken');
    }).not.toThrow();
    await expect(answered as Promise<SqlClient>).rejects.toThrow(
      "Config validation failed for 'database.dispatch-broken'",
    );
  });
});
