import { type InferConfig, useConfig } from '@putnami/runtime';
import type postgres from 'postgres';
import {
  type PoolHealthStats,
  recordPoolClosed,
  recordPoolCount,
  recordPoolCreated,
  recordPoolHealth,
  recordPoolSize,
} from './observability';
import { type ResolvedDatabaseBinding, resolveDatabaseBinding } from './postgres/binding';
import { PostgresConfig } from './postgres/config';
import { createDatasourceClient } from './postgres/datasource-client';
import { applyConnectionParams, buildOptions } from './postgres/options';
import { acquirePhysical, endAllPhysicalPools, type PhysicalTuning, physicalPoolCount } from './postgres/physical-pool';
import { primarySearchPath, resolveDatasourceName } from './primary-datasource';
import type { SqlClient } from './sql-client';

export { physicalPoolCount } from './postgres/physical-pool';

/**
 * One opened datasource: its logical, datasource-bound client (what
 * `database(name)` hands out) plus the release of its handle on the shared
 * physical pool.
 */
interface LogicalEntry {
  client: SqlClient;
  connectionName?: string;
}

/**
 * The logical registry: datasource key → opened client. Datasources are
 * logical; physical pools are per database. Two datasources whose effective
 * connections are byte-identical (several owned schemas of one database) get
 * two distinct logical clients over ONE physical pool (see
 * postgres/physical-pool.ts and doc/adr/0003); what differs between them —
 * `search_path` — is set per operation by the client, never as a startup
 * parameter of the shared pool.
 */
const registry = new Map<string, LogicalEntry>();
/** In-flight first opens of one datasource, so concurrent callers share one open. */
const loading = new Map<string, Promise<SqlClient>>();

function buildKey(config: InferConfig<typeof PostgresConfig>, connectionName?: string): string {
  const prefix = connectionName ? `[${connectionName}]` : '';
  return `${prefix}postgres://${config.user ?? ''}@${config.host}:${config.port}/${config.database}`;
}

function resolveConnectionTarget(
  name: string | undefined,
  init?: InferConfig<typeof PostgresConfig>,
): { resolvedName: string; connectionName?: string } {
  const resolvedName = init === undefined ? resolveDatasourceName(name) || 'default' : name || 'default';
  const connectionName = name || (init === undefined && resolvedName !== 'default' ? resolvedName : undefined);
  return { resolvedName, connectionName };
}

/**
 * effectiveConfig resolves the connection config for a logical datasource. When
 * the deployer injected a canonical database binding — the document merged into
 * the `database` config section, falling back to DATABASE_BINDINGS (see
 * resolveDatabaseBinding) — the named datasource's connection and owning schema
 * are taken from it and are authoritative: the binding's connection fields
 * override any committed `database.<name>` config, while tuning (poolSize,
 * timeouts) still comes from config/defaults. With no binding, behaviour is
 * unchanged: config drives the connection. This is the TypeScript counterpart
 * of the Go adapter's datasource-from-binding resolution, so both languages
 * share one contract.
 */
function effectiveConfig(
  name: string | undefined,
  init?: InferConfig<typeof PostgresConfig>,
): {
  conf: InferConfig<typeof PostgresConfig>;
  binding?: ResolvedDatabaseBinding;
  resolvedName: string;
  connectionName?: string;
} {
  const { resolvedName, connectionName } = resolveConnectionTarget(name, init);
  const path = `database.${resolvedName}`;
  const binding = resolveDatabaseBinding(resolvedName);
  // Pass the binding connection as confInit so a required field (database) is
  // satisfied even with no config file; then re-apply it on top so the binding
  // wins over any committed config.
  const confInit = binding ? ({ ...binding.config, ...(init ?? {}) } as InferConfig<typeof PostgresConfig>) : init;
  const base = useConfig(PostgresConfig, { path, confInit });
  const conf = binding ? { ...base, ...binding.config } : base;
  return { conf, binding, resolvedName, connectionName };
}

/**
 * The startup parameters that belong to the pool's tuning, not to its
 * identity: two datasources that differ on them resolve to the same physical
 * database and are reported as a tuning disagreement, not silently given two
 * pools.
 */
const TUNING_STARTUP_PARAMS = new Set(['statement_timeout', 'idle_in_transaction_session_timeout']);

