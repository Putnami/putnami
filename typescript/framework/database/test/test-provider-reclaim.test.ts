import { describe, expect, it } from 'bun:test';
import { isolatedPrefix, pgIdent, planDatabases } from '../src/test-provider';
import {
  createdAt,
  isolatedSuffix,
  MAX_RECLAIM_PER_PROVISION,
  ORPHAN_AGE_MS,
  type OrphanServer,
  reclaimOrphans,
  staleOrphans,
  SUFFIX_LEN,
} from '../src/test-provider-reclaim';

const NOW = new Date(1_790_000_000 * 1000);
const HOUR = 60 * 60 * 1000;

/** stamped builds a per-suite database name created at `at`, the way the provider names one. */
function stamped(prefix: string, at: Date, random: string): string {
  return (
    prefix +
    Math.floor(at.getTime() / 1000)
      .toString(16)
      .padStart(8, '0') +
    random
  );
}

const key = (name: string): number => name.length;

/**
 * A Postgres server for reclaimOrphans: the databases that exist, how many
 * connections each has open, and the advisory locks other sessions hold.
 */
class FakeServer implements OrphanServer {
  dropped: string[] = [];
  unlocked: number[] = [];
  heldLocks = new Set<number>();
  lockError?: Error;
  listError?: Error;
  /** A suite that connects between the listing and the DROP. */
  connectBeforeDrop?: string;

  constructor(readonly databases: Map<string, number>) {}

  async tryLock(k: number): Promise<boolean> {
    if (this.lockError) {
      throw this.lockError;
    }
    return !this.heldLocks.has(k);
  }

  async unlock(k: number): Promise<void> {
    this.unlocked.push(k);
  }

  async idleDatabases(prefix: string): Promise<string[]> {
    if (this.listError) {
      throw this.listError;
    }
    return [...this.databases].filter(([n, c]) => n.startsWith(prefix) && c === 0).map(([n]) => n);
  }

  async dropIdle(name: string): Promise<void> {
    if (name === this.connectBeforeDrop) {
      this.databases.set(name, (this.databases.get(name) ?? 0) + 1);
    }
    if ((this.databases.get(name) ?? 0) > 0) {
      throw new Error('database is being accessed by other users');
    }
    this.databases.delete(name);
    this.dropped.push(name);
  }
}

describe('isolated database names', () => {
  it('carry their creation time', () => {
    const suffix = isolatedSuffix(NOW);
    expect(suffix).toHaveLength(SUFFIX_LEN);
    const prefix = isolatedPrefix('putnami_platform_test');
    expect(createdAt(prefix + suffix, prefix)?.getTime()).toBe(NOW.getTime());
    expect(isolatedSuffix(NOW)).not.toBe(suffix);
  });

  it('share the prefix the reclaim pass matches, even when the base is clamped', () => {
    for (const base of ['putnami_platform_test', 'Auth-DB', 'x'.repeat(100)]) {
      const name = pgIdent(base, 't', isolatedSuffix(NOW));
      const prefix = isolatedPrefix(base);
      expect(name.startsWith(prefix)).toBe(true);
      expect(createdAt(name, prefix)).toBeDefined();
      expect(name.length).toBeLessThanOrEqual(63);
    }
  });

  it('are planned with the reclaim prefix', () => {
    const [plan] = planDatabases(
      {
        protocolVersion: 1,
        databases: {
          auth: {
            engine: 'postgres',
            schema: 'iam',
            connection: { host: 'h', port: 5432, database: 'putnami_platform_test' },
          },
        },
      },
      () => 'aaa',
    );
    expect(plan.isoPrefix).toBe('putnami_platform_test_t_');
  });

  it('cannot be aged when they do not carry a time', () => {
    for (const name of [
      'base_t_be4612317a',
      'base_tmpl_0123456789ab',
      'other_t_68b1c2d30123abcd',
      'base_t_68B1C2D30123ABCD',
      'base_t_68b1c2d30123abcg',
      'base_t_68b1c2d30123abcd0',
      'base',
      'base_t_68b1c2d30123abc',
    ]) {
      expect(createdAt(name, 'base_t_')).toBeUndefined();
    }
  });
});

describe('staleOrphans', () => {
  it('selects idle databases older than the bound, oldest first', () => {
    const prefix = 'base_t_';
    const oldest = stamped(prefix, new Date(NOW.getTime() - 72 * HOUR), '00000001');
    const old = stamped(prefix, new Date(NOW.getTime() - ORPHAN_AGE_MS), '00000002');
    const young = stamped(prefix, new Date(NOW.getTime() - ORPHAN_AGE_MS + 1000), '00000003');
    const future = stamped(prefix, new Date(NOW.getTime() + HOUR), '00000004');
    expect(staleOrphans([young, old, 'base_t_be4612317a', future, oldest], prefix, NOW)).toEqual([oldest, old]);
  });

  it('caps one pass', () => {
    const prefix = 'base_t_';
    const idle = Array.from({ length: MAX_RECLAIM_PER_PROVISION + 5 }, (_, i) =>
      stamped(prefix, new Date(NOW.getTime() - (i + 2) * HOUR), i.toString(16).padStart(8, '0')),
    );
    const got = staleOrphans(idle, prefix, NOW);
    expect(got).toHaveLength(MAX_RECLAIM_PER_PROVISION);
    expect(got[0]).toBe(idle[idle.length - 1]);
  });
});

describe('reclaimOrphans', () => {
  const prefix = 'base_t_';
  const threeHoursAgo = new Date(NOW.getTime() - 3 * HOUR);

  it("never drops a live suite's database", async () => {
    const orphan = stamped(prefix, threeHoursAgo, '0000000a');
    const liveConnected = stamped(prefix, threeHoursAgo, '0000000b');
    const liveIdleYoung = stamped(prefix, new Date(NOW.getTime() - 5 * 60 * 1000), '0000000c');
    const liveConnecting = stamped(prefix, threeHoursAgo, '0000000d');
    const server = new FakeServer(
      new Map([
        [orphan, 0],
        [liveConnected, 1],
        [liveIdleYoung, 0],
        [liveConnecting, 0],
        ['base_t_be4612317a', 0],
        ['base', 3],
      ]),
    );
    server.connectBeforeDrop = liveConnecting;

    expect(await reclaimOrphans(server, prefix, NOW, key)).toEqual([orphan]);
    for (const live of [liveConnected, liveIdleYoung, liveConnecting, 'base_t_be4612317a', 'base']) {
      expect(server.databases.has(live)).toBe(true);
    }
    expect(server.unlocked).toHaveLength(1);
  });

  it('skips while another suite reclaims the same base', async () => {
    const server = new FakeServer(new Map([[stamped(prefix, threeHoursAgo, '0000000a'), 0]]));
    server.heldLocks.add(key(`reclaim:${prefix}`));
    expect(await reclaimOrphans(server, prefix, NOW, key)).toEqual([]);
    expect(server.unlocked).toHaveLength(0);
  });

  it('is best-effort when the server errors', async () => {
    const locking = new FakeServer(new Map([[stamped(prefix, threeHoursAgo, '0000000a'), 0]]));
    locking.lockError = new Error('boom');
    expect(await reclaimOrphans(locking, prefix, NOW, key)).toEqual([]);

    const listing = new FakeServer(new Map([[stamped(prefix, threeHoursAgo, '0000000a'), 0]]));
    listing.listError = new Error('boom');
    expect(await reclaimOrphans(listing, prefix, NOW, key)).toEqual([]);
    expect(listing.unlocked).toHaveLength(1);
  });
});
