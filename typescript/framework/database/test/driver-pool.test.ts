import { describe, expect, it } from 'bun:test';
import type { SQL } from 'bun';
import { specTest } from '@putnami/spectest';
import {
  createDriverPool,
  DEFAULT_LIFETIME_MAX_SECONDS,
  DEFAULT_LIFETIME_MIN_SECONDS,
  type DriverPoolOptions,
  leaseMember,
  memberLifetime,
  SHARED_STALL_MS,
  UNKNOWN_SEARCH_PATH,
} from '../src/postgres/driver-pool';

const FEATURE = 'typescript/shared-datasource-pooling';

/**
 * A single-connection stand-in for a `Bun.SQL` instance: it records what runs
 * on it, so the pool's growth, reuse and waiting are provable without a
 * database.
 */
interface FakeMember {
  id: number;
  options: DriverPoolOptions;
  log: string[];
  ended: boolean;
}

function fakeMembers() {
  const created: FakeMember[] = [];
  const factory = (options: DriverPoolOptions): SQL => {
    const member: FakeMember = { id: created.length + 1, options, log: [], ended: false };
    created.push(member);
    const run = (strings: TemplateStringsArray | string, ...values: unknown[]) => {
      if (typeof strings === 'string') {
        return { builder: strings, values };
      }
      member.log.push(strings.join('?'));
      return Promise.resolve(Object.assign([{ member: member.id }], { count: 1, command: 'SELECT' }));
    };
    return Object.assign(run, {
      reserve: async () => {
        member.log.push('reserve');
        return Object.assign((strings: TemplateStringsArray) => run(strings), {
          release: () => member.log.push('release'),
        });
      },
      begin: async (fn: (tx: unknown) => unknown) => {
        member.log.push('begin');
        return fn(run);
      },
      listen: async (channel: string) => {
        member.log.push(`listen:${channel}`);
        return { unlisten: async () => {} };
      },
      json: (value: unknown) => ({ json: value }),
      array: (values: unknown[], type?: unknown) => ({ array: values, type }),
      end: async () => {
        member.ended = true;
      },
      close: async () => {
        member.ended = true;
      },
    }) as unknown as SQL;
  };
  return { created, factory };
}

type Reserved = SQL & { release(): void };

