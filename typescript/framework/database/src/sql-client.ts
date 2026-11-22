/**
 * Putnami-owned SQL client surface.
 *
 * `database()`, `Repository.conn()`, `useTxConnection()` and `reserve()` hand
 * out a {@link SqlClient}, never the driver's own object (see
 * doc/adr/0005-bun-sql-is-the-driver.md). The client runs on `Bun.SQL`: a
 * query it builds captures its template and runs only when awaited or
 * executed, on the connection the client chooses. The high-level API
 * (`Table`, `Repository`, `runInTransaction`, migrations, session store) never
 * exposed the driver in the first place.
 */

/** A single result row, keyed by column name in camelCase. */
// biome-ignore lint/suspicious/noExplicitAny: a row's column values are untyped
export type SqlRow = Record<string, any>;

/**
 * A settled query result: the rows plus the result metadata the server
 * reported.
 */
// biome-ignore lint/suspicious/noExplicitAny: mirrors the row-tuple bound of the query
export type SqlResult<T extends readonly any[] = SqlRow[]> = T & {
  /** Rows the statement returned or affected, as the server reported it. */
  readonly count: number;
  /** The SQL command tag, such as `SELECT` or `INSERT`. */
  readonly command: string;
};

/**
 * What a builder makes: an identifier, a row list, an `IN` list, a JSON or
 * array parameter. It is meaningful only as a value of a query's template.
 */
export type SqlFragment = object;

/**
 * A pending tagged-template query. It runs when first awaited or executed;
 * it can be cancelled (see {@link import('./abort').abortableQuery}) and used
 * as a fragment of a larger query, where it runs on that query's connection.
 */
// biome-ignore lint/suspicious/noExplicitAny: mirrors the row-tuple bound of the query
export interface SqlQuery<T extends readonly any[] = SqlRow[]> extends PromiseLike<SqlResult<T>> {
  catch<R = never>(onrejected?: ((reason: unknown) => R | PromiseLike<R>) | null): Promise<SqlResult<T> | R>;
  finally(onfinally?: (() => void) | null): Promise<SqlResult<T>>;
  /** Return each row as an array of column values, in column order, untransformed. */
  // biome-ignore lint/suspicious/noExplicitAny: a row's column values are untyped
  values(): SqlQuery<any[][]>;
  /** Return each row as an array of the raw column bytes, untransformed. */
  // biome-ignore lint/suspicious/noExplicitAny: a row's column values are untyped
  raw(): SqlQuery<any[][]>;
  /** Send the statement over the simple protocol, which runs several statements and takes no parameter. */
  simple(): SqlQuery<T>;
  /** Start the query now instead of on its first await. */
  execute(): SqlQuery<T>;
  /**
   * Cancel the query: one not dispatched yet never runs and rejects with
   * SQLSTATE `57014`; a running one is cancelled at the protocol level.
   */
  cancel(): void;
}

/** A `LISTEN` registration; see {@link SqlClient.listen}. */
export interface SqlListener {
  /** Stop receiving the channel's notifications. */
  unlisten(): Promise<void>;
}

/** The settled value of a transaction callback: an array of queries settles to their results. */
export type SqlTransactionResult<T> = T extends readonly unknown[]
  ? { -readonly [K in keyof T]: Awaited<T[K]> }
  : Awaited<T>;

/**
 * The SQL client the public API hands out. Call it as a template tag
 * (`sql`SELECT 1``), or use `sql.unsafe(...)` for dynamic SQL.
 *
 * Called with a value instead of a template, it builds a fragment:
 * `sql('createdAt')` is the identifier `"created_at"`, `sql(row, ...columns)`
 * and `sql(rows, ...columns)` the column list and values of an `INSERT` (or
 * the assignments of an `UPDATE`), `sql([1, 2])` an `IN` list.
 */
export interface SqlClient {
  // biome-ignore lint/suspicious/noExplicitAny: mirrors the row-tuple bound of the query
  <T extends readonly any[] = SqlRow[]>(strings: TemplateStringsArray, ...parameters: readonly unknown[]): SqlQuery<T>;
  (identifierOrList: string | readonly unknown[]): SqlFragment;
  (rows: object | readonly object[], ...columns: readonly (string | readonly string[])[]): SqlFragment;
  /** A statement given as text, with `$1`-style parameters. Without parameters it may hold several statements. */
  // biome-ignore lint/suspicious/noExplicitAny: mirrors the row-tuple bound of the query
  unsafe<T extends readonly any[] = SqlRow[]>(query: string, parameters?: readonly unknown[]): SqlQuery<T>;
  /** A statement read from a file, with `$1`-style parameters. */
  // biome-ignore lint/suspicious/noExplicitAny: mirrors the row-tuple bound of the query
  file<T extends readonly any[] = SqlRow[]>(path: string, parameters?: readonly unknown[]): SqlQuery<T>;
  /** A parameter sent as JSON, whatever its JavaScript type. */
  json(value: unknown): SqlFragment;
  /** A parameter sent as a PostgreSQL array of the given element type. */
  array(values: readonly unknown[], type?: string | number): SqlFragment;
  /** `NOTIFY` a channel through `pg_notify`. */
  notify(channel: string, payload: string): Promise<void>;
  /** `LISTEN` on a channel, on a connection the pool dedicates to notifications. */
  listen(channel: string, onnotify: (payload: string) => void, onlisten?: () => void): Promise<SqlListener>;
  /** Reserve one connection for exclusive use; `release()` it when done. */
  reserve(): Promise<ReservedSqlClient>;
  /** Run `fn` in a transaction on one connection: it commits when `fn` settles, and rolls back when it throws. */
  begin<T>(fn: (sql: TransactionSqlClient) => T | Promise<T>): Promise<SqlTransactionResult<T>>;
  begin<T>(options: string, fn: (sql: TransactionSqlClient) => T | Promise<T>): Promise<SqlTransactionResult<T>>;
  /** Close this client; see `closeDatabase()`. */
  end(options?: { timeout?: number }): Promise<void>;
  /** Alias of {@link end}. */
  close(options?: { timeout?: number }): Promise<void>;
}

/**
 * A connection reserved out of the pool for exclusive use. The transaction
 * machinery holds one per enrolled datasource between `BEGIN` and
 * `COMMIT`/`ROLLBACK`; callers must `release()` it when done.
 */
export interface ReservedSqlClient extends SqlClient {
  release(): void;
}

/** The client a transaction callback receives. */
export interface TransactionSqlClient extends SqlClient {
  /** Run `fn` inside a savepoint of the transaction. */
  savepoint<T>(fn: (sql: TransactionSqlClient) => T | Promise<T>): Promise<SqlTransactionResult<T>>;
  savepoint<T>(name: string, fn: (sql: TransactionSqlClient) => T | Promise<T>): Promise<SqlTransactionResult<T>>;
}
