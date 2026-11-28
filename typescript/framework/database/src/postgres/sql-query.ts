import type { SQL } from 'bun';
import { REDACTED, useLogger } from '@putnami/runtime';
import type {
  ReservedSqlClient,
  SqlClient,
  SqlFragment,
  SqlListener,
  SqlQuery,
  TransactionSqlClient,
} from '../sql-client';

/**
 * The query machinery behind every {@link SqlClient} the package hands out
 * (doc/adr/0005-bun-sql-is-the-driver.md).
 *
 * A {@link CapturedQuery} records a template and runs only when first awaited
 * or executed: its client's dispatch picks the connection (a reserved one with
 * the datasource's `search_path`, a transaction's, …) and runs it there. A
 * captured query used as a value of another query's template is rebuilt on the
 * connection that query runs on. Builders (identifiers, row lists, `IN`
 * lists, JSON and array parameters) come from the driver pool and touch no
 * connection.
 *
 * The client keeps the naming the framework had on postgres.js with its
 * `camel` transform: row queries return camelCase keys, builders write
 * identifiers and row keys in snake_case and send an `undefined` field as
 * `NULL`. Server errors carry the SQLSTATE in `code`.
 */

/** The driver surface a query runs on: the pool, a reserved connection, a transaction or a savepoint. */
export type DriverSql = SQL;

/** The driver's query object, as far as the client drives it. */
interface DriverQuery extends PromiseLike<unknown> {
  execute(): DriverQuery;
  values(): DriverQuery;
  raw(): DriverQuery;
  simple(): DriverQuery;
  cancel(): unknown;
}

/** What a captured query runs. */
export type QueryTemplate =
  | { readonly kind: 'template'; readonly strings: TemplateStringsArray; readonly values: readonly unknown[] }
  | { readonly kind: 'unsafe'; readonly text: string; readonly parameters: readonly unknown[] | undefined }
  | { readonly kind: 'file'; readonly path: string; readonly parameters: readonly unknown[] | undefined };

type Mode = 'rows' | 'values' | 'raw';

/** Runs a captured query on a connection the client chooses and settles with its result. */
export type Dispatch = (query: CapturedQuery) => Promise<unknown>;

const NOTIFY_SQL = 'SELECT pg_notify($1, $2)';

/** The error a query cancelled before it ran rejects with: SQLSTATE 57014, as the server reports a cancel. */
export function queryCancelledError(): Error {
  return Object.assign(new Error('canceling statement due to user request'), { code: '57014' });
}

const SQLSTATE = /^[0-9A-Z]{5}$/;

/**
 * Give a driver server error the SQLSTATE as `code`, keeping the driver's own
 * code as `driverCode`. `Bun.SQL` reports the SQLSTATE in `errno`. Any other
 * value is returned untouched; normalizing twice changes nothing.
 */
export function normalizeError(error: unknown): unknown {
  if (!(error instanceof Error)) return error;
  const fields = error as Error & { code?: unknown; errno?: unknown };
  if (typeof fields.errno === 'string' && SQLSTATE.test(fields.errno) && fields.code !== fields.errno) {
    const driverCode = fields.code;
    Object.defineProperty(error, 'code', { value: fields.errno, configurable: true, enumerable: true, writable: true });
    Object.defineProperty(error, 'driverCode', {
      value: driverCode,
      configurable: true,
      enumerable: false,
      writable: true,
    });
  }
  return error;
}

/**
 * A column name as postgres.js's `toCamel` wrote it: each character after an
 * underscore is upper-cased and the underscore dropped (`created_at` →
 * `createdAt`); the first character is kept as is.
 */
export function toCamel(name: string): string {
  let out = name[0] ?? '';
  for (let i = 1; i < name.length; i++) {
    if (name[i] === '_') {
      i++;
      out += (name[i] ?? '').toUpperCase();
    } else {
      out += name[i];
    }
  }
  return out;
}

/** An identifier as postgres.js's `fromCamel` wrote it: `createdAt` → `created_at`. */
export function fromCamel(name: string): string {
  return name.replace(/([A-Z])/g, '_$1').toLowerCase();
}

const CAMEL_CACHE_LIMIT = 4096;
const camelCache = new Map<string, string>();

function camelKey(name: string): string {
  let out = camelCache.get(name);
  if (out === undefined) {
    out = toCamel(name);
    if (camelCache.size < CAMEL_CACHE_LIMIT) camelCache.set(name, out);
  }
  return out;
}

type DriverResult = unknown[] & { count?: number | null; command?: string };

