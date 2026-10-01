/**
 * Putnami-owned SQL client seam.
 *
 * These aliases are the only driver types the public API is allowed to
 * expose: `database()`, `Repository.conn()`, `useTxConnection()`, and
 * `abortableQuery()` all speak in terms of this module, never the driver's
 * own type names. Today each alias points at postgres.js; a future driver
 * swap (e.g. Bun.SQL) re-points them here and the public surface does not
 * move — application code keeps compiling unchanged.
 *
 * The aliases are structural, not nominal: existing code that annotates
 * `postgres.Sql` keeps working, and code written against `SqlClient` is
 * exactly as capable. Only the raw escape hatch is covered here — the
 * high-level API (`Table`, `Repository`, `runInTransaction`, migrations,
 * session store) never exposed the driver in the first place.
 */
import type postgres from 'postgres';

/**
 * The SQL tagged-template client the public API hands out: `database()`,
 * `Repository.conn()`, and `useTxConnection()` all resolve to one.
 * Call it as a template tag (`sql`SELECT 1``), or use `sql.unsafe(...)`
 * for dynamic DDL.
 */
export type SqlClient = postgres.Sql;

/**
 * A connection reserved out of the pool for exclusive use. The transaction
 * machinery holds one per enrolled datasource between `BEGIN` and
 * `COMMIT`/`ROLLBACK`; callers must `release()` it when done.
 */
export type ReservedSqlClient = postgres.ReservedSql;

/** A single result row, keyed by column name. */
export type SqlRow = postgres.Row;

/**
 * A pending tagged-template query. Awaitable, cancellable
 * (see {@link import('./abort').abortableQuery}), and composable as a
 * fragment into a larger query.
 */
// biome-ignore lint/suspicious/noExplicitAny: mirrors the driver's own row-tuple bound
export type SqlQuery<T extends readonly any[] = SqlRow[]> = postgres.PendingQuery<T>;

/** A settled query result: the rows plus result metadata such as `count`. */
// biome-ignore lint/suspicious/noExplicitAny: mirrors the driver's own row-tuple bound
export type SqlResult<T extends readonly any[] = SqlRow[]> = postgres.RowList<T>;
