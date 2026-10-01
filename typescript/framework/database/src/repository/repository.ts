import { useConfig, type InferConfig } from '@putnami/runtime';
import type postgres from 'postgres';
import { abortableQuery, throwIfAborted } from '../abort';
import { classifyOutcome, RepositoryError, type RepositoryErrorCode } from '../errors';
import { assertSafeIdentifier, type EntityHelper, entityHelper, quoteIdentifier } from '../metadata';
import { type QueryMetrics, type SqlOperation, recordQuery, recordQueryError, recordSlowQuery } from '../observability';
import { PostgresConfig } from '../postgres/config';
import { resolveDatasourceName } from '../primary-datasource';
import type { SqlClient } from '../sql-client';
import type { TableDefinition } from '../table';
import type { InferTable } from '../table/table-inference';
import { Outcome } from '../transaction-outcome';
import { useTxConnection } from '../transaction';
import type { QueryFilters } from './query-builder';
import { buildWhere, buildWhereFromColumnNames } from './repository-where';

/** Implicit page size for `find()` when no `limit` is supplied. */
const DEFAULT_FIND_LIMIT = 1000;

/** Inferred entity type alias for a given TableDefinition */
type Entity<T extends TableDefinition> = InferTable<T>;

/** The parameter-array type postgres.js's `sql.unsafe(query, params)` accepts. */
type SqlUnsafeParams = Parameters<postgres.Sql['unsafe']>[1];

/**
 * RotateSpec describes an atomic predecessor→successor rotation for
 * {@link Repository.rotateRow}: the predecessor row to revoke (a compare-and-set
 * on `stateColumn`) and the successor row to insert once the revoke applies. It
 * mirrors the Go adapter's `RotateSpec` (`go/framework/database/cas.go`).
 */
export interface RotateSpec {
  /** Identifies the predecessor row (`keyColumn = predecessorKey`). */
  keyColumn: string;
  predecessorKey: unknown;
  /**
   * The predecessor column transitioned `expected` → `revoked` to retire it. The
   * revoke only applies while it currently equals `expected`, so a concurrent
   * rotation that already revoked it is reported as a conflict rather than
   * double-applied.
   */
  stateColumn: string;
  expected: unknown;
  revoked: unknown;
  /**
   * The row inserted into the same table once the predecessor is revoked.
   * `successorColumns` and `successorValues` must be equal length.
   */
  successorColumns: string[];
  successorValues: unknown[];
}

/**
 * Base repository class for database operations.
 * Parameterized by a TableDefinition — the entity type is inferred.
 */
export class Repository<T extends TableDefinition> {
  protected helper: EntityHelper<T>;
  private _slowQueryThresholdMs: number | undefined;
  private _maxRowLimit: number | undefined;

  constructor(private tableDef: T) {
    this.helper = entityHelper(this.tableDef);
  }

  /** Lazily resolve and cache the slow query threshold — avoids a DI lookup on every query. */
  private get slowQueryThresholdMs(): number {
    if (this._slowQueryThresholdMs === undefined) {
      try {
        this._slowQueryThresholdMs = this.useConfig().slowQueryThresholdMs;
      } catch {
        this._slowQueryThresholdMs = 0;
      }
    }
    return this._slowQueryThresholdMs;
  }

  /** Lazily resolve and cache the hard `find()` row-limit ceiling. */
  private get maxRowLimit(): number {
    if (this._maxRowLimit === undefined) {
      try {
        const configured = this.useConfig().maxRowLimit;
        this._maxRowLimit = configured > 0 ? configured : DEFAULT_FIND_LIMIT;
      } catch {
        this._maxRowLimit = DEFAULT_FIND_LIMIT;
      }
    }
    return this._maxRowLimit;
  }

  /**
   * The datasource this repository's table targets: its explicit `db` when set,
   * otherwise the workload's declared primary datasource (`sql({ datasource })`),
   * otherwise `undefined` — which resolves to `default` downstream. This is where
   * a `Table()` with no `db` inherits the app's primary datasource, so reads and
   * writes follow the same named datasource as the migrations and health probe.
   */
  private datasourceName(): string | undefined {
    return resolveDatasourceName(this.helper.db);
  }

