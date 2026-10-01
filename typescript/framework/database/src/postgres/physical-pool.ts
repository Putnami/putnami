import postgres from 'postgres';

/**
 * The physical pool registry: one postgres.js `Sql` per CONNECTION IDENTITY,
 * shared by every datasource whose effective connection is byte-identical.
 *
 * A datasource is one schema of one database. A workload that owns several
 * schemas of one physical database declares several datasources whose
 * connections are identical and whose only difference is `search_path`. A
 * factory that opened one `Sql` (one pool of `poolSize` connections) per
 * datasource would scale idle backends and the connection ceiling with
 * schemas instead of databases. This module keys the physical pool by the
 * connection identity — never by datasource name — and hands each datasource a
 * reference-counted handle on it; `search_path` is set per operation by the
 * datasource-bound client (see datasource-client.ts and doc/adr/0003).
 *
 * The identity carries the password, so it never appears in a log or an
 * error: every diagnostic names datasources only.
 */

/**
 * The subset of the connection config a physical pool is CREATED with, and
 * that every datasource sharing the pool must therefore declare identically. A
 * shared pool takes one tuning; a disagreement is a declaration error, never a
 * silently larger ceiling or a silent second pool. `search_path`, the
 * observability switches (`slowQueryThresholdMs`, `queryProfiling`) and
 * `maxRowLimit` are per logical client and stay out of it.
 *
 * Identity hooks (`__setIdentityResolverForTests`, `__setTokenFetcherForTests`)
 * are process-global in TypeScript — one resolver and one fetcher for the whole
 * process — so, unlike the Go adapter's per-pool `IdentityResolver` and
 * `TokenFetcher`, there is nothing to compare.
 */
export interface PhysicalTuning {
  readonly poolSize: number;
  readonly connectTimeout: number;
  readonly idleTimeout: number;
  readonly maxLifetime: number;
  readonly statementTimeoutMs: number;
  readonly idleInTransactionTimeoutMs: number;
  readonly debug: boolean;
}

const TUNING_FIELDS: readonly (keyof PhysicalTuning)[] = [
  'poolSize',
  'connectTimeout',
  'idleTimeout',
  'maxLifetime',
  'statementTimeoutMs',
  'idleInTransactionTimeoutMs',
  'debug',
];

/**
 * diffTuning lists every field on which `other` disagrees with `created`,
 * rendered for the declaration error (`poolSize 10 vs 4`). Empty when the
 * tunings agree.
 */
function diffTuning(created: PhysicalTuning, other: PhysicalTuning): string[] {
  const diffs: string[] = [];
  for (const field of TUNING_FIELDS) {
    if (created[field] !== other[field]) {
      diffs.push(`${field} ${String(created[field])} vs ${String(other[field])}`);
    }
  }
  return diffs;
}

/**
 * tuningMismatchError is the declaration error raised when datasource `other`
 * resolves to the physical database `opener` already opened with a different
 * tuning. It names both datasources and each differing field — never the
 * connection identity, which carries the password. The wording mirrors the Go
 * adapter's `db.datasource` error so both frameworks report one contract.
 */
export function tuningMismatchError(opener: string, other: string, diffs: string[]): Error {
  return new Error(
    `database: datasources ${JSON.stringify(opener)} and ${JSON.stringify(other)} resolve to the same physical database but declare different pool tuning (${diffs.join(', ')}); a shared pool takes one tuning — declare it identically on every datasource of that database`,
  );
}

/**
 * PhysicalPool is one postgres.js `Sql` shared by every datasource-bound
 * client whose datasource resolves to the same connection identity. Clients
 * hold references: `release` drops one and the `Sql` ends when the last handle
 * is released, so closing one datasource never kills another datasource's
 * connections.
 */
class PhysicalPool {
  private refs = 1;
  private closed = false;
  private ending: Promise<void> | undefined;

  constructor(
    /** The shared driver pool. */
    readonly sql: postgres.Sql,
    /** What the pool was created with; every later datasource must match it. */
    readonly tuning: PhysicalTuning,
    /** The datasource that created the pool, named in the mismatch diagnostic. */
    readonly opener: string,
    /** The identity the registry keys this pool under (never logged). */
    private readonly identity: string,
  ) {}

  /**
   * retain takes one more reference. It reports false when the pool has already
   * ended (every handle was released), so a registry holding a stale entry
   * re-opens instead of handing out an ended pool.
   */
  retain(): boolean {
    if (this.closed) {
      return false;
    }
    this.refs++;
    return true;
  }

  /**
   * release drops one reference and ends the driver pool when it was the last.
   * The returned promise settles when the pool has ended (or immediately when
   * other handles remain). Once ended, further calls are no-ops.
   */
  release(options?: { timeout?: number }): Promise<void> {
    if (this.closed) {
      return this.ending ?? Promise.resolve();
    }
    this.refs--;
    if (this.refs > 0) {
      return Promise.resolve();
    }
    this.closed = true;
    if (physical.get(this.identity) === this) {
      physical.delete(this.identity);
    }
    this.ending = this.sql.end(options);
    return this.ending;
  }

