import { sha256Hex } from '@putnami/migration';
import { useLogger } from '@putnami/runtime';
import type postgres from 'postgres';
import { database } from '../factory';
import { MigrationError } from '../errors';
import {
  asError,
  DATABASE_MIGRATION_LOGGER,
  logMigrationApplied,
  logMigrationFailed,
  migrationFields,
} from '../observability/database-logging';
import { canonicalMigrationId, type Migration } from './migration.entity';
import type { SQLDefinition } from './sql-source';

/**
 * Plain code-unit comparison (`<`/`>`). Migration ordering MUST be
 * byte-identical to the Go runner's `defs[i].Name < defs[j].Name`
 * (go/framework/database/migration.go) and the bundle protocol — both
 * runners share one state store keyed `${datasource}:${name}`, so the
 * apply/rollback order must not vary by runner, locale, or machine.
 * `String.prototype.localeCompare` is ICU/locale-dependent and diverges
 * from byte order (e.g. `billing/…` vs `billing-api/…`, or lowercase
 * before uppercase), so it must not be used here.
 */
const byteCompare = (a: string, b: string): number => (a < b ? -1 : a > b ? 1 : 0);

/**
 * Per-datasource SQL migration engine. Constructed by the SQLRunner
 * fan-out; tests can construct it directly with an explicit
 * definition slice. Mirrors Go's `database.Migrator`.
 *
 * Definitions are taken as the authoritative migration set — the
 * Migrator does NOT consult any global registry.
 */
export class Migrator {
  private connection?: postgres.Sql;
  private readonly definitions: SQLDefinition[];
  private readonly logger = useLogger(DATABASE_MIGRATION_LOGGER);

  constructor(
    readonly datasource: string,
    definitions: readonly SQLDefinition[],
    /**
     * The schema this datasource's migrations create their objects in — the
     * schema half of the source's Datasource. When set, it is applied as the
     * transaction-local search_path before each up/down body, so migrations
     * use unqualified DDL and one migration set can target many schemas via
     * distinct Datasource{name, schema} pairs. Empty leaves the connection's
     * own search_path (pool/binding) in force. Mirrors Go's MigrationConfig.Schema.
     */
    readonly schema = '',
  ) {
    this.definitions = [...definitions].sort((a, b) => byteCompare(a.name, b.name));
  }

  /** Returns a defensive copy of the migrations registered with this Migrator. */
  getDefinitions(): SQLDefinition[] {
    return [...this.definitions];
  }

  /** Apply every pending migration registered for this datasource. */
  async up(): Promise<Migration[]> {
    return this.upTo();
  }

  /**
   * Apply pending migrations forward through `name` (inclusive). The
   * name may be either the fully namespaced form or the bare basename
   * when it is unambiguous; ambiguity is an error.
   */
  async upTo(name?: string): Promise<Migration[]> {
    if (this.definitions.length === 0) {
      this.logger.debug('no migrations registered', migrationFields({ datasource: this.datasource }));
      return [];
    }
    const target = name ? this.resolveName(name) : undefined;

    const result: Migration[] = [];
    await this.withLock(async (sql) => {
      const applied = await this.fetchApplied(sql);
      const appliedHashes = new Map(applied.map((m) => [m.name, m.hash]));
      for (const def of this.definitions) {
        const storedHash = appliedHashes.get(def.name);
        if (storedHash !== undefined) {
          // Already applied: the committed body must still hash to what was
          // applied. A divergence means the migration's SQL was edited after
          // it ran — the schema and the source no longer agree and a plain
          // forward apply (which skips by name) would silently ignore it. Fail
          // loudly rather than let the two drift apart unnoticed.
          // biome-ignore lint/performance/noAwaitInLoops: serial drift check
          const currentHash = await sha256Hex(def.sql);
          if (currentHash !== storedHash) {
            throw new MigrationError(
              `Migration "${def.name}" body has changed since it was applied to datasource "${this.datasource}" ` +
                `(applied hash ${storedHash}, current hash ${currentHash}). The committed SQL diverges from the ` +
                'applied schema; revert the edit or create a new migration.',
              def.name,
            );
          }
          if (target && def.name === target) break;
          continue;
        }
        // biome-ignore lint/performance/noAwaitInLoops: serial apply
        const record = await this.apply(sql, def);
        result.push(record);
        if (target && def.name === target) break;
      }
    });
    return result;
  }

  /** Return every recorded migration row for this datasource. */
  async status(): Promise<Migration[]> {
    let out: Migration[] = [];
    await this.withLock(async (sql) => {
      out = await this.fetchAll(sql);
    });
    return out;
  }