/**
 * A column value as the client hands it out: the driver reads integer and
 * float arrays of the binary protocol as typed arrays (`Int32Array`, …); they
 * become plain arrays, as text-protocol arrays already are. A byte buffer
 * (`bytea`) stays one.
 */
function plainValue(value: unknown): unknown {
  if (value === null || typeof value !== 'object') return value;
  if (ArrayBuffer.isView(value) && !(value instanceof Uint8Array) && !(value instanceof DataView)) {
    return Array.from(value as unknown as ArrayLike<unknown>);
  }
  if (Array.isArray(value) && value.length > 0 && ArrayBuffer.isView(value[0])) {
    return value.map(plainValue);
  }
  return value;
}

/**
 * One row as the client hands it out: `columns[j]` of `row` under `keys[j]`.
 * It relies on what `Bun.SQL` returns: the rows of one statement are plain
 * objects with the statement's columns, as own keys in one order. A row that
 * lacks one of them is converted on its own column names; a row with a column
 * the first row lacks, which no statement returns, would lose it.
 */
function convertRow(
  row: Record<string, unknown>,
  columns: readonly string[],
  keys: readonly string[],
  camel: boolean,
): Record<string, unknown> {
  const record: Record<string, unknown> = {};
  for (let j = 0; j < columns.length; j++) {
    const column = columns[j] as string;
    if (!Object.hasOwn(row, column)) {
      const own = Object.keys(row);
      return convertRow(row, own, camel ? own.map(camelKey) : own, camel);
    }
    record[keys[j] as string] = plainValue(row[column]);
  }
  return record;
}

/**
 * A result as the client hands it out, its `count` and `command` kept: a row
 * query's rows get camelCase keys, a `values()` query's rows stay arrays;
 * nested results (a multi-statement simple query) too.
 */
function clientResult(result: unknown, camel: boolean): unknown {
  if (!Array.isArray(result)) return result;
  const source = result as DriverResult;
  const out = new Array(source.length) as DriverResult;
  // The rows of one statement share its columns: their names are read, and
  // camelCased, once per result rather than once per row.
  let columns: string[] | undefined;
  let keys: string[] | undefined;
  for (let i = 0; i < source.length; i++) {
    const row = source[i];
    if (Array.isArray(row)) {
      // A row query's rows are objects, so an array is a nested result (a
      // multi-statement simple query); a values() query's rows are arrays,
      // and its nested results carry their command tag.
      out[i] = camel || 'command' in row ? clientResult(row, camel) : row.map(plainValue);
    } else if (row !== null && typeof row === 'object') {
      if (columns === undefined) {
        columns = Object.keys(row);
        keys = camel ? columns.map(camelKey) : columns;
      }
      out[i] = convertRow(row as Record<string, unknown>, columns, keys as string[], camel);
    } else {
      out[i] = row;
    }
  }
  out.count = source.count ?? 0;
  out.command = source.command ?? '';
  return out;
}

/**
 * A row a row builder converts: any object but an array, a date or binary
 * data — a class instance too, as postgres.js converted it.
 */
function isRowObject(value: unknown): value is Record<string, unknown> {
  return (
    value !== null &&
    typeof value === 'object' &&
    !Array.isArray(value) &&
    !(value instanceof Date) &&
    !(value instanceof ArrayBuffer) &&
    !ArrayBuffer.isView(value)
  );
}

function driverRow(row: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const key of Object.keys(row)) {
    const value = row[key];
    out[fromCamel(key)] = value === undefined ? null : value;
  }
  return out;
}

/**
 * Build a fragment on the driver pool: an identifier and the keys and columns
 * of a row builder in snake_case, an `undefined` field as `NULL`; an `IN` list
 * and anything else untouched.
 */
function buildFragment(pool: DriverSql, first: unknown, rest: readonly unknown[]): SqlFragment {
  const call = pool as unknown as (...args: unknown[]) => SqlFragment;
  if (typeof first === 'string') {
    return call(fromCamel(first));
  }
  const columns = rest.flat().map((column) => fromCamel(String(column)));
  if (Array.isArray(first) && first.length > 0 && first.every(isRowObject)) {
    return call(first.map(driverRow), ...columns);
  }
  if (isRowObject(first)) {
    return call(driverRow(first), ...columns);
  }
  return call(first, ...rest);
}

function isTemplateStringsArray(value: unknown): value is TemplateStringsArray {
  return Array.isArray(value) && Array.isArray((value as { raw?: unknown }).raw);
}

/**
 * The statement shape the debug log shows: a template's strings with `$n`
 * for each parameter, a nested query inlined. It never holds a value.
 */
