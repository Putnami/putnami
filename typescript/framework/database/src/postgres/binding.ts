import { type InferConfig, useRawConfigSection } from '@putnami/runtime';
import type { PostgresConfig } from './config';

/**
 * Canonical database protocol — TypeScript view.
 *
 * These shapes mirror `go.putnami.dev/protocol/database` (the JSON schemas under
 * `protocols/database/schemas`). They let the TypeScript runtime adapter
 * consume the *same* deployer-injected binding the Go adapter does, so a named
 * datasource resolves to identical connection semantics across both languages.
 *
 * Only the runtime `Binding` document is modelled here — the requirement
 * manifest (secret-free, projected into infra) and the test binding live with
 * their producers/consumers.
 */

/** Database protocol version this adapter accepts. */
export const DATABASE_PROTOCOL_VERSION = 1;

/**
 * EnvBinding is the environment variable a deploy target injects to hand a
 * workload the resolved physical connections for the logical datasources it
 * declared. The value is a canonical database `Binding` encoded as JSON — a map
 * keyed by logical datasource name, identical to the Go adapter's
 * `DATABASE_BINDINGS`; bindings carry secrets and must never be written into
 * `infra/requirements.json`. Deploy targets also merge the same document into
 * the `database` config section (CONFIG_SECTION_DATABASE), which wins when
 * both are present; the env var is the fallback.
 */
export const ENV_DATABASE_BINDINGS = 'DATABASE_BINDINGS';

/**
 * CONFIG_SECTION_DATABASE is the resolved-config section a deploy target
 * injects the managed database binding document into — the config-resolution
 * counterpart of ENV_DATABASE_BINDINGS. The control plane merges the canonical
 * `Binding` document into this section of the workload's resolved config;
 * operator-authored keys in the section are preserved, managed keys win.
 */
const CONFIG_SECTION_DATABASE = 'database';

const DATASOURCE_NAME_PATTERN = /^[a-z0-9][a-z0-9_./-]{0,63}$/;

/** One datasource's resolved physical connection (exactly one strategy). */
export interface DatabaseConnectionSpec {
  dsn?: string;
  host?: string;
  port?: number;
  database?: string;
  user?: string;
  password?: string;
  ssl?: boolean;
  params?: Record<string, string>;
  instance?: string;
}

/** One datasource's binding entry: engine, owning schema, and connection. */
export interface DatabaseBindingEntry {
  engine: string;
  schema?: string;
  connection?: DatabaseConnectionSpec;
}

/** The canonical runtime binding document, keyed by logical datasource name. */
export interface DatabaseBinding {
  $schema?: string;
  protocolVersion: number;
  databases?: Record<string, DatabaseBindingEntry>;
}

/**
 * A binding entry resolved into the inputs the connection factory consumes:
 * the connection half of a PostgresConfig and the owning schema applied as the
 * session `search_path`. `connectionParams` carries extra DSN query parameters
 * the structured config does not model (e.g. `application_name`).
 */
export interface ResolvedDatabaseBinding {
  config: Partial<InferConfig<typeof PostgresConfig>>;
  searchPath?: string;
  connectionParams?: Record<string, string>;
}

function fail(message: string): never {
  throw new Error(`${ENV_DATABASE_BINDINGS}: ${message}`);
}

/**
 * parseDatabaseBinding strictly parses and validates a canonical database
 * binding, with the same invariants the Go protocol enforces: a supported
 * protocol version, canonical datasource names, the postgres engine, a present
 * schema, and a connection declaring exactly one transport strategy. It throws
 * a descriptive error on any violation so a deploy-time misconfiguration fails
 * loudly rather than producing an opaque connection failure later.
 */