describe('driver pool — connections on demand', () => {
  specTest(
    'opens a connection only when every connection it holds is leased',
    {
      feature: FEATURE,
      requirement: 'connections-on-demand',
      check: 'a-pool-opens-a-connection-only-when-all-are-leased',
    },
    async () => {
      const { created, factory } = fakeMembers();
      const pool = createDriverPool({ max: 5 }, factory);

      // Sequential use keeps one connection.
      for (let i = 0; i < 3; i++) {
        const conn = (await pool.reserve()) as Reserved;
        await conn`SELECT 1`;
        conn.release();
      }
      expect(created).toHaveLength(1);
      expect(created[0]?.options.max).toBe(1);

      // Three operations at once need three.
      const held = (await Promise.all([pool.reserve(), pool.reserve(), pool.reserve()])) as Reserved[];
      expect(created).toHaveLength(3);
      for (const conn of held) {
        conn.release();
      }
    },
  );

  specTest(
    'makes an operation that finds every connection leased wait for one to be released',
    {
      feature: FEATURE,
      requirement: 'connections-on-demand',
      check: 'a-full-pool-makes-the-next-operation-wait',
    },
    async () => {
      const { created, factory } = fakeMembers();
      const pool = createDriverPool({ max: 2 }, factory);
      const first = (await pool.reserve()) as Reserved;
      const second = (await pool.reserve()) as Reserved;
      let third: Reserved | undefined;
      const waiting = pool.reserve().then((conn) => {
        third = conn as Reserved;
      });
      await new Promise((resolve) => setTimeout(resolve, 5));
      expect(third).toBeUndefined();
      expect(created).toHaveLength(2);

      first.release();
      await waiting;
      expect(third).toBeDefined();
      expect(created).toHaveLength(2);
      // A second release() of the same connection gives nothing back twice.
      first.release();
      second.release();
      third?.release();
    },
  );

  specTest(
    'hands out the most recently released connection first',
    {
      feature: FEATURE,
      requirement: 'connections-on-demand',
      check: 'the-most-recently-released-connection-is-reused-first',
    },
    async () => {
      const { created, factory } = fakeMembers();
      const pool = createDriverPool({ max: 3 }, factory);
      const [a, b, c] = (await Promise.all([pool.reserve(), pool.reserve(), pool.reserve()])) as Reserved[];
      a?.release();
      c?.release();
      b?.release();
      // b went back last: it serves the next operation, and keeps serving
      // sequential ones while a and c stay idle.
      for (let i = 0; i < 3; i++) {
        const conn = (await pool.reserve()) as Reserved;
        const [row] = (await conn`SELECT 1`) as unknown as { member: number }[];
        expect(row?.member).toBe(2);
        conn.release();
      }
      expect(created.map((m) => m.log.filter((l) => l === 'reserve').length)).toEqual([1, 4, 1]);
    },
  );

  it('runs a template, begin and listen on members, and builders on an instance that runs nothing', async () => {
    const { created, factory } = fakeMembers();
    const pool = createDriverPool({ max: 2 }, factory);
    const callable = pool as unknown as (...args: unknown[]) => unknown;

    expect(callable('my_table')).toEqual({ builder: 'my_table', values: [] });
    expect(pool.array([1], 'int4')).toEqual({ array: [1], type: 'int4' } as never);
    expect((pool as unknown as { json(v: unknown): unknown }).json({ k: 1 })).toEqual({ json: { k: 1 } });
    const builder = created[0];
    expect(builder?.log).toEqual([]);

    const strings = Object.assign(['SELECT 1'], { raw: ['SELECT 1'] }) as unknown as TemplateStringsArray;
    await callable(strings);
    expect(await pool.begin(async () => 'done')).toBe('done');
    await pool.listen('events', () => {});
    const logs = created.map((m) => m.log);
    // The builder instance ran nothing; one member ran the statement and the
    // transaction; LISTEN has an instance of its own.
    expect(logs).toEqual([[], ['SELECT 1', 'begin'], ['listen:events']]);

    await pool.end();
    expect(created.every((m) => m.ended)).toBe(true);
    await expect(pool.reserve()).rejects.toThrow('the connection pool is closed');
  });

  it('gives a member back when its connection cannot be reserved', async () => {
    let fail = true;
    const { created, factory } = fakeMembers();
    const pool = createDriverPool({ max: 1 }, (options) => {
      const member = factory(options);
      const reserve = member.reserve.bind(member);
      return Object.assign(member, {
        reserve: async () => {
          if (fail) {
            fail = false;
            throw new Error('connection refused');
          }
          return reserve();
        },
      });
    });
    await expect(pool.reserve()).rejects.toThrow('connection refused');
    const conn = (await pool.reserve()) as Reserved;
    expect(created).toHaveLength(1);
    conn.release();
  });

  it('rejects the operations still waiting when it ends', async () => {
    const { factory } = fakeMembers();
    const pool = createDriverPool({ max: 1 }, factory);
    const held = (await pool.reserve()) as Reserved;
    const waiting = pool.reserve();
    await pool.end();
    await expect(waiting).rejects.toThrow('the connection pool is closed');
    held.release();
  });
});