  async conn(mode: 'read' | 'write' = 'write'): Promise<SqlClient> {
    return useTxConnection(this.datasourceName(), mode);
  }

  /**
   * Get database config
   */
  private useConfig(): InferConfig<typeof PostgresConfig> {
    const resolvedDb = this.datasourceName() || 'default';
    return useConfig(PostgresConfig, { path: `database.${resolvedDb}` });
  }

  /**
   * Observe a query operation: measure timing, emit telemetry metrics,
   * and produce structured log entries.
   *
   * Replaces the former `executeQuery` profiling wrapper.
   * All repository operations go through this method so that every
   * SQL interaction is visible to the logging and telemetry systems.
   */
  private async observe<R>(
    operation: SqlOperation,
    queryFn: () => Promise<R>,
    rowCount?: (result: R) => number,
  ): Promise<R> {
    throwIfAborted();

    // performance.now() rather than Date.now(): the log record carries both
    // durationMs and durationUs (sub-millisecond queries are the norm), and
    // Date.now()'s millisecond granularity would report every fast query as 0µs.
    const start = performance.now();
    const table = this.helper.tableName;
    const datasource = this.datasourceName();

    try {
      const result = await queryFn();
      const duration = performance.now() - start;
      const metrics: QueryMetrics = {
        operation,
        table,
        duration,
        rowCount: rowCount?.(result),
        datasource,
      };
      recordQuery(metrics);

      if (this.slowQueryThresholdMs > 0) {
        recordSlowQuery(metrics, this.slowQueryThresholdMs);
      }

      return result;
    } catch (error) {
      const duration = performance.now() - start;
      recordQueryError(operation, table, duration, error, datasource);
      throw error;
    }
  }

  /**
   * Check if an entity exists by its primary key(s)
   */
  async exists(partials: Partial<Entity<T>>): Promise<boolean> {
    const keys = this.helper.extractKeys(partials as Record<string, unknown>);
    const hasKeys = Object.entries(keys).some(([, value]) => value !== undefined && value !== null);
    if (!hasKeys) {
      return false;
    }

    return this.observe(
      'exists',
      async () => {
        const sql = await this.conn('read');
        const { fragment: where } = buildWhereFromColumnNames(sql, keys);

        const rows = await abortableQuery(sql`
          SELECT 1
          FROM ${sql(this.helper.tableName)}
          WHERE ${where}
          LIMIT 1
        `);

        return rows.length > 0;
      },
      (found) => (found ? 1 : 0),
    );
  }

  /**
   * Get an entity by its primary key(s)
   */
  get(keys: Partial<Entity<T>>): Promise<Entity<T> | undefined> {
    return this.findOne(keys);
  }