export function parseDatabaseBinding(raw: string): DatabaseBinding {
  let doc: DatabaseBinding;
  try {
    doc = JSON.parse(raw) as DatabaseBinding;
  } catch (err) {
    fail(`invalid JSON: ${err instanceof Error ? err.message : String(err)}`);
  }
  if (typeof doc !== 'object' || doc === null) {
    fail('binding must be a JSON object');
  }
  if (doc.protocolVersion !== DATABASE_PROTOCOL_VERSION) {
    fail(`unsupported protocolVersion ${String(doc.protocolVersion)} (supported: ${DATABASE_PROTOCOL_VERSION})`);
  }
  const databases = doc.databases ?? {};
  for (const [name, entry] of Object.entries(databases)) {
    if (!DATASOURCE_NAME_PATTERN.test(name)) {
      fail(`datasource name ${JSON.stringify(name)} is not a canonical identifier`);
    }
    validateEntry(name, entry);
  }
  return doc;
}

function validateEntry(name: string, entry: DatabaseBindingEntry): void {
  if (!entry || typeof entry !== 'object') {
    fail(`datasource ${JSON.stringify(name)} has no entry`);
  }
  if (entry.engine !== 'postgres') {
    fail(`datasource ${JSON.stringify(name)} uses unsupported engine ${JSON.stringify(entry.engine)} (only postgres)`);
  }
  if (!entry.schema || entry.schema.trim() === '') {
    fail(`datasource ${JSON.stringify(name)} requires a schema`);
  }
  if (!entry.connection) {
    fail(`datasource ${JSON.stringify(name)} has no connection`);
  }
  validateConnection(name, entry.connection);
}

function validateConnection(name: string, conn: DatabaseConnectionSpec): void {
  const hasDSN = !!conn.dsn?.trim();
  const hasHost = !!conn.host?.trim();
  const hasInstance = !!conn.instance?.trim();
  const strategies = [hasDSN, hasHost, hasInstance].filter(Boolean).length;
  if (strategies !== 1) {
    fail(`datasource ${JSON.stringify(name)} connection must declare exactly one of dsn, host, or instance`);
  }
  if (
    hasDSN &&
    (hasHost ||
      hasInstance ||
      conn.port ||
      conn.database ||
      conn.user ||
      conn.password ||
      conn.ssl !== undefined ||
      conn.params)
  ) {
    fail(`datasource ${JSON.stringify(name)} dsn connection must not carry other connection fields`);
  }
  if (hasInstance && (conn.port || conn.ssl !== undefined)) {
    fail(`datasource ${JSON.stringify(name)} instance connection must not carry port or ssl`);
  }
}

/**
 * resolveBindingEntry resolves one datasource entry of an already-parsed binding
 * into the connection overrides + search_path the factory applies, choosing the
 * single declared transport strategy (dsn / structured host / Cloud SQL instance
 * socket). It throws when the datasource is absent, listing the datasources the
 * binding does declare.
 */
export function resolveBindingEntry(binding: DatabaseBinding, datasource: string): ResolvedDatabaseBinding {
  const name = datasource.trim();
  if (!name) {
    fail('a datasource name is required to resolve a binding');
  }
  const entry = binding.databases?.[name];
  if (!entry) {
    const available = Object.keys(binding.databases ?? {}).sort();
    fail(
      `no binding for datasource ${JSON.stringify(name)}; declares: ${available.length ? available.join(', ') : '(none)'}`,
    );
  }
  validateEntry(name, entry);
  const conn = entry.connection as DatabaseConnectionSpec;
  const searchPath = entry.schema?.trim() || undefined;
  return { ...connectionToConfig(conn), searchPath };
}

/**
 * The pgx connection-pool parameters a connection may carry. They configure the
 * CLIENT's pool, not the server session: pgx consumes them itself
 * (`pgxpool.ParseConfig`), and the Go adapter lets a connection that states one
 * win over its `PoolConfig` field.
 *
 * They must never reach `connectionParams`. Everything left there is put into
 * postgres.js's `connection` option, which is the STARTUP packet, and
 * PostgreSQL refuses a startup parameter it does not know
 * (`unrecognized configuration parameter "pool_max_conns"`) — so one pool
 * parameter in a binding would take down every connection this adapter opens.
 * `dbtestenv` puts three of them in every test binding it synthesizes, for Go
 * and TypeScript projects alike.
 */
