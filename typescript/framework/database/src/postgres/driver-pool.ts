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
 * holds is in use, hands out the most recently released one first so the
 * others reach their idle timeout, and makes an operation that finds `max`
 * in use, and none it may share, wait for one.
 *
 * A datasource operation names the `search_path` it needs. Once every member
 * is in use, it shares the least busy member whose session holds that path,
 * and its statement follows the others on that connection, as on `Bun.SQL`'s
 * own pool (doc/adr/0007-datasource-operations-share-a-connection.md). A set,
 * a reservation, a transaction and a template call hold their member alone.
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
 * A member lives `maxLifetime` seconds. When the configuration leaves it at 0,
 * each member draws a lifetime between 45 and 90 minutes, as postgres.js did:
 * `Bun.SQL`'s own default never recycles a connection, and every connection
 * keeps one prepared statement per distinct statement text it ran. The pool
 * retires the member itself, once no operation runs on it: `Bun.SQL` would
 * retire a busy connection and run the statements queued on it on a new
 * session at the server-default `search_path`
 * (doc/adr/0007-datasource-operations-share-a-connection.md).
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
 * undefined when `pool` is not a pool {@link createDriverPool} built. With the
 * `searchPath` the operation needs, a member whose session holds it may be
 * shared with other operations; without it, the member is leased alone.
 */
export function leaseMember(pool: SQL, searchPath?: string): LeasedMember | Promise<LeasedMember> | undefined {
  const lease = (pool as unknown as { [LEASE]?: (searchPath?: string) => LeasedMember | Promise<LeasedMember> })[LEASE];
  return lease?.(searchPath);
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
  /** When the member retires, in the pool clock's milliseconds: the pool owns the lifetime, not `Bun.SQL`. */
  readonly retiresAt: number;
  /** Operations running on the member under a shared lease. */
  shared: number;
  /** Whether one caller holds the member alone (a set, a reservation, a transaction, a template call). */
  exclusive: boolean;
  /** When a shared operation last joined an unused member or finished, in the pool clock's milliseconds. */
  progressAt: number;
}

interface Waiter {
  /** The path a shared operation needs; undefined for an exclusive lease. */
  readonly searchPath: string | undefined;
  resolve(lease: Grant): void;
  reject(reason: unknown): void;
}

/** A member handed to one operation, and whether that operation shares it. */
interface Grant {
  readonly member: Member;
  readonly shared: boolean;
}

/**
 * How long a shared member may go without a statement finishing before new
 * operations stop joining it, in milliseconds: a statement waiting on a lock
 * then holds back only the statements that joined it already.
 */
export const SHARED_STALL_MS = 100;

/** The pool's clock, in milliseconds; a test seam. */
export type PoolClock = () => number;

function poolClosedError(): Error {
  return Object.assign(new Error('database: the connection pool is closed'), {
    code: 'ERR_POSTGRES_CONNECTION_CLOSED',
  });
}

function isTemplateStringsArray(value: unknown): value is TemplateStringsArray {
  return Array.isArray(value) && Array.isArray((value as { raw?: unknown }).raw);
}

/**
 * The members of one pool and who uses them: the lease state machine of
 * doc/adr/0007-datasource-operations-share-a-connection.md.
 */
class MemberSet {
  readonly members: Member[] = [];
  /**
   * Released members, the most recently released last: reused first, so the
   * members at the bottom go idle and their connection times out.
   */
  private readonly free: Member[] = [];
  private readonly waiters: Waiter[] = [];

  constructor(
    private readonly max: number,
    private readonly maxLifetime: number | undefined,
    private readonly open: (session: MemberSession) => SQL,
    private readonly now: PoolClock,
  ) {}

  private create(): Member {
    const session: MemberSession = { searchPath: UNKNOWN_SEARCH_PATH, closes: 0 };
    const created = this.now();
    const member: Member = {
      sql: this.open(session),
      session,
      retiresAt: created + memberLifetime(this.maxLifetime) * 1000,
      shared: 0,
      exclusive: false,
      progressAt: created,
    };
    this.members.push(member);
    return member;
  }

