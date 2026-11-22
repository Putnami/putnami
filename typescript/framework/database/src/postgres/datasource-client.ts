import type { ReservedSqlClient, SqlClient, TransactionSqlClient } from '../sql-client';
import {
  beginOn,
  type CapturedQuery,
  createSqlClient,
  type DriverSql,
  normalizeError,
  type PinnedOptions,
  queryCancelledError,
  reservedClient,
} from './sql-query';

/**
 * The datasource-bound logical client: a {@link SqlClient} over the shared
 * physical `Bun.SQL` pool that binds every operation to one datasource's
 * `search_path`.
 *
 * Several datasources — several owned schemas of one physical database — share
 * one physical pool (see physical-pool.ts and doc/adr/0003), so a connection
 * handed to datasource A may have last served datasource B. `search_path` is
 * therefore not a startup parameter of the pool: it is a session setting this
 * client sets on the connection each operation uses, before the operation's
 * own statement.
 *
 * `Bun.SQL` has no acquire hook and does not expose which connection a query
 * or a `reserve()` landed on, so the marker-and-skip optimization the Go
 * adapter's `PrepareConn` hook makes cannot exist here. Every operation
 * therefore reserves a connection, executes the set (or `RESET search_path`
 * for a datasource that runs on the server default — it must still neutralize
 * what another datasource left behind) and then its statement back to back on
 * that reserved connection, which pipelines them: the set costs server-side
 * microseconds and no extra network round trip. The connection is released
 * once both settled; when the set failed, the operation rejects with the set's
 * error — a result is never returned under an unverified `search_path`.
 *
 * The queries this client hands out capture their template (see
 * sql-query.ts): a query used as a fragment of another runs on that other
 * query's connection, and `values()`, `raw()`, `simple()` and `cancel()` reach
 * the driver query. Builders touch no connection and come from the pool.
 * `reserve()` and `begin()` set the path on the pinned connection they hand
 * out, which is what the transaction machinery and the migrator need.
 */

/** The statement that binds a connection's session to a datasource's `search_path`. */
export const SET_SEARCH_PATH_SQL = "SELECT set_config('search_path', $1, false)";
/** The statement that returns a connection's session to the server default. */
export const RESET_SEARCH_PATH_SQL = 'RESET search_path';

/** What the logical client needs to know about its datasource. */
export interface DatasourceClientOptions {
  /** The datasource this client is bound to; named in diagnostics. */
  readonly datasource: string;
  /**
   * The datasource's `search_path`, bound as a parameter (a comma list keeps
   * working; no quoting), or `undefined` for the server default.
   */
  readonly searchPath: string | undefined;
  /** Log each statement with its parameter values redacted (dev only). */
  readonly debug?: boolean;
  /** Called once, on the first `end()`/`close()`: releases this datasource's handle. */
  readonly onEnd: (options?: { timeout?: number }) => Promise<void>;
}

/** The driver statement a set runs: what the pool, a reserved connection and a transaction all offer. */
type Unsafe = { unsafe(query: string, parameters?: unknown[]): PromiseLike<unknown> & { execute(): unknown } };

/**
 * setSearchPath issues the session-level set (or reset) for `searchPath` on
 * `conn` and returns the driver statement, already executed, WITHOUT awaiting
 * it: a statement the caller executes on `conn` right after is pipelined
 * behind it.
 */
function setSearchPath(conn: Unsafe, searchPath: string | undefined): PromiseLike<unknown> {
  const path = searchPath?.trim();
  const statement = path ? conn.unsafe(SET_SEARCH_PATH_SQL, [path]) : conn.unsafe(RESET_SEARCH_PATH_SQL);
  statement.execute();
  return statement;
}

function closedError(datasource: string): Error {
  return new Error(`database: datasource ${JSON.stringify(datasource)} is closed; call database(name) to reopen it`);
}

/**
 * createDatasourceClient returns the datasource-bound logical client over the
 * shared `physical` pool.
 */
export function createDatasourceClient(physical: DriverSql, options: DatasourceClientOptions): SqlClient {
  let closed = false;
  let ending: Promise<void> | undefined;
  const debug = options.debug ?? false;

  const reserveDriver = async (): Promise<DriverSql & { release(): void }> => {
    try {
      return (await physical.reserve()) as DriverSql & { release(): void };
    } catch (error) {
      throw normalizeError(error);
    }
  };

  /**
   * dispatch runs `query` on a connection reserved for this operation: it
   * executes the set, then the query, back to back on it, fails the query
   * with the set's error when the set failed, and releases the connection once
   * both settled.
   */
  const dispatch = async (query: CapturedQuery): Promise<unknown> => {
    if (closed) {
      throw closedError(options.datasource);
    }
    if (query.cancelled) {
      // Cancelled before it was dispatched: reserving a connection for it would
      // only hold one for nothing.
      throw queryCancelledError();
    }
    const reserved = await reserveDriver();
    try {
      if (query.cancelled) {
        throw queryCancelledError();
      }
      const set = setSearchPath(reserved, options.searchPath);
      const statement = query.runOn(reserved, debug);
      const [setOutcome, statementOutcome] = await Promise.allSettled([set, statement]);
      if (setOutcome.status === 'rejected') {
        // Fail closed: never hand out a result the set did not vouch for.
        throw normalizeError(setOutcome.reason);
      }
      if (statementOutcome.status === 'rejected') {
        throw statementOutcome.reason;
      }
      return statementOutcome.value;
    } finally {
      reserved.release();
    }
  };

  const pinned: PinnedOptions = {
    pool: physical,
    debug,
    reserve: async (): Promise<ReservedSqlClient> => {
      if (closed) {
        throw closedError(options.datasource);
      }
      const reserved = await reserveDriver();
      try {
        await setSearchPath(reserved, options.searchPath);
      } catch (error) {
        reserved.release();
        throw normalizeError(error);
      }
      return reservedClient(reserved, pinned);
    },
  };

  return createSqlClient({
    pool: physical,
    dispatch,
    reserve: pinned.reserve,
    begin: async (mode: string | undefined, fn: (sql: TransactionSqlClient) => unknown): Promise<unknown> => {
      if (closed) {
        throw closedError(options.datasource);
      }
      return beginOn(physical, mode, fn, pinned, (tx) =>
        Promise.resolve(setSearchPath(tx as Unsafe, options.searchPath)),
      );
    },
    end: (endOptions?: { timeout?: number }): Promise<void> => {
      if (!closed) {
        closed = true;
        ending = options.onEnd(endOptions);
      }
      return ending ?? Promise.resolve();
    },
  });
}