  /** isClosed reports whether the pool has ended. */
  isClosed(): boolean {
    return this.closed;
  }

  /**
   * forceEnd ends the driver pool whatever the reference count — the
   * `closeAllDatabases()` sweep. Handles released afterwards are no-ops.
   */
  forceEnd(): Promise<void> {
    if (this.closed) {
      return this.ending ?? Promise.resolve();
    }
    this.closed = true;
    this.refs = 0;
    if (physical.get(this.identity) === this) {
      physical.delete(this.identity);
    }
    this.ending = this.sql.end();
    return this.ending;
  }
}

type PhysicalFactory = (options: postgres.Options<Record<string, postgres.PostgresType>>) => postgres.Sql;

const realFactory: PhysicalFactory = (options) => postgres(options);

let factory: PhysicalFactory = realFactory;

/**
 * @internal Test seam: substitute the `postgres()` constructor the registry
 * opens physical pools with, so the sharing, dedup, tuning, and lifetime rules
 * are provable without a database. Pass `undefined` to restore the driver.
 * Not part of the public API — this module is excluded from the package
 * barrel, like `__setTransactionConnectionFactory`.
 */
export function __setPhysicalPoolFactoryForTests(fn: PhysicalFactory | undefined): void {
  factory = fn ?? realFactory;
}

/** Opened physical pools: connection identity → shared pool. */
const physical = new Map<string, PhysicalPool>();
/** In-flight first opens: connection identity → the connect that will settle it. */
const connecting = new Map<string, Promise<PhysicalPool>>();

/**
 * acquirePhysical returns the physical pool for `identity` with one reference
 * taken for `datasource`, connecting it when no datasource has yet. A pool
 * already opened by another datasource is shared only when `tuning` agrees
 * with the tuning it was created with; a disagreement is a declaration error
 * naming both datasources and each differing field. Concurrent first opens of
 * one identity collapse to one connect: the second datasource awaits the
 * first's connect, then re-enters to find the pool (and have its tuning
 * checked) or, when the connect failed, to label the shared error with its own
 * name. A failed connect is never cached, so a later call retries.
 */
export async function acquirePhysical(
  identity: string,
  datasource: string,
  options: postgres.Options<Record<string, postgres.PostgresType>>,
  tuning: PhysicalTuning,
): Promise<PhysicalPool> {
  for (;;) {
    const existing = physical.get(identity);
    if (existing) {
      const diffs = diffTuning(existing.tuning, tuning);
      if (diffs.length > 0) {
        throw tuningMismatchError(existing.opener, datasource, diffs);
      }
      if (existing.retain()) {
        return existing;
      }
      // Every handle was released and the pool ended underneath the cache; the
      // entry is stale, so drop it and connect anew below.
      physical.delete(identity);
    }
    const inflight = connecting.get(identity);
    if (inflight) {
      try {
        await inflight;
      } catch (err) {
        throw labelConnectError(err, datasource);
      }
      continue;
    }
    const connect = (async () => {
      const sql = factory(options);
      try {
        // A pool that cannot answer a ping is not cached; the next call retries.
        await sql`SELECT 1`;
      } catch (err) {
        await sql.end().catch(() => {});
        throw err;
      }
      const pool = new PhysicalPool(sql, tuning, datasource, identity);
      physical.set(identity, pool);
      return pool;
    })().finally(() => {
      connecting.delete(identity);
    });
    connecting.set(identity, connect);
    try {
      return await connect;
    } catch (err) {
      throw labelConnectError(err, datasource);
    }
  }
}

function labelConnectError(err: unknown, datasource: string): Error {
  const cause = err instanceof Error ? err : new Error(String(err));
  const labeled = new Error(`database: open pool for datasource ${JSON.stringify(datasource)}: ${cause.message}`, {
    cause,
  });
  return labeled;
}

/**
 * physicalPoolCount reports the number of distinct physical pools currently
 * open — the number of databases the opened datasources reach, not the number
 * of datasources. It is the observable a test uses to prove sharing without a
 * database, and what the `sql.pool.count` gauge reports.
 */
export function physicalPoolCount(): number {
  let n = 0;
  for (const pool of physical.values()) {
    if (!pool.isClosed()) {
      n++;
    }
  }
  return n;
}

/**
 * endAllPhysicalPools ends every physical pool still open whatever its
 * reference count — the `closeAllDatabases()` sweep — and drops the cache, so
 * a later open connects anew.
 */
export async function endAllPhysicalPools(): Promise<void> {
  const pools = Array.from(physical.values());
  physical.clear();
  await Promise.all(pools.map((pool) => pool.forceEnd()));
}