  /** Closes a member no operation uses and drops it from the pool; a replacement opens on demand. */
  private retire(member: Member): void {
    const at = this.members.indexOf(member);
    if (at >= 0) this.members.splice(at, 1);
    const idle = this.free.indexOf(member);
    if (idle >= 0) this.free.splice(idle, 1);
    try {
      Promise.resolve(member.sql.close()).catch(() => {});
    } catch {
      // A member that cannot close has nothing left to release.
    }
  }

  /**
   * A member no operation uses: an idle one whose session holds `searchPath`
   * (the most recently released first), else the most recently released, else
   * a new one while the pool holds fewer than `max`. An idle member past its
   * lifetime retires instead.
   */
  private takeIdle(searchPath: string | undefined): Member | undefined {
    const at = this.now();
    for (let i = this.free.length - 1; i >= 0; i--) {
      const member = this.free[i] as Member;
      if (at >= member.retiresAt) {
        this.retire(member);
      } else if (searchPath !== undefined && member.session.searchPath === searchPath) {
        this.free.splice(i, 1);
        return member;
      }
    }
    const member = this.free.pop();
    if (member) return member;
    return this.members.length < this.max ? this.create() : undefined;
  }

  /**
   * The least busy member already shared under `searchPath` that a new
   * operation may join: within its lifetime, and with a statement finished,
   * or sharing begun, less than {@link SHARED_STALL_MS} ago.
   */
  private takeShared(searchPath: string): Member | undefined {
    const at = this.now();
    let best: Member | undefined;
    for (const member of this.members) {
      if (member.exclusive || member.shared === 0 || member.session.searchPath !== searchPath) continue;
      if (at >= member.retiresAt || at - member.progressAt >= SHARED_STALL_MS) continue;
      if (best === undefined || member.shared < best.shared) best = member;
    }
    return best;
  }

  /** Hands `member`, which no operation uses, to one operation: shared when its session holds the path, else alone. */
  private grant(member: Member, searchPath: string | undefined): Grant {
    if (searchPath !== undefined && member.session.searchPath === searchPath) {
      if (member.shared === 0) member.progressAt = this.now();
      member.shared++;
      return { member, shared: true };
    }
    member.exclusive = true;
    return { member, shared: false };
  }

  /**
   * Gives back a member no operation uses any more. A member past its lifetime
   * retires, and the first waiter gets a new member when the pool has room.
   * Otherwise the member goes to the first waiter; when that waiter shares it,
   * the waiters right behind it that need the same path share it too, up to
   * an even share of the waiting operations per member. Without a waiter, it
   * goes back to the idle list.
   */
  private settle(member: Member): void {
    let next = member;
    if (this.now() >= member.retiresAt) {
      this.retire(member);
      if (this.waiters.length === 0 || this.members.length >= this.max) return;
      next = this.create();
    }
    const waiter = this.waiters.shift();
    if (!waiter) {
      this.free.push(next);
      return;
    }
    const first = this.grant(next, waiter.searchPath);
    waiter.resolve(first);
    if (!first.shared) return;
    const share = Math.ceil((this.waiters.length + 1) / Math.max(1, this.members.length));
    while (next.shared < share && this.waiters[0]?.searchPath === next.session.searchPath) {
      (this.waiters.shift() as Waiter).resolve(this.grant(next, next.session.searchPath as string));
    }
  }

  private enqueue(searchPath: string | undefined): Promise<Grant> {
    return new Promise<Grant>((resolve, reject) => this.waiters.push({ searchPath, resolve, reject }));
  }

  /**
   * Leases a member, alone without `searchPath`. With it, a member whose
   * session holds it is shared: the statements of several operations then go
   * one after another on its one connection, as `Bun.SQL`'s own pool does. A
   * member whose session holds another path, or none known, is leased alone,
   * so the client sets the path before any statement goes on it. While an
   * operation waits, a new one waits behind it rather than join a shared
   * member: a stream of shared operations never starves a reservation, a
   * transaction or another datasource's set.
   */
  lease(searchPath: string | undefined): Grant | Promise<Grant> {
    if (this.waiters.length === 0) {
      const idle = this.takeIdle(searchPath);
      if (idle) return this.grant(idle, searchPath);
      if (searchPath !== undefined) {
        const busy = this.takeShared(searchPath);
        if (busy) {
          busy.shared++;
          return { member: busy, shared: true };
        }
      }
    }
    return this.enqueue(searchPath);
  }