  /**
   * Save an entity (insert or update).
   * Use `{ strict: true }` to validate all required fields (recommended for inserts).
   */
  async save(entity: Partial<Entity<T>>, options?: { strict?: boolean }): Promise<Entity<T>> {
    if (!entity || Object.keys(entity).length === 0) {
      throw new RepositoryError('Cannot save an empty entity', 'EMPTY_ENTITY');
    }

    const keys = this.helper.extractKeys(entity as Record<string, unknown>);
    const hasKeys = Object.entries(keys).some(([, value]) => value !== undefined && value !== null);
    if (!hasKeys) {
      throw new RepositoryError('Cannot save an entity without primary key', 'MISSING_PRIMARY_KEY');
    }

    // Validate entity against schema constraints
    const validationErrors = options?.strict
      ? this.helper.validateFullEntity(entity as Record<string, unknown>)
      : this.helper.validateEntity(entity as Record<string, unknown>);
    if (validationErrors.length > 0) {
      const messages = validationErrors.map((e) => `${e.field}: ${e.message}`).join('; ');
      throw new RepositoryError(`Validation failed: ${messages}`, 'VALIDATION_ERROR');
    }

    return this.observe(
      'save',
      async () => {
        const sql = await this.conn();

        const keyProperties = this.helper.keyProperties;
        const keyColumnNames = Object.keys(keys);

        // Build the insert row keyed by column name and the ordered column list.
        // Identifiers are asserted safe and emitted via postgres.js's `sql(...)`
        // helpers — no manual quoting, no `sql.unsafe`.
        const row: Record<string, unknown> = {};
        const insertColumns: string[] = [];
        const updateColumns: string[] = [];
        for (const [prop, value] of Object.entries(entity as Record<string, unknown>)) {
          const columnName = this.helper.columnName(prop);
          assertSafeIdentifier(columnName, 'insert column');
          row[columnName] = this.helper.toDatabaseValue(prop, value);
          insertColumns.push(columnName);
          if (!keyProperties.has(prop)) {
            updateColumns.push(columnName);
          }
        }

        const setClause = this.upsertSetClause(sql, updateColumns, keyColumnNames);
        const conflictTarget = this.identList(sql, keyColumnNames);

        try {
          const rows = await abortableQuery(sql`
            INSERT INTO ${sql(this.helper.tableName)} ${sql(row, ...insertColumns)}
            ON CONFLICT (${conflictTarget}) DO UPDATE
            SET ${setClause}
            RETURNING *
          `);

          if (rows.length === 0) {
            throw new RepositoryError('Insert/Update failed: no row returned', 'SAVE_FAILED');
          }

          return this.helper.toEntity<Entity<T>>(rows[0]);
        } catch (error) {
          throw RepositoryError.fromDatabaseError('Failed to save entity', error);
        }
      },
      () => 1,
    );
  }

  /**
   * Build the `ON CONFLICT ... DO UPDATE SET` clause as a safe fragment.
   * Non-key columns are set from `EXCLUDED`; for key-only entities (nothing to
   * update) it degrades to a no-op self-assignment of the first key column so
   * the upsert still returns the existing row.
   */
  private upsertSetClause(
    sql: postgres.Sql,
    updateColumns: string[],
    keyColumnNames: string[],
  ): postgres.PendingQuery<postgres.Row[]> {
    if (updateColumns.length === 0) {
      const keyCol = keyColumnNames[0];
      assertSafeIdentifier(keyCol, 'conflict key column');
      return sql`${sql(keyCol)} = ${sql(this.helper.tableName)}.${sql(keyCol)}`;
    }

    let clause: postgres.PendingQuery<postgres.Row[]> | undefined;
    for (const col of updateColumns) {
      assertSafeIdentifier(col, 'update column');
      const assignment = sql`${sql(col)} = EXCLUDED.${sql(col)}`;
      clause = clause ? sql`${clause}, ${assignment}` : assignment;
    }
    return clause as postgres.PendingQuery<postgres.Row[]>;
  }

  /**
   * Build a comma-separated identifier list (e.g. an `ON CONFLICT` target) as a
   * fragment. Each name is emitted via `sql(name)`, which postgres.js renders as
   * a quoted identifier regardless of the surrounding keywords — unlike passing
   * a string array, which the driver would treat as a `VALUES`/insert builder.
   */
  private identList(sql: postgres.Sql, names: string[]): postgres.PendingQuery<postgres.Row[]> {
    let list: postgres.PendingQuery<postgres.Row[]> | undefined;
    for (const name of names) {
      assertSafeIdentifier(name, 'identifier list');
      const ident = sql`${sql(name)}`;
      list = list ? sql`${list}, ${ident}` : ident;
    }
    return (list ?? sql``) as postgres.PendingQuery<postgres.Row[]>;
  }