export function statementText(template: QueryTemplate, counter = { n: 0 }): string {
  switch (template.kind) {
    case 'unsafe':
      return template.text;
    case 'file':
      return `-- file ${template.path}`;
    case 'template': {
      let text = template.strings[0] ?? '';
      for (let i = 0; i < template.values.length; i++) {
        const value = template.values[i];
        text += value instanceof CapturedQuery ? statementText(value.template, counter) : `$${++counter.n}`;
        text += template.strings[i + 1] ?? '';
      }
      return text;
    }
  }
}

function parameterCount(template: QueryTemplate): number {
  if (template.kind === 'template') {
    let n = 0;
    for (const value of template.values) {
      n += value instanceof CapturedQuery ? parameterCount(value.template) : 1;
    }
    return n;
  }
  return template.parameters?.length ?? 0;
}

/**
 * Log a statement before it runs, every parameter value redacted: a bound
 * value may be a password, a token or a `Sensitive()` field. `debug` is a
 * dev-only switch.
 */
function logStatement(template: QueryTemplate): void {
  useLogger().debug('postgres query', {
    query: statementText(template),
    parameters: Array.from({ length: parameterCount(template) }, () => REDACTED),
  });
}

/** A query that captures its template and runs, when first awaited or executed, through its client's dispatch. */
export class CapturedQuery implements SqlQuery {
  #mode: Mode = 'rows';
  #simple = false;
  #cancelled = false;
  #driver: DriverQuery | undefined;
  #promise: Promise<unknown> | undefined;

  constructor(
    readonly dispatch: Dispatch,
    readonly template: QueryTemplate,
  ) {}

  /** Whether {@link cancel} was called. */
  get cancelled(): boolean {
    return this.#cancelled;
  }

  // biome-ignore lint/suspicious/noExplicitAny: a row's column values are untyped
  values(): SqlQuery<any[][]> {
    this.#mode = 'values';
    // biome-ignore lint/suspicious/noExplicitAny: a row's column values are untyped
    return this as unknown as SqlQuery<any[][]>;
  }

  // biome-ignore lint/suspicious/noExplicitAny: a row's column values are untyped
  raw(): SqlQuery<any[][]> {
    this.#mode = 'raw';
    // biome-ignore lint/suspicious/noExplicitAny: a row's column values are untyped
    return this as unknown as SqlQuery<any[][]>;
  }

  simple(): this {
    this.#simple = true;
    return this;
  }

  execute(): this {
    // The caller settles it through a later await; a rejection nobody awaits
    // is not an unhandled one.
    this.#start().catch(() => {});
    return this;
  }

  cancel(): void {
    this.#cancelled = true;
    this.#driver?.cancel();
  }

  // biome-ignore lint/suspicious/noThenProperty: a query is awaitable by contract
  then<A = never, B = never>(
    onfulfilled?: ((value: never) => A | PromiseLike<A>) | null,
    onrejected?: ((reason: unknown) => B | PromiseLike<B>) | null,
  ): Promise<A | B> {
    return this.#start().then(onfulfilled as (value: unknown) => A | PromiseLike<A>, onrejected);
  }

  catch<R = never>(onrejected?: ((reason: unknown) => R | PromiseLike<R>) | null): Promise<never> {
    return this.#start().catch(onrejected) as Promise<never>;
  }

  finally(onfinally?: (() => void) | null): Promise<never> {
    return this.#start().finally(onfinally) as Promise<never>;
  }

  get [Symbol.toStringTag](): string {
    return 'SqlQuery';
  }

