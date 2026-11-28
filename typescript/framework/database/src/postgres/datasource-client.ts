import type { ReservedSqlClient, SqlClient, TransactionSqlClient } from '../sql-client';
import { type LeasedMember, leaseMember, UNKNOWN_SEARCH_PATH } from './driver-pool';
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
 * The pool is the package's own (driver-pool.ts): each member holds one
 * connection and the pool records the `search_path` its session holds,
 * forgetting it whenever the connection closes. An operation leases a member
 * for this datasource's path and, when the member's session already holds it,
 * runs its statement with no set at all, on a member other operations under
 * the same path may share — the common case of a workload with one
 * datasource per database. Otherwise it first awaits
 * `SELECT set_config('search_path', $1, false)` (or `RESET search_path` for a
 * datasource on the server default, which must still neutralize what another
 * datasource left behind), records the path, then runs its statement: a
 * failed set rejects the operation before its statement was sent, so a
 * statement never runs under an unverified `search_path`. A connection that
 * closed while the set ran is set again on its new session.
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

/** The driver statement a set runs on: a member, a reserved connection or a transaction. */
type SetTarget = Pick<DriverSql, 'unsafe'> & ((strings: TemplateStringsArray, ...values: unknown[]) => unknown);

/**
 * setSearchPath sets the session-level `search_path` of `conn` to `path`, or
 * resets it to the server default when `path` is empty. Both run on the
 * extended protocol.
 */
function setSearchPath(conn: SetTarget, path: string): Promise<unknown> {
  return Promise.resolve(path ? conn.unsafe(SET_SEARCH_PATH_SQL, [path]) : conn`RESET search_path`);
}

/** How many times an operation sets the path of one connection that keeps closing under it. */
const SET_ATTEMPTS = 2;

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

  const wanted = options.searchPath?.trim() ?? '';

  const reserveDriver = async (): Promise<DriverSql & { release(): void }> => {
    try {
      return (await physical.reserve()) as DriverSql & { release(): void };
    } catch (error) {
      throw normalizeError(error);
    }
  };

  /** A member for one operation, at once when the pool has one for it. */
  const leaseDriver = (): LeasedMember | Promise<LeasedMember> => {
    const leased = leaseMember(physical, wanted);
    if (!leased) {
      return Promise.reject(new Error('database: a datasource client runs on a pool built by createDriverPool'));
    }
    return leased instanceof Promise
      ? leased.then(undefined, (error: unknown) => {
          throw normalizeError(error);
        })
      : leased;
  };

  /**
   * dispatch runs `query` on a member of the pool. A member leased at once
   * whose session holds the datasource's path runs it in this turn; any other
   * goes through {@link runLeased}, which sets the path first.
   */
  const dispatch = (query: CapturedQuery): Promise<unknown> => {
    if (closed) {
      return Promise.reject(closedError(options.datasource));
    }
    if (query.cancelled) {
      // Cancelled before it was dispatched: leasing a connection for it would
      // only hold one for nothing.
      return Promise.reject(queryCancelledError());
    }
    const leased = leaseDriver();
    if (leased instanceof Promise) {
      return leased.then((member) => runLeased(query, member));
    }
    if (leased.session.searchPath !== wanted) {
      return runLeased(query, leased);
    }
    // The common case: a member whose session holds the path, at once. The
    // statement goes in this turn, with no frame of its own.
    let running: Promise<unknown>;
    try {
      running = query.runOn(leased.sql, debug);
    } catch (error) {
      leased.release();
      return Promise.reject(error);
    }
    return running.then(
      (result) => {
        leased.release();
        return result;
      },
      (error: unknown) => {
        leased.release();
        throw error;
      },
    );
  };

  /**
   * Runs `query` on a leased member: it sets the datasource's path first when
   * the member's session does not hold it, then runs the query, and gives the
   * member back once the query settled.
   */
  const runLeased = async (query: CapturedQuery, member: LeasedMember): Promise<unknown> => {
    try {
      if (query.cancelled) {
        throw queryCancelledError();
      }
      for (let attempt = 1; member.session.searchPath !== wanted; attempt++) {
        if (attempt > SET_ATTEMPTS) {
          throw new Error(
            `database: datasource ${JSON.stringify(options.datasource)}: the connection closed while its search_path was being set`,
          );
        }
        const closes = member.session.closes;
        member.session.searchPath = UNKNOWN_SEARCH_PATH;
        try {
          await setSearchPath(member.sql as unknown as SetTarget, wanted);
        } catch (error) {
          throw normalizeError(error);
        }
        if (member.session.closes === closes) {
          member.session.searchPath = wanted;
        }
      }
      if (query.cancelled) {
        throw queryCancelledError();
      }
      // Sent in the same turn as the check above: no close can come between.
      return await query.runOn(member.sql, debug);
    } finally {
      member.release();
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
        await setSearchPath(reserved as unknown as SetTarget, wanted);
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
      return beginOn(physical, mode, fn, pinned, (tx) => setSearchPath(tx as unknown as SetTarget, wanted));
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