const POOL_CONNECTION_PARAMS = new Set([
  'pool_max_conns',
  'pool_min_conns',
  'pool_max_conn_lifetime',
  'pool_max_conn_lifetime_jitter',
  'pool_max_conn_idle_time',
  'pool_health_check_period',
]);

/**
 * splitPoolParams separates the pgx pool parameters from the server parameters.
 *
 * `pool_max_conns` has an exact twin in this adapter — `poolSize`, which
 * postgres.js takes as `max` — so it is honored, with the same precedence the
 * Go adapter applies: a connection that states the size wins over the
 * configured one. The others describe bounds postgres.js either owns itself
 * (`idle_timeout`, `max_lifetime`, both already configured in seconds) or does
 * not have, so they are dropped rather than guessed at.
 */
function splitPoolParams(params: Record<string, string> | undefined): {
  pool: Partial<InferConfig<typeof PostgresConfig>>;
  connectionParams?: Record<string, string>;
} {
  if (!params) {
    return { pool: {} };
  }
  const pool: Partial<InferConfig<typeof PostgresConfig>> = {};
  const rest: Record<string, string> = {};
  for (const [key, value] of Object.entries(params)) {
    const name = key.trim().toLowerCase();
    if (!POOL_CONNECTION_PARAMS.has(name)) {
      rest[key] = value;
      continue;
    }
    if (name === 'pool_max_conns') {
      const max = Number.parseInt(value, 10);
      if (Number.isInteger(max) && max > 0) {
        pool.poolSize = max;
      }
    }
  }
  return { pool, connectionParams: Object.keys(rest).length > 0 ? rest : undefined };
}

/**
 * connectionToConfig maps a single connection spec onto the structured config
 * (and any extra DSN params) the connection factory consumes, choosing the one
 * declared transport strategy: dsn (decomposed like pgx), structured host (TCP),
 * or Cloud SQL instance (the `/cloudsql/<instance>` socket host). It carries no
 * schema — that is the binding entry's concern.
 */
export function connectionToConfig(conn: DatabaseConnectionSpec): {
  config: Partial<InferConfig<typeof PostgresConfig>>;
  connectionParams?: Record<string, string>;
} {
  if (conn.dsn?.trim()) {
    return dsnToConfig(conn.dsn.trim());
  }
  const { pool, connectionParams } = splitPoolParams(conn.params);
  if (conn.host?.trim()) {
    return {
      config: pruneUndefined({
        host: conn.host.trim(),
        port: conn.port,
        database: conn.database,
        user: conn.user,
        password: conn.password,
        ssl: conn.ssl,
        ...pool,
      }),
      connectionParams,
    };
  }
  // Cloud SQL instance socket, mirroring the Go adapter's /cloudsql/<instance>.
  return {
    config: pruneUndefined({
      host: `/cloudsql/${conn.instance?.trim()}`,
      database: conn.database,
      user: conn.user,
      password: conn.password,
      ...pool,
    }),
    connectionParams,
  };
}

/**
 * configBindingDocument extracts the managed binding document a deploy target
 * merged into the `database` section of the resolved config and re-encodes it
 * as the canonical JSON document, so the config transport reuses the exact
 * parse + validation (parseDatabaseBinding) the env transport applies.
 * Only the protocol document keys (`$schema`, `protocolVersion`, `databases`)
 * are extracted — the section also carries operator-authored keys (e.g. the
 * per-datasource `database.<name>` connection blocks), which are ignored here.
 *
 * Returns undefined when the section carries no databases key (or an empty
 * one) so the caller falls back to the env transport; throws when the key is
 * present but not an object, so a malformed managed document fails loudly
 * instead of silently degrading to env or local configuration.
 */
