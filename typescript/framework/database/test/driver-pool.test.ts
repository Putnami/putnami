import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/spectest';
import type { SQL } from 'bun';
import {
  createDriverPool,
  DEFAULT_LIFETIME_MAX_SECONDS,
  DEFAULT_LIFETIME_MIN_SECONDS,
  type DriverPoolOptions,
  leaseMember,
  memberLifetime,
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

      const { created, factory } = fakeMembers();
      const pool = createDriverPool({ max: 3 }, factory);
      const held = await Promise.all([pool.reserve(), pool.reserve(), pool.reserve()]);
      for (const conn of held as Reserved[]) {
        conn.release();
      }
      expect(created).toHaveLength(3);
      for (const member of created) {
        const lifetime = member.options.maxLifetime ?? 0;
        expect(lifetime).toBeGreaterThanOrEqual(DEFAULT_LIFETIME_MIN_SECONDS);
        expect(lifetime).toBeLessThanOrEqual(DEFAULT_LIFETIME_MAX_SECONDS);
      }

      const configured = fakeMembers();
      const fixed = createDriverPool({ max: 1, maxLifetime: 120 }, configured.factory);
      await fixed`SELECT 1`;
      expect(configured.created[0]?.options.maxLifetime).toBe(120);
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
