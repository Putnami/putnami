import { SQL } from 'bun';

/**
 * The physical pool the package runs on (doc/adr/0005-bun-sql-is-the-driver.md):
 * single-connection `Bun.SQL` instances, created as concurrent demand needs
 * them, up to `max`.
 *
 * `Bun.SQL`'s own pool dials every connection up to `max` whenever it has none
 * ready — on its first query and again after its idle timeout closed them
 * all — so a workload would hold `poolSize` backends from its first query and
 * reopen them all, each with its TLS and authentication handshake, after
 * every quiet period. This pool opens a connection only when every one it
 * holds is leased, hands out the most recently released one first so the
 * others reach their idle timeout, and makes an operation that finds `max`
 * leased wait for one.
 *
 * It answers the members the clients use on a driver pool: the builders
 * (`pool(value)`, `json`, `array`), which come from an instance that never
 * runs a query; a template call, which runs one statement on a leased
 * connection; `reserve()`, `begin()`, `listen()` on an instance of its own,
 * and `end()`/`close()`. The datasource client also leases a member directly
 * through {@link leaseMember}, with the session state the pool keeps for it.
 *
 * Each member holds one connection, so the pool knows which connection a
 * statement runs on. It records the `search_path` that connection's session
 * holds ({@link MemberSession}) and forgets it whenever the connection closes:
 * `Bun.SQL` reports every close (idle timeout, lifetime, a server or network
 * error) through `onclose`, and fails the statements already sent on a closed
 * connection instead of replaying them on the next one. The client sends no
 * statement on a member whose path it has not verified, except the set
 * itself, and it checks the set against the close count.
 *
 * A member's connection lives at most `maxLifetime` seconds. When the
 * configuration leaves it at 0, each member draws a lifetime between 45 and 90
 * minutes, as postgres.js did: `Bun.SQL`'s own default never recycles a
 * connection, and every connection keeps one prepared statement per distinct
 * statement text it ran.
 */

/** The `Bun.SQL` options of the whole pool; `max` is its size. */
export type DriverPoolOptions = NonNullable<ConstructorParameters<typeof SQL>[0]> & {
  max?: number;
  maxLifetime?: number;
};

/** Builds one single-connection instance; a test seam. */
export type MemberFactory = (options: DriverPoolOptions) => SQL;

const realMember: MemberFactory = (options) => new SQL(options);

type Reserved = SQL & { release(): void };

/** The value of {@link MemberSession.searchPath} when the pool does not know it. */
export const UNKNOWN_SEARCH_PATH: unique symbol = Symbol('unknown search_path');

/** What the pool knows of the session of a member's connection. */
export interface MemberSession {
  /**
   * The `search_path` the session holds, as the datasource client set it: a
   * path, `''` for the server default, or {@link UNKNOWN_SEARCH_PATH} after a
   * close, a reservation or a transaction, which may have changed it.
   */
  searchPath: string | typeof UNKNOWN_SEARCH_PATH;
  /** Counts the closes of the member's connection; a set compares it before and after. */
  closes: number;
}

/** A member leased for one operation: its single-connection instance and its session state. */
export interface LeasedMember {
  readonly sql: SQL;
  readonly session: MemberSession;
  /** Gives the member back to the pool; later calls do nothing. */
  release(): void;
}

const LEASE: unique symbol = Symbol('putnami.database.lease');

/**
 * leaseMember leases a member of `pool` for one operation, or returns
 * undefined when `pool` is not a pool {@link createDriverPool} built.
 */
export function leaseMember(pool: SQL): Promise<LeasedMember> | undefined {
  const lease = (pool as unknown as { [LEASE]?: () => Promise<LeasedMember> })[LEASE];
  return lease?.();
}

/** The lower bound of the lifetime a member draws when the configuration sets none, in seconds. */
export const DEFAULT_LIFETIME_MIN_SECONDS = 45 * 60;
/** The upper bound of that lifetime, in seconds. */
export const DEFAULT_LIFETIME_MAX_SECONDS = 90 * 60;

/** memberLifetime returns the configured lifetime, or draws one between 45 and 90 minutes. */
export function memberLifetime(configured: number | undefined, random: () => number = Math.random): number {
  if (configured !== undefined && configured > 0) {
    return configured;
  }
  return Math.round(
    DEFAULT_LIFETIME_MIN_SECONDS + random() * (DEFAULT_LIFETIME_MAX_SECONDS - DEFAULT_LIFETIME_MIN_SECONDS),
  );
}

interface Member {
  readonly sql: SQL;
  readonly session: MemberSession;
}

interface Waiter {
  resolve(member: Member): void;
  reject(reason: unknown): void;
}

function poolClosedError(): Error {
  return Object.assign(new Error('database: the connection pool is closed'), {
    code: 'ERR_POSTGRES_CONNECTION_CLOSED',
  });
}

function isTemplateStringsArray(value: unknown): value is TemplateStringsArray {
  return Array.isArray(value) && Array.isArray((value as { raw?: unknown }).raw);
}