  /**
   * Delete an entity by its primary key(s)
   */
  async delete(filters: Partial<Entity<T>>): Promise<{ success: boolean; item?: Entity<T> }> {
    // Restrict to primary-key columns to match the documented "by its primary
    // key(s)" contract, and require at least one — symmetric with deleteMany,
    // which throws on an empty filter rather than silently no-op'ing.
    const keys = this.helper.extractKeys(filters as Record<string, unknown>);
    const hasKeys = Object.entries(keys).some(([, value]) => value !== undefined && value !== null);
    if (!hasKeys) {
      throw new RepositoryError('Cannot delete without a primary key for safety', 'DELETE_WITHOUT_FILTERS');
    }

    return this.observe(
      'delete',
      async () => {
        const sql = await this.conn();
        const { fragment: where } = buildWhereFromColumnNames(sql, keys);

        const deleted = await abortableQuery(sql`
          DELETE FROM ${sql(this.helper.tableName)}
          WHERE ${where}
          RETURNING *
        `);

        if (deleted.length === 0) {
          return { success: false };
        }

        return {
          success: true,
          item: this.helper.toEntity<Entity<T>>(deleted[0]),
        };
      },
      (result) => (result.success ? 1 : 0),
    );
  }

  /**
   * Save multiple entities (bulk insert/update).
   * Builds multi-row INSERT ... ON CONFLICT DO UPDATE statements for efficiency.
   * Entities are grouped by their property set (column list) and batched to
   * stay within PostgreSQL's parameter limit (~65 535).
   */
  async saveMany(entities: Partial<Entity<T>>[]): Promise<Entity<T>[]> {
    if (!entities || entities.length === 0) {
      throw new RepositoryError('Cannot save empty array of entities', 'EMPTY_ENTITIES');
    }

    return this.observe(
      'saveMany',
      async () => {
        const keyProperties = this.helper.keyProperties;

        // Validate all entities upfront
        for (const entity of entities) {
          if (!entity || Object.keys(entity as Record<string, unknown>).length === 0) {
            throw new RepositoryError('Cannot save an empty entity', 'EMPTY_ENTITY');
          }
          const keys = this.helper.extractKeys(entity as Record<string, unknown>);
          const hasKeys = Object.entries(keys).some(([, v]) => v !== undefined && v !== null);
          if (!hasKeys) {
            throw new RepositoryError('Cannot save an entity without primary key', 'MISSING_PRIMARY_KEY');
          }
          const errors = this.helper.validateEntity(entity as Record<string, unknown>);
          if (errors.length > 0) {
            const messages = errors.map((e) => `${e.field}: ${e.message}`).join('; ');
            throw new RepositoryError(`Validation failed: ${messages}`, 'VALIDATION_ERROR');
          }
        }

        // Group entities by their property key set for consistent column lists
        const groups = new Map<string, { props: string[]; entities: Partial<Entity<T>>[] }>();
        for (const entity of entities) {
          const props = Object.keys(entity as Record<string, unknown>).sort();
          const groupKey = props.join(',');
          let group = groups.get(groupKey);
          if (!group) {
            group = { props, entities: [] };
            groups.set(groupKey, group);
          }
          group.entities.push(entity);
        }

        const sql = await this.conn();
        const results: Entity<T>[] = [];

        for (const { props, entities: groupEntities } of groups.values()) {
          // Build column metadata once per group. Identifiers are asserted safe;
          // postgres.js's `sql(rows, ...columns)` helper emits the column list
          // and parameterized VALUES tuples.
          const columns = props.map((prop) => {
            const columnName = this.helper.columnName(prop);
            assertSafeIdentifier(columnName, 'insert column');
            return { prop, columnName, isKey: keyProperties.has(prop) };
          });

          const insertColumns = columns.map((c) => c.columnName);
          const keyColumnNames = columns.filter((c) => c.isKey).map((c) => c.columnName);
          const updateColumns = columns.filter((c) => !c.isKey).map((c) => c.columnName);
          const setClause = this.upsertSetClause(sql, updateColumns, keyColumnNames);
          const conflictTarget = this.identList(sql, keyColumnNames);

          // Batch to stay within PostgreSQL's ~65 535 parameter limit
          const maxBatchSize = Math.max(1, Math.floor(60_000 / props.length));

          for (let i = 0; i < groupEntities.length; i += maxBatchSize) {
            const batch = groupEntities.slice(i, i + maxBatchSize);

            const rows = batch.map((entity) => {
              const record = entity as Record<string, unknown>;
              const row: Record<string, unknown> = {};
              for (const col of columns) {
                row[col.columnName] = this.helper.toDatabaseValue(col.prop, record[col.prop]);
              }
              return row;
            });

            try {
              const saved = await abortableQuery(sql`
                INSERT INTO ${sql(this.helper.tableName)} ${sql(rows, ...insertColumns)}
                ON CONFLICT (${conflictTarget}) DO UPDATE
                SET ${setClause}
                RETURNING *
              `);
              for (const row of saved) {
                results.push(this.helper.toEntity<Entity<T>>(row));
              }
            } catch (error) {
              throw RepositoryError.fromDatabaseError('Failed to save entities', error);
            }
          }
        }

        return results;
      },
      (results) => results.length,
    );
  }