/**
 * connectionIdentity renders the effective connection of a datasource — the
 * built options minus tuning and minus `search_path`: host, port, database,
 * the resolved username, the password source, ssl, and the remaining startup
 * parameters, in a fixed key order. Byte-identical identities share one
 * physical pool. It carries the password, so it is never logged or put in an
 * error.
 */
function connectionIdentity(
  conf: InferConfig<typeof PostgresConfig>,
  options: postgres.Options<Record<string, postgres.PostgresType>>,
): string {
  const explicitPassword = conf.password?.trim();
  const passwordSource = explicitPassword
    ? `explicit:${explicitPassword}`
    : conf.host.startsWith('/')
      ? 'iam-token'
      : 'none';
  const startup = Object.entries((options.connection ?? {}) as Record<string, string>)
    .filter(([key]) => key !== 'search_path' && !TUNING_STARTUP_PARAMS.has(key))
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
  return JSON.stringify({
    host: options.host,
    port: options.port ?? null,
    database: options.database,
    username: options.username,
    password: passwordSource,
    ssl: options.ssl ?? false,
    startup,
  });
}

/** tuningOf projects the config onto the compared subset (see PhysicalTuning). */
function tuningOf(conf: InferConfig<typeof PostgresConfig>): PhysicalTuning {
  return {
    poolSize: conf.poolSize,
    connectTimeout: conf.connectTimeout,
    idleTimeout: conf.idleTimeout,
    maxLifetime: conf.maxLifetime,
    statementTimeoutMs: conf.statementTimeoutMs,
    idleInTransactionTimeoutMs: conf.idleInTransactionTimeoutMs,
    debug: conf.debug,
  };
}

/**
 * Get or create the datasource-bound client for a datasource.
 *
 * Every call for one datasource returns the same client. Datasources whose
 * effective connections are identical share one physical pool of `poolSize`
 * connections and must declare the same tuning — a disagreement rejects with
 * an error naming both datasources and each differing field. The client sets
 * the datasource's `search_path` on every connection it uses, so unqualified
 * queries resolve in the datasource's schema whichever datasource last used
 * that connection.
 *
 * @param name Optional datasource name (e.g., 'auth', 'wealth').
 *             When provided, loads config from 'database.${name}' path.
 * @param config Optional explicit config (overrides name parameter).
 */
export const database = async (name?: string, _config?: InferConfig<typeof PostgresConfig>): Promise<SqlClient> => {
  const { conf, binding, resolvedName, connectionName } = effectiveConfig(name, _config);

  const key = buildKey(conf, connectionName);

  const entry = registry.get(key);
  if (entry) {
    return entry.client;
  }
  const inflight = loading.get(key);
  if (inflight) {
    return inflight;
  }

  const open = openDatasource(key, conf, binding, resolvedName, connectionName).finally(() => {
    loading.delete(key);
  });
  loading.set(key, open);
  return open;
};

async function openDatasource(
  key: string,
  conf: InferConfig<typeof PostgresConfig>,
  binding: ResolvedDatabaseBinding | undefined,
  resolvedName: string,
  connectionName: string | undefined,
): Promise<SqlClient> {
  const options = await buildOptions(conf);
  // search_path is not a startup parameter: the shared physical pool has no
  // single value, so the datasource-bound client sets it per operation. A
  // binding's extra connection params (e.g. application_name) ride the startup
  // packet and are part of the connection identity.
  applyConnectionParams(options, undefined, binding?.connectionParams);
  // A binding's schema always wins; without a binding, the primary datasource
  // may declare one for local dev (see primarySearchPath).
  const searchPath = binding ? binding.searchPath : primarySearchPath(resolvedName);
  const datasource = connectionName ?? resolvedName;

  const before = physicalPoolCount();
  const physical = await acquirePhysical(connectionIdentity(conf, options), datasource, options, tuningOf(conf));
  const client = createDatasourceClient(physical.sql, {
    datasource,
    searchPath,
    onEnd: async (endOptions) => {
      if (registry.get(key)?.client === client) {
        registry.delete(key);
      }
      await physical.release(endOptions);
      if (physical.isClosed()) {
        recordPoolClosed(connectionName);
      }
      recordPoolCount(physicalPoolCount());
    },
  });

  registry.set(key, { client, connectionName });
  if (physicalPoolCount() > before) {
    // A new PHYSICAL pool: the lifecycle counters count databases reached, not
    // datasources opened.
    recordPoolCreated(connectionName);
    recordPoolSize(conf.poolSize);
  }
  recordPoolCount(physicalPoolCount());
  return client;
}