/** A driver pool that grows with demand; see the module comment. */
export function createDriverPool(options: DriverPoolOptions, makeMember: MemberFactory = realMember): SQL {
  const max = Math.max(1, options.max ?? 10);
  const memberOptions: DriverPoolOptions = { ...options, max: 1 };
  const members: Member[] = [];
  // Released members, the most recently released last: reused first, so the
  // members at the bottom go idle and their connection times out.
  const free: Member[] = [];
  const waiters: Waiter[] = [];
  let builder: SQL | undefined;
  let listener: SQL | undefined;
  let ending: Promise<void> | undefined;

  const createMember = (): Member => {
    const session: MemberSession = { searchPath: UNKNOWN_SEARCH_PATH, closes: 0 };
    const onclose = (memberOptions as { onclose?: (error: unknown) => void }).onclose;
    const sql = makeMember({
      ...memberOptions,
      maxLifetime: memberLifetime(options.maxLifetime),
      onclose: (error: unknown) => {
        session.searchPath = UNKNOWN_SEARCH_PATH;
        session.closes++;
        onclose?.(error);
      },
    } as DriverPoolOptions);
    return { sql, session };
  };

  const lease = (): Promise<Member> => {
    if (ending) {
      return Promise.reject(poolClosedError());
    }
    const member = free.pop();
    if (member) {
      return Promise.resolve(member);
    }
    if (members.length < max) {
      const created = createMember();
      members.push(created);
      return Promise.resolve(created);
    }
    return new Promise<Member>((resolve, reject) => waiters.push({ resolve, reject }));
  };

  const giveBack = (member: Member): void => {
    const waiter = waiters.shift();
    if (waiter) {
      waiter.resolve(member);
    } else {
      free.push(member);
    }
  };

  /** Gives `member` back after a use the pool cannot vouch for: its session may hold another path. */
  const giveBackUnknown = (member: Member): void => {
    member.session.searchPath = UNKNOWN_SEARCH_PATH;
    giveBack(member);
  };

  const reserve = async (): Promise<Reserved> => {
    const member = await lease();
    let reserved: Reserved;
    try {
      reserved = (await member.sql.reserve()) as Reserved;
    } catch (error) {
      giveBackUnknown(member);
      throw error;
    }
    let released = false;
    const release = (): void => {
      if (!released) {
        released = true;
        reserved.release();
        giveBackUnknown(member);
      }
    };
    return new Proxy(reserved, {
      get(target, property) {
        if (property === 'release') {
          return release;
        }
        const value = Reflect.get(target, property, target);
        return typeof value === 'function' ? value.bind(target) : value;
      },
    });
  };

  /** Run `fn` with a leased member, and give it back once `fn` settled. */
  const withMember = async <T>(fn: (member: SQL) => Promise<T>): Promise<T> => {
    const member = await lease();
    try {
      return await fn(member.sql);
    } finally {
      giveBackUnknown(member);
    }
  };

  const leaseForOperation = async (): Promise<LeasedMember> => {
    const member = await lease();
    let released = false;
    return {
      sql: member.sql,
      session: member.session,
      release: () => {
        if (!released) {
          released = true;
          giveBack(member);
        }
      },
    };
  };

  const helpers = (): SQL => {
    builder ??= makeMember(memberOptions);
    return builder;
  };

  const pool = (first: unknown, ...rest: unknown[]): unknown => {
    if (isTemplateStringsArray(first)) {
      return withMember((member) => Promise.resolve(member(first, ...(rest as never[]))));
    }
    return (helpers() as unknown as (...args: unknown[]) => unknown)(first, ...rest);
  };

  const end = (endOptions?: { timeout?: number }): Promise<void> => {
    if (!ending) {
      for (const waiter of waiters.splice(0)) {
        waiter.reject(poolClosedError());
      }
      const instances = [...members.map((member) => member.sql), builder, listener].filter(
        (instance): instance is SQL => instance !== undefined,
      );
      ending = Promise.all(instances.map((instance) => instance.end(endOptions))).then(() => {});
    }
    return ending;
  };

  return Object.assign(pool, {
    json: (value: unknown) => (helpers() as unknown as { json(value: unknown): unknown }).json(value),
    array: (values: unknown[], type?: unknown) =>
      (helpers() as unknown as { array(values: unknown[], type?: unknown): unknown }).array(values, type),
    reserve,
    begin: (...args: unknown[]) =>
      withMember((member) => (member.begin as unknown as (...a: unknown[]) => Promise<unknown>)(...args)),
    listen: (channel: string, onnotify: (payload: string) => void, onlisten?: () => void) => {
      if (ending) {
        return Promise.reject(poolClosedError());
      }
      listener ??= makeMember(memberOptions);
      return listener.listen(channel, onnotify, onlisten);
    },
    end,
    close: end,
    [LEASE]: leaseForOperation,
  }) as unknown as SQL;
}
