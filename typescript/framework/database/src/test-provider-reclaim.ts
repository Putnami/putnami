import type postgres from 'postgres';

/**
 * Reclaiming the per-suite databases a killed test process left behind — the
 * TypeScript half of `go.putnami.dev/database/testprovider`'s reclaim.go. Both
 * providers write and read the same name format, so each reclaims what the
 * other left behind.
 *
 * A per-suite database is named `<base>_t_<suffix>`, where the suffix is the
 * creation time (Unix seconds, 8 lowercase hex digits) followed by 8 random hex
 * digits. Postgres records no creation time for a database, so the name carries
 * it: that is what tells an abandoned database from one a live suite still uses.
 */

const SUFFIX_TIME_LEN = 8;
const SUFFIX_RAND_LEN = 8;
export const SUFFIX_LEN = SUFFIX_TIME_LEN + SUFFIX_RAND_LEN;

/**
 * How old an isolated database with no open connection must be before a later
 * `provision` drops it. A suite whose process is killed never runs `cleanup`,
 * so its database outlives it. A live suite can hold no connection for a moment
 * (its pool releases idle connections), but no live suite is an hour old.
 */
export const ORPHAN_AGE_MS = 60 * 60 * 1000;

/**
 * How many orphans one `provision` drops. Every DROP DATABASE forces a
 * cluster-wide checkpoint, so the cap keeps one suite's setup bounded; the next
 * `provision` continues where this one stopped.
 */
export const MAX_RECLAIM_PER_PROVISION = 16;

const HEX_SUFFIX = /^[0-9a-f]+$/;

/** isolatedSuffix is the per-suite suffix: the creation time followed by random hex. */
export function isolatedSuffix(now: Date): string {
  const seconds = Math.floor(now.getTime() / 1000) >>> 0;
  const random = crypto.getRandomValues(new Uint8Array(SUFFIX_RAND_LEN / 2));
  return (
    seconds.toString(16).padStart(SUFFIX_TIME_LEN, '0') +
    Array.from(random, (b) => b.toString(16).padStart(2, '0')).join('')
  );
}

/**
 * createdAt reads the creation time a per-suite database name carries, or
 * undefined for any name that is not `<prefix><16 lowercase hex digits>` —
 * including every name written before the suffix carried a time: those cannot
 * be aged, so they are never reclaimed.
 */
export function createdAt(name: string, prefix: string): Date | undefined {
  if (!name.startsWith(prefix)) {
    return undefined;
  }
  const suffix = name.slice(prefix.length);
  if (suffix.length !== SUFFIX_LEN || !HEX_SUFFIX.test(suffix)) {
    return undefined;
  }
  return new Date(Number.parseInt(suffix.slice(0, SUFFIX_TIME_LEN), 16) * 1000);
}

/**
 * staleOrphans selects, oldest first and at most MAX_RECLAIM_PER_PROVISION, the
 * idle databases old enough to reclaim. `idle` lists databases that had no open
 * connection when the server was asked.
 */
export function staleOrphans(idle: readonly string[], prefix: string, now: Date): string[] {
  const stale: Array<{ name: string; created: number }> = [];
  for (const name of idle) {
    const created = createdAt(name, prefix);
    if (created && now.getTime() - created.getTime() >= ORPHAN_AGE_MS) {
      stale.push({ name, created: created.getTime() });
    }
  }
  stale.sort((a, b) => a.created - b.created || (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  return stale.slice(0, MAX_RECLAIM_PER_PROVISION).map((o) => o.name);
}

/** What reclaiming needs from the server. `pgOrphanServer` is the Postgres implementation. */
export interface OrphanServer {
  /** Takes a session advisory lock without waiting and reports whether it got it. */
  tryLock(key: number): Promise<boolean>;
  unlock(key: number): Promise<void>;
  /** Lists the databases whose name starts with prefix and that have no open connection. */
  idleDatabases(prefix: string): Promise<string[]>;
  /** Drops a database only if nothing is connected to it. */
  dropIdle(name: string): Promise<void>;
}

/**
 * reclaimOrphans drops the per-suite databases of one base that a killed test
 * process left behind. It is best-effort: it never throws, and it returns the
 * names it dropped.
 *
 * A live suite's database is never dropped. Three conditions protect it: the
 * database must have no open connection, it must be older than ORPHAN_AGE_MS,
 * and the DROP carries no FORCE, so a suite that connects between the listing
 * and the DROP makes the DROP fail instead of losing its database.
 */
export async function reclaimOrphans(
  server: OrphanServer,
  prefix: string,
  now: Date,
  lockKey: (name: string) => number,
): Promise<string[]> {
  const key = lockKey(`reclaim:${prefix}`);
  try {
    if (!(await server.tryLock(key))) {
      return [];
    }
  } catch {
    return [];
  }
  const dropped: string[] = [];
  try {
    const idle = await server.idleDatabases(prefix);
    for (const name of staleOrphans(idle, prefix, now)) {
      try {
        await server.dropIdle(name);
        dropped.push(name);
      } catch {
        // A suite connected in between, or the server refused: leave it.
      }
    }
  } catch {
    // Listing failed: reclaim nothing this time.
  } finally {
    await server.unlock(key).catch(() => {});
  }
  return dropped;
}

/** pgOrphanServer runs the reclaim statements on a single-connection admin client. */
export function pgOrphanServer(admin: postgres.Sql, quoteIdent: (name: string) => string): OrphanServer {
  return {
    async tryLock(key) {
      const rows = await admin.unsafe('SELECT pg_try_advisory_lock($1) AS locked', [key]);
      return rows[0]?.['locked'] === true;
    },
    async unlock(key) {
      await admin.unsafe('SELECT pg_advisory_unlock($1)', [key]);
    },
    async idleDatabases(prefix) {
      const rows = await admin.unsafe(
        `SELECT d.datname FROM pg_database d
WHERE left(d.datname, length($1::text)) = $1::text
  AND NOT d.datistemplate
  AND NOT EXISTS (SELECT 1 FROM pg_stat_activity a WHERE a.datname = d.datname)`,
        [prefix],
      );
      return rows.map((r) => String(r['datname']));
    },
    async dropIdle(name) {
      await admin.unsafe(`DROP DATABASE IF EXISTS ${quoteIdent(name)}`);
    },
  };
}