  /** Gives back what one operation held. */
  release({ member, shared }: Grant): void {
    if (shared) {
      member.shared--;
      member.progressAt = this.now();
      if (member.shared === 0 && !member.exclusive) this.settle(member);
    } else {
      member.exclusive = false;
      this.settle(member);
    }
  }

  /** Rejects every waiting operation. */
  rejectWaiters(reason: Error): void {
    for (const waiter of this.waiters.splice(0)) {
      waiter.reject(reason);
    }
  }
}

/** A driver pool that grows with demand; see the module comment. */
export function createDriverPool(
  options: DriverPoolOptions,
  makeMember: MemberFactory = realMember,
  now: PoolClock = () => performance.now(),
): SQL {
  // The pool retires its members itself: `Bun.SQL` would retire a busy
  // connection and run the statements queued on it on a new session, whose
  // search_path is the server default, before `onclose` reports the close.
  const memberOptions: DriverPoolOptions = { ...options, max: 1, maxLifetime: 0 };
  const onclose = (memberOptions as { onclose?: (error: unknown) => void }).onclose;
  const set = new MemberSet(
    Math.max(1, options.max ?? 10),
    options.maxLifetime,
    (session) =>
      makeMember({
        ...memberOptions,
        onclose: (error: unknown) => {
          session.searchPath = UNKNOWN_SEARCH_PATH;
          session.closes++;
          onclose?.(error);
        },
      } as DriverPoolOptions),
    now,
  );
  let builder: SQL | undefined;
  let listener: SQL | undefined;
  let ending: Promise<void> | undefined;

  /** Leases a member alone; its path is unknown once given back. */
  const leaseAlone = async (): Promise<{ sql: SQL; giveBack: () => void }> => {
    if (ending) throw poolClosedError();
    const granted = await set.lease(undefined);
    return {
      sql: granted.member.sql,
      giveBack: () => {
        granted.member.session.searchPath = UNKNOWN_SEARCH_PATH;
        set.release(granted);
      },
    };
  };

  const reserve = async (): Promise<Reserved> => {
    const { sql, giveBack } = await leaseAlone();
    let reserved: Reserved;
    try {
      reserved = (await sql.reserve()) as Reserved;
    } catch (error) {
      giveBack();
      throw error;
    }
    let released = false;
    const release = (): void => {
      if (!released) {
        released = true;
        reserved.release();
        giveBack();
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

  const withMember = async <T>(fn: (member: SQL) => Promise<T>): Promise<T> => {
    const { sql, giveBack } = await leaseAlone();
    try {
      return await fn(sql);
    } finally {
      giveBack();
    }
  };

  const leased = (granted: Grant): LeasedMember => {
    let released = false;
    return {
      sql: granted.member.sql,
      session: granted.member.session,
      release: () => {
        if (!released) {
          released = true;
          set.release(granted);
        }
      },
    };
  };

  /** A member for one operation: at once when one is free or shareable, else once one is given back. */
  const leaseForOperation = (searchPath?: string): LeasedMember | Promise<LeasedMember> => {
    if (ending) {
      return Promise.reject(poolClosedError());
    }
    const granted = set.lease(searchPath);
    return granted instanceof Promise ? granted.then(leased) : leased(granted);
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
      set.rejectWaiters(poolClosedError());
      const instances = [...set.members.map((member) => member.sql), builder, listener].filter(
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
      // The listener keeps the configured lifetime: it runs no pooled
      // statement, so nothing of it can land on a new session.
      listener ??= makeMember({ ...memberOptions, maxLifetime: options.maxLifetime });
      return listener.listen(channel, onnotify, onlisten);
    },
    end,
    close: end,
    [LEASE]: leaseForOperation,
  }) as unknown as SQL;
}