  /** Roll back the most recently applied migration. Returns undefined when nothing has been applied. */
  async rollback(): Promise<Migration | undefined> {
    let out: Migration | undefined;
    await this.withLock(async (sql) => {
      const applied = await this.fetchApplied(sql);
      if (applied.length === 0) return;
      // Roll back in the strict reverse of the apply order, which is by name
      // (see the constructor). Ordering by executedAt instead is unstable for
      // migrations applied in the same millisecond (batch up()) or under clock
      // skew, which can drop objects out of dependency order.
      const sorted = [...applied].sort((a, b) => byteCompare(b.name, a.name));
      out = await this.executeRollback(sql, sorted[0]!);
    });
    return out;
  }

  /** Roll back every migration applied after `name` in reverse execution order. */
  async rollbackTo(name: string): Promise<Migration[]> {
    const resolved = this.resolveName(name);
    const out: Migration[] = [];
    await this.withLock(async (sql) => {
      const applied = await this.fetchApplied(sql);
      // Roll back in the strict reverse of the apply order, which is by name
      // (see the constructor). Ordering by executedAt instead is unstable for
      // migrations applied in the same millisecond (batch up()) or under clock
      // skew, which can drop objects out of dependency order.
      const sorted = [...applied].sort((a, b) => byteCompare(b.name, a.name));
      const targetIndex = sorted.findIndex((m) => m.name === resolved);
      if (targetIndex === -1) {
        throw new MigrationError(
          `migration "${name}" not found in applied migrations for datasource "${this.datasource}"`,
          name,
        );
      }
      for (const mig of sorted.slice(0, targetIndex)) {
        // biome-ignore lint/performance/noAwaitInLoops: serial rollback
        const record = await this.executeRollback(sql, mig);
        out.push(record);
      }
    });
    return out;
  }

  /** Roll back every applied migration in reverse execution order. */
  async reset(): Promise<Migration[]> {
    const out: Migration[] = [];
    await this.withLock(async (sql) => {
      const applied = await this.fetchApplied(sql);
      // Roll back in the strict reverse of the apply order, which is by name
      // (see the constructor). Ordering by executedAt instead is unstable for
      // migrations applied in the same millisecond (batch up()) or under clock
      // skew, which can drop objects out of dependency order.
      const sorted = [...applied].sort((a, b) => byteCompare(b.name, a.name));
      for (const mig of sorted) {
        // biome-ignore lint/performance/noAwaitInLoops: serial rollback
        const record = await this.executeRollback(sql, mig);
        out.push(record);
      }
    });
    return out;
  }

  /**
   * Resolve a basename or full name to the unique full name from this
   * Migrator's definitions. Ambiguous basenames throw with the list of
   * candidates.
   */
  resolveName(name: string): string {
    if (name.includes('/')) {
      const found = this.definitions.find((d) => d.name === name);
      if (!found)
        throw new MigrationError(`migration "${name}" not registered for datasource "${this.datasource}"`, name);
      return name;
    }
    const matches = this.definitions.filter((d) => d.name.endsWith(`/${name}`)).map((d) => d.name);
    if (matches.length === 0) {
      throw new MigrationError(`migration "${name}" not registered for datasource "${this.datasource}"`, name);
    }
    if (matches.length > 1) {
      throw new MigrationError(
        `migration basename "${name}" is ambiguous; candidates: ${matches.sort().join(', ')}`,
        name,
      );
    }
    return matches[0]!;
  }

  // --- private --------------------------------------------------------------

  /**
   * Set search_path for the current transaction only (is_local = true), so a
   * migration body's unqualified DDL resolves against this datasource's declared
   * schema and reverts at COMMIT/ROLLBACK — never leaking onto the pooled
   * connection. A blank schema is a no-op, leaving the connection's own
   * search_path (pool/binding) in force — the path the test provider relies on.
   * The state-store SQL is fully qualified (migration.migrations) and so is
   * unaffected either way. Mirrors Go's `applySearchPath`.
   */
  private async applySearchPath(sql: postgres.ReservedSql): Promise<void> {
    if (!this.schema) return;
    await sql`SELECT set_config('search_path', ${this.schema}, true)`;
  }

  /**
   * Disable `statement_timeout` for the current transaction only (is_local =
   * true) by setting it to `0` (unlimited), so a long migration body (table
   * rewrite, large index build, backfill) is not aborted by the pool's runtime
   * `statement_timeout` (`database.statementTimeoutMs`, default 30s) that bounds
   * request-serving queries. Because it is set transaction-locally it reverts at
   * COMMIT/ROLLBACK and never leaks onto the pooled connection or any other
   * query. Mirrors Go's `clearStatementTimeout`, which likewise hardcodes `0`.
   */
  private async clearStatementTimeout(sql: postgres.ReservedSql): Promise<void> {
    await sql`SELECT set_config('statement_timeout', '0', true)`;
  }

