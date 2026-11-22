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
 * and `end()`/`close()`.
 */

/** The `Bun.SQL` options of the whole pool; `max` is its size. */
export type DriverPoolOptions = NonNullable<ConstructorParameters<typeof SQL>[0]> & { max?: number };

/** Builds one single-connection instance; a test seam. */
export type MemberFactory = (options: DriverPoolOptions) => SQL;

const realMember: MemberFactory = (options) => new SQL(options);

type Reserved = SQL & { release(): void };

interface Waiter {
  resolve(member: SQL): void;
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
  const members: SQL[] = [];
  // Released members, the most recently released last: reused first, so the
  // members at the bottom go idle and their connection times out.
  const free: SQL[] = [];
  const waiters: Waiter[] = [];
  let builder: SQL | undefined;
  let listener: SQL | undefined;
  let ending: Promise<void> | undefined;

  const lease = (): Promise<SQL> => {
    if (ending) {
      return Promise.reject(poolClosedError());
    }
    const member = free.pop();
    if (member) {
      return Promise.resolve(member);
    }
    if (members.length < max) {
      const created = makeMember(memberOptions);
      members.push(created);
      return Promise.resolve(created);
    }
    return new Promise<SQL>((resolve, reject) => waiters.push({ resolve, reject }));
  };

  const giveBack = (member: SQL): void => {
    const waiter = waiters.shift();
    if (waiter) {
      waiter.resolve(member);
    } else {
      free.push(member);
    }
  };

  const reserve = async (): Promise<Reserved> => {
    const member = await lease();
    let reserved: Reserved;
    try {
      reserved = (await member.reserve()) as Reserved;
    } catch (error) {
      giveBack(member);
      throw error;
    }
    let released = false;
    const release = (): void => {
      if (!released) {
        released = true;
        reserved.release();
        giveBack(member);
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
      return await fn(member);
    } finally {
      giveBack(member);
    }
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
      const instances = [...members, builder, listener].filter((instance): instance is SQL => instance !== undefined);
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
  }) as unknown as SQL;
}