function configBindingDocument(): string | undefined {
  const section = useRawConfigSection(CONFIG_SECTION_DATABASE);
  const databases = section?.['databases'];
  if (databases === undefined || databases === null) {
    return undefined;
  }
  if (typeof databases !== 'object' || Array.isArray(databases)) {
    throw new Error(
      `the ${JSON.stringify(CONFIG_SECTION_DATABASE)} config section carries a malformed managed binding: "databases" must be an object keyed by datasource name`,
    );
  }
  if (Object.keys(databases).length === 0) {
    return undefined;
  }
  const doc: Record<string, unknown> = { databases };
  if (section?.['$schema'] !== undefined) {
    doc['$schema'] = section['$schema'];
  }
  if (section?.['protocolVersion'] !== undefined) {
    doc['protocolVersion'] = section['protocolVersion'];
  }
  return JSON.stringify(doc);
}

// The process-wide programmatic binding pin (see setDatabaseBindingOverride).
let bindingOverride: string | undefined;

/**
 * setDatabaseBindingOverride pins the runtime binding document this process
 * resolves, taking precedence over both deploy transports — the `database`
 * config section and ENV_DATABASE_BINDINGS. The test provider pins the
 * binding of its provisioned, isolated databases here (in addition to the env
 * var) so a managed config binding present in the test environment (e.g. via
 * CONFIG_DATA) can never silently route the suite at the managed databases.
 *
 * Returns the previous override so nested pin/restore sequences (provision →
 * applyInto) unwind correctly; pass undefined to clear.
 */
export function setDatabaseBindingOverride(raw: string | undefined): string | undefined {
  const prev = bindingOverride;
  bindingOverride = raw;
  return prev;
}

/**
 * resolveDatabaseBinding resolves datasource into connection overrides from
 * the managed binding a deploy target injected. Precedence: a programmatic
 * pin (setDatabaseBindingOverride — the test provider's provisioned binding)
 * wins over everything; then the document carried by the `database` section
 * of the resolved config (see CONFIG_SECTION_DATABASE); then the
 * ENV_DATABASE_BINDINGS env var. Config beats env; both are built from the
 * same ledger, so retiring the env transport changes no value. It returns
 * undefined when no transport
 * carries a binding (raw unset or blank), so the factory falls back to
 * file/`useConfig` configuration; it throws when the injected binding is
 * malformed or omits the datasource. Passing `raw` explicitly bypasses every
 * transport (test seam).
 */
export function resolveDatabaseBinding(datasource: string, raw?: string): ResolvedDatabaseBinding | undefined {
  const injected = raw ?? bindingOverride ?? configBindingDocument() ?? process.env[ENV_DATABASE_BINDINGS];
  const trimmed = injected?.trim();
  if (!trimmed) {
    return undefined;
  }
  const binding = parseDatabaseBinding(trimmed);
  return resolveBindingEntry(binding, datasource);
}

/**
 * dsnToConfig parses a postgres:// connection string into the structured config
 * fields the connection factory consumes — the same decomposition pgx performs
 * for the Go adapter, so a DSN binding yields uniform semantics. The Cloud SQL
 * `?host=/cloudsql/...` form (empty URL host, host in the query) is honored, and
 * `sslmode` maps to the ssl flag (disable → false, anything else → true), and
 * the pgx pool parameters are taken out of the query (see splitPoolParams).
 * Remaining query parameters are returned as connectionParams.
 */
