import { readFile } from 'node:fs';
import type postgres from 'postgres';
import type { SqlClient } from '../sql-client';

/**
 * The datasource-bound logical client: a `Proxy` over the shared physical
 * postgres.js `Sql` that binds every operation to one datasource's
 * `search_path`.
 *
 * Several datasources — several owned schemas of one physical database — share
 * one physical pool (see physical-pool.ts and doc/adr/0003), so a connection
 * handed to datasource A may have last served datasource B. `search_path` is
 * therefore not a startup parameter of the pool: it is a session setting this
 * client sets on the connection each operation uses, before the operation's
 * own statement.
 *
 * postgres.js 3.4.8 has no acquire hook and does not expose which connection a
 * query or a `reserve()` landed on, so the marker-and-skip optimization the Go
 * adapter's `PrepareConn` hook makes cannot exist here. Every operation
 * therefore reserves a connection, issues the set (or `RESET search_path` for
 * a datasource that runs on the server default — it must still neutralize
 * what another datasource left behind), then its statement, PIPELINED on that
 * reserved connection: statements dispatched back-to-back are written without
 * waiting for the first to answer, so the set costs server-side microseconds
 * and no extra network round trip. The connection is released once the
 * statement settled; when the set failed, the operation rejects with the set's
 * error — a result is never returned under an unverified `search_path`.
 *
 * The queries this client hands out are the driver's own `Query` objects,
 * created by the physical `Sql` and then re-pointed at a datasource-bound
 * dispatch: they stay `instanceof` the driver's class, so a query used as a
 * fragment inside another query (`sql\`${sql\`a = b\`}\``, the Repository's
 * upsert assignments) is still inlined by the driver, and `values()`, `raw()`,
 * `simple()`, `describe()`, `cursor()`, `forEach()`, `execute()`, `cancel()`,
 * `readable()`/`writable()` keep their native behavior. Builders
 * (`sql(identifier)`, `sql(rows, ...columns)`) touch no connection and forward
 * untouched. `reserve()` and `begin()` set the path on the pinned connection
 * they hand out, which is what the transaction machinery and the migrator need.
 */

/** The statement that binds a connection's session to a datasource's `search_path`. */
export const SET_SEARCH_PATH_SQL = "SELECT set_config('search_path', $1, false)";
/** The statement that returns a connection's session to the server default. */
export const RESET_SEARCH_PATH_SQL = 'RESET search_path';

/** What the logical client needs to know about its datasource. */
interface DatasourceClientOptions {
  /** The datasource this client is bound to; named in diagnostics. */
  readonly datasource: string;
  /**
   * The datasource's `search_path`, bound as a parameter (a comma list keeps
   * working; no quoting), or `undefined` for the server default.
   */
  readonly searchPath: string | undefined;
  /** Called once, on the first `end()`/`close()`: releases this datasource's handle. */
  readonly onEnd: (options?: { timeout?: number }) => Promise<void>;
}

/** The minimal statement surface a set needs: what `Sql`, `ReservedSql`, and `TransactionSql` all offer. */
type Unsafe = { unsafe(query: string, parameters?: unknown[]): PromiseLike<unknown> };

/**
 * setSearchPath issues the session-level set (or reset) for `searchPath` on
 * `conn` and returns the pending statement WITHOUT awaiting it, so a caller
 * that dispatches its own statement right after gets both pipelined on the
 * same connection.
 */
function setSearchPath(conn: Unsafe, searchPath: string | undefined): PromiseLike<unknown> {
  const path = searchPath?.trim();
  return path ? conn.unsafe(SET_SEARCH_PATH_SQL, [path]) : conn.unsafe(RESET_SEARCH_PATH_SQL);
}

/**
 * The driver's `Query` internals this client relies on. They are plain
 * instance fields set by the `Query` constructor in postgres.js 3.x: `handler`
 * receives the query when it is first awaited or executed, `executed` guards
 * against a second dispatch, `resolve`/`reject` settle it (the cursor iterator
 * re-points them per step), and `streaming` marks a COPY stream whose
 * connection stays busy after the query resolved.
 */
interface DriverQuery extends PromiseLike<unknown> {
  handler: (query: DriverQuery) => void;
  executed: boolean;
  resolve: (value: unknown) => void;
  reject: (reason: unknown) => void;
  streaming?: boolean;
  cancelled?: unknown;
  strings: unknown;
}