  private async repository(): Promise<postgres.Sql> {
    if (this.connection) return this.connection;
    const sql = await database(this.datasource);
    await sql`CREATE SCHEMA IF NOT EXISTS migration;`;
    await sql`CREATE TABLE IF NOT EXISTS migration.migrations (
      id TEXT PRIMARY KEY,
      db_name TEXT NOT NULL,
      name TEXT NOT NULL,
      hash TEXT NOT NULL,
      executed_at TEXT NOT NULL,
      execution_time_ms INTEGER NOT NULL,
      success INTEGER NOT NULL,
      error_message TEXT,
      down_sql TEXT,
      down_hash TEXT
    );`;
    // Backfill columns for databases that predate the canonical schema.
    await sql`ALTER TABLE migration.migrations ADD COLUMN IF NOT EXISTS down_sql TEXT;`;
    await sql`ALTER TABLE migration.migrations ADD COLUMN IF NOT EXISTS down_hash TEXT;`;
    await sql`ALTER TABLE migration.migrations ADD COLUMN IF NOT EXISTS db_name TEXT;`;
    await sql`UPDATE migration.migrations SET db_name = ${this.datasource} WHERE db_name IS NULL;`;
    await sql`ALTER TABLE migration.migrations ALTER COLUMN db_name SET NOT NULL;`.catch(() => {});
    this.connection = sql;
    return sql;
  }

  /**
   * Hold a session-scoped advisory lock on the datasource while running fn.
   * Cross-language coordination: Go and TS hash the same literal
   * (`putnami.migration:<datasource>`) server-side via hashtext().
   */
  protected async withLock<T>(fn: (sql: postgres.ReservedSql) => Promise<T>): Promise<T> {
    const pool = await this.repository();
    const reserved = await pool.reserve();
    const lockKey = `putnami.migration:${this.datasource}`;
    try {
      await reserved`SELECT pg_advisory_lock(hashtext(${lockKey})::bigint)`;
      try {
        return await fn(reserved);
      } finally {
        try {
          await reserved`SELECT pg_advisory_unlock(hashtext(${lockKey})::bigint)`;
        } catch (e) {
          // An independent operational signal (the lock outlives the run), with
          // the thrown value coerced to a real Error so it reaches entry.error.
          this.logger.warn(
            'failed to release migration advisory lock',
            asError(e),
            migrationFields({ datasource: this.datasource }),
          );
        }
      }
    } finally {
      reserved.release();
    }
  }

  protected async fetchApplied(sql: postgres.Sql): Promise<Migration[]> {
    return await this.fetch(sql, true);
  }

  private async fetchAll(sql: postgres.Sql): Promise<Migration[]> {
    return await this.fetch(sql, false);
  }

  private async fetch(sql: postgres.Sql, successOnly: boolean): Promise<Migration[]> {
    const result = successOnly
      ? await sql`SELECT * FROM migration.migrations WHERE db_name = ${this.datasource} AND success = 1 ORDER BY executed_at ASC, name ASC`
      : await sql`SELECT * FROM migration.migrations WHERE db_name = ${this.datasource} ORDER BY executed_at ASC, name ASC`;
    return result.map((row) => ({
      id: row['id'],
      dbName: row['dbName'] ?? row['db_name'] ?? this.datasource,
      name: row['name'],
      hash: row['hash'],
      executedAt: row['executedAt'] ?? row['executed_at'],
      executionTimeMs: row['executionTimeMs'] ?? row['execution_time_ms'],
      success: row['success'],
      errorMessage: row['errorMessage'] ?? row['error_message'] ?? undefined,
      downSql: row['downSql'] ?? row['down_sql'] ?? undefined,
      downHash: row['downHash'] ?? row['down_hash'] ?? undefined,
      db: this.datasource,
    }));
  }