/**
 * Close a specific datasource by name: releases its handle on the shared
 * physical pool, which ends when the last datasource of that database closes —
 * so closing one datasource never interrupts another datasource of the same
 * database. A later `database(name)` re-opens.
 * @param name Optional datasource name (e.g., 'auth', 'wealth').
 *             When provided, closes the connection for that database.
 *             When not provided, closes the default database connection.
 */
export const closeDatabase = async (name?: string): Promise<void> => {
  const { connectionName } = resolveConnectionTarget(name);
  try {
    const { conf } = effectiveConfig(name);
    const key = buildKey(conf, connectionName);
    const entry = registry.get(key);
    if (!entry) {
      return;
    }
    await entry.client.end();
  } catch {
    // Config doesn't exist or connection not found - check the registry by the
    // exact `[name]` key prefix buildKey produces, for a connection created
    // while its config is missing. A substring match would be wrong: closing
    // 'auth' must not close 'auth_replica'.
    if (connectionName) {
      const prefix = `[${connectionName}]`;
      for (const [key, entry] of registry.entries()) {
        if (key.startsWith(prefix)) {
          await entry.client.end();
          return;
        }
      }
    }
    // Silently return if no connection found
  }
};

/**
 * Close every datasource and end every physical pool, whatever its reference
 * count. A later `database(name)` re-opens.
 */
export const closeAllDatabases = async (): Promise<void> => {
  const entries = Array.from(registry.values());
  registry.clear();
  await Promise.all(entries.map((entry) => entry.client.end()));
  await endAllPhysicalPools();
  recordPoolCount(0);
};

/**
 * Ping the named datasource. Resolves the client (creating it if necessary)
 * and runs a trivial `SELECT 1` through it — under the datasource's
 * `search_path`, on the shared pool. Used by the platform plugin's `/healthz`
 * to surface pool connectivity under the `database` key.
 *
 * `database(name)` creates the pool on first use, so the first `/healthz` after
 * boot can synthesise the pool under the platform plugin's `probeTimeoutMs`. A
 * probe that can't materialise the pool surfaces unhealthy rather than masking
 * a misconfiguration, so the first probe carries connection-establishment cost
 * on top of `SELECT 1`. Subsequent probes reuse the cached connection.
 *
 * @param name Optional datasource name. Uses the declared primary/default connection when omitted.
 */
export const pingDatabase = async (name?: string): Promise<void> => {
  const conn = await database(name);
  await conn`SELECT 1`;
};

/**
 * Collect connection pool health stats by querying pg_stat_activity through
 * the datasource's logical client. Records gauge metrics for active, idle, and
 * total connections. `max` is `poolSize` — the ceiling of the PHYSICAL pool,
 * shared by every datasource of that database.
 *
 * Call this from a health check endpoint or periodic metric collector.
 *
 * @param name Optional datasource name. Uses the declared primary/default connection when omitted.
 */
export const collectPoolHealth = async (name?: string): Promise<PoolHealthStats | undefined> => {
  const { conf, connectionName } = effectiveConfig(name);
  const key = buildKey(conf, connectionName);
  const conn = registry.get(key)?.client;
  if (!conn) return undefined;

  const [row] = await conn`
    SELECT
      count(*)::int AS total,
      count(*) FILTER (WHERE state = 'active')::int AS active,
      count(*) FILTER (WHERE state = 'idle')::int AS idle,
      count(*) FILTER (WHERE state = 'idle in transaction')::int AS idle_in_transaction
    FROM pg_stat_activity
    WHERE pid != pg_backend_pid()
      AND datname = current_database()
  `;

  const stats: PoolHealthStats = {
    total: row['total'],
    active: row['active'],
    idle: row['idle'],
    idleInTransaction: row['idleInTransaction'],
    max: conf.poolSize,
  };

  recordPoolHealth(stats);
  return stats;
};