describe('driver pool — connection lifetime', () => {
  specTest(
    'each connection draws a lifetime between 45 and 90 minutes when none is configured, and keeps a configured one',
    {
      feature: FEATURE,
      requirement: 'connection-lifetime',
      check: 'an-unset-lifetime-draws-45-to-90-minutes-per-connection',
    },
    async () => {
      expect(memberLifetime(undefined, () => 0)).toBe(DEFAULT_LIFETIME_MIN_SECONDS);
      expect(memberLifetime(0, () => 1)).toBe(DEFAULT_LIFETIME_MAX_SECONDS);
      expect(memberLifetime(600, () => 0.5)).toBe(600);

      // The pool owns the lifetime: Bun.SQL never retires a member's connection.
      let clock = 0;
      const { created, factory } = fakeMembers();
      const pool = createDriverPool({ max: 2 }, factory, () => clock);
      const first = (await pool.reserve()) as Reserved;
      first.release();
      expect(created[0]?.options.maxLifetime).toBe(0);
      clock = DEFAULT_LIFETIME_MIN_SECONDS * 1000 - 1;
      const reused = (await pool.reserve()) as Reserved;
      reused.release();
      expect(created).toHaveLength(1);
      clock = DEFAULT_LIFETIME_MAX_SECONDS * 1000;
      const renewed = (await pool.reserve()) as Reserved;
      renewed.release();
      expect(created).toHaveLength(2);
      expect(created[0]?.ended).toBe(true);

      clock = 0;
      const configured = fakeMembers();
      const fixed = createDriverPool({ max: 1, maxLifetime: 120 }, configured.factory, () => clock);
      await fixed`SELECT 1`;
      clock = 119_999;
      await fixed`SELECT 1`;
      expect(configured.created).toHaveLength(1);
      clock = 120_000;
      await fixed`SELECT 1`;
      expect(configured.created).toHaveLength(2);
      expect(configured.created[0]?.ended).toBe(true);
    },
  );

  specTest(
    'a member past its lifetime takes no new operation and retires once nothing runs on it',
    {
      feature: FEATURE,
      requirement: 'connection-lifetime',
      check: 'a-busy-member-retires-once-drained-and-takes-no-new-operation',
    },
    async () => {
      let clock = 0;
      const { created, factory } = fakeMembers();
      const pool = createDriverPool({ max: 1, maxLifetime: 60 }, factory, () => clock);
      const primed = await leaseMember(pool, 'app');
      if (!primed) throw new Error('no lease');
      primed.session.searchPath = 'app';
      primed.release();

      clock = 59_950;
      const running = await leaseMember(pool, 'app');
      // A statement finishes just before the lifetime ends, so only the lifetime stops new joins.
      clock = 59_990;
      const quick = await leaseMember(pool, 'app');
      expect(quick?.sql).toBe(running?.sql);
      quick?.release();
      clock = 60_000;
      // Past its lifetime, the busy member takes no joiner: the next operation waits.
      let joined = false;
      const joining = (
        leaseMember(pool, 'app') as Promise<{ sql: SQL; session: { searchPath: unknown }; release(): void }>
      ).then((member) => {
        joined = true;
        return member;
      });
      await new Promise((resolve) => setTimeout(resolve, 5));
      expect(joined).toBe(false);
      expect(created[0]?.ended).toBe(false);

      // Once it drains, it closes, and the waiter gets a new member whose path is unknown.
      running?.release();
      const next = await joining;
      expect(created[0]?.ended).toBe(true);
      expect(created).toHaveLength(2);
      expect(next.sql).not.toBe(running?.sql);
      expect(next.session.searchPath).toBe(UNKNOWN_SEARCH_PATH);
      next.release();
    },
  );
});

describe('driver pool — what it knows of a session', () => {
  specTest(
    'a leased member carries the path its session holds until its connection closes, a reservation or a transaction',
    {
      feature: FEATURE,
      requirement: 'acquire-time-search-path',
      check: 'the-pool-forgets-a-path-on-close-reserve-and-begin',
    },
    async () => {
      const { created, factory } = fakeMembers();
      const pool = createDriverPool({ max: 1 }, factory);
      const lease = async () => {
        const leased = await leaseMember(pool);
        if (!leased) throw new Error('no lease');
        return leased;
      };

      const first = await lease();
      expect(first.session.searchPath).toBe(UNKNOWN_SEARCH_PATH);
      first.session.searchPath = 'app';
      first.release();
      first.release();
      const again = await lease();
      expect(again.sql).toBe(first.sql);
      expect(again.session.searchPath).toBe('app');

      // The driver reports a close: the path is forgotten and the close counted.
      const options = created[0]?.options as { onclose?: (error: unknown) => void } | undefined;
      options?.onclose?.(new Error('idle'));
      expect(again.session.searchPath).toBe(UNKNOWN_SEARCH_PATH);
      expect(again.session.closes).toBe(1);
      again.session.searchPath = 'app';
      again.release();

      const reserved = (await pool.reserve()) as Reserved;
      reserved.release();
      const afterReserve = await lease();
      expect(afterReserve.session.searchPath).toBe(UNKNOWN_SEARCH_PATH);
      afterReserve.session.searchPath = 'app';
      afterReserve.release();

      await pool.begin(async () => 1);
      const afterBegin = await lease();
      expect(afterBegin.session.searchPath).toBe(UNKNOWN_SEARCH_PATH);
      afterBegin.release();

      expect(leaseMember((() => undefined) as unknown as SQL)).toBeUndefined();
    },
  );
});