  /**
   * Delete multiple entities matching filters
   */
  async deleteMany(filters: Partial<Entity<T>> | QueryFilters<Entity<T>>): Promise<number> {
    return this.observe(
      'deleteMany',
      async () => {
        const sql = await this.conn();
        const { fragment: where, hasConditions } = buildWhere(sql, filters, (prop) => this.helper.columnName(prop));

        if (!hasConditions) {
          throw new RepositoryError('Cannot delete without filters for safety', 'DELETE_WITHOUT_FILTERS');
        }

        const deletedRows = await abortableQuery(sql`
          DELETE FROM ${sql(this.helper.tableName)}
          WHERE ${where}
          RETURNING *
        `);

        return Array.isArray(deletedRows) ? deletedRows.length : 0;
      },
      (count) => count,
    );
  }

  /**
   * Find a single entity matching filters
   */
  async findOne(filters: Partial<Entity<T>>): Promise<Entity<T> | undefined> {
    const results = await this.find(filters, { limit: 1 });
    return results[0];
  }

  /**
   * Find multiple entities matching filters
   */
  /**
   * Count entities matching the filters with `SELECT count(*)` — use this
   * alongside paginated `find()` instead of loading every row to measure
   * a total.
   */
  async count(filters: Partial<Entity<T>> | QueryFilters<Entity<T>> = {} as Partial<Entity<T>>): Promise<number> {
    return this.observe(
      'count',
      async () => {
        const sql = await this.conn('read');
        const { fragment: where, hasConditions } = buildWhere(sql, filters, (prop) => this.helper.columnName(prop));
        const whereClause: postgres.PendingQuery<postgres.Row[]> = hasConditions ? sql`WHERE ${where}` : sql``;

        const rows = await abortableQuery(sql`
          SELECT count(*)::int AS total
          FROM ${sql(this.helper.tableName)}
          ${whereClause}
        `);
        return Number(rows[0]?.['total'] ?? 0);
      },
      (total) => total,
    );
  }

  async find(
    filters: Partial<Entity<T>> | QueryFilters<Entity<T>> = {} as Partial<Entity<T>>,
    options: { limit?: number; offset?: number; orderBy?: string } = {},
  ): Promise<Entity<T>[]> {
    return this.observe(
      'find',
      async () => {
        // Clamp the requested limit to a hard ceiling so request-controlled
        // input can't ask for an unbounded result set (memory-exhaustion DoS).
        const maxRowLimit = this.maxRowLimit;
        const requested = Math.max(0, Math.floor(Number(options.limit ?? DEFAULT_FIND_LIMIT))) || DEFAULT_FIND_LIMIT;
        const limit = Math.min(requested, maxRowLimit);
        const offset = Math.max(0, Math.floor(Number(options.offset ?? 0)));
        const sql = await this.conn('read');
        const { fragment: where, hasConditions } = buildWhere(sql, filters, (prop) => this.helper.columnName(prop));
        const whereClause: postgres.PendingQuery<postgres.Row[]> = hasConditions ? sql`WHERE ${where}` : sql``;
        const orderBy = this.buildOrderBy(sql, options.orderBy);
        const offsetClause: postgres.PendingQuery<postgres.Row[]> = offset > 0 ? sql`OFFSET ${offset}` : sql``;

        const rows = await abortableQuery(sql`
          SELECT * FROM ${sql(this.helper.tableName)}
          ${whereClause}
          ${orderBy}
          LIMIT ${limit}
          ${offsetClause}
        `);
        return rows.map((row) => this.helper.toEntity<Entity<T>>(row));
      },
      (rows) => rows.length,
    );
  }