  #start(): Promise<unknown> {
    if (!this.#promise) {
      this.#promise = this.#cancelled ? Promise.reject(queryCancelledError()) : this.dispatch(this);
    }
    return this.#promise;
  }

  /** Build the driver query on `conn` without running it; a nested captured query becomes a fragment built on `conn`. */
  build(conn: DriverSql): DriverQuery {
    const template = this.template;
    let query: DriverQuery;
    switch (template.kind) {
      case 'template': {
        const values = template.values.map((value) => (value instanceof CapturedQuery ? value.build(conn) : value));
        query = (conn as unknown as (...args: unknown[]) => DriverQuery)(template.strings, ...values);
        break;
      }
      case 'unsafe':
        query = conn.unsafe(template.text, template.parameters ? [...template.parameters] : undefined) as never;
        break;
      case 'file':
        query = conn.file(template.path, template.parameters ? [...template.parameters] : undefined) as never;
        break;
    }
    if (this.#mode === 'values') query = query.values();
    if (this.#mode === 'raw') query = query.raw();
    if (this.#simple) query = query.simple();
    return query;
  }

  /**
   * Run this query on `conn` and settle with its result: rows camelCased for
   * a row query, a server error normalized. The driver query is built and
   * executed synchronously, so a statement the caller executed on `conn`
   * just before runs first.
   */
  runOn(conn: DriverSql, debug = false): Promise<unknown> {
    if (this.#cancelled) {
      return Promise.reject(queryCancelledError());
    }
    if (debug) logStatement(this.template);
    let query: DriverQuery;
    try {
      query = this.build(conn);
      this.#driver = query;
      query.execute();
    } catch (error) {
      return Promise.reject(normalizeError(error));
    }
    const mode = this.#mode;
    // The driver query is a promise: continuing it directly skips the job a
    // Promise.resolve of a thenable adds.
    return (query as unknown as Promise<unknown>).then(
      (result) => (mode === 'raw' ? result : clientResult(result, mode === 'rows')),
      (error) => {
        throw normalizeError(error);
      },
    );
  }
}

/** What a client is made of: where its queries run, and the members that do not run a query. */
export interface ClientParts {
  /** The driver pool: builders, JSON and array parameters, `LISTEN`. */
  readonly pool: DriverSql;
  /** Runs a query this client built. */
  readonly dispatch: Dispatch;
  readonly reserve: () => Promise<ReservedSqlClient>;
  readonly begin: (options: string | undefined, fn: (sql: TransactionSqlClient) => unknown) => Promise<unknown>;
  readonly end: (options?: { timeout?: number }) => Promise<void>;
  /** Members a reserved or transaction client adds. */
  readonly extra?: Readonly<Record<string, unknown>>;
}

/** Build a {@link SqlClient} from its parts. */
export function createSqlClient<C extends SqlClient = SqlClient>(parts: ClientParts): C {
  const capture = (template: QueryTemplate): CapturedQuery => new CapturedQuery(parts.dispatch, template);
  const client = (first: unknown, ...rest: unknown[]): unknown =>
    isTemplateStringsArray(first)
      ? capture({ kind: 'template', strings: first, values: rest })
      : buildFragment(parts.pool, first, rest);
  const members: Record<PropertyKey, unknown> = {
    unsafe: (text: string, parameters?: readonly unknown[]) => capture({ kind: 'unsafe', text, parameters }),
    file: (path: string, parameters?: readonly unknown[]) => capture({ kind: 'file', path, parameters }),
    json: (value: unknown) => (parts.pool as unknown as { json(value: unknown): SqlFragment }).json(value),
    array: (values: readonly unknown[], type?: string | number) =>
      (parts.pool as unknown as { array(values: unknown[], type?: unknown): SqlFragment }).array([...values], type),
    notify: async (channel: string, payload: string): Promise<void> => {
      await capture({ kind: 'unsafe', text: NOTIFY_SQL, parameters: [channel, `${payload}`] });
    },
    listen: async (
      channel: string,
      onnotify: (payload: string) => void,
      onlisten?: () => void,
    ): Promise<SqlListener> => {
      try {
        return await parts.pool.listen(channel, onnotify, onlisten);
      } catch (error) {
        throw normalizeError(error);
      }
    },
    reserve: () => parts.reserve(),
    begin: (first: unknown, second?: unknown) =>
      typeof first === 'function'
        ? parts.begin(undefined, first as (sql: TransactionSqlClient) => unknown)
        : parts.begin(first as string, second as (sql: TransactionSqlClient) => unknown),
    end: (options?: { timeout?: number }) => parts.end(options),
    close: (options?: { timeout?: number }) => parts.end(options),
    ...parts.extra,
  };
  return Object.assign(client, members) as unknown as C;
}

/**
 * Run a transaction callback with its client: an array of queries the
 * callback returns settles to their results, as the driver does.
 */
async function runCallback(fn: (sql: TransactionSqlClient) => unknown, client: TransactionSqlClient): Promise<unknown> {
  const result = await fn(client);
  return Array.isArray(result) ? Promise.all(result) : result;
}

/** What a client over one pinned driver connection needs besides the connection. */
export interface PinnedOptions {
  /** The driver pool the connection belongs to. */
  readonly pool: DriverSql;
  /** Reserves another connection, with the same setup as this one. */
  readonly reserve: () => Promise<ReservedSqlClient>;
  /** Log each statement, values redacted. */
  readonly debug: boolean;
}

/** Begin a transaction on `conn` and run `fn` with its client; driver errors normalized. */
export async function beginOn(
  conn: DriverSql,
  options: string | undefined,
  fn: (sql: TransactionSqlClient) => unknown,
  pinned: PinnedOptions,
  setup?: (tx: DriverSql) => Promise<unknown>,
): Promise<unknown> {
  const run = async (tx: DriverSql) => {
    await setup?.(tx);
    return runCallback(fn, transactionClient(tx, pinned));
  };
  try {
    return await (options === undefined ? conn.begin(run as never) : conn.begin(options, run as never));
  } catch (error) {
    throw normalizeError(error);
  }
}

function pinnedParts(conn: DriverSql, pinned: PinnedOptions, extra: Record<string, unknown>): ClientParts {
  return {
    pool: pinned.pool,
    dispatch: (query) => query.runOn(conn, pinned.debug),
    reserve: pinned.reserve,
    begin: (options, fn) => beginOn(conn, options, fn, pinned),
    end: async () => {},
    extra,
  };
}

/** A client whose queries all run on the reserved connection `conn`, in order. */
export function reservedClient(conn: DriverSql & { release(): void }, pinned: PinnedOptions): ReservedSqlClient {
  let released = false;
  return createSqlClient<ReservedSqlClient>(
    pinnedParts(conn, pinned, {
      release: () => {
        if (!released) {
          released = true;
          conn.release();
        }
      },
    }),
  );
}

/** A client whose queries all run in the transaction (or savepoint) `tx`. */
export function transactionClient(tx: DriverSql, pinned: PinnedOptions): TransactionSqlClient {
  const savepoint = (first: unknown, second?: unknown): Promise<unknown> => {
    const fn = (typeof first === 'function' ? first : second) as (sql: TransactionSqlClient) => unknown;
    const run = (sp: DriverSql) => runCallback(fn, transactionClient(sp, pinned));
    const driver = tx as unknown as {
      savepoint(nameOrFn: unknown, fn?: unknown): Promise<unknown>;
    };
    const pending = typeof first === 'function' ? driver.savepoint(run) : driver.savepoint(first, run);
    return pending.catch((error: unknown) => {
      throw normalizeError(error);
    });
  };
  return createSqlClient<TransactionSqlClient>(pinnedParts(tx, pinned, { savepoint }));
}

/**
 * A client whose queries check `guard` when built and again when dispatched,
 * so a query held across awaits cannot run once the guard fails. Builders
 * check it when called; every other member is the client's own.
 */
export function guardClient<C extends SqlClient>(client: C, guard: () => void): C {
  const guarded = (result: unknown): unknown => {
    if (!(result instanceof CapturedQuery)) {
      return result;
    }
    const dispatch = result.dispatch;
    return new CapturedQuery((query) => {
      try {
        guard();
      } catch (error) {
        return Promise.reject(error);
      }
      return dispatch(query);
    }, result.template);
  };
  return new Proxy(client, {
    apply(target, thisArg, args) {
      guard();
      return guarded(Reflect.apply(target, thisArg, args));
    },
    get(target, property, receiver) {
      const value = Reflect.get(target, property, receiver);
      if ((property === 'unsafe' || property === 'file') && typeof value === 'function') {
        return (...args: unknown[]) => {
          guard();
          return guarded(Reflect.apply(value, target, args));
        };
      }
      return value;
    },
  });
}

/**
 * A client over the driver pool itself, bound to no datasource: each query
 * reserves a pooled connection, runs on it and releases it. The test
 * provider's admin connection uses it; datasources use datasource-client.ts.
 */
export function poolClient(pool: DriverSql, debug = false): SqlClient {
  const pinned: PinnedOptions = {
    pool,
    debug,
    reserve: async (): Promise<ReservedSqlClient> => {
      let reserved: DriverSql & { release(): void };
      try {
        reserved = (await pool.reserve()) as DriverSql & { release(): void };
      } catch (error) {
        throw normalizeError(error);
      }
      return reservedClient(reserved, pinned);
    },
  };
  return createSqlClient({
    pool,
    dispatch: async (query) => {
      if (query.cancelled) {
        throw queryCancelledError();
      }
      let reserved: DriverSql & { release(): void };
      try {
        reserved = (await pool.reserve()) as DriverSql & { release(): void };
      } catch (error) {
        throw normalizeError(error);
      }
      try {
        return await query.runOn(reserved, debug);
      } finally {
        reserved.release();
      }
    },
    reserve: pinned.reserve,
    begin: (mode, fn) => beginOn(pool, mode, fn, pinned),
    end: (options) => pool.end(options),
  });
}