describe('driver pool — operations share a connection', () => {
  const tick = () => new Promise((resolve) => setTimeout(resolve, 5));

  /** Leases one member for `path`, or fails when the pool is not a driver pool. */
  const leaseFor = async (pool: SQL, path?: string) => {
    const leased = await leaseMember(pool, path);
    if (!leased) throw new Error('no lease');
    return leased;
  };

  /** Opens `count` members whose sessions hold `path`, then gives them back idle. */
  const prime = async (pool: SQL, count: number, path: string) => {
    const held = await Promise.all(Array.from({ length: count }, () => leaseFor(pool, path)));
    for (const member of held) {
      member.session.searchPath = path;
    }
    for (const member of held) {
      member.release();
    }
  };

  specTest(
    'a full pool lets an operation join the least busy connection that holds its path',
    {
      feature: FEATURE,
      requirement: 'operations-share-a-connection',
      check: 'a-full-pool-shares-a-connection-that-holds-the-path',
    },
    async () => {
      const { created, factory } = fakeMembers();
      const pool = createDriverPool({ max: 2 }, factory);
      await prime(pool, 2, 'app');

      const first = await leaseFor(pool, 'app');
      const second = await leaseFor(pool, 'app');
      expect(second.sql).not.toBe(first.sql);

      // Both connections are in use: the next operations share them, one each, and open none.
      const third = await leaseFor(pool, 'app');
      const fourth = await leaseFor(pool, 'app');
      expect(created).toHaveLength(2);
      expect([third.sql, fourth.sql]).toContain(first.sql);
      expect([third.sql, fourth.sql]).toContain(second.sql);
      expect(third.sql).not.toBe(fourth.sql);

      for (const member of [first, second, third, fourth]) {
        member.release();
      }
      // Once every shared operation released, the connections are idle again.
      const reserved = (await Promise.all([pool.reserve(), pool.reserve()])) as Reserved[];
      expect(created).toHaveLength(2);
      for (const conn of reserved) {
        conn.release();
      }
    },
  );

  specTest(
    'a connection is shared only under the path its session holds',
    {
      feature: FEATURE,
      requirement: 'operations-share-a-connection',
      check: 'a-connection-is-shared-only-under-the-path-its-session-holds',
    },
    async () => {
      const { created, factory } = fakeMembers();
      const pool = createDriverPool({ max: 1 }, factory);

      // A member whose path is unknown is leased alone, even to an operation that names a path.
      const unknown = await leaseFor(pool, 'app');
      let joined = false;
      const joining = leaseFor(pool, 'app').then((member) => {
        joined = true;
        return member;
      });
      await tick();
      expect(joined).toBe(false);
      unknown.session.searchPath = 'app';
      unknown.release();
      const app = await joining;
      expect(app.session.searchPath).toBe('app');

      // Another datasource's operation waits: it needs the connection alone to set its path.
      let other: Awaited<ReturnType<typeof leaseFor>> | undefined;
      const otherWaiting = leaseFor(pool, 'other').then((member) => {
        other = member;
      });
      await tick();
      expect(other).toBeUndefined();

      app.release();
      await otherWaiting;
      expect(other?.session.searchPath).toBe('app');

      // While it holds the connection alone, an operation on the first path waits too.
      let back: Awaited<ReturnType<typeof leaseFor>> | undefined;
      const backWaiting = leaseFor(pool, 'app').then((member) => {
        back = member;
      });
      await tick();
      expect(back).toBeUndefined();
      if (other) other.session.searchPath = 'other';
      other?.release();
      await backWaiting;
      expect(back?.session.searchPath).toBe('other');
      back?.release();
      expect(created).toHaveLength(1);
    },
  );

  specTest(
    'a reservation, a transaction and a template call wait until no operation runs on the connection',
    {
      feature: FEATURE,
      requirement: 'operations-share-a-connection',
      check: 'an-exclusive-lease-waits-until-no-operation-runs-on-the-connection',
    },
    async () => {
      const { created, factory } = fakeMembers();
      const pool = createDriverPool({ max: 1 }, factory);
      await prime(pool, 1, 'app');

      const a = await leaseFor(pool, 'app');
      const b = await leaseFor(pool, 'app');
      expect(b.sql).toBe(a.sql);

      let reserved: Reserved | undefined;
      const reserving = pool.reserve().then((conn) => {
        reserved = conn as Reserved;
      });
      let began = false;
      const beginning = pool.begin(async () => {
        began = true;
      });
      let queried = false;
      const querying = Promise.resolve(pool`SELECT 1`).then(() => {
        queried = true;
      });
      await tick();
      expect(reserved).toBeUndefined();

      a.release();
      await tick();
      expect(reserved).toBeUndefined();

      // A shared operation that arrives while an exclusive lease waits does not get ahead of it.
      let late = false;
      const lateWaiting = leaseFor(pool, 'app').then((member) => {
        late = true;
        return member;
      });

      b.release();
      await reserving;
      expect(reserved).toBeDefined();
      expect(began).toBe(false);
      expect(queried).toBe(false);
      expect(late).toBe(false);

      reserved?.release();
      await beginning;
      await querying;
      const lateMember = await lateWaiting;
      // The reservation and the transaction made the path unknown: the late operation holds the connection alone.
      expect(lateMember.session.searchPath).toBe(UNKNOWN_SEARCH_PATH);
      lateMember.release();
      expect(created).toHaveLength(1);
      expect(created[0]?.log).toEqual(['reserve', 'release', 'begin', 'SELECT 1']);
    },
  );
});