  protected async apply(sql: postgres.ReservedSql, def: SQLDefinition): Promise<Migration> {
    const startTime = Date.now();
    const hash = await sha256Hex(def.sql);
    const downSql = def.down ?? null;
    const downHash = downSql ? await sha256Hex(downSql) : null;
    const executedAt = new Date().toISOString();
    const id = canonicalMigrationId(this.datasource, def.name);

    this.logger.debug(
      'executing migration',
      migrationFields({ name: def.name, datasource: this.datasource, hash, source: def.source }),
    );

    // The up SQL and its success-INSERT bookkeeping row must commit
    // atomically: if the DDL committed but the row did not (crash/timeout),
    // the next up() would re-apply the migration. Wrap both in a single
    // transaction on the reserved connection, mirroring executeRollback and
    // the Go runner's `withTx`.
    await sql`BEGIN`;
    try {
      // Disable the pool's statement_timeout for this transaction only, so a
      // long DDL/backfill body isn't aborted by the request-serving timeout.
      // Must precede the migration DDL.
      await this.clearStatementTimeout(sql);
      await this.applySearchPath(sql);
      await sql.unsafe(def.sql);
      await sql`
        INSERT INTO migration.migrations (
          id, db_name, name, hash, executed_at, execution_time_ms, success, error_message, down_sql, down_hash
        )
        VALUES (
          ${id}, ${this.datasource}, ${def.name}, ${hash}, ${executedAt}, ${Date.now() - startTime}, 1, null, ${downSql}, ${downHash}
        )
        ON CONFLICT (id) DO UPDATE SET
          db_name = ${this.datasource},
          name = ${def.name},
          hash = ${hash},
          executed_at = ${executedAt},
          execution_time_ms = ${Date.now() - startTime},
          success = 1,
          error_message = null,
          down_sql = ${downSql},
          down_hash = ${downHash};
      `;
      await sql`COMMIT`;
      // One elapsed measurement for the terminal record and the returned row, so
      // the two agree.
      const executionTimeMs = Date.now() - startTime;
      logMigrationApplied(def.name, this.datasource, executionTimeMs);
      return {
        id,
        dbName: this.datasource,
        name: def.name,
        hash,
        executedAt,
        executionTimeMs,
        success: 1,
        downSql: downSql ?? undefined,
        downHash: downHash ?? undefined,
      } as Migration;
    } catch (e) {
      try {
        await sql`ROLLBACK`;
      } catch {
        // surface the original error
      }
      // Record the failure outside the rolled-back transaction so the
      // bookkeeping row survives. Best-effort: never mask the apply error.
      const errorMessage = e instanceof Error ? e.message : String(e);
      try {
        await sql`
          INSERT INTO migration.migrations (
            id, db_name, name, hash, executed_at, execution_time_ms, success, error_message, down_sql, down_hash
          )
          VALUES (
            ${id}, ${this.datasource}, ${def.name}, ${hash}, ${executedAt}, ${Date.now() - startTime}, 0, ${errorMessage}, ${downSql}, ${downHash}
          )
          ON CONFLICT (id) DO UPDATE SET
            db_name = ${this.datasource},
            name = ${def.name},
            hash = ${hash},
            executed_at = ${executedAt},
            execution_time_ms = ${Date.now() - startTime},
            success = 0,
            error_message = ${errorMessage},
            down_sql = ${downSql},
            down_hash = ${downHash};
        `;
      } catch (recordErr) {
        // Bookkeeping failure — an independent signal from the migration's own
        // terminal record, emitted just below.
        this.logger.warn(
          'failed to record migration failure',
          asError(recordErr),
          migrationFields({ name: def.name, datasource: this.datasource }),
        );
      }
      // The per-migration terminal record. Logged once here with the raw cause;
      // the throw below still aborts the run.
      logMigrationFailed(def.name, this.datasource, Date.now() - startTime, e);
      throw new MigrationError(
        `Migration "${def.name}" failed: ${errorMessage}`,
        def.name,
        e instanceof Error ? e : undefined,
      );
    }
  }

  private async executeRollback(sql: postgres.ReservedSql, mig: Migration): Promise<Migration> {
    const downSql = mig.downSql ?? this.definitions.find((d) => d.name === mig.name)?.down;
    if (!downSql) {
      throw new MigrationError(`Migration "${mig.name}" has no down SQL — cannot roll back`, mig.name);
    }
    if (mig.downSql && mig.downHash && (await sha256Hex(mig.downSql)) !== mig.downHash) {
      throw new MigrationError(
        `Rollback SQL integrity check failed for "${mig.name}" — stored down_sql has been tampered with`,
        mig.name,
      );
    }

    this.logger.debug(
      'executing rollback',
      migrationFields({
        name: mig.name,
        datasource: this.datasource,
        source: mig.downSql ? 'database' : 'registry',
      }),
    );

    await sql`BEGIN`;
    try {
      const claimed = await sql`
        DELETE FROM migration.migrations
        WHERE id = ${mig.id} AND success = 1
        RETURNING id
      `;
      if (claimed.length > 0) {
        // Same statement_timeout disable as the up-path: a rollback body can be
        // just as long-running (dropping/rebuilding a large index), so it must
        // not be bounded by the request-serving timeout either. Applied only
        // once a row is claimed, mirroring the Go runner's ordering.
        await this.clearStatementTimeout(sql);
        await this.applySearchPath(sql);
        await sql.unsafe(downSql);
      }
      await sql`COMMIT`;
    } catch (e) {
      try {
        await sql`ROLLBACK`;
      } catch {
        // surface the original error
      }
      const errorMessage = e instanceof Error ? e.message : String(e);
      throw new MigrationError(
        `Rollback of "${mig.name}" failed: ${errorMessage}`,
        mig.name,
        e instanceof Error ? e : undefined,
      );
    }

    return { ...mig, executedAt: new Date().toISOString() };
  }
}