  /**
   * Build an ORDER BY fragment from a comma-separated `prop [ASC|DESC]` string.
   *
   * Each property is validated against the schema (unknown names throw, which
   * blocks identifier injection through user input) and emitted via `sql(name)`.
   * The direction is constrained to the `ASC`/`DESC` literals.
   */
  private buildOrderBy(sql: postgres.Sql, orderBy?: string): postgres.PendingQuery<postgres.Row[]> {
    if (!orderBy) {
      return sql``;
    }

    const columns = orderBy
      .split(',')
      .map((col) => col.trim())
      .filter(Boolean);

    let clause: postgres.PendingQuery<postgres.Row[]> | undefined;
    for (const col of columns) {
      const parts = col.split(/\s+/);
      const columnOrProp = parts[0];
      const direction = parts[1]?.toUpperCase() || '';

      if (!this.helper.hasProperty(columnOrProp)) {
        throw new RepositoryError(
          `Unknown property "${columnOrProp}" in orderBy. Valid properties: ${[...this.helper.columnNames].join(', ')}`,
          'UNKNOWN' as RepositoryErrorCode,
        );
      }

      const columnName = this.helper.columnName(columnOrProp);
      assertSafeIdentifier(columnName, 'orderBy column');
      const ident = sql(columnName);
      // ASC is SQL's default; emitting it explicitly when no (or an invalid)
      // direction is given keeps every branch's fragment well-formed.
      const part = direction === 'DESC' ? sql`${ident} DESC` : sql`${ident} ASC`;
      clause = clause ? sql`${clause}, ${part}` : part;
    }

    return clause ? sql`ORDER BY ${clause}` : sql``;
  }

  // ---------------------------------------------------------------------------
  // Optimistic-concurrency primitives (CAS / consume-once / rotation).
  //
  // These helpers mirror the Go adapter (go/framework/database/cas.go) so both
  // languages return the identical transaction {@link Outcome} code for the same
  // scenario. Each returns a typed Outcome — applied / already-consumed-conflict
  // / not-found / retryable-serialization-failure — rather than a bare row count,
  // and a genuine I/O failure still surfaces as a raw RepositoryError.
  //
  // # Atomicity contract
  //
  // Every helper routes its writes through `this.conn()` (useTxConnection), so
  // inside an active `withTransaction` / request-scoped `UnitOfWork` they all
  // join that one transaction. The consume-once safety property — at most one
  // caller ever observes `Outcome.Applied` for a given row — is enforced by the
  // UPDATE's WHERE predicate and Postgres row locking, and therefore holds even
  // in autocommit: two racing claims serialize on the row, and only the first
  // matches the still-unclaimed predicate. The follow-up existence check that
  // distinguishes already-consumed from not-found is snapshot-consistent with the
  // UPDATE only when both run in the same transaction; run these helpers inside
  // `withTransaction` / `UnitOfWork` when that distinction must be exact.
  // `rotateRow` performs two writes and is atomic ONLY inside a transaction —
  // outside one, the revoke would commit even if the successor insert fails.

  /**
   * Apply the SET assignments to every row matching the WHERE predicate and
   * return the number of rows affected. `set` and `where` are raw SQL fragments
   * sharing one positional-parameter space (`$1, $2, …`) across the whole
   * statement, with the SET args listed before the WHERE args in `args`. It joins
   * an active transaction when one is open. Mirrors Go's `UpdateWhere`.
   */
  async updateWhere(set: string, where: string, ...args: unknown[]): Promise<number> {
    const sql = await this.conn();
    const table = quoteIdentifier(this.helper.tableName, 'table');
    const query = `UPDATE ${table} SET ${set} WHERE ${where}`;
    try {
      const result = await abortableQuery(sql.unsafe(query, args as unknown as SqlUnsafeParams));
      return result.count;
    } catch (error) {
      throw RepositoryError.fromDatabaseError('Update where failed', error);
    }
  }