function dsnToConfig(dsn: string): ResolvedDatabaseBinding {
  let url: URL;
  try {
    url = new URL(dsn);
  } catch {
    fail(`connection dsn is not a valid postgres URL: ${dsn}`);
  }
  const params = new Map<string, string>();
  for (const [k, v] of url.searchParams.entries()) {
    params.set(k, v);
  }

  const config: Partial<InferConfig<typeof PostgresConfig>> = {};
  const hostParam = params.get('host');
  if (url.hostname) {
    config.host = decodeURIComponent(url.hostname);
    if (url.port) {
      config.port = Number(url.port);
    }
  } else if (hostParam) {
    config.host = hostParam;
  }
  params.delete('host');

  const database = decodeURIComponent(url.pathname.replace(/^\//, ''));
  if (database) {
    config.database = database;
  }
  if (url.username) {
    config.user = decodeURIComponent(url.username);
  }
  if (url.password) {
    config.password = decodeURIComponent(url.password);
  }
  const sslmode = params.get('sslmode');
  if (sslmode) {
    config.ssl = sslmode !== 'disable';
    params.delete('sslmode');
  }

  const { pool, connectionParams } = splitPoolParams(params.size > 0 ? Object.fromEntries(params) : undefined);
  return { config: { ...config, ...pool }, connectionParams };
}

function pruneUndefined<T extends Record<string, unknown>>(obj: T): Partial<T> {
  const out: Partial<T> = {};
  for (const [k, v] of Object.entries(obj)) {
    if (v !== undefined) {
      (out as Record<string, unknown>)[k] = v;
    }
  }
  return out;
}

/** Test-provider behaviour when no usable binding/provider exists. */
export type TestMode = 'skip' | 'require' | 'auto';
/** Per-datasource isolation boundary the test provider creates. */
export type TestIsolation = 'database' | 'schema';
/** Migrated-database caching policy. */
export type TestReuse = 'none' | 'bundle-template';

/**
 * DatabaseTestBinding is the canonical test binding: a runtime binding extended
 * with the test provisioning/isolation policy, keyed by logical datasource name.
 * It mirrors `go.putnami.dev/protocol/database` TestBinding so the Go and TS test
 * providers read the same contract.
 */
export interface DatabaseTestBinding {
  $schema?: string;
  protocolVersion: number;
  mode?: TestMode;
  isolation?: TestIsolation;
  reuse?: TestReuse;
  applyMigrations?: boolean;
  /**
   * keepDatabases, when true, has the provider leave the isolated databases it
   * created in place: the per-suite teardown closes its connections but skips
   * DROP DATABASE. Set it when the server's own lifetime is the cleanup —
   * e.g. a CI server that dies with the run — where every DROP forces an
   * immediate cluster-wide checkpoint that stalls behind the whole fleet's
   * concurrent writes.
   */
  keepDatabases?: boolean;
  databases?: Record<string, DatabaseBindingEntry>;
}

const TEST_MODES: ReadonlySet<string> = new Set<TestMode>(['skip', 'require', 'auto']);
const TEST_ISOLATIONS: ReadonlySet<string> = new Set<TestIsolation>(['database', 'schema']);
const TEST_REUSES: ReadonlySet<string> = new Set<TestReuse>(['none', 'bundle-template']);

/**
 * parseTestBinding strictly parses and validates a canonical test binding,
 * applying the same datasource/connection invariants as parseDatabaseBinding
 * plus the closed test-policy enums (mode/isolation/reuse). It throws a
 * descriptive error on any violation.
 */
export function parseTestBinding(raw: string): DatabaseTestBinding {
  let doc: DatabaseTestBinding;
  try {
    doc = JSON.parse(raw) as DatabaseTestBinding;
  } catch (err) {
    fail(`invalid JSON: ${err instanceof Error ? err.message : String(err)}`);
  }
  if (typeof doc !== 'object' || doc === null) {
    fail('test binding must be a JSON object');
  }
  if (doc.protocolVersion !== DATABASE_PROTOCOL_VERSION) {
    fail(`unsupported protocolVersion ${String(doc.protocolVersion)} (supported: ${DATABASE_PROTOCOL_VERSION})`);
  }
  if (doc.mode !== undefined && !TEST_MODES.has(doc.mode)) {
    fail(`invalid mode ${JSON.stringify(doc.mode)} (expected skip | require | auto)`);
  }
  if (doc.isolation !== undefined && !TEST_ISOLATIONS.has(doc.isolation)) {
    fail(`invalid isolation ${JSON.stringify(doc.isolation)} (expected database | schema)`);
  }
  if (doc.reuse !== undefined && !TEST_REUSES.has(doc.reuse)) {
    fail(`invalid reuse ${JSON.stringify(doc.reuse)} (expected none | bundle-template)`);
  }
  for (const [name, entry] of Object.entries(doc.databases ?? {})) {
    if (!DATASOURCE_NAME_PATTERN.test(name)) {
      fail(`datasource name ${JSON.stringify(name)} is not a canonical identifier`);
    }
    validateEntry(name, entry);
  }
  return doc;
}