function closedError(datasource: string): Error {
  return new Error(`database: datasource ${JSON.stringify(datasource)} is closed; call database(name) to reopen it`);
}

function isTemplateStringsArray(value: unknown): value is TemplateStringsArray {
  return Array.isArray(value) && Array.isArray((value as { raw?: unknown }).raw);
}

/**
 * createDatasourceClient returns the datasource-bound logical client over the
 * shared `physical` pool. It satisfies the `SqlClient` (= `postgres.Sql`)
 * surface structurally: `typeof client === 'function'`, tagged-template calls,
 * `unsafe`, `file`, `notify`, `reserve`, `begin`, `end`/`close` are bound to
 * the datasource; `sql(identifier)`, `sql(rows, ...columns)`, `json`, `array`,
 * `typed`, `types`, `options`, `parameters`, `listen`, `subscribe`,
 * `largeObject`, `PostgresError`, `CLOSE`, `END` and anything else forward to
 * the physical `Sql` untouched.
 */
export function createDatasourceClient(physical: postgres.Sql, options: DatasourceClientOptions): SqlClient {
  let closed = false;
  let ending: Promise<void> | undefined;

  /**
   * dispatch runs `query` — a driver Query the physical `Sql` built but has
   * not dispatched — on a connection reserved for this operation: it issues
   * the set, then the query, back-to-back through the reserved connection's
   * own handler (pipelined), fails the query with the set's error when the set
   * failed, and releases the connection once the query settled.
   */
  const dispatch = async (query: DriverQuery): Promise<void> => {
    if (closed) {
      query.reject(closedError(options.datasource));
      return;
    }
    if (query.cancelled) {
      // Cancelled before it was dispatched: the driver already rejected it
      // with 57014; reserving a connection for it would only leak the handle.
      return;
    }
    let reserved: postgres.ReservedSql;
    try {
      reserved = await physical.reserve();
    } catch (err) {
      query.reject(err);
      return;
    }
    if (query.cancelled) {
      // Cancelled while the reserve was pending: the driver already rejected
      // the query, and connection.execute() would skip it without settling it
      // again, leaving this connection reserved for ever. Hand it back unused.
      reserved.release();
      return;
    }
    let released = false;
    const release = () => {
      if (!released) {
        released = true;
        reserved.release();
      }
    };
    // The set is a driver Query created by the reserved connection, so its
    // handler is that connection's: executing it and then our query through
    // the same handler puts both on the same connection, in order.
    const set = setSearchPath(reserved, options.searchPath) as DriverQuery;
    const handler = set.handler;
    // Marked executed before anything can call its then(): the dispatch below
    // is the one and only execution of the set.
    set.executed = true;
    // Nobody awaits the set itself — its verdict travels through the hooks
    // below — so give its promise a handler: a failed set (connection drop,
    // pool ended mid-flight) must not surface as an unhandled rejection. Safe
    // because Query.then() calls handle(), a no-op once `executed` is true.
    set.then(undefined, () => {});
    // The server answers the pipelined pair in order and the driver settles
    // each synchronously as it reads, so one socket read can settle both.
    // Track the set's verdict synchronously through its own settlement hooks:
    // a promise callback would observe it one microtask too late, after the
    // statement had already resolved with a result read under an unverified
    // path.
    let setOutcome: { ok: true } | { ok: false; error: unknown } | undefined;
    const setResolve = set.resolve;
    const setReject = set.reject;
    set.resolve = (value) => {
      setOutcome = { ok: true };
      setResolve(value);
    };
    set.reject = (error) => {
      setOutcome = { ok: false, error };
      setReject(error);
    };
    // Release once the driver has finished handling the message that settled
    // the query (a microtask later), the moment transaction.ts releases after
    // its COMMIT. The cursor iterator re-points resolve/reject on every step,
    // so observe them through accessors instead of wrapping them once. A COPY
    // stream keeps the connection busy after the query resolved with the
    // stream; release when that stream closes instead.
    const finish = () => queueMicrotask(release);
    let innerResolve = query.resolve;
    let innerReject = query.reject;
    Object.defineProperty(query, 'resolve', {
      configurable: true,
      get: () => (value: unknown) => {
        if (!setOutcome?.ok) {
          // Fail closed: never hand out a result the set did not vouch for.
          innerReject(
            setOutcome
              ? setOutcome.error
              : new Error(
                  `database: search_path for datasource ${JSON.stringify(options.datasource)} was not confirmed before the statement completed`,
                ),
          );
          finish();
          return;
        }
        innerResolve(value);
        if (query.streaming && value && typeof (value as { once?: unknown }).once === 'function') {
          const stream = value as { once(event: string, cb: () => void): unknown };
          stream.once('close', finish);
          stream.once('error', finish);
        } else {
          finish();
        }
      },
      set: (fn: (value: unknown) => void) => {
        innerResolve = fn;
      },
    });
    Object.defineProperty(query, 'reject', {
      configurable: true,
      get: () => (reason: unknown) => {
        innerReject(setOutcome && !setOutcome.ok ? setOutcome.error : reason);
        finish();
      },
      set: (fn: (reason: unknown) => void) => {
        innerReject = fn;
      },
    });
    handler(set);
    handler(query);
  };

  /** bind re-points a physical-pool Query at the datasource-bound dispatch. */
  const bind = <Q>(query: Q): Q => {
    (query as unknown as DriverQuery).handler = (q) => void dispatch(q);
    return query;
  };

  const bound: Record<PropertyKey, unknown> = {
    unsafe: (...args: unknown[]) => bind(Reflect.apply(physical.unsafe, physical, args)),
    file: (path: unknown, args: unknown = [], fileOptions: unknown = {}) => {
      // Mirror the driver's own file(): read, then run as an unsafe statement
      // with the same simple-protocol default, bound to the datasource.
      if (!Array.isArray(args)) {
        fileOptions = args;
        args = [];
      }
      const parameters = args as unknown[];
      // `simple` is a real driver option its typings do not declare.
      const unsafeOptions = {
        ...(fileOptions as object),
        simple:
          'simple' in (fileOptions as object) ? (fileOptions as { simple: boolean }).simple : parameters.length === 0,
      } as postgres.UnsafeQueryOptions;
      const query = physical.unsafe('', parameters as never, unsafeOptions) as unknown as DriverQuery;
      query.handler = (q) => {
        readFile(path as string, 'utf8', (err, text) => {
          if (err) {
            q.reject(err);
            return;
          }
          q.strings = [text];
          void dispatch(q);
        });
      };
      return query;
    },
    notify: async (channel: string, payload: string) => {
      const strings = Object.assign(['select pg_notify(', ', ', ')'], { raw: ['select pg_notify(', ', ', ')'] });
      return await bind(physical(strings as unknown as TemplateStringsArray, channel, `${payload}`));
    },
    reserve: async (): Promise<postgres.ReservedSql> => {
      if (closed) {
        throw closedError(options.datasource);
      }
      const reserved = await physical.reserve();
      try {
        await setSearchPath(reserved, options.searchPath);
      } catch (err) {
        reserved.release();
        throw err;
      }
      return reserved;
    },
    begin: (...args: unknown[]): Promise<unknown> => {
      if (closed) {
        return Promise.reject(closedError(options.datasource));
      }
      const [first, second] = args;
      const fn = (typeof first === 'function' ? first : second) as (tx: postgres.TransactionSql) => unknown;
      const scoped = async (tx: postgres.TransactionSql) => {
        await setSearchPath(tx, options.searchPath);
        const result = fn(tx);
        // The driver awaits an array of queries returned from the callback;
        // keep that contract now that the callback's result rides a promise.
        return Array.isArray(result) ? Promise.all(result) : result;
      };
      return typeof first === 'function'
        ? physical.begin(scoped as never)
        : physical.begin(first as string, scoped as never);
    },
    end: (endOptions?: { timeout?: number }): Promise<void> => {
      if (!closed) {
        closed = true;
        ending = options.onEnd(endOptions);
      }
      return ending ?? Promise.resolve();
    },
  };
  bound['close'] = bound['end'];

  const client = new Proxy(physical as unknown as (...args: unknown[]) => unknown, {
    apply(target, thisArg, args) {
      if (isTemplateStringsArray(args[0])) {
        return bind(Reflect.apply(target, thisArg, args));
      }
      // Identifier and row/column builders: no connection involved.
      return Reflect.apply(target, thisArg, args);
    },
    get(target, property) {
      // Own keys only: `'toString' in bound` would otherwise answer with
      // Object.prototype's member instead of the physical Sql's.
      if (Object.hasOwn(bound, property)) {
        return bound[property];
      }
      return Reflect.get(target, property, target);
    },
  });
  return client as unknown as SqlClient;
}