  /**
   * Atomically transition `column` from `expected` to `next` for the row
   * identified by `keyColumn = key`, reporting the typed {@link Outcome}:
   * `Applied` when the row matched and moved; `AlreadyConsumedConflict` when the
   * row exists but `column != expected`; `NotFound` when no row has
   * `keyColumn = key`; `RetryableSerializationFailure` on a serialization/deadlock
   * abort. Value-equality CAS: a nullish `expected` does NOT match a SQL NULL
   * (`col = NULL` is never true) — use {@link consumeOnce} with an explicit
   * `IS NULL` guard for that. `keyColumn`/`column` are developer-supplied
   * identifiers; an invalid one throws. Mirrors Go's `CompareAndSet`.
   */
  async compareAndSet(
    keyColumn: string,
    key: unknown,
    column: string,
    expected: unknown,
    next: unknown,
  ): Promise<Outcome> {
    const qKey = quoteIdentifier(keyColumn, 'compareAndSet key column');
    const qCol = quoteIdentifier(column, 'compareAndSet column');
    const table = quoteIdentifier(this.helper.tableName, 'table');
    const query = `UPDATE ${table} SET ${qCol} = $1 WHERE ${qKey} = $2 AND ${qCol} = $3`;
    return this.resolveTransition(query, [next, key, expected], qKey, key);
  }

  /**
   * Atomically claim a once-only row identified by `keyColumn = key`. It applies
   * the SET transition `set` only while the still-unclaimed condition
   * `claimGuard` holds (e.g. `"consumed = false"` or `"consumed_at IS NULL"`); the
   * key match is appended automatically. Reports `Applied` when this call claimed
   * the row; `AlreadyConsumedConflict` when the row exists but `claimGuard` no
   * longer holds; `NotFound` when no row has `keyColumn = key`;
   * `RetryableSerializationFailure` on a serialization/deadlock abort. Positional
   * params in `set` and `claimGuard` share one space, numbered `$1..$N` in the
   * order they appear (SET first, then claimGuard); `args` must list the SET args
   * before the guard args. The key is bound as the final parameter — callers must
   * not reference it in `set` or `claimGuard`. Mirrors Go's `ConsumeOnce`.
   */
  async consumeOnce(
    keyColumn: string,
    key: unknown,
    claimGuard: string,
    set: string,
    ...args: unknown[]
  ): Promise<Outcome> {
    const qKey = quoteIdentifier(keyColumn, 'consumeOnce key column');
    const table = quoteIdentifier(this.helper.tableName, 'table');
    // The key is bound as the parameter after the caller's set/guard args, so its
    // placeholder index is args.length + 1.
    const keyParam = args.length + 1;
    const query = `UPDATE ${table} SET ${set} WHERE (${claimGuard}) AND ${qKey} = $${keyParam}`;
    return this.resolveTransition(query, [...args, key], qKey, key);
  }