describe('driver pool — shared operations under load', () => {
  const tick = () => new Promise((resolve) => setTimeout(resolve, 5));
  type Leased = NonNullable<Awaited<ReturnType<typeof leaseMember>>>;
  const leaseFor = async (pool: SQL, path?: string): Promise<Leased> => {
    const leased = await leaseMember(pool, path);
    if (!leased) throw new Error('no lease');
    return leased;
  };
  const prime = async (pool: SQL, count: number, path: string) => {
    const held = await Promise.all(Array.from({ length: count }, () => leaseFor(pool, path)));
    for (const member of held) member.session.searchPath = path;
    for (const member of held) member.release();
  };
  /** Leases for `path` and records, once granted, which member each operation got. */
  const track = (pool: SQL, path: string, got: Leased[]) =>
    leaseFor(pool, path).then((member) => {
      got.push(member);
      return member;
    });

  specTest(
    'a freed member serves the waiters behind it up to an even share, and never ahead of an earlier exclusive waiter',
    {
      feature: FEATURE,
      requirement: 'operations-share-a-connection',
      check: 'freed-members-share-the-waiters-evenly-in-their-order',
    },
    async () => {
      const { factory } = fakeMembers();
      const pool = createDriverPool({ max: 2 }, factory);
      await prime(pool, 2, 'app');
      const four = [
        await leaseFor(pool, 'app'),
        await leaseFor(pool, 'app'),
        await leaseFor(pool, 'app'),
        await leaseFor(pool, 'app'),
      ];
      // Two operations on each member: the joiners went to the least busy.
      const onFirst = four.filter((member) => member.sql === four[0]?.sql);
      const onOther = four.filter((member) => member.sql !== four[0]?.sql);
      expect(onFirst).toHaveLength(2);
      expect(onOther).toHaveLength(2);
      const [x1, x2] = onFirst as [Leased, Leased];
      const [y1, y2] = onOther as [Leased, Leased];

      // A reservation waits; four operations arrive behind it.
      let reserved: Reserved | undefined;
      const reserving = pool.reserve().then((conn) => {
        reserved = conn as Reserved;
      });
      const got: Leased[] = [];
      const waiting = Array.from({ length: 4 }, () => track(pool, 'app', got));
      await tick();
      expect(got).toHaveLength(0);

      // The first member to drain goes to the reservation, not to the operations behind it.
      x1.release();
      x2.release();
      await reserving;
      expect(got).toHaveLength(0);
      if (reserved) reserved.release();
      await tick();
      // Its path is unknown after the reservation: the first waiter gets it alone, to set it.
      expect(got).toHaveLength(1);
      const alone = got[0] as Leased;
      expect(alone.session.searchPath).toBe(UNKNOWN_SEARCH_PATH);
      alone.session.searchPath = 'app';

      // The other member drains: of the three waiters left, it takes an even share, two.
      y1.release();
      y2.release();
      await tick();
      expect(got.filter((member) => member.sql === y1.sql)).toHaveLength(2);
      expect(got).toHaveLength(3);
      // The last one waits for the next member to drain, and shares it under the path its session now holds.
      alone.release();
      await Promise.all(waiting);
      expect(got[3]?.sql).toBe(alone.sql);
      for (const member of got.slice(1)) member.release();

      // With [shared, exclusive, shared] waiting on a member whose path is known, a freed
      // member serves the first and stops at the exclusive waiter.
      let clock = 0;
      const single = createDriverPool({ max: 1 }, fakeMembers().factory, () => clock);
      await prime(single, 1, 'app');
      const stalled = await leaseFor(single, 'app');
      clock = 2 * SHARED_STALL_MS;
      const order: string[] = [];
      const first = leaseFor(single, 'app').then((member) => {
        order.push('first');
        return member;
      });
      const exclusive = single.reserve().then((conn) => {
        order.push('reserve');
        return conn as Reserved;
      });
      const third = leaseFor(single, 'app').then((member) => {
        order.push('third');
        return member;
      });
      await tick();
      expect(order).toEqual([]);
      stalled.release();
      const firstMember = await first;
      await tick();
      expect(order).toEqual(['first']);
      expect(firstMember.session.searchPath).toBe('app');
      firstMember.release();
      (await exclusive).release();
      (await third).release();
      expect(order).toEqual(['first', 'reserve', 'third']);
    },
  );

  specTest(
    'operations stop joining a member whose statements have not finished for a while',
    {
      feature: FEATURE,
      requirement: 'operations-share-a-connection',
      check: 'a-stalled-member-takes-no-joiner',
    },
    async () => {
      let clock = 0;
      const { factory } = fakeMembers();
      const pool = createDriverPool({ max: 1 }, factory, () => clock);
      await prime(pool, 1, 'app');
      const blocked = await leaseFor(pool, 'app');
      clock = SHARED_STALL_MS - 1;
      const joined = await leaseFor(pool, 'app');
      expect(joined.sql).toBe(blocked.sql);
      joined.release();
      // The last statement finished at SHARED_STALL_MS - 1; past the stall time, nothing joins.
      clock = 2 * SHARED_STALL_MS;
      let late: Leased | undefined;
      const waiting = leaseFor(pool, 'app').then((member) => {
        late = member;
      });
      await tick();
      expect(late).toBeUndefined();
      blocked.release();
      await waiting;
      expect(late?.sql).toBe(blocked.sql);
      late?.release();
    },
  );

  specTest(
    'a close while shared stops new joins, and end() rejects the operations still waiting',
    {
      feature: FEATURE,
      requirement: 'operations-share-a-connection',
      check: 'a-connection-is-shared-only-under-the-path-its-session-holds',
    },
    async () => {
      const { created, factory } = fakeMembers();
      const pool = createDriverPool({ max: 1 }, factory);
      await prime(pool, 1, 'app');
      const running = await leaseFor(pool, 'app');
      const options = created[0]?.options as { onclose?: (error: unknown) => void } | undefined;
      options?.onclose?.(new Error('terminated'));
      expect(running.session.searchPath).toBe(UNKNOWN_SEARCH_PATH);

      let next: Leased | undefined;
      const waiting = leaseFor(pool, 'app').then((member) => {
        next = member;
      });
      await tick();
      expect(next).toBeUndefined();
      running.release();
      await waiting;
      expect(next?.session.searchPath).toBe(UNKNOWN_SEARCH_PATH);

      // The waiter holds the member alone; another waits, and end() rejects it.
      const rejected = leaseFor(pool, 'app').then(
        () => 'leased',
        (error: Error) => error.message,
      );
      await pool.end();
      expect(await rejected).toContain('closed');
      next?.release();
    },
  );
});