  /**
   * Atomically retire a predecessor and install its successor as one unit of
   * work: it first compare-and-set-transitions the predecessor's `stateColumn`
   * from `expected` to `revoked`; only when that applies does it INSERT the
   * successor row. Returns `Applied` when the predecessor was revoked and the
   * successor inserted; `AlreadyConsumedConflict` / `NotFound` when the
   * predecessor could not be revoked (already rotated, or absent — the successor
   * is NOT inserted); `AlreadyConsumedConflict` when the successor insert hits a
   * unique violation; `RetryableSerializationFailure` on a serialization/deadlock
   * abort. `rotateRow` joins the ambient transaction rather than opening its
   * own, so callers MUST wrap it in `withTransaction` / `UnitOfWork` for the
   * revoke and the insert to commit or roll back together. Mirrors Go's
   * `Rotate` — named `rotateRow` here because a TypeScript base-class member
   * would collide with the domain-specific `rotate` methods subclasses commonly
   * define (e.g. an ApiKeyRepository's `rotate(id, prefix, tokenHash)`), which
   * Go's embedding-based shadowing tolerates but a TS override cannot.
   */
  async rotateRow(spec: RotateSpec): Promise<Outcome> {
    if (spec.successorColumns.length !== spec.successorValues.length) {
      throw new Error(
        `Repository.rotateRow: successor columns/values length mismatch: ${spec.successorColumns.length} != ${spec.successorValues.length}`,
      );
    }
    if (spec.successorColumns.length === 0) {
      throw new Error('Repository.rotateRow: successor row has no columns');
    }

    // Revoke the predecessor. Any non-applied outcome (conflict, not-found,
    // retryable) short-circuits without touching the successor.
    const outcome = await this.compareAndSet(
      spec.keyColumn,
      spec.predecessorKey,
      spec.stateColumn,
      spec.expected,
      spec.revoked,
    );
    if (outcome !== Outcome.Applied) {
      return outcome;
    }

    // Insert the successor; both writes share the ambient transaction, so a
    // failure here rolls the revoke back with it.
    const sql = await this.conn();
    const table = quoteIdentifier(this.helper.tableName, 'table');
    const columns = spec.successorColumns.map((c) => quoteIdentifier(c, 'rotate successor column')).join(', ');
    const placeholders = spec.successorValues.map((_, i) => `$${i + 1}`).join(', ');
    const insertQuery = `INSERT INTO ${table} (${columns}) VALUES (${placeholders})`;
    try {
      await abortableQuery(sql.unsafe(insertQuery, spec.successorValues as unknown as SqlUnsafeParams));
      return Outcome.Applied;
    } catch (error) {
      // A unique violation on the successor is a conflict, not an I/O error.
      const classified = classifyOutcome(error);
      if (classified) return classified;
      throw RepositoryError.fromDatabaseError('Rotate insert successor failed', error);
    }
  }

  /**
   * Run a conditional-transition UPDATE and map the result to a typed
   * {@link Outcome}. When the UPDATE transitions a row it is `Applied`. When it
   * transitions nothing it disambiguates already-consumed from not-found with an
   * existence check on `qKey = $1` bound to `keyArg`: an existing row is
   * `AlreadyConsumedConflict`, an absent row is `NotFound`. Both statements route
   * through `this.conn()`, so inside a transaction the disambiguation is atomic
   * with the transition. A serialization/deadlock (or unique) failure surfaced by
   * either statement is classified into its typed outcome; any other error is
   * wrapped and thrown so genuine I/O failures are never masked as a business
   * outcome. Mirrors Go's `resolveTransition`.
   */
  private async resolveTransition(query: string, args: unknown[], qKey: string, keyArg: unknown): Promise<Outcome> {
    const sql = await this.conn();

    let count: number;
    try {
      const result = await abortableQuery(sql.unsafe(query, args as unknown as SqlUnsafeParams));
      count = result.count;
    } catch (error) {
      const outcome = classifyOutcome(error);
      if (outcome) return outcome;
      throw RepositoryError.fromDatabaseError('Conditional update failed', error);
    }
    if (count > 0) {
      return Outcome.Applied;
    }

    const table = quoteIdentifier(this.helper.tableName, 'table');
    const existsQuery = `SELECT EXISTS(SELECT 1 FROM ${table} WHERE ${qKey} = $1) AS present`;
    try {
      const rows = await abortableQuery(
        sql.unsafe<{ present: boolean }[]>(existsQuery, [keyArg] as unknown as SqlUnsafeParams),
      );
      return rows[0]?.present ? Outcome.AlreadyConsumedConflict : Outcome.NotFound;
    } catch (error) {
      const outcome = classifyOutcome(error);
      if (outcome) return outcome;
      throw RepositoryError.fromDatabaseError('Conditional update existence check failed', error);
    }
  }
}
